package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"sort"

	"github.com/pelletier/go-toml/v2"
	"github.com/z2z23n0/tooltend/internal/model"
)

type ProjectManifest struct {
	Version    int                `toml:"version" json:"version"`
	Components []ProjectComponent `toml:"component" json:"components"`
}

type ProjectComponent struct {
	Name       string              `toml:"name" json:"name"`
	Source     string              `toml:"source" json:"source"`
	Kind       model.ComponentKind `toml:"kind" json:"kind"`
	Agents     []model.HostKind    `toml:"agents" json:"agents"`
	Track      model.TrackChannel  `toml:"track" json:"track"`
	Constraint string              `toml:"constraint,omitempty" json:"constraint,omitempty"`
}

type ProjectLock struct {
	Version    int                    `toml:"version" json:"version"`
	Components []ProjectLockComponent `toml:"component" json:"components"`
}

type ProjectLockComponent struct {
	LogicalKey      string `toml:"logical_key" json:"logical_key"`
	ResolvedVersion string `toml:"resolved_version,omitempty" json:"resolved_version,omitempty"`
	Commit          string `toml:"commit,omitempty" json:"commit,omitempty"`
	Integrity       string `toml:"integrity" json:"integrity"`
}

func (m ProjectManifest) Validate() error {
	if m.Version != ConfigVersion {
		return fmt.Errorf("config: unsupported project manifest version %d", m.Version)
	}
	seen := make(map[string]struct{}, len(m.Components))
	for i, component := range m.Components {
		if component.Name == "" || component.Source == "" {
			return fmt.Errorf("config: component %d requires name and source", i)
		}
		if err := component.Kind.Validate(); err != nil {
			return err
		}
		if err := component.Track.Validate(); err != nil {
			return err
		}
		for _, agent := range component.Agents {
			if err := agent.Validate(); err != nil {
				return err
			}
		}
		key := component.Name + "\x00" + component.Source + "\x00" + string(component.Kind)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("config: duplicate project component %q", component.Name)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func LoadProjectManifest(path string) (ProjectManifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ProjectManifest{}, fmt.Errorf("config: read project manifest: %w", err)
	}
	var m ProjectManifest
	if err := toml.Unmarshal(b, &m); err != nil {
		return ProjectManifest{}, fmt.Errorf("config: decode project manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return ProjectManifest{}, err
	}
	return m, nil
}

func SaveProjectManifestAtomic(path string, manifest ProjectManifest) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	b, err := toml.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("config: encode project manifest: %w", err)
	}
	return writeAtomic(path, b, 0o644)
}

func LoadProjectLock(path string) (ProjectLock, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ProjectLock{}, fmt.Errorf("config: read project lock: %w", err)
	}
	var lock ProjectLock
	if err := toml.Unmarshal(b, &lock); err != nil {
		return ProjectLock{}, fmt.Errorf("config: decode project lock: %w", err)
	}
	if err := lock.Validate(); err != nil {
		return ProjectLock{}, err
	}
	return lock, nil
}

func (l ProjectLock) Validate() error {
	if l.Version != ConfigVersion {
		return fmt.Errorf("config: unsupported project lock version %d", l.Version)
	}
	seen := make(map[string]struct{}, len(l.Components))
	for _, component := range l.Components {
		if component.LogicalKey == "" || component.Integrity == "" {
			return fmt.Errorf("config: lock entry requires logical_key and integrity")
		}
		digest, err := hex.DecodeString(component.Integrity)
		if err != nil || len(digest) != 32 {
			return fmt.Errorf("config: lock entry %q requires a SHA-256 integrity hash", component.LogicalKey)
		}
		if _, exists := seen[component.LogicalKey]; exists {
			return fmt.Errorf("config: duplicate lock entry %q", component.LogicalKey)
		}
		seen[component.LogicalKey] = struct{}{}
	}
	return nil
}

func SaveProjectLockAtomic(path string, lock ProjectLock) error {
	if err := lock.Validate(); err != nil {
		return err
	}
	lock.Components = append([]ProjectLockComponent(nil), lock.Components...)
	sort.Slice(lock.Components, func(i, j int) bool { return lock.Components[i].LogicalKey < lock.Components[j].LogicalKey })
	b, err := toml.Marshal(lock)
	if err != nil {
		return fmt.Errorf("config: encode project lock: %w", err)
	}
	return writeAtomic(path, b, 0o644)
}
