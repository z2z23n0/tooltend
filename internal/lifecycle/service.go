package lifecycle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/z2z23n0/tooltend/internal/adapter"
	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/lockfile"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/objectstore"
	"github.com/z2z23n0/tooltend/internal/store"
)

type Service struct {
	Database *store.Store
	Objects  *objectstore.Store
	Paths    config.Paths
	Adapters *adapter.Registry
	Now      func() time.Time
	// PathEnv is the effective interactive PATH used to verify CLI shim
	// precedence. Ordinary commands leave it empty and read the current process;
	// the scheduler worker supplies the init-configured shim prefix explicitly.
	PathEnv string
	// ActivationLockHeld is set only by the one-shot worker, which acquires the
	// global lock before constructing lifecycle callbacks and runs recovery
	// before dispatching any task.
	ActivationLockHeld bool
	// AdoptionFailpoint is used by subprocess crash tests. Production services
	// leave it nil.
	AdoptionFailpoint func(AdoptionFailpoint) error
	// SnapshotFailpoint lets tests mutate a live generation immediately after
	// it has been captured. Production services leave it nil.
	SnapshotFailpoint func(SnapshotFailpoint) error
}

func (s *Service) pathEnv() string {
	if s.PathEnv != "" {
		return s.PathEnv
	}
	return os.Getenv("PATH")
}

type SnapshotFailpoint string

const (
	SnapshotAfterObservedCapture  SnapshotFailpoint = "after_observed_capture"
	SnapshotAfterCandidateCapture SnapshotFailpoint = "after_candidate_capture"
	SnapshotAfterRollbackCapture  SnapshotFailpoint = "after_rollback_capture"
)

func (s *Service) failAdoption(point AdoptionFailpoint) error {
	if s.AdoptionFailpoint == nil {
		return nil
	}
	if err := s.AdoptionFailpoint(point); err != nil {
		return fmt.Errorf("lifecycle: adoption failpoint %s: %w", point, err)
	}
	return nil
}

func (s *Service) failSnapshot(point SnapshotFailpoint) error {
	if s.SnapshotFailpoint == nil {
		return nil
	}
	if err := s.SnapshotFailpoint(point); err != nil {
		return fmt.Errorf("lifecycle: snapshot failpoint %s: %w", point, err)
	}
	return nil
}

func (s *Service) withMutationLock(ctx context.Context, action func() error) (err error) {
	if s.ActivationLockHeld {
		return action()
	}
	lock, err := lockfile.Try(s.Paths.ActivationLock)
	if err != nil {
		return fmt.Errorf("lifecycle: acquire activation lock: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if _, err := RecoverAdoptions(ctx, s.Database, s.Paths); err != nil {
		return err
	}
	if _, err := RecoverActivations(ctx, s.Database, s.Paths); err != nil {
		return err
	}
	return action()
}

func New(database *store.Store, objects *objectstore.Store, paths config.Paths) (*Service, error) {
	if database == nil || objects == nil {
		return nil, errors.New("lifecycle: database and object store are required")
	}
	registry, err := adapter.NewRegistry(
		adapter.Git{}, adapter.NPM{}, adapter.Python{}, adapter.Homebrew{}, adapter.RemoteMCP{},
		adapter.Unsupported{Kind: adapter.SourceLocal}, adapter.Unsupported{Kind: adapter.SourceUnknown},
	)
	if err != nil {
		return nil, err
	}
	if paths.ActivationLock == "" && paths.StateDir != "" {
		paths.ActivationLock = filepath.Join(paths.StateDir, "activation.lock")
	}
	return &Service{Database: database, Objects: objects, Paths: paths, Adapters: registry, Now: time.Now}, nil
}

func (s *Service) Component(ctx context.Context, selector string) (model.LogicalComponent, []model.Binding, error) {
	components, err := s.Database.ListComponents(ctx)
	if err != nil {
		return model.LogicalComponent{}, nil, err
	}
	var matches []model.LogicalComponent
	for _, component := range components {
		if component.ID == selector || component.LogicalKey == selector || strings.EqualFold(component.Name, selector) {
			matches = append(matches, component)
		}
	}
	if len(matches) == 0 {
		return model.LogicalComponent{}, nil, sql.ErrNoRows
	}
	if len(matches) > 1 {
		return model.LogicalComponent{}, nil, fmt.Errorf("lifecycle: component selector %q is ambiguous", selector)
	}
	bindings, err := s.Database.ListBindings(ctx, matches[0].ID)
	return matches[0], bindings, err
}

func SelectBinding(bindings []model.Binding, bindingID string) (model.Binding, error) {
	if bindingID != "" {
		for _, binding := range bindings {
			if binding.ID == bindingID {
				return binding, nil
			}
		}
		return model.Binding{}, fmt.Errorf("lifecycle: binding %s not found", bindingID)
	}
	if len(bindings) == 0 {
		return model.Binding{}, errors.New("lifecycle: component has no binding")
	}
	if len(bindings) != 1 {
		return model.Binding{}, errors.New("lifecycle: component has multiple bindings; select one explicitly")
	}
	return bindings[0], nil
}

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func sourceForAdapter(source model.Source) adapter.Source {
	return adapter.Source{
		Kind:        adapter.SourceKind(source.Kind),
		Locator:     source.Locator,
		PackageName: source.PackageName,
		Subdir:      source.Subdir,
	}
}

func trackForAdapter(value model.Policy) adapter.Track {
	return adapter.Track{Channel: string(value.TrackChannel), Constraint: value.Constraint}
}
