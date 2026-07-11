package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/z2z23n0/tooltend/internal/adapter"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/objectstore"
)

// ResolveUpdate resolves the exact immutable ref that an interactive preview
// is asking the user to approve. UpdateOptions.ExpectedRef binds the later
// mutating call to this value and prevents a moving tag or channel from being
// substituted after confirmation.
func (s *Service) ResolveUpdate(ctx context.Context, selector, bindingID string) (UpdateTarget, error) {
	component, bindings, err := s.Component(ctx, selector)
	if err != nil {
		return UpdateTarget{}, err
	}
	binding, err := SelectBinding(bindings, bindingID)
	if err != nil {
		return UpdateTarget{}, err
	}
	if component.SourceID == "" {
		return UpdateTarget{}, errors.New("lifecycle: component source is unknown")
	}
	source, err := s.Database.GetSource(ctx, component.SourceID)
	if err != nil {
		return UpdateTarget{}, err
	}
	value, err := s.loadPolicy(ctx, binding.ID)
	if err != nil {
		return UpdateTarget{}, err
	}
	provider, ok := s.Adapters.For(adapter.SourceKind(source.Kind))
	if !ok {
		return UpdateTarget{}, fmt.Errorf("lifecycle: no adapter for source %s", source.Kind)
	}
	if !provider.Capabilities().Check {
		return UpdateTarget{}, errors.New("lifecycle: adapter cannot resolve updates safely")
	}
	normalized, err := provider.Normalize(sourceForAdapter(source))
	if err != nil {
		return UpdateTarget{}, err
	}
	resolved, err := provider.Resolve(ctx, normalized, trackForAdapter(value))
	if err != nil {
		return UpdateTarget{}, fmt.Errorf("lifecycle: resolve update: %w", err)
	}
	if resolved.Ref == "" {
		return UpdateTarget{}, errors.New("lifecycle: adapter returned an empty resolved ref")
	}
	return UpdateTarget{
		ComponentID: component.ID, BindingID: binding.ID,
		ResolvedRef: resolved.Ref, Version: resolved.Version, ActiveGeneration: binding.ActiveGenerationID,
	}, nil
}

// ResolveAdopt captures the immutable upstream ref and current local/config
// fingerprints shown in an interactive adoption preview. Supplying all three
// values back through AdoptOptions makes the mutating operation fail closed if
// either side changes while the user is deciding.
func (s *Service) ResolveAdopt(ctx context.Context, selector string, options AdoptOptions) (AdoptTarget, error) {
	component, bindings, err := s.Component(ctx, selector)
	if err != nil {
		return AdoptTarget{}, err
	}
	binding, err := SelectBinding(bindings, options.BindingID)
	if err != nil {
		return AdoptTarget{}, err
	}
	if err := validateBindingPathID(binding.ID); err != nil {
		return AdoptTarget{}, err
	}
	if binding.Managed {
		return AdoptTarget{}, errors.New("lifecycle: binding is already managed")
	}
	resolvedSource, err := s.resolveAdoptSource(options)
	if err != nil {
		return AdoptTarget{}, err
	}
	normalized, provider := resolvedSource.Source, resolvedSource.Provider
	target := AdoptTarget{
		ComponentID: component.ID, BindingID: binding.ID,
		Subdir: normalized.Subdir, SourceIdentity: resolvedSource.Identity,
	}
	runtimeComponent := (normalized.Kind == adapter.SourceNPM || normalized.Kind == adapter.SourcePyPI) &&
		(component.Kind == model.ComponentCLI || component.Kind == model.ComponentStdioMCP)
	switch {
	case component.Kind == model.ComponentHook:
		if normalized.Kind != adapter.SourceGit {
			return AdoptTarget{}, errors.New("lifecycle: managed hooks require a Git source in v1")
		}
		if binding.ConfigPath == "" || binding.ConfigPointer == "" || strings.TrimSpace(options.Executable) == "" {
			return AdoptTarget{}, errors.New("lifecycle: hook adoption preview requires its config locator and --executable")
		}
		target.ConfigHash, err = fingerprintObservedFile(binding.ConfigPath)
		if err != nil {
			return AdoptTarget{}, err
		}
		track := adapter.Track{Channel: string(model.TrackStable)}
		if strings.TrimSpace(options.Version) != "" {
			track.Channel, track.Constraint = string(model.TrackExact), strings.TrimSpace(options.Version)
		}
		resolved, resolveErr := provider.Resolve(ctx, normalized, track)
		if resolveErr != nil {
			return AdoptTarget{}, fmt.Errorf("lifecycle: resolve hook source: %w", resolveErr)
		}
		target.ResolvedRef, target.Version = resolved.Ref, resolved.Version
	case runtimeComponent:
		version := strings.TrimSpace(options.Version)
		if version == "" {
			version = strings.TrimSpace(binding.ObservedVersion)
		}
		if version == "" {
			return AdoptTarget{}, errors.New("lifecycle: runtime adoption requires an exact current version")
		}
		resolved, resolveErr := provider.Resolve(ctx, normalized, adapter.Track{Channel: string(model.TrackExact), Constraint: version})
		if resolveErr != nil {
			return AdoptTarget{}, fmt.Errorf("lifecycle: resolve exact runtime: %w", resolveErr)
		}
		target.ResolvedRef, target.Version = resolved.Ref, resolved.Version
		if component.Kind == model.ComponentStdioMCP {
			if binding.ConfigPath == "" || binding.ConfigPointer == "" {
				return AdoptTarget{}, errors.New("lifecycle: stdio MCP binding is missing its host config locator")
			}
			target.ConfigHash, err = fingerprintObservedFile(binding.ConfigPath)
		} else if filepath.IsAbs(binding.InstallPath) {
			target.ObservedHash, err = fingerprintObservedFile(binding.InstallPath)
		}
		if err != nil {
			return AdoptTarget{}, err
		}
	default:
		target.ObservedHash, err = s.Objects.FingerprintTree(ctx, binding.InstallPath, objectstore.CaptureOptions{})
		if err != nil {
			return AdoptTarget{}, err
		}
		if strings.TrimSpace(options.Version) == "" {
			target.ResolvedRef = "adopted:" + target.ObservedHash
			break
		}
		resolved, resolveErr := provider.Resolve(ctx, normalized, adapter.Track{Channel: string(model.TrackExact), Constraint: strings.TrimSpace(options.Version)})
		if resolveErr != nil {
			return AdoptTarget{}, fmt.Errorf("lifecycle: resolve adoption baseline: %w", resolveErr)
		}
		target.ResolvedRef, target.Version = resolved.Ref, resolved.Version
	}
	if target.ResolvedRef == "" {
		return AdoptTarget{}, errors.New("lifecycle: adapter returned an empty resolved ref")
	}
	return target, nil
}

func fingerprintObservedFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("lifecycle: observed file path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	_, _ = fmt.Fprintf(hasher, "%s\x00%o\x00", info.Mode().Type().String(), info.Mode().Perm())
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		_, _ = io.WriteString(hasher, target)
	} else if info.Mode().IsRegular() {
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(hasher, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return "", errors.Join(copyErr, closeErr)
		}
	} else {
		return "", errors.New("lifecycle: observed path is not a regular file or symlink")
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func checkExpectedObservedFile(path, expected string) error {
	if expected == "" {
		return nil
	}
	actual, err := fingerprintObservedFile(path)
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New("lifecycle: observed file changed after confirmation preview")
	}
	return nil
}
