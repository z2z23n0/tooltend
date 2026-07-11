package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

const (
	ManifestName = "tooltend.toml"
	LockName     = "tooltend.lock"
)

var ErrSyncStateChanged = errors.New("project: policy state changed after sync preview")

type ExportResult struct {
	Manifest config.ProjectManifest `json:"manifest"`
	Lock     config.ProjectLock     `json:"lock"`
}

type SyncChange struct {
	Component     string             `json:"component"`
	BindingID     string             `json:"binding_id,omitempty"`
	Action        string             `json:"action"`
	Detail        string             `json:"detail"`
	OldTrack      model.TrackChannel `json:"old_track,omitempty"`
	NewTrack      model.TrackChannel `json:"new_track,omitempty"`
	OldConstraint string             `json:"old_constraint,omitempty"`
	NewConstraint string             `json:"new_constraint,omitempty"`
	OldIntegrity  string             `json:"old_integrity,omitempty"`
	NewIntegrity  string             `json:"new_integrity,omitempty"`
}

type SyncResult struct {
	Changes         []SyncChange     `json:"changes"`
	Warnings        []string         `json:"warnings,omitempty"`
	PolicySnapshots []PolicySnapshot `json:"policy_snapshots,omitempty"`
}

type PolicySnapshot struct {
	BindingID         string             `json:"binding_id"`
	Track             model.TrackChannel `json:"track"`
	Constraint        string             `json:"constraint,omitempty"`
	ExpectedIntegrity string             `json:"expected_integrity,omitempty"`
	ApplyMode         model.ApplyMode    `json:"apply_mode"`
	NotifyMode        model.NotifyMode   `json:"notify_mode"`
	LocalCapMode      model.ApplyMode    `json:"local_cap_mode"`
	UpdatedAt         string             `json:"updated_at"`
}

func Init(root string) (ExportResult, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return ExportResult{}, err
	}
	result := ExportResult{
		Manifest: config.ProjectManifest{Version: config.ConfigVersion, Components: []config.ProjectComponent{}},
		Lock:     config.ProjectLock{Version: config.ConfigVersion, Components: []config.ProjectLockComponent{}},
	}
	if err := config.SaveProjectManifestAtomic(filepath.Join(root, ManifestName), result.Manifest); err != nil {
		return ExportResult{}, err
	}
	if err := config.SaveProjectLockAtomic(filepath.Join(root, LockName), result.Lock); err != nil {
		return ExportResult{}, err
	}
	return result, nil
}

func Export(ctx context.Context, database *store.Store, root string) (ExportResult, error) {
	if database == nil {
		return ExportResult{}, errors.New("project: store is required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return ExportResult{}, err
	}
	projects, err := database.ListProjects(ctx)
	if err != nil {
		return ExportResult{}, err
	}
	projectID := ""
	for _, item := range projects {
		if filepath.Clean(item.RootPath) == filepath.Clean(root) {
			projectID = item.ID
			break
		}
	}
	if projectID == "" {
		return ExportResult{}, errors.New("project: project is not selected in ToolTend")
	}

	components, err := database.ListComponents(ctx)
	if err != nil {
		return ExportResult{}, err
	}
	result := ExportResult{Manifest: config.ProjectManifest{Version: config.ConfigVersion}, Lock: config.ProjectLock{Version: config.ConfigVersion}}
	for _, component := range components {
		bindings, listErr := database.ListBindings(ctx, component.ID)
		if listErr != nil {
			return ExportResult{}, listErr
		}
		var selected []model.Binding
		for _, binding := range bindings {
			if binding.ProjectID == projectID {
				selected = append(selected, binding)
			}
		}
		if len(selected) == 0 {
			continue
		}
		source, sourceErr := database.GetSource(ctx, component.SourceID)
		if sourceErr != nil {
			return ExportResult{}, sourceErr
		}
		agents := make([]model.HostKind, 0, len(selected))
		for _, binding := range selected {
			agents = appendUniqueHost(agents, binding.Host)
		}
		policy, policyErr := database.GetPolicy(ctx, selected[0].ID)
		if policyErr != nil {
			return ExportResult{}, policyErr
		}
		result.Manifest.Components = append(result.Manifest.Components, config.ProjectComponent{
			Name: component.Name, Source: sourceString(source), Kind: component.Kind, Agents: agents, Track: policy.TrackChannel, Constraint: policy.Constraint,
		})
		integrity, version, commit := bindingIntegrity(ctx, database, selected[0])
		if integrity != "" {
			result.Lock.Components = append(result.Lock.Components, config.ProjectLockComponent{LogicalKey: component.LogicalKey, ResolvedVersion: version, Commit: commit, Integrity: integrity})
		}
	}
	sort.Slice(result.Manifest.Components, func(i, j int) bool { return result.Manifest.Components[i].Name < result.Manifest.Components[j].Name })
	return result, nil
}

func WriteExport(root string, result ExportResult) error {
	if err := config.SaveProjectManifestAtomic(filepath.Join(root, ManifestName), result.Manifest); err != nil {
		return err
	}
	return config.SaveProjectLockAtomic(filepath.Join(root, LockName), result.Lock)
}

// Sync applies only version-target information to already-observed local
// bindings. It never writes source_trust, never changes apply mode, and never
// loosens a machine-local exact pin.
func Sync(ctx context.Context, database *store.Store, root string, apply bool) (SyncResult, error) {
	preview, err := previewSync(ctx, database, root)
	if err != nil || !apply {
		return preview, err
	}
	return ApplySyncPreview(ctx, database, preview)
}

func previewSync(ctx context.Context, database *store.Store, root string) (SyncResult, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return SyncResult{}, err
	}
	manifest, err := config.LoadProjectManifest(filepath.Join(root, ManifestName))
	if err != nil {
		return SyncResult{}, err
	}
	lockEntries := map[string]config.ProjectLockComponent{}
	lock, lockErr := config.LoadProjectLock(filepath.Join(root, LockName))
	if lockErr == nil {
		for _, entry := range lock.Components {
			lockEntries[entry.LogicalKey] = entry
		}
	} else if !errors.Is(lockErr, os.ErrNotExist) {
		return SyncResult{}, lockErr
	}
	projects, err := database.ListProjects(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	projectID := ""
	for _, item := range projects {
		if filepath.Clean(item.RootPath) == filepath.Clean(root) {
			projectID = item.ID
			break
		}
	}
	if projectID == "" {
		return SyncResult{}, errors.New("project: project is not selected in ToolTend")
	}
	components, err := database.ListComponents(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	result := SyncResult{Changes: []SyncChange{}}
	usedLocks := map[string]bool{}
	for _, desired := range manifest.Components {
		var matched *model.LogicalComponent
		for index := range components {
			if components[index].Name != desired.Name || components[index].Kind != desired.Kind {
				continue
			}
			source, sourceErr := database.GetSource(ctx, components[index].SourceID)
			if sourceErr == nil && sourceString(source) == desired.Source {
				matched = &components[index]
				break
			}
		}
		if matched == nil {
			result.Changes = append(result.Changes, SyncChange{Component: desired.Name, Action: "missing", Detail: "component is declared but not installed locally"})
			continue
		}
		locked, hasLock := lockEntries[matched.LogicalKey]
		if hasLock {
			usedLocks[matched.LogicalKey] = true
		}
		bindings, listErr := database.ListBindings(ctx, matched.ID)
		if listErr != nil {
			return result, listErr
		}
		for _, binding := range bindings {
			// A repository manifest is scoped to that repository. It must never
			// mutate a global binding or the same component in another project.
			if binding.ProjectID != projectID || !containsHost(desired.Agents, binding.Host) {
				continue
			}
			policy, policyErr := database.GetPolicy(ctx, binding.ID)
			if policyErr != nil {
				return result, policyErr
			}
			result.PolicySnapshots = append(result.PolicySnapshots, policySnapshot(policy))
			desiredTrack, desiredConstraint := desired.Track, desired.Constraint
			desiredIntegrity := ""
			if hasLock {
				desiredIntegrity = locked.Integrity
				if locked.Commit != "" {
					desiredTrack, desiredConstraint = model.TrackExact, locked.Commit
				} else if locked.ResolvedVersion != "" {
					desiredTrack, desiredConstraint = model.TrackExact, locked.ResolvedVersion
				}
			} else if policy.ExpectedIntegrity != "" && (policy.TrackChannel != desiredTrack || policy.Constraint != desiredConstraint) {
				result.Warnings = append(result.Warnings, fmt.Sprintf("%s keeps integrity-locked local target because tooltend.lock has no matching entry", desired.Name))
				continue
			}
			if policy.TrackChannel == model.TrackExact && (desiredTrack != model.TrackExact || policy.Constraint != desiredConstraint) {
				result.Warnings = append(result.Warnings, fmt.Sprintf("%s keeps stricter local exact pin", desired.Name))
				continue
			}
			if policy.TrackChannel == model.TrackExact && policy.Constraint == desiredConstraint && policy.ExpectedIntegrity != "" && desiredIntegrity != "" && policy.ExpectedIntegrity != desiredIntegrity {
				result.Warnings = append(result.Warnings, fmt.Sprintf("%s keeps stricter local integrity pin", desired.Name))
				continue
			}
			if policy.TrackChannel == desiredTrack && policy.Constraint == desiredConstraint && (desiredIntegrity == "" || policy.ExpectedIntegrity == desiredIntegrity) {
				continue
			}
			result.Changes = append(result.Changes, SyncChange{
				Component: desired.Name, BindingID: binding.ID, Action: "track",
				Detail:   string(policy.TrackChannel) + ":" + policy.Constraint + " -> " + string(desiredTrack) + ":" + desiredConstraint,
				OldTrack: policy.TrackChannel, NewTrack: desiredTrack,
				OldConstraint: policy.Constraint, NewConstraint: desiredConstraint,
				OldIntegrity: policy.ExpectedIntegrity, NewIntegrity: desiredIntegrity,
			})
		}
	}
	for logicalKey := range lockEntries {
		if !usedLocks[logicalKey] {
			result.Warnings = append(result.Warnings, fmt.Sprintf("lock entry %s has no matching declared local component", logicalKey))
		}
	}
	sort.Slice(result.PolicySnapshots, func(i, j int) bool { return result.PolicySnapshots[i].BindingID < result.PolicySnapshots[j].BindingID })
	return result, nil
}

// ApplySyncPreview applies only the frozen changes that were shown to the
// user. Every consulted policy is checked inside the same transaction and each
// write is a compare-and-set, so a concurrent policy edit cannot create an
// unpreviewed change or leave a partially applied sync.
func ApplySyncPreview(ctx context.Context, database *store.Store, preview SyncResult) (SyncResult, error) {
	if database == nil {
		return SyncResult{}, errors.New("project: store is required")
	}
	expected := make(map[string]PolicySnapshot, len(preview.PolicySnapshots))
	for _, snapshot := range preview.PolicySnapshots {
		if snapshot.BindingID == "" {
			return SyncResult{}, errors.New("project: sync preview has an invalid policy snapshot")
		}
		if _, exists := expected[snapshot.BindingID]; exists {
			return SyncResult{}, errors.New("project: sync preview has duplicate policy snapshots")
		}
		expected[snapshot.BindingID] = snapshot
	}
	seenChanges := map[string]bool{}
	err := database.WithTx(ctx, func(tx *sql.Tx) error {
		for _, snapshot := range preview.PolicySnapshots {
			current, err := loadPolicySnapshot(ctx, tx, snapshot.BindingID)
			if err != nil {
				return err
			}
			if current != snapshot {
				return fmt.Errorf("%w: %s", ErrSyncStateChanged, snapshot.BindingID)
			}
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		for _, change := range preview.Changes {
			if change.Action != "track" || change.BindingID == "" {
				continue
			}
			if seenChanges[change.BindingID] {
				return fmt.Errorf("project: sync preview changes binding %s more than once", change.BindingID)
			}
			seenChanges[change.BindingID] = true
			snapshot, ok := expected[change.BindingID]
			if !ok {
				return fmt.Errorf("project: sync preview is missing policy snapshot for %s", change.BindingID)
			}
			result, err := tx.ExecContext(ctx, `UPDATE policies SET track_channel=?,constraint_text=?,expected_integrity=?,updated_at=?
				WHERE binding_id=? AND track_channel=? AND constraint_text=? AND expected_integrity=? AND apply_mode=? AND notify_mode=? AND local_cap_mode=? AND updated_at=?`,
				change.NewTrack, change.NewConstraint, change.NewIntegrity, now,
				change.BindingID, snapshot.Track, snapshot.Constraint, snapshot.ExpectedIntegrity,
				snapshot.ApplyMode, snapshot.NotifyMode, snapshot.LocalCapMode, snapshot.UpdatedAt)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count != 1 {
				return fmt.Errorf("%w: %s", ErrSyncStateChanged, change.BindingID)
			}
		}
		return nil
	})
	if err != nil {
		return SyncResult{}, err
	}
	return preview, nil
}

func policySnapshot(value model.Policy) PolicySnapshot {
	return PolicySnapshot{
		BindingID: value.BindingID, Track: value.TrackChannel, Constraint: value.Constraint,
		ExpectedIntegrity: value.ExpectedIntegrity, ApplyMode: value.ApplyMode, NotifyMode: value.NotifyMode,
		LocalCapMode: value.LocalCapMode, UpdatedAt: value.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func loadPolicySnapshot(ctx context.Context, tx *sql.Tx, bindingID string) (PolicySnapshot, error) {
	var value PolicySnapshot
	err := tx.QueryRowContext(ctx, `SELECT binding_id,track_channel,constraint_text,expected_integrity,apply_mode,notify_mode,local_cap_mode,updated_at FROM policies WHERE binding_id=?`, bindingID).
		Scan(&value.BindingID, &value.Track, &value.Constraint, &value.ExpectedIntegrity, &value.ApplyMode, &value.NotifyMode, &value.LocalCapMode, &value.UpdatedAt)
	return value, err
}

func sourceString(source model.Source) string {
	if source.PackageName != "" {
		return string(source.Kind) + ":" + source.PackageName
	}
	return source.Locator
}

func bindingIntegrity(ctx context.Context, database *store.Store, binding model.Binding) (integrity, version, commit string) {
	if binding.ActiveGenerationID == "" {
		return binding.ObservedHash, binding.ObservedVersion, ""
	}
	var resolved, tree string
	var candidateID sql.NullString
	err := database.DB().QueryRowContext(ctx, `SELECT resolved_ref,tree_hash,candidate_id FROM generations WHERE id=? AND binding_id=?`, binding.ActiveGenerationID, binding.ID).Scan(&resolved, &tree, &candidateID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", ""
	}
	if candidateID.Valid {
		var upstream string
		if err := database.DB().QueryRowContext(ctx, `SELECT upstream_tree_hash FROM candidates WHERE id=?`, candidateID.String).Scan(&upstream); err == nil && upstream != "" {
			tree = upstream
		}
	}
	if gitCommitPattern.MatchString(resolved) {
		commit = resolved
	} else {
		version = resolved
	}
	return tree, version, commit
}

var gitCommitPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

func appendUniqueHost(values []model.HostKind, value model.HostKind) []model.HostKind {
	if containsHost(values, value) {
		return values
	}
	return append(values, value)
}

func containsHost(values []model.HostKind, value model.HostKind) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
