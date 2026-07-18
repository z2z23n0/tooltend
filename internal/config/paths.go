package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const EnvHome = "TOOLTEND_HOME"

type Paths struct {
	ConfigDir      string `json:"config_dir"`
	ConfigFile     string `json:"config_file"`
	StateDir       string `json:"state_dir"`
	LogsDir        string `json:"logs_dir"`
	DatabaseFile   string `json:"database_file"`
	DataDir        string `json:"data_dir"`
	ObjectsDir     string `json:"objects_dir"`
	StagingDir     string `json:"staging_dir"`
	GenerationsDir string `json:"generations_dir"`
	RuntimesDir    string `json:"runtimes_dir"`
	ActivationLock string `json:"activation_lock"`
	ShimDir        string `json:"shim_dir"`
}

// Resolve only calculates paths. It never creates, opens, or mutates them.
func Resolve() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("config: resolve home: %w", err)
	}
	return ResolveWith(home, os.Getenv), nil
}

// ResolveWith is the deterministic, side-effect free form used by init and tests.
func ResolveWith(home string, getenv func(string) string) Paths {
	if override := strings.TrimSpace(getenv(EnvHome)); override != "" {
		root := cleanRoot(override, home)
		return pathsFor(root, root, root, filepath.Join(root, "bin"))
	}
	configRoot := xdgRoot(getenv("XDG_CONFIG_HOME"), home, ".config")
	stateRoot := xdgRoot(getenv("XDG_STATE_HOME"), home, filepath.Join(".local", "state"))
	dataRoot := xdgRoot(getenv("XDG_DATA_HOME"), home, filepath.Join(".local", "share"))
	return pathsFor(
		filepath.Join(configRoot, "tooltend"),
		filepath.Join(stateRoot, "tooltend"),
		filepath.Join(dataRoot, "tooltend"),
		filepath.Join(home, ".local", "bin"),
	)
}

func pathsFor(configDir, stateDir, dataDir, shimDir string) Paths {
	return Paths{
		ConfigDir:      configDir,
		ConfigFile:     filepath.Join(configDir, "config.toml"),
		StateDir:       stateDir,
		LogsDir:        filepath.Join(stateDir, "logs"),
		DatabaseFile:   filepath.Join(stateDir, "state.db"),
		DataDir:        dataDir,
		ObjectsDir:     filepath.Join(dataDir, "objects"),
		StagingDir:     filepath.Join(dataDir, "staging"),
		GenerationsDir: filepath.Join(dataDir, "generations"),
		RuntimesDir:    filepath.Join(dataDir, "runtimes"),
		ActivationLock: filepath.Join(stateDir, "activation.lock"),
		ShimDir:        shimDir,
	}
}

func xdgRoot(value, home, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" || !filepath.IsAbs(value) {
		return filepath.Join(home, fallback)
	}
	return cleanRoot(value, home)
}

func cleanRoot(value, home string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "~/") {
		value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(home, value)
	}
	return filepath.Clean(value)
}

// Ensure creates ToolTend-owned roots. Call it only after a confirmed write plan.
func (p Paths) Ensure() error {
	for _, dir := range []string{p.ConfigDir, p.StateDir, p.LogsDir, p.ObjectsDir, p.StagingDir, p.GenerationsDir, p.RuntimesDir} {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("config: create %s: %w", dir, err)
		}
	}
	return nil
}
