package adapter

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

type SourceKind string

const (
	SourceGit      SourceKind = "git"
	SourceNPM      SourceKind = "npm"
	SourcePyPI     SourceKind = "pypi"
	SourceHomebrew SourceKind = "homebrew"
	SourceHTTP     SourceKind = "http"
	SourceLocal    SourceKind = "local"
	SourceUnknown  SourceKind = "unknown"
)

type Source struct {
	Kind        SourceKind        `json:"kind"`
	Locator     string            `json:"locator"`
	PackageName string            `json:"package_name,omitempty"`
	Subdir      string            `json:"subdir,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type Track struct {
	Channel    string `json:"channel"`
	Constraint string `json:"constraint,omitempty"`
}

type Resolved struct {
	Version   string            `json:"version"`
	Ref       string            `json:"ref"`
	Integrity string            `json:"integrity,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type Artifact struct {
	Root       string `json:"root"`
	Integrity  string `json:"integrity,omitempty"`
	Executable string `json:"executable,omitempty"`
}

type Capabilities struct {
	Check       bool `json:"check"`
	Stage       bool `json:"stage"`
	ManagedAuto bool `json:"managed_auto"`
	Rollback    bool `json:"rollback"`
	RemoteOnly  bool `json:"remote_only"`
}

type Adapter interface {
	Name() string
	Kinds() []SourceKind
	Capabilities() Capabilities
	Normalize(Source) (Source, error)
	Resolve(context.Context, Source, Track) (Resolved, error)
	Fetch(context.Context, Source, Resolved, string) (Artifact, error)
	Verify(context.Context, Source, Resolved, Artifact) error
}

type Registry struct {
	byKind map[SourceKind]Adapter
}

func NewRegistry(adapters ...Adapter) (*Registry, error) {
	registry := &Registry{byKind: make(map[SourceKind]Adapter)}
	for _, item := range adapters {
		if item == nil {
			return nil, errors.New("nil adapter")
		}
		for _, kind := range item.Kinds() {
			if _, exists := registry.byKind[kind]; exists {
				return nil, fmt.Errorf("duplicate adapter for %s", kind)
			}
			registry.byKind[kind] = item
		}
	}
	return registry, nil
}

func (r *Registry) For(kind SourceKind) (Adapter, bool) {
	if r == nil {
		return nil, false
	}
	item, ok := r.byKind[kind]
	return item, ok
}

func ValidateSubdir(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "." {
		return "", nil
	}
	if filepath.IsAbs(value) || strings.ContainsAny(value, "\\\x00\r\n") {
		return "", errors.New("source subdirectory must be relative")
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("source subdirectory escapes source root")
	}
	return strings.TrimPrefix(clean, "./"), nil
}

type Unsupported struct {
	Kind SourceKind
}

func (a Unsupported) Name() string                            { return "unsupported" }
func (a Unsupported) Kinds() []SourceKind                     { return []SourceKind{a.Kind} }
func (a Unsupported) Capabilities() Capabilities              { return Capabilities{} }
func (a Unsupported) Normalize(source Source) (Source, error) { return source, nil }
func (a Unsupported) Resolve(context.Context, Source, Track) (Resolved, error) {
	return Resolved{}, errors.New("source cannot be resolved safely")
}
func (a Unsupported) Fetch(context.Context, Source, Resolved, string) (Artifact, error) {
	return Artifact{}, errors.New("source cannot be staged safely")
}
func (a Unsupported) Verify(context.Context, Source, Resolved, Artifact) error {
	return errors.New("source cannot be verified safely")
}
