package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/z2z23n0/tooltend/internal/model"
)

// ErrAdoptionStateChanged means inventory or policy state no longer matches
// the state used to prepare an adoption. The caller must unwind host changes
// instead of overwriting the newer decision.
var ErrAdoptionStateChanged = errors.New("store: adoption state changed")

// AdoptionCommit is the complete database side of a successful adoption.
// Object records may be written before this transaction because they are
// immutable content-addressed data; every mutable inventory and lifecycle row
// is committed together.
type AdoptionCommit struct {
	Source            model.Source           `json:"source"`
	Trust             model.SourceTrust      `json:"trust"`
	ExpectedComponent model.LogicalComponent `json:"expected_component"`
	Component         model.LogicalComponent `json:"component"`
	ExpectedBinding   model.Binding          `json:"expected_binding"`
	Binding           model.Binding          `json:"binding"`
	Generation        model.Generation       `json:"generation"`
	Baseline          *model.Baseline        `json:"baseline,omitempty"`
	ExpectedPolicy    *model.Policy          `json:"expected_policy,omitempty"`
	Policy            *model.Policy          `json:"policy,omitempty"`
	Receipt           model.Receipt          `json:"receipt"`
}

// CommitAdoption atomically binds an observed component to a verified source,
// activates its first generation, records its baseline or manual-only policy,
// and writes the adoption receipt. Component, binding, and optional policy
// updates use compare-and-set semantics so a stale confirmation cannot win.
func (s *Store) CommitAdoption(ctx context.Context, value AdoptionCommit) error {
	if err := validateAdoptionCommit(value); err != nil {
		return err
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		return commitAdoptionTx(ctx, tx, value)
	})
}

func commitAdoptionTx(ctx context.Context, tx *sql.Tx, value AdoptionCommit) error {
	if err := upsertAdoptionSource(ctx, tx, value.Source); err != nil {
		return err
	}
	if err := compareAndSetAdoptionComponent(ctx, tx, value.ExpectedComponent, value.Component); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO source_trust(source_id,level,approved_by,approved_at) VALUES(?,?,?,?)
		ON CONFLICT(source_id) DO UPDATE SET
		level=CASE WHEN source_trust.level='pinned' THEN source_trust.level ELSE excluded.level END,
		approved_by=CASE WHEN source_trust.level='pinned' THEN source_trust.approved_by ELSE excluded.approved_by END,
		approved_at=CASE WHEN source_trust.level='pinned' THEN source_trust.approved_at ELSE excluded.approved_at END`,
		value.Trust.SourceID, value.Trust.Level, value.Trust.ApprovedBy, timeText(value.Trust.ApprovedAt)); err != nil {
		return fmt.Errorf("store: set adoption source trust: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO generations(id,binding_id,candidate_id,resolved_ref,tree_hash,integrity_hash,state,created_at,activated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, value.Generation.ID, value.Generation.BindingID, nullIfEmpty(value.Generation.CandidateID),
		value.Generation.ResolvedRef, value.Generation.TreeHash, value.Generation.IntegrityHash, value.Generation.State,
		timeText(value.Generation.CreatedAt), nullableTimeText(value.Generation.ActivatedAt)); err != nil {
		return fmt.Errorf("store: insert adoption generation: %w", err)
	}
	if err := compareAndSetAdoptionBinding(ctx, tx, value.ExpectedBinding, value.Binding); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE generations SET state='inactive' WHERE binding_id=? AND state='active' AND id<>?`, value.Binding.ID, value.Generation.ID); err != nil {
		return fmt.Errorf("store: deactivate previous adoption generation: %w", err)
	}
	activatedAt := value.Receipt.CreatedAt
	if activatedAt.IsZero() {
		activatedAt = value.Generation.CreatedAt
	}
	result, err := tx.ExecContext(ctx, `UPDATE generations SET state='active',activated_at=? WHERE id=? AND binding_id=?`,
		timeText(activatedAt), value.Generation.ID, value.Binding.ID)
	if err != nil {
		return fmt.Errorf("store: activate adoption generation: %w", err)
	}
	if err := requireOneRow(result, "adoption generation"); err != nil {
		return err
	}
	if value.Baseline != nil {
		result, err = tx.ExecContext(ctx, `INSERT INTO baselines(id,binding_id,source_id,resolved_ref,tree_hash,created_at) VALUES(?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET id=excluded.id
			WHERE baselines.binding_id=excluded.binding_id AND baselines.source_id=excluded.source_id AND baselines.resolved_ref=excluded.resolved_ref AND baselines.tree_hash=excluded.tree_hash`,
			value.Baseline.ID, value.Baseline.BindingID, value.Baseline.SourceID, value.Baseline.ResolvedRef,
			value.Baseline.TreeHash, timeText(value.Baseline.CreatedAt))
		if err != nil {
			return fmt.Errorf("store: insert adoption baseline: %w", err)
		}
		if err := requireOneRow(result, "adoption baseline"); err != nil {
			return err
		}
	}
	if value.Policy != nil {
		if err := compareAndSetAdoptionPolicy(ctx, tx, value.ExpectedPolicy, *value.Policy); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO receipts(id,binding_id,candidate_id,action,old_generation_id,new_generation_id,from_ref,to_ref,candidate_hash,status,summary_json,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, value.Receipt.ID, value.Receipt.BindingID, nullIfEmpty(value.Receipt.CandidateID),
		value.Receipt.Action, nullIfEmpty(value.Receipt.OldGenerationID), nullIfEmpty(value.Receipt.NewGenerationID),
		value.Receipt.FromRef, value.Receipt.ToRef, value.Receipt.CandidateHash, value.Receipt.Status,
		value.Receipt.SummaryJSON, timeText(value.Receipt.CreatedAt)); err != nil {
		return fmt.Errorf("store: insert adoption receipt: %w", err)
	}
	return nil
}

func validateAdoptionCommit(value AdoptionCommit) error {
	if value.Source.ID == "" || value.Source.IdentityHash == "" || value.Component.ID == "" || value.Binding.ID == "" || value.Generation.ID == "" || value.Receipt.ID == "" {
		return errors.New("store: adoption identity is incomplete")
	}
	if err := value.Source.Kind.Validate(); err != nil {
		return err
	}
	if value.Source.MetadataJSON != "" && !json.Valid([]byte(value.Source.MetadataJSON)) {
		return errors.New("store: adoption source metadata is not valid JSON")
	}
	if err := value.Trust.Level.Validate(); err != nil {
		return err
	}
	if value.Trust.SourceID != value.Source.ID {
		return errors.New("store: adoption trust source mismatch")
	}
	if err := value.Component.Kind.Validate(); err != nil {
		return err
	}
	if value.ExpectedComponent.ID != value.Component.ID || value.Component.SourceID != value.Source.ID {
		return errors.New("store: adoption component mismatch")
	}
	if err := validateAdoptionBinding(value.ExpectedBinding); err != nil {
		return err
	}
	if err := validateAdoptionBinding(value.Binding); err != nil {
		return err
	}
	if value.ExpectedBinding.ID != value.Binding.ID || value.Binding.ComponentID != value.Component.ID || value.ExpectedBinding.Managed || !value.Binding.Managed {
		return errors.New("store: adoption binding transition is invalid")
	}
	if err := value.Generation.State.Validate(); err != nil {
		return err
	}
	if value.Generation.State != model.GenerationOriginal || value.Generation.BindingID != value.Binding.ID || value.Binding.ActiveGenerationID != value.Generation.ID {
		return errors.New("store: adoption generation mismatch")
	}
	if value.Baseline != nil && (value.Baseline.BindingID != value.Binding.ID || value.Baseline.SourceID != value.Source.ID || value.Baseline.TreeHash == "") {
		return errors.New("store: adoption baseline mismatch")
	}
	if value.ExpectedPolicy != nil && value.Policy == nil {
		return errors.New("store: adoption policy transition is incomplete")
	}
	if value.ExpectedPolicy != nil {
		if value.Policy == nil || value.ExpectedPolicy.BindingID != value.Binding.ID {
			return errors.New("store: adoption expected policy mismatch")
		}
		if err := value.ExpectedPolicy.Validate(); err != nil {
			return err
		}
	}
	if value.Policy != nil {
		if value.Policy.BindingID != value.Binding.ID {
			return errors.New("store: adoption policy binding mismatch")
		}
		if err := value.Policy.Validate(); err != nil {
			return err
		}
	}
	if err := value.Receipt.Action.Validate(); err != nil {
		return err
	}
	if err := value.Receipt.Status.Validate(); err != nil {
		return err
	}
	if value.Receipt.Action != model.ReceiptAdopt || value.Receipt.Status != model.ReceiptSucceeded || value.Receipt.BindingID != value.Binding.ID || value.Receipt.NewGenerationID != value.Generation.ID {
		return errors.New("store: adoption receipt mismatch")
	}
	if value.Receipt.SummaryJSON != "" && !json.Valid([]byte(value.Receipt.SummaryJSON)) {
		return errors.New("store: adoption receipt summary is not valid JSON")
	}
	return nil
}

func validateAdoptionBinding(value model.Binding) error {
	if err := value.Host.Validate(); err != nil {
		return err
	}
	if err := value.Scope.Validate(); err != nil {
		return err
	}
	return value.Classification.Validate()
}

func upsertAdoptionSource(ctx context.Context, tx *sql.Tx, value model.Source) error {
	metadata := value.MetadataJSON
	if metadata == "" {
		metadata = "{}"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO sources(id,kind,locator,subdir,package_name,identity_hash,metadata_json,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET kind=excluded.kind,locator=excluded.locator,subdir=excluded.subdir,
		package_name=excluded.package_name,identity_hash=excluded.identity_hash,metadata_json=excluded.metadata_json,updated_at=excluded.updated_at`,
		value.ID, value.Kind, value.Locator, value.Subdir, value.PackageName, value.IdentityHash, metadata,
		timeText(value.CreatedAt), timeText(value.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: upsert adoption source: %w", err)
	}
	return nil
}

func compareAndSetAdoptionComponent(ctx context.Context, tx *sql.Tx, expected, next model.LogicalComponent) error {
	result, err := tx.ExecContext(ctx, `UPDATE components SET kind=?,name=?,source_id=?,logical_key=?,updated_at=?
		WHERE id=? AND kind=? AND name=? AND COALESCE(source_id,'')=? AND logical_key=? AND created_at=? AND updated_at=?`,
		next.Kind, next.Name, nullIfEmpty(next.SourceID), next.LogicalKey, timeText(next.UpdatedAt),
		expected.ID, expected.Kind, expected.Name, expected.SourceID, expected.LogicalKey, timeText(expected.CreatedAt), timeText(expected.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: compare and set adoption component: %w", err)
	}
	if err := requireOneRow(result, "component"); err != nil {
		return err
	}
	return nil
}

func compareAndSetAdoptionBinding(ctx context.Context, tx *sql.Tx, expected, next model.Binding) error {
	result, err := tx.ExecContext(ctx, `UPDATE bindings SET component_id=?,host=?,project_id=?,scope=?,install_path=?,config_path=?,config_pointer=?,install_method=?,managed=?,classification=?,observed_hash=?,observed_version=?,trust_hash=?,active_generation_id=?,last_seen_at=?
		WHERE id=? AND component_id=? AND host=? AND COALESCE(project_id,'')=? AND scope=? AND install_path=? AND config_path=? AND config_pointer=? AND install_method=? AND managed=? AND classification=? AND observed_hash=? AND observed_version=? AND trust_hash=? AND COALESCE(active_generation_id,'')=? AND last_seen_at=?`,
		next.ComponentID, next.Host, nullIfEmpty(next.ProjectID), next.Scope, next.InstallPath, next.ConfigPath, next.ConfigPointer,
		next.InstallMethod, boolInt(next.Managed), next.Classification, next.ObservedHash, next.ObservedVersion, next.TrustHash,
		nullIfEmpty(next.ActiveGenerationID), timeText(next.LastSeenAt),
		expected.ID, expected.ComponentID, expected.Host, expected.ProjectID, expected.Scope, expected.InstallPath, expected.ConfigPath,
		expected.ConfigPointer, expected.InstallMethod, boolInt(expected.Managed), expected.Classification, expected.ObservedHash,
		expected.ObservedVersion, expected.TrustHash, expected.ActiveGenerationID, timeText(expected.LastSeenAt))
	if err != nil {
		return fmt.Errorf("store: compare and set adoption binding: %w", err)
	}
	return requireOneRow(result, "binding")
}

func compareAndSetAdoptionPolicy(ctx context.Context, tx *sql.Tx, expected *model.Policy, next model.Policy) error {
	if expected == nil {
		result, err := tx.ExecContext(ctx, `INSERT INTO policies(binding_id,track_channel,constraint_text,expected_integrity,apply_mode,notify_mode,local_cap_mode,updated_at)
			VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(binding_id) DO NOTHING`, next.BindingID, next.TrackChannel, next.Constraint,
			next.ExpectedIntegrity, next.ApplyMode, next.NotifyMode, next.LocalCapMode, timeText(next.UpdatedAt))
		if err != nil {
			return fmt.Errorf("store: insert adoption policy: %w", err)
		}
		return requireOneRow(result, "policy")
	}
	result, err := tx.ExecContext(ctx, `UPDATE policies SET track_channel=?,constraint_text=?,expected_integrity=?,apply_mode=?,notify_mode=?,local_cap_mode=?,updated_at=?
		WHERE binding_id=? AND track_channel=? AND constraint_text=? AND expected_integrity=? AND apply_mode=? AND notify_mode=? AND local_cap_mode=? AND updated_at=?`,
		next.TrackChannel, next.Constraint, next.ExpectedIntegrity, next.ApplyMode, next.NotifyMode, next.LocalCapMode, timeText(next.UpdatedAt),
		expected.BindingID, expected.TrackChannel, expected.Constraint, expected.ExpectedIntegrity, expected.ApplyMode,
		expected.NotifyMode, expected.LocalCapMode, timeText(expected.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: compare and set adoption policy: %w", err)
	}
	return requireOneRow(result, "policy")
}

func requireOneRow(result sql.Result, subject string) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%w: %s changed after preview", ErrAdoptionStateChanged, subject)
	}
	return nil
}
