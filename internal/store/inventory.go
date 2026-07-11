package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/z2z23n0/tooltend/internal/model"
)

func (s *Store) UpsertProject(ctx context.Context, value model.Project) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO projects(id,root_path,root_fingerprint,selected,discovered_via,last_seen_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET root_path=excluded.root_path,root_fingerprint=excluded.root_fingerprint,
		selected=excluded.selected,discovered_via=excluded.discovered_via,last_seen_at=excluded.last_seen_at`,
		value.ID, value.RootPath, value.RootFingerprint, boolInt(value.Selected), value.DiscoveredVia, timeText(value.LastSeenAt))
	if err != nil {
		return fmt.Errorf("store: upsert project: %w", err)
	}
	return nil
}

func (s *Store) ListProjects(ctx context.Context) ([]model.Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,root_path,root_fingerprint,selected,discovered_via,last_seen_at FROM projects ORDER BY root_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.Project{}
	for rows.Next() {
		var value model.Project
		var selected int
		var lastSeen string
		if err := rows.Scan(&value.ID, &value.RootPath, &value.RootFingerprint, &selected, &value.DiscoveredVia, &lastSeen); err != nil {
			return nil, err
		}
		value.Selected = selected != 0
		value.LastSeenAt, err = parseTime(lastSeen)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) UpsertSource(ctx context.Context, value model.Source) error {
	if err := value.Kind.Validate(); err != nil {
		return err
	}
	if value.MetadataJSON == "" {
		value.MetadataJSON = "{}"
	}
	now := time.Now().UTC()
	if value.CreatedAt.IsZero() {
		value.CreatedAt = now
	}
	if value.UpdatedAt.IsZero() {
		value.UpdatedAt = now
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sources(id,kind,locator,subdir,package_name,identity_hash,metadata_json,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET kind=excluded.kind,locator=excluded.locator,subdir=excluded.subdir,
		package_name=excluded.package_name,identity_hash=excluded.identity_hash,metadata_json=excluded.metadata_json,updated_at=excluded.updated_at`,
		value.ID, value.Kind, value.Locator, value.Subdir, value.PackageName, value.IdentityHash, value.MetadataJSON, timeText(value.CreatedAt), timeText(value.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: upsert source: %w", err)
	}
	return nil
}

func (s *Store) GetSource(ctx context.Context, id string) (model.Source, error) {
	return scanSource(s.db.QueryRowContext(ctx, `SELECT id,kind,locator,subdir,package_name,identity_hash,metadata_json,created_at,updated_at FROM sources WHERE id=?`, id))
}

func (s *Store) GetSourceByIdentity(ctx context.Context, identityHash string) (model.Source, error) {
	return scanSource(s.db.QueryRowContext(ctx, `SELECT id,kind,locator,subdir,package_name,identity_hash,metadata_json,created_at,updated_at FROM sources WHERE identity_hash=?`, identityHash))
}

func scanSource(row *sql.Row) (model.Source, error) {
	var value model.Source
	var created, updated string
	if err := row.Scan(&value.ID, &value.Kind, &value.Locator, &value.Subdir, &value.PackageName, &value.IdentityHash, &value.MetadataJSON, &created, &updated); err != nil {
		return value, err
	}
	var err error
	value.CreatedAt, err = parseTime(created)
	if err != nil {
		return value, err
	}
	value.UpdatedAt, err = parseTime(updated)
	return value, err
}

func (s *Store) SetSourceTrust(ctx context.Context, value model.SourceTrust) error {
	if err := value.Level.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO source_trust(source_id,level,approved_by,approved_at) VALUES(?,?,?,?)
		ON CONFLICT(source_id) DO UPDATE SET level=excluded.level,approved_by=excluded.approved_by,approved_at=excluded.approved_at`,
		value.SourceID, value.Level, value.ApprovedBy, timeText(value.ApprovedAt))
	if err != nil {
		return fmt.Errorf("store: set source trust: %w", err)
	}
	return nil
}

func (s *Store) GetSourceTrust(ctx context.Context, sourceID string) (model.SourceTrust, error) {
	var value model.SourceTrust
	var approved string
	err := s.db.QueryRowContext(ctx, `SELECT source_id,level,approved_by,approved_at FROM source_trust WHERE source_id=?`, sourceID).Scan(&value.SourceID, &value.Level, &value.ApprovedBy, &approved)
	if err != nil {
		return value, err
	}
	value.ApprovedAt, err = parseTime(approved)
	return value, err
}

func (s *Store) UpsertComponent(ctx context.Context, value model.LogicalComponent) error {
	if err := value.Kind.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	if value.CreatedAt.IsZero() {
		value.CreatedAt = now
	}
	if value.UpdatedAt.IsZero() {
		value.UpdatedAt = now
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO components(id,kind,name,source_id,logical_key,created_at,updated_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET kind=excluded.kind,name=excluded.name,source_id=excluded.source_id,logical_key=excluded.logical_key,updated_at=excluded.updated_at`,
		value.ID, value.Kind, value.Name, nullIfEmpty(value.SourceID), value.LogicalKey, timeText(value.CreatedAt), timeText(value.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: upsert component: %w", err)
	}
	return nil
}

func (s *Store) ListComponents(ctx context.Context) ([]model.LogicalComponent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,kind,name,source_id,logical_key,created_at,updated_at FROM components ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.LogicalComponent{}
	for rows.Next() {
		var value model.LogicalComponent
		var source sql.NullString
		var created, updated string
		if err := rows.Scan(&value.ID, &value.Kind, &value.Name, &source, &value.LogicalKey, &created, &updated); err != nil {
			return nil, err
		}
		value.SourceID = source.String
		value.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		value.UpdatedAt, err = parseTime(updated)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) UpsertBinding(ctx context.Context, value model.Binding) error {
	if err := value.Host.Validate(); err != nil {
		return err
	}
	if err := value.Scope.Validate(); err != nil {
		return err
	}
	if err := value.Classification.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO bindings(id,component_id,host,project_id,scope,install_path,config_path,config_pointer,install_method,managed,classification,observed_hash,observed_version,trust_hash,active_generation_id,last_seen_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET component_id=excluded.component_id,host=excluded.host,project_id=excluded.project_id,
		scope=excluded.scope,install_path=excluded.install_path,config_path=excluded.config_path,config_pointer=excluded.config_pointer,install_method=excluded.install_method,managed=excluded.managed,classification=excluded.classification,
		observed_hash=excluded.observed_hash,observed_version=excluded.observed_version,trust_hash=excluded.trust_hash,active_generation_id=excluded.active_generation_id,last_seen_at=excluded.last_seen_at`,
		value.ID, value.ComponentID, value.Host, nullIfEmpty(value.ProjectID), value.Scope, value.InstallPath, value.ConfigPath, value.ConfigPointer, value.InstallMethod, boolInt(value.Managed), value.Classification,
		value.ObservedHash, value.ObservedVersion, value.TrustHash, nullIfEmpty(value.ActiveGenerationID), timeText(value.LastSeenAt))
	if err != nil {
		return fmt.Errorf("store: upsert binding: %w", err)
	}
	return nil
}

func (s *Store) GetBinding(ctx context.Context, id string) (model.Binding, error) {
	return scanBinding(s.db.QueryRowContext(ctx, `SELECT id,component_id,host,project_id,scope,install_path,config_path,config_pointer,install_method,managed,classification,observed_hash,observed_version,trust_hash,active_generation_id,last_seen_at FROM bindings WHERE id=?`, id))
}

func scanBinding(row *sql.Row) (model.Binding, error) {
	var value model.Binding
	var project, generation sql.NullString
	var managed int
	var seen string
	err := row.Scan(&value.ID, &value.ComponentID, &value.Host, &project, &value.Scope, &value.InstallPath, &value.ConfigPath, &value.ConfigPointer, &value.InstallMethod, &managed, &value.Classification, &value.ObservedHash, &value.ObservedVersion, &value.TrustHash, &generation, &seen)
	if err != nil {
		return value, err
	}
	value.ProjectID = project.String
	value.ActiveGenerationID = generation.String
	value.Managed = managed != 0
	value.LastSeenAt, err = parseTime(seen)
	return value, err
}

func (s *Store) ListBindings(ctx context.Context, componentID string) ([]model.Binding, error) {
	query := `SELECT id,component_id,host,project_id,scope,install_path,config_path,config_pointer,install_method,managed,classification,observed_hash,observed_version,trust_hash,active_generation_id,last_seen_at FROM bindings`
	var rows *sql.Rows
	var err error
	if componentID == "" {
		rows, err = s.db.QueryContext(ctx, query+` ORDER BY install_path`)
	} else {
		rows, err = s.db.QueryContext(ctx, query+` WHERE component_id=? ORDER BY install_path`, componentID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.Binding{}
	for rows.Next() {
		var value model.Binding
		var project, generation sql.NullString
		var managed int
		var seen string
		if err := rows.Scan(&value.ID, &value.ComponentID, &value.Host, &project, &value.Scope, &value.InstallPath, &value.ConfigPath, &value.ConfigPointer, &value.InstallMethod, &managed, &value.Classification, &value.ObservedHash, &value.ObservedVersion, &value.TrustHash, &generation, &seen); err != nil {
			return nil, err
		}
		value.ProjectID = project.String
		value.ActiveGenerationID = generation.String
		value.Managed = managed != 0
		value.LastSeenAt, err = parseTime(seen)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) SetPolicy(ctx context.Context, value model.Policy) error {
	if err := value.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO policies(binding_id,track_channel,constraint_text,expected_integrity,apply_mode,notify_mode,local_cap_mode,updated_at) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(binding_id) DO UPDATE SET track_channel=excluded.track_channel,constraint_text=excluded.constraint_text,expected_integrity=excluded.expected_integrity,apply_mode=excluded.apply_mode,notify_mode=excluded.notify_mode,local_cap_mode=excluded.local_cap_mode,updated_at=excluded.updated_at`,
		value.BindingID, value.TrackChannel, value.Constraint, value.ExpectedIntegrity, value.ApplyMode, value.NotifyMode, value.LocalCapMode, timeText(value.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: set policy: %w", err)
	}
	return nil
}

func (s *Store) GetPolicy(ctx context.Context, bindingID string) (model.Policy, error) {
	var value model.Policy
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT binding_id,track_channel,constraint_text,expected_integrity,apply_mode,notify_mode,local_cap_mode,updated_at FROM policies WHERE binding_id=?`, bindingID).
		Scan(&value.BindingID, &value.TrackChannel, &value.Constraint, &value.ExpectedIntegrity, &value.ApplyMode, &value.NotifyMode, &value.LocalCapMode, &updated)
	if err != nil {
		return value, err
	}
	value.UpdatedAt, err = parseTime(updated)
	return value, err
}

// CompareAndSetPolicy prevents a confirmation-time or background worker race
// from overwriting a newer local policy decision.
func (s *Store) CompareAndSetPolicy(ctx context.Context, expected, next model.Policy) error {
	if expected.BindingID == "" || next.BindingID != expected.BindingID {
		return errors.New("store: policy compare-and-set binding mismatch")
	}
	if err := next.Validate(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE policies SET track_channel=?,constraint_text=?,expected_integrity=?,apply_mode=?,notify_mode=?,local_cap_mode=?,updated_at=?
		WHERE binding_id=? AND track_channel=? AND constraint_text=? AND expected_integrity=? AND apply_mode=? AND notify_mode=? AND local_cap_mode=? AND updated_at=?`,
		next.TrackChannel, next.Constraint, next.ExpectedIntegrity, next.ApplyMode, next.NotifyMode, next.LocalCapMode, timeText(next.UpdatedAt),
		expected.BindingID, expected.TrackChannel, expected.Constraint, expected.ExpectedIntegrity, expected.ApplyMode, expected.NotifyMode, expected.LocalCapMode, timeText(expected.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: compare and set policy: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("store: policy changed after preview")
	}
	return nil
}

func (s *Store) PutObjectRecord(ctx context.Context, value model.ObjectRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO objects(hash,kind,size,relative_path,verified_at,created_at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(hash) DO UPDATE SET kind=excluded.kind,size=excluded.size,relative_path=excluded.relative_path,verified_at=excluded.verified_at`,
		value.Hash, value.Kind, value.Size, value.RelativePath, timeText(value.VerifiedAt), timeText(value.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: put object record: %w", err)
	}
	return nil
}

func (s *Store) PutBaseline(ctx context.Context, value model.Baseline) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO baselines(id,binding_id,source_id,resolved_ref,tree_hash,created_at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET id=excluded.id
		WHERE baselines.binding_id=excluded.binding_id AND baselines.source_id=excluded.source_id AND baselines.resolved_ref=excluded.resolved_ref AND baselines.tree_hash=excluded.tree_hash`,
		value.ID, value.BindingID, value.SourceID, value.ResolvedRef, value.TreeHash, timeText(value.CreatedAt))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("store: baseline identity is immutable")
	}
	return nil
}

func (s *Store) LatestBaseline(ctx context.Context, bindingID string) (model.Baseline, error) {
	var value model.Baseline
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id,binding_id,source_id,resolved_ref,tree_hash,created_at FROM baselines WHERE binding_id=? ORDER BY created_at DESC LIMIT 1`, bindingID).
		Scan(&value.ID, &value.BindingID, &value.SourceID, &value.ResolvedRef, &value.TreeHash, &created)
	if err != nil {
		return value, err
	}
	value.CreatedAt, err = parseTime(created)
	return value, err
}

func (s *Store) GetBaseline(ctx context.Context, id string) (model.Baseline, error) {
	var value model.Baseline
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id,binding_id,source_id,resolved_ref,tree_hash,created_at FROM baselines WHERE id=?`, id).
		Scan(&value.ID, &value.BindingID, &value.SourceID, &value.ResolvedRef, &value.TreeHash, &created)
	if err != nil {
		return value, err
	}
	value.CreatedAt, err = parseTime(created)
	return value, err
}

func (s *Store) PutOverlay(ctx context.Context, value model.Overlay) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO overlays(id,binding_id,baseline_id,tree_hash,status,created_at) VALUES(?,?,?,?,?,?)`, value.ID, value.BindingID, value.BaselineID, value.TreeHash, value.Status, timeText(value.CreatedAt))
	return err
}

func (s *Store) GetOverlay(ctx context.Context, id string) (model.Overlay, error) {
	var value model.Overlay
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id,binding_id,baseline_id,tree_hash,status,created_at FROM overlays WHERE id=?`, id).
		Scan(&value.ID, &value.BindingID, &value.BaselineID, &value.TreeHash, &value.Status, &created)
	if err != nil {
		return value, err
	}
	value.CreatedAt, err = parseTime(created)
	return value, err
}

func (s *Store) PutDependency(ctx context.Context, value model.Dependency) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO dependencies(id,from_component_id,to_component_id,package_identity,constraint_text,evidence_path,evidence_line,explicit) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET to_component_id=excluded.to_component_id,package_identity=excluded.package_identity,constraint_text=excluded.constraint_text,evidence_path=excluded.evidence_path,evidence_line=excluded.evidence_line,explicit=excluded.explicit`,
		value.ID, value.FromComponentID, nullIfEmpty(value.ToComponentID), value.PackageIdentity, value.Constraint, value.EvidencePath, value.EvidenceLine, boolInt(value.Explicit))
	return err
}

func (s *Store) PutGeneration(ctx context.Context, value model.Generation) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO generations(id,binding_id,candidate_id,resolved_ref,tree_hash,integrity_hash,state,created_at,activated_at) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET candidate_id=excluded.candidate_id,resolved_ref=excluded.resolved_ref,tree_hash=excluded.tree_hash,integrity_hash=excluded.integrity_hash,state=excluded.state,activated_at=excluded.activated_at`,
		value.ID, value.BindingID, nullIfEmpty(value.CandidateID), value.ResolvedRef, value.TreeHash, value.IntegrityHash, value.State, timeText(value.CreatedAt), nullableTimeText(value.ActivatedAt))
	return err
}

// FailPreparedGenerationIfUnjournaled retires a materialized generation when
// activation failed before its durable intent was written. Once an intent
// exists, recovery owns both the directory and state transition instead.
func (s *Store) FailPreparedGenerationIfUnjournaled(ctx context.Context, bindingID, generationID string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE generations SET state='failed'
		WHERE id=? AND binding_id=? AND state='prepared'
		AND NOT EXISTS (SELECT 1 FROM activation_intents WHERE new_generation_id=?)`, generationID, bindingID, generationID)
	if err != nil {
		return false, fmt.Errorf("store: fail unjournaled generation: %w", err)
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

// CompareAndSetGenerationSnapshot freezes the exact effective contents of an
// active managed generation before leaving it. This makes a customized
// generation a verifiable rollback target without allowing a stale scan to
// overwrite a concurrently changed pointer or snapshot.
func (s *Store) CompareAndSetGenerationSnapshot(ctx context.Context, bindingID, generationID, expectedTree, expectedIntegrity, nextTree, nextIntegrity string) error {
	if bindingID == "" || generationID == "" || nextTree == "" || nextIntegrity == "" {
		return errors.New("store: generation snapshot identity is incomplete")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE generations SET tree_hash=?,integrity_hash=?
		WHERE id=? AND binding_id=? AND tree_hash=? AND integrity_hash=? AND state='active'
		AND EXISTS (SELECT 1 FROM bindings WHERE id=? AND active_generation_id=?)`,
		nextTree, nextIntegrity, generationID, bindingID, expectedTree, expectedIntegrity, bindingID, generationID)
	if err != nil {
		return fmt.Errorf("store: freeze generation snapshot: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("store: active generation changed before snapshot freeze")
	}
	return nil
}

func (s *Store) GetGeneration(ctx context.Context, bindingID, generationID string) (model.Generation, error) {
	return scanGeneration(s.db.QueryRowContext(ctx, `SELECT id,binding_id,candidate_id,resolved_ref,tree_hash,integrity_hash,state,created_at,activated_at FROM generations WHERE id=? AND binding_id=?`, generationID, bindingID))
}

func scanGeneration(row *sql.Row) (model.Generation, error) {
	var value model.Generation
	var candidate, activated sql.NullString
	var created string
	err := row.Scan(&value.ID, &value.BindingID, &candidate, &value.ResolvedRef, &value.TreeHash, &value.IntegrityHash, &value.State, &created, &activated)
	if err != nil {
		return value, err
	}
	value.CandidateID = candidate.String
	value.CreatedAt, err = parseTime(created)
	if err != nil {
		return value, err
	}
	value.ActivatedAt, err = scanNullableTime(activated)
	return value, err
}

func (s *Store) ListGenerations(ctx context.Context, bindingID string) ([]model.Generation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,binding_id,candidate_id,resolved_ref,tree_hash,integrity_hash,state,created_at,activated_at FROM generations WHERE binding_id=? ORDER BY created_at DESC,id DESC`, bindingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.Generation{}
	for rows.Next() {
		var value model.Generation
		var candidate, activated sql.NullString
		var created string
		if err := rows.Scan(&value.ID, &value.BindingID, &candidate, &value.ResolvedRef, &value.TreeHash, &value.IntegrityHash, &value.State, &created, &activated); err != nil {
			return nil, err
		}
		value.CandidateID = candidate.String
		value.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		value.ActivatedAt, err = scanNullableTime(activated)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) SetActiveGeneration(ctx context.Context, bindingID, generationID string) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE generations SET state='inactive' WHERE binding_id=? AND state='active'`, bindingID); err != nil {
			return err
		}
		generationResult, err := tx.ExecContext(ctx, `UPDATE generations SET state='active',activated_at=? WHERE id=? AND binding_id=?`, timeText(time.Now()), generationID, bindingID)
		if err != nil {
			return err
		}
		generationCount, err := generationResult.RowsAffected()
		if err != nil {
			return err
		}
		if generationCount != 1 {
			return fmt.Errorf("store: generation %s not found for binding %s", generationID, bindingID)
		}
		result, err := tx.ExecContext(ctx, `UPDATE bindings SET active_generation_id=? WHERE id=?`, generationID, bindingID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("store: binding %s not found", bindingID)
		}
		return nil
	})
}
