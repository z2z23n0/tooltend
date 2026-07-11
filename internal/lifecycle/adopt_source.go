package lifecycle

import (
	"errors"
	"fmt"
	"strings"

	"github.com/z2z23n0/tooltend/internal/adapter"
)

type resolvedAdoptSource struct {
	Source   adapter.Source
	Provider adapter.Adapter
	Identity string
}

// resolveAdoptSource is shared by preview and mutation so a Git monorepo
// subdirectory is normalized and bound to the same confirmed source identity.
func (s *Service) resolveAdoptSource(options AdoptOptions) (resolvedAdoptSource, error) {
	rawSource, err := parseAdoptSource(options.Source)
	if err != nil {
		return resolvedAdoptSource{}, err
	}
	requestedSubdir := strings.TrimSpace(options.Subdir)
	if requestedSubdir != "" {
		if rawSource.Kind != adapter.SourceGit {
			return resolvedAdoptSource{}, errors.New("lifecycle: --subdir is supported only for Git sources")
		}
		rawSource.Subdir, err = adapter.ValidateSubdir(requestedSubdir)
		if err != nil {
			return resolvedAdoptSource{}, fmt.Errorf("lifecycle: invalid Git source subdirectory: %w", err)
		}
	}
	provider, ok := s.Adapters.For(rawSource.Kind)
	if !ok {
		return resolvedAdoptSource{}, fmt.Errorf("lifecycle: no adapter for source %s", rawSource.Kind)
	}
	normalized, err := provider.Normalize(rawSource)
	if err != nil {
		return resolvedAdoptSource{}, err
	}
	if normalized.Kind == adapter.SourceGit {
		normalized.Subdir, err = adapter.ValidateSubdir(normalized.Subdir)
		if err != nil {
			return resolvedAdoptSource{}, fmt.Errorf("lifecycle: invalid Git source subdirectory: %w", err)
		}
	} else if normalized.Subdir != "" {
		return resolvedAdoptSource{}, errors.New("lifecycle: source subdirectories are supported only for Git sources")
	}
	capabilities := provider.Capabilities()
	if capabilities.RemoteOnly || !capabilities.Stage {
		return resolvedAdoptSource{}, errors.New("lifecycle: this adapter is observe-only and cannot be adopted safely")
	}
	identity, err := stableHash(struct {
		Kind, Locator, Package, Subdir string
	}{string(normalized.Kind), normalized.Locator, normalized.PackageName, normalized.Subdir})
	if err != nil {
		return resolvedAdoptSource{}, err
	}
	return resolvedAdoptSource{Source: normalized, Provider: provider, Identity: identity}, nil
}
