package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Name string

const (
	Codex  Name = "codex"
	Claude Name = "claude"
)

type Scope string

const (
	ScopeSystem  Scope = "system"
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
	ScopeLocal   Scope = "local"
	ScopePlugin  Scope = "plugin"
)

type ComponentKind string

const (
	ComponentSkill    ComponentKind = "skill"
	ComponentPlugin   ComponentKind = "plugin"
	ComponentHook     ComponentKind = "hook"
	ComponentStdioMCP ComponentKind = "stdio_mcp"
	ComponentHTTPMCP  ComponentKind = "http_mcp"
)

type ConfigSource struct {
	Host       Name   `json:"host"`
	Path       string `json:"path"`
	Format     string `json:"format"`
	Scope      Scope  `json:"scope"`
	Project    string `json:"project,omitempty"`
	Layer      string `json:"layer"`
	Precedence int    `json:"precedence"`
	Legacy     bool   `json:"legacy,omitempty"`
}

type Evidence struct {
	Path    string `json:"path"`
	Format  string `json:"format"`
	Layer   string `json:"layer"`
	Pointer string `json:"pointer,omitempty"`
}

// SourceRef is deliberately limited to source identity. It must never contain
// credentials, registry tokens, raw command lines, or environment values.
type SourceRef struct {
	Kind    string `json:"kind,omitempty"`
	Locator string `json:"locator,omitempty"`
	Subdir  string `json:"subdir,omitempty"`
	Package string `json:"package,omitempty"`
	Version string `json:"version,omitempty"`
	Ref     string `json:"ref,omitempty"`
}

// SecretRef records only that a secret-bearing field exists, and when safe,
// the environment variable or header name used to supply it.
type SecretRef struct {
	Field   string `json:"field"`
	Name    string `json:"name,omitempty"`
	Present bool   `json:"present"`
}

// DependencyRef is emitted only for an explicit extension reference. Carrier
// tools such as npx, node, bash, git and uvx are never represented as the
// dependency itself.
type DependencyRef struct {
	PackageIdentity string    `json:"package_identity"`
	Constraint      string    `json:"constraint,omitempty"`
	Source          SourceRef `json:"source"`
	Executable      string    `json:"executable,omitempty"`
	InstallPath     string    `json:"install_path,omitempty"`
	Carrier         string    `json:"carrier,omitempty"`
	EvidencePath    string    `json:"evidence_path"`
	EvidenceLine    int       `json:"evidence_line,omitempty"`
}

type Observation struct {
	Key          string            `json:"key"`
	Host         Name              `json:"host"`
	Kind         ComponentKind     `json:"kind"`
	Name         string            `json:"name"`
	Version      string            `json:"version,omitempty"`
	Path         string            `json:"path,omitempty"`
	Scope        Scope             `json:"scope"`
	Project      string            `json:"project,omitempty"`
	Transport    string            `json:"transport,omitempty"`
	Enabled      *bool             `json:"enabled,omitempty"`
	Legacy       bool              `json:"legacy,omitempty"`
	Source       SourceRef         `json:"source"`
	Evidence     []Evidence        `json:"evidence"`
	Secrets      []SecretRef       `json:"secrets,omitempty"`
	Dependencies []DependencyRef   `json:"dependencies,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type Binding struct {
	Host         Name   `json:"host"`
	ComponentKey string `json:"component_key"`
	Scope        Scope  `json:"scope"`
	Project      string `json:"project,omitempty"`
	InstallPath  string `json:"install_path,omitempty"`
	ConfigPath   string `json:"config_path,omitempty"`
	Enabled      *bool  `json:"enabled,omitempty"`
	Legacy       bool   `json:"legacy,omitempty"`
}

type Warning struct {
	Host    Name   `json:"host"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
	Project string `json:"project,omitempty"`
}

type Result struct {
	ConfigSources []ConfigSource `json:"config_sources"`
	Observations  []Observation  `json:"observations"`
	Bindings      []Binding      `json:"bindings"`
	Warnings      []Warning      `json:"warnings,omitempty"`
}

type ScanOptions struct {
	HomeDir          string
	CurrentProject   string
	Projects         []string
	CodexProfile     string
	CodexSystemDir   string
	ClaudeManagedDir string
	Getenv           func(string) string
}

func (o ScanOptions) normalized() (ScanOptions, error) {
	if o.HomeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ScanOptions{}, err
		}
		o.HomeDir = home
	}
	absHome, err := filepath.Abs(o.HomeDir)
	if err != nil {
		return ScanOptions{}, err
	}
	o.HomeDir = filepath.Clean(absHome)
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.CurrentProject != "" {
		o.CurrentProject, err = cleanAbsolute(o.CurrentProject)
		if err != nil {
			return ScanOptions{}, err
		}
	}
	projects := make([]string, 0, len(o.Projects)+1)
	if o.CurrentProject != "" {
		projects = append(projects, o.CurrentProject)
	}
	for _, project := range o.Projects {
		if strings.TrimSpace(project) == "" {
			continue
		}
		cleaned, cleanErr := cleanAbsolute(project)
		if cleanErr != nil {
			return ScanOptions{}, cleanErr
		}
		projects = append(projects, cleaned)
	}
	o.Projects = uniqueStrings(projects)
	if o.CodexSystemDir == "" {
		o.CodexSystemDir = "/etc/codex"
	}
	if o.ClaudeManagedDir == "" {
		o.ClaudeManagedDir = defaultClaudeManagedDir()
	}
	return o, nil
}

func cleanAbsolute(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

type Host interface {
	Name() Name
	Scan(context.Context, ScanOptions) (Result, error)
	PlanHookInstall(context.Context, HookInstallOptions) (MutationPlan, error)
}

func DefaultHosts() []Host {
	return []Host{NewCodex(), NewClaude()}
}

func ScanAll(ctx context.Context, options ScanOptions, hosts ...Host) (Result, error) {
	options, err := options.normalized()
	if err != nil {
		return Result{}, err
	}
	if len(hosts) == 0 {
		hosts = DefaultHosts()
	}
	var result Result
	for _, adapter := range hosts {
		if adapter == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		part, scanErr := adapter.Scan(ctx, options)
		if scanErr != nil {
			return result, scanErr
		}
		result.merge(part)
	}
	result.sortAndDedupe()
	return result, nil
}

func (r *Result) merge(other Result) {
	r.ConfigSources = append(r.ConfigSources, other.ConfigSources...)
	r.Observations = append(r.Observations, other.Observations...)
	r.Bindings = append(r.Bindings, other.Bindings...)
	r.Warnings = append(r.Warnings, other.Warnings...)
}

func (r *Result) sortAndDedupe() {
	r.ConfigSources = dedupe(r.ConfigSources, func(v ConfigSource) string {
		return string(v.Host) + "\x00" + v.Path + "\x00" + v.Layer
	})
	r.Observations = dedupe(r.Observations, func(v Observation) string {
		return v.Key + "\x00" + v.Path + "\x00" + firstEvidencePath(v.Evidence)
	})
	r.Bindings = dedupe(r.Bindings, func(v Binding) string {
		return string(v.Host) + "\x00" + v.ComponentKey + "\x00" + v.InstallPath + "\x00" + v.ConfigPath
	})
	r.Warnings = dedupe(r.Warnings, func(v Warning) string {
		return string(v.Host) + "\x00" + v.Code + "\x00" + v.Path + "\x00" + v.Message
	})
	sort.Slice(r.ConfigSources, func(i, j int) bool {
		if r.ConfigSources[i].Host != r.ConfigSources[j].Host {
			return r.ConfigSources[i].Host < r.ConfigSources[j].Host
		}
		if r.ConfigSources[i].Precedence != r.ConfigSources[j].Precedence {
			return r.ConfigSources[i].Precedence < r.ConfigSources[j].Precedence
		}
		return r.ConfigSources[i].Path < r.ConfigSources[j].Path
	})
	sort.Slice(r.Observations, func(i, j int) bool {
		if r.Observations[i].Host != r.Observations[j].Host {
			return r.Observations[i].Host < r.Observations[j].Host
		}
		if r.Observations[i].Kind != r.Observations[j].Kind {
			return r.Observations[i].Kind < r.Observations[j].Kind
		}
		if r.Observations[i].Name != r.Observations[j].Name {
			return r.Observations[i].Name < r.Observations[j].Name
		}
		return r.Observations[i].Path < r.Observations[j].Path
	})
	sort.Slice(r.Bindings, func(i, j int) bool {
		if r.Bindings[i].Host != r.Bindings[j].Host {
			return r.Bindings[i].Host < r.Bindings[j].Host
		}
		if r.Bindings[i].ComponentKey != r.Bindings[j].ComponentKey {
			return r.Bindings[i].ComponentKey < r.Bindings[j].ComponentKey
		}
		return r.Bindings[i].InstallPath < r.Bindings[j].InstallPath
	})
	sort.Slice(r.Warnings, func(i, j int) bool {
		if r.Warnings[i].Host != r.Warnings[j].Host {
			return r.Warnings[i].Host < r.Warnings[j].Host
		}
		if r.Warnings[i].Code != r.Warnings[j].Code {
			return r.Warnings[i].Code < r.Warnings[j].Code
		}
		return r.Warnings[i].Path < r.Warnings[j].Path
	})
}

func firstEvidencePath(evidence []Evidence) string {
	if len(evidence) == 0 {
		return ""
	}
	return evidence[0].Path
}

func dedupe[T any](values []T, key func(T) string) []T {
	seen := make(map[string]struct{}, len(values))
	result := make([]T, 0, len(values))
	for _, value := range values {
		k := key(value)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		result = append(result, value)
	}
	return result
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func boolPtr(value bool) *bool { return &value }

func componentKey(host Name, kind ComponentKind, source SourceRef, name string) string {
	raw := strings.Join([]string{string(host), string(kind), source.Kind, source.Locator, source.Subdir, source.Package, name}, "\x00")
	digest := sha256.Sum256([]byte(raw))
	return string(host) + ":" + string(kind) + ":" + hex.EncodeToString(digest[:12])
}
