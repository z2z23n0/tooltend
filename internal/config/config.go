package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/z2z23n0/tooltend/internal/model"
)

const ConfigVersion = 1

type Config struct {
	Version  int              `toml:"version" json:"version"`
	Agents   []model.HostKind `toml:"agents" json:"agents"`
	Projects []string         `toml:"projects" json:"projects"`
	Check    CheckConfig      `toml:"check" json:"check"`
	Notify   NotifyConfig     `toml:"notify" json:"notify"`
	Runtime  RuntimeConfig    `toml:"runtime" json:"runtime"`
}

type CheckConfig struct {
	Interval time.Duration `toml:"interval" json:"interval"`
	Jitter   time.Duration `toml:"jitter" json:"jitter"`
}

type NotifyConfig struct {
	Mode model.NotifyMode `toml:"mode" json:"mode"`
}
type RuntimeConfig struct {
	ShimDir string `toml:"shim_dir,omitempty" json:"shim_dir,omitempty"`
}

func Default() Config {
	return Config{
		Version: ConfigVersion,
		Agents:  []model.HostKind{model.HostCodex, model.HostClaude},
		Check:   CheckConfig{Interval: 24 * time.Hour, Jitter: 30 * time.Minute},
		Notify:  NotifyConfig{Mode: model.NotifyFailures},
	}
}

func (c Config) Validate() error {
	if c.Version != ConfigVersion {
		return fmt.Errorf("config: unsupported version %d", c.Version)
	}
	if c.Check.Interval <= 0 {
		return errors.New("config: check interval must be positive")
	}
	if c.Check.Jitter < 0 || c.Check.Jitter >= c.Check.Interval {
		return errors.New("config: jitter must be non-negative and shorter than interval")
	}
	if err := c.Notify.Mode.Validate(); err != nil {
		return err
	}
	seen := make(map[model.HostKind]struct{}, len(c.Agents))
	for _, agent := range c.Agents {
		if err := agent.Validate(); err != nil {
			return err
		}
		if _, exists := seen[agent]; exists {
			return fmt.Errorf("config: duplicate agent %q", agent)
		}
		seen[agent] = struct{}{}
	}
	for _, project := range c.Projects {
		if !filepath.IsAbs(project) {
			return fmt.Errorf("config: project path must be absolute: %q", project)
		}
	}
	if c.Runtime.ShimDir != "" {
		if strings.ContainsAny(c.Runtime.ShimDir, "\x00\r\n") || !filepath.IsAbs(c.Runtime.ShimDir) {
			return fmt.Errorf("config: runtime shim directory must be an absolute path: %q", c.Runtime.ShimDir)
		}
	}
	return nil
}

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	var c Config
	if err := toml.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("config: decode %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func SaveAtomic(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	c.Projects = append([]string(nil), c.Projects...)
	sort.Strings(c.Projects)
	b, err := toml.Marshal(c)
	if err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	return writeAtomic(path, b, 0o600)
}

func writeAtomic(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: create parent: %w", err)
	}
	f, err := os.CreateTemp(dir, ".tooltend-*.tmp")
	if err != nil {
		return fmt.Errorf("config: create temp: %w", err)
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(mode); err != nil {
		return fmt.Errorf("config: chmod temp: %w", err)
	}
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("config: write temp: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("config: sync temp: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("config: close temp: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("config: replace %s: %w", path, err)
	}
	if d, openErr := os.Open(dir); openErr == nil {
		defer d.Close()
		if syncErr := d.Sync(); syncErr != nil {
			return fmt.Errorf("config: sync parent: %w", syncErr)
		}
	}
	return nil
}
