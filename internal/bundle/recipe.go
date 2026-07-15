package bundle

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/z2z23n0/tooltend/internal/model"
)

const RecipeSchema = "bundle-recipe-v1"

const (
	DefaultResolveTimeout = 30 * time.Second
	DefaultInstallTimeout = 5 * time.Minute
	DefaultHealthTimeout  = 30 * time.Second
	DefaultRetries        = 3
)

//go:embed recipes/*.toml
var builtinRecipes embed.FS

type Recipe struct {
	Schema      string                 `toml:"schema" json:"schema"`
	ID          string                 `toml:"id" json:"id"`
	Version     string                 `toml:"version" json:"version"`
	Name        string                 `toml:"name" json:"name"`
	Owner       model.LifecycleOwner   `toml:"owner" json:"owner"`
	Confidence  model.BundleConfidence `toml:"confidence" json:"confidence"`
	Description string                 `toml:"description" json:"description,omitempty"`
	Artifacts   []ArtifactRecipe       `toml:"artifacts" json:"artifacts"`
	Source      string                 `toml:"-" json:"source"`
}

type ArtifactRecipe struct {
	Key          string             `toml:"key" json:"key"`
	Name         string             `toml:"name" json:"name"`
	Kind         model.ArtifactKind `toml:"kind" json:"kind"`
	Driver       string             `toml:"driver" json:"driver"`
	Source       string             `toml:"source" json:"source,omitempty"`
	Subdir       string             `toml:"subdir" json:"subdir,omitempty"`
	Required     bool               `toml:"required" json:"required"`
	Selectors    []Selector         `toml:"selectors" json:"selectors"`
	Probes       []string           `toml:"probes" json:"probes,omitempty"`
	ResolveArgv  []string           `toml:"resolve_argv" json:"resolve_argv,omitempty"`
	StageArgv    []string           `toml:"stage_argv" json:"stage_argv,omitempty"`
	ActivateArgv []string           `toml:"activate_argv" json:"activate_argv,omitempty"`
	RollbackArgv []string           `toml:"rollback_argv" json:"rollback_argv,omitempty"`
	HealthArgv   []string           `toml:"health_argv" json:"health_argv,omitempty"`
}

type Selector struct {
	Field    string `toml:"field" json:"field"`
	Equals   string `toml:"equals" json:"equals,omitempty"`
	Contains string `toml:"contains" json:"contains,omitempty"`
}

type Catalog struct {
	recipes map[string]Recipe
}

func LoadCatalog(localDir string) (Catalog, error) {
	result := Catalog{recipes: map[string]Recipe{}}
	if err := loadRecipeFS(builtinRecipes, "recipes", "builtin", result.recipes); err != nil {
		return Catalog{}, err
	}
	if localDir != "" {
		entries, err := os.ReadDir(localDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Catalog{}, fmt.Errorf("bundle recipes: read local directory: %w", err)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".toml" {
				continue
			}
			path := filepath.Join(localDir, entry.Name())
			info, err := os.Lstat(path)
			if err != nil {
				return Catalog{}, err
			}
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
				return Catalog{}, fmt.Errorf("bundle recipes: local recipe must be a regular file that is not group/world writable: %s", path)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return Catalog{}, err
			}
			recipe, err := decodeRecipe(data, "local")
			if err != nil {
				return Catalog{}, fmt.Errorf("bundle recipes: %s: %w", path, err)
			}
			// Local recipes may intentionally override a built-in recipe, but
			// configuration remains trust-gated by recipe_source=local.
			result.recipes[recipe.ID] = recipe
		}
	}
	return result, nil
}

func loadRecipeFS(files fs.FS, dir, source string, destination map[string]Recipe) error {
	entries, err := fs.ReadDir(files, dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".toml" {
			continue
		}
		data, err := fs.ReadFile(files, filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		recipe, err := decodeRecipe(data, source)
		if err != nil {
			return fmt.Errorf("bundle recipes: %s: %w", entry.Name(), err)
		}
		if _, exists := destination[recipe.ID]; exists {
			return fmt.Errorf("bundle recipes: duplicate recipe %q", recipe.ID)
		}
		destination[recipe.ID] = recipe
	}
	return nil
}

func decodeRecipe(data []byte, source string) (Recipe, error) {
	var value Recipe
	decoder := toml.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Recipe{}, err
	}
	value.Source = source
	if err := value.Validate(); err != nil {
		return Recipe{}, err
	}
	return value, nil
}

func (c Catalog) Recipes() []Recipe {
	result := make([]Recipe, 0, len(c.recipes))
	for _, recipe := range c.recipes {
		result = append(result, recipe)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (c Catalog) Get(id string) (Recipe, bool) {
	value, ok := c.recipes[id]
	return value, ok
}

func (r Recipe) Validate() error {
	if r.Schema != RecipeSchema {
		return fmt.Errorf("unsupported schema %q", r.Schema)
	}
	if !safeIdentifier(r.ID) || r.Version == "" || strings.TrimSpace(r.Name) == "" {
		return errors.New("recipe identity is incomplete")
	}
	if err := r.Owner.Validate(); err != nil {
		return err
	}
	if err := r.Confidence.Validate(); err != nil {
		return err
	}
	if len(r.Artifacts) == 0 {
		return errors.New("recipe has no artifacts")
	}
	seen := map[string]struct{}{}
	for index, artifact := range r.Artifacts {
		if !safeIdentifier(artifact.Key) || strings.TrimSpace(artifact.Name) == "" || strings.TrimSpace(artifact.Driver) == "" {
			return fmt.Errorf("artifact %d identity is incomplete", index)
		}
		if _, exists := seen[artifact.Key]; exists {
			return fmt.Errorf("duplicate artifact key %q", artifact.Key)
		}
		seen[artifact.Key] = struct{}{}
		if err := artifact.Kind.Validate(); err != nil {
			return err
		}
		if strings.ContainsAny(artifact.Source+artifact.Subdir, "\x00\r\n") {
			return fmt.Errorf("artifact %s source contains invalid characters", artifact.Key)
		}
		if artifact.Subdir != "" && (filepath.IsAbs(artifact.Subdir) || strings.HasPrefix(filepath.Clean(artifact.Subdir), "..")) {
			return fmt.Errorf("artifact %s source subdirectory must be relative", artifact.Key)
		}
		for _, selector := range artifact.Selectors {
			if err := selector.Validate(); err != nil {
				return fmt.Errorf("artifact %s: %w", artifact.Key, err)
			}
		}
		for _, probe := range artifact.Probes {
			if !strings.HasPrefix(probe, "path:") && !strings.HasPrefix(probe, "command:") {
				return fmt.Errorf("artifact %s: invalid probe %q", artifact.Key, probe)
			}
			if strings.ContainsAny(probe, "\x00\r\n") {
				return fmt.Errorf("artifact %s: invalid probe characters", artifact.Key)
			}
		}
		for name, argv := range map[string][]string{
			"resolve": artifact.ResolveArgv, "stage": artifact.StageArgv, "activate": artifact.ActivateArgv,
			"rollback": artifact.RollbackArgv, "health": artifact.HealthArgv,
		} {
			if err := validateStaticArgv(argv); err != nil {
				return fmt.Errorf("artifact %s %s argv: %w", artifact.Key, name, err)
			}
		}
		if r.Owner == model.LifecycleToolTend || r.Owner == model.LifecycleDelegated {
			if len(artifact.ActivateArgv) > 0 && len(artifact.RollbackArgv) == 0 && r.Owner == model.LifecycleToolTend {
				return fmt.Errorf("artifact %s: tooltend activation requires rollback argv", artifact.Key)
			}
		} else if len(artifact.ResolveArgv)+len(artifact.StageArgv)+len(artifact.ActivateArgv)+len(artifact.RollbackArgv) != 0 {
			return fmt.Errorf("artifact %s: observation-only owner cannot declare mutation argv", artifact.Key)
		}
	}
	return nil
}

func (s Selector) Validate() error {
	switch s.Field {
	case "name", "kind", "path", "package", "source", "dependency", "host":
	default:
		return fmt.Errorf("invalid selector field %q", s.Field)
	}
	if (s.Equals == "") == (s.Contains == "") {
		return errors.New("selector requires exactly one of equals or contains")
	}
	if strings.ContainsAny(s.Equals+s.Contains, "\x00\r\n") {
		return errors.New("selector contains invalid characters")
	}
	return nil
}

func validateStaticArgv(argv []string) error {
	if len(argv) == 0 {
		return nil
	}
	if strings.TrimSpace(argv[0]) == "" || strings.ContainsAny(argv[0], "/\\") {
		return errors.New("command must be a program name resolved from PATH")
	}
	allowedVariables := []string{"${version}", "${resolved_ref}", "${stage}", "${path}", "${previous_version}", "${rollback_version}"}
	for _, argument := range argv {
		if argument == "" || strings.ContainsAny(argument, "\x00\r\n") {
			return errors.New("argument is empty or contains control characters")
		}
		if strings.ContainsAny(argument, ";|&`><") || strings.Contains(argument, "$(") {
			return errors.New("shell syntax is not allowed")
		}
		for start := strings.Index(argument, "${"); start >= 0; start = strings.Index(argument, "${") {
			end := strings.Index(argument[start:], "}")
			if end < 0 {
				return errors.New("unterminated template variable")
			}
			variable := argument[start : start+end+1]
			allowed := false
			for _, candidate := range allowedVariables {
				if variable == candidate {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("template variable %q is not allowed", variable)
			}
			argument = argument[start+end+1:]
		}
	}
	return nil
}

func safeIdentifier(value string) bool {
	if value == "" || len(value) > 96 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}
