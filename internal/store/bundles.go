package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/z2z23n0/tooltend/internal/model"
)

type BundleCounts struct {
	Total              int `json:"total"`
	Configured         int `json:"configured"`
	Unconfigured       int `json:"unconfigured"`
	Managed            int `json:"managed"`
	Observe            int `json:"observe"`
	Unresolved         int `json:"unresolved"`
	FailedTransactions int `json:"failed_transactions"`
	UpdatesAvailable   int `json:"updates_available"`
}

type InstallationObservation struct {
	InstallationID string
	Version        string
	Hash           string
}

func (s *Store) UpsertBundle(ctx context.Context, value model.Bundle) error {
	if err := value.Owner.Validate(); err != nil {
		return err
	}
	if err := value.ConfigState.Validate(); err != nil {
		return err
	}
	if err := value.Confidence.Validate(); err != nil {
		return err
	}
	if value.ID == "" || value.Slug == "" || value.Name == "" || value.RecipeID == "" || value.RecipeVersion == "" {
		return errors.New("store: bundle identity is incomplete")
	}
	if value.MetadataJSON == "" {
		value.MetadataJSON = "{}"
	}
	now := time.Now().UTC()
	if value.DiscoveredAt.IsZero() {
		value.DiscoveredAt = now
	}
	if value.LastSeenAt.IsZero() {
		value.LastSeenAt = now
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO bundles(id,slug,name,recipe_id,recipe_version,recipe_source,lifecycle_owner,config_state,confidence,current_release_id,metadata_json,discovered_at,last_seen_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET slug=excluded.slug,name=excluded.name,
		recipe_id=excluded.recipe_id,recipe_version=excluded.recipe_version,recipe_source=excluded.recipe_source,
		lifecycle_owner=excluded.lifecycle_owner,confidence=excluded.confidence,metadata_json=excluded.metadata_json,last_seen_at=excluded.last_seen_at`,
		value.ID, value.Slug, value.Name, value.RecipeID, value.RecipeVersion, value.RecipeSource, value.Owner,
		value.ConfigState, value.Confidence, nullIfEmpty(value.CurrentReleaseID), value.MetadataJSON,
		timeText(value.DiscoveredAt), timeText(value.LastSeenAt))
	if err != nil {
		return fmt.Errorf("store: upsert bundle: %w", err)
	}
	return nil
}

func (s *Store) GetBundle(ctx context.Context, id string) (model.Bundle, error) {
	return scanBundle(s.db.QueryRowContext(ctx, `SELECT id,slug,name,recipe_id,recipe_version,recipe_source,lifecycle_owner,config_state,confidence,current_release_id,metadata_json,discovered_at,last_seen_at FROM bundles WHERE id=?`, id))
}

func (s *Store) GetBundleBySlug(ctx context.Context, slug string) (model.Bundle, error) {
	return scanBundle(s.db.QueryRowContext(ctx, `SELECT id,slug,name,recipe_id,recipe_version,recipe_source,lifecycle_owner,config_state,confidence,current_release_id,metadata_json,discovered_at,last_seen_at FROM bundles WHERE slug=? COLLATE NOCASE`, slug))
}

func (s *Store) ListBundles(ctx context.Context) ([]model.Bundle, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,slug,name,recipe_id,recipe_version,recipe_source,lifecycle_owner,config_state,confidence,current_release_id,metadata_json,discovered_at,last_seen_at FROM bundles ORDER BY lower(name),slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.Bundle{}
	for rows.Next() {
		value, scanErr := scanBundle(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

type bundleScanner interface{ Scan(...any) error }

func scanBundle(row bundleScanner) (model.Bundle, error) {
	var value model.Bundle
	var current sql.NullString
	var discovered, seen string
	if err := row.Scan(&value.ID, &value.Slug, &value.Name, &value.RecipeID, &value.RecipeVersion, &value.RecipeSource,
		&value.Owner, &value.ConfigState, &value.Confidence, &current, &value.MetadataJSON, &discovered, &seen); err != nil {
		return value, err
	}
	value.CurrentReleaseID = current.String
	var err error
	value.DiscoveredAt, err = parseTime(discovered)
	if err != nil {
		return value, err
	}
	value.LastSeenAt, err = parseTime(seen)
	return value, err
}

// PruneUnconfiguredBundles removes stale discovery-only rows. Configured
// bundles and their receipts are durable lifecycle state and are never
// deleted by a scan.
func (s *Store) PruneUnconfiguredBundles(ctx context.Context, seenBefore time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM bundles WHERE config_state='unconfigured' AND last_seen_at<?`, timeText(seenBefore))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) UpsertBundleRelease(ctx context.Context, value model.BundleRelease) error {
	if value.ManifestJSON == "" {
		value.ManifestJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO bundle_releases(id,bundle_id,version,resolved_ref,manifest_json,status,created_at)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET manifest_json=excluded.manifest_json,status=excluded.status`,
		value.ID, value.BundleID, value.Version, value.ResolvedRef, value.ManifestJSON, value.Status, timeText(value.CreatedAt))
	return err
}

func (s *Store) GetBundleRelease(ctx context.Context, id string) (model.BundleRelease, error) {
	var value model.BundleRelease
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id,bundle_id,version,resolved_ref,manifest_json,status,created_at FROM bundle_releases WHERE id=?`, id).
		Scan(&value.ID, &value.BundleID, &value.Version, &value.ResolvedRef, &value.ManifestJSON, &value.Status, &created)
	if err == nil {
		value.CreatedAt, err = parseTime(created)
	}
	return value, err
}

func (s *Store) CommitStagedBundleTransaction(ctx context.Context, transactionID, releaseID string, completed time.Time) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE bundle_transactions SET status='committed',error_code='',error_summary='',updated_at=?,completed_at=? WHERE id=? AND status='staging'`,
			timeText(completed), timeText(completed), transactionID)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			if err != nil {
				return err
			}
			return sql.ErrNoRows
		}
		result, err = tx.ExecContext(ctx, `UPDATE bundle_releases SET status='staged' WHERE id=?`, releaseID)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			if err != nil {
				return err
			}
			return sql.ErrNoRows
		}
		return nil
	})
}

func (s *Store) CommitBundleActivation(ctx context.Context, transactionID, bundleID, fromReleaseID, toReleaseID string, observations []InstallationObservation, receipt model.BundleReceipt, completed time.Time) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE bundle_transactions SET status='committed',error_code='',error_summary='',updated_at=?,completed_at=? WHERE id=? AND status IN ('activating','rolling_back')`,
			timeText(completed), timeText(completed), transactionID)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			if err != nil {
				return err
			}
			return sql.ErrNoRows
		}
		result, err = tx.ExecContext(ctx, `UPDATE bundles SET current_release_id=? WHERE id=?`, toReleaseID, bundleID)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			if err != nil {
				return err
			}
			return sql.ErrNoRows
		}
		if fromReleaseID != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE bundle_releases SET status='superseded' WHERE id=?`, fromReleaseID); err != nil {
				return err
			}
		}
		result, err = tx.ExecContext(ctx, `UPDATE bundle_releases SET status='active' WHERE id=? AND bundle_id=?`, toReleaseID, bundleID)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			if err != nil {
				return err
			}
			return sql.ErrNoRows
		}
		for _, observation := range observations {
			result, err = tx.ExecContext(ctx, `UPDATE installations SET observed_version=?,observed_hash=?,last_seen_at=? WHERE id=? AND bundle_id=?`,
				observation.Version, observation.Hash, timeText(completed), observation.InstallationID, bundleID)
			if err != nil {
				return err
			}
			if count, err := result.RowsAffected(); err != nil || count != 1 {
				if err != nil {
					return err
				}
				return sql.ErrNoRows
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO bundle_receipts(id,bundle_id,transaction_id,release_id,action,status,summary_json,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			receipt.ID, bundleID, transactionID, toReleaseID, receipt.Action, receipt.Status, jsonOr(receipt.SummaryJSON, "{}"), timeText(receipt.CreatedAt))
		return err
	})
}

func (s *Store) CommitBundleCompensation(ctx context.Context, transactionID, errorCode string, receipt model.BundleReceipt, completed time.Time) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE bundle_transactions SET status='rolled_back',error_code=?,error_summary='',updated_at=?,completed_at=? WHERE id=? AND status='rolling_back'`,
			errorCode, timeText(completed), timeText(completed), transactionID)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			if err != nil {
				return err
			}
			return sql.ErrNoRows
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO bundle_receipts(id,bundle_id,transaction_id,release_id,action,status,summary_json,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			receipt.ID, receipt.BundleID, transactionID, nullIfEmpty(receipt.ReleaseID), receipt.Action, receipt.Status,
			jsonOr(receipt.SummaryJSON, "{}"), timeText(receipt.CreatedAt))
		return err
	})
}

func (s *Store) SetBundleCurrentRelease(ctx context.Context, bundleID, releaseID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE bundles SET current_release_id=? WHERE id=?`, nullIfEmpty(releaseID), bundleID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) UpsertBundleArtifact(ctx context.Context, value model.BundleArtifact) error {
	if err := value.Kind.Validate(); err != nil {
		return err
	}
	if value.MetadataJSON == "" {
		value.MetadataJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO bundle_artifacts(id,bundle_id,release_id,recipe_key,kind,name,ordinal,required,driver,metadata_json)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET release_id=excluded.release_id,kind=excluded.kind,
		name=excluded.name,ordinal=excluded.ordinal,required=excluded.required,driver=excluded.driver,metadata_json=excluded.metadata_json`,
		value.ID, value.BundleID, nullIfEmpty(value.ReleaseID), value.RecipeKey, value.Kind, value.Name,
		value.Ordinal, boolInt(value.Required), value.Driver, value.MetadataJSON)
	return err
}

func (s *Store) ListBundleArtifacts(ctx context.Context, bundleID string) ([]model.BundleArtifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,bundle_id,release_id,recipe_key,kind,name,ordinal,required,driver,metadata_json FROM bundle_artifacts WHERE bundle_id=? ORDER BY ordinal,id`, bundleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.BundleArtifact{}
	for rows.Next() {
		var value model.BundleArtifact
		var release sql.NullString
		var required int
		if err := rows.Scan(&value.ID, &value.BundleID, &release, &value.RecipeKey, &value.Kind, &value.Name, &value.Ordinal, &required, &value.Driver, &value.MetadataJSON); err != nil {
			return nil, err
		}
		value.ReleaseID, value.Required = release.String, required != 0
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) UpsertInstallation(ctx context.Context, value model.Installation) error {
	if err := value.Owner.Validate(); err != nil {
		return err
	}
	if value.MetadataJSON == "" {
		value.MetadataJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO installations(id,bundle_id,artifact_id,driver,normalized_path,package_identity,source_identity,observed_version,observed_hash,lifecycle_owner,managed,metadata_json,last_seen_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET bundle_id=excluded.bundle_id,artifact_id=excluded.artifact_id,
		driver=excluded.driver,normalized_path=excluded.normalized_path,package_identity=excluded.package_identity,source_identity=excluded.source_identity,
		observed_version=excluded.observed_version,observed_hash=excluded.observed_hash,lifecycle_owner=excluded.lifecycle_owner,
		metadata_json=excluded.metadata_json,last_seen_at=excluded.last_seen_at`,
		value.ID, value.BundleID, nullIfEmpty(value.ArtifactID), value.Driver, value.Path, value.PackageIdentity, value.SourceIdentity,
		value.ObservedVersion, value.ObservedHash, value.Owner, boolInt(value.Managed), value.MetadataJSON, timeText(value.LastSeenAt))
	return err
}

func (s *Store) ListInstallations(ctx context.Context, bundleID string) ([]model.Installation, error) {
	query := `SELECT id,bundle_id,artifact_id,driver,normalized_path,package_identity,source_identity,observed_version,observed_hash,lifecycle_owner,managed,metadata_json,last_seen_at FROM installations`
	var rows *sql.Rows
	var err error
	if bundleID == "" {
		rows, err = s.db.QueryContext(ctx, query+` ORDER BY normalized_path,id`)
	} else {
		rows, err = s.db.QueryContext(ctx, query+` WHERE bundle_id=? ORDER BY normalized_path,id`, bundleID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.Installation{}
	for rows.Next() {
		var value model.Installation
		var artifact sql.NullString
		var managed int
		var seen string
		if err := rows.Scan(&value.ID, &value.BundleID, &artifact, &value.Driver, &value.Path, &value.PackageIdentity, &value.SourceIdentity,
			&value.ObservedVersion, &value.ObservedHash, &value.Owner, &managed, &value.MetadataJSON, &seen); err != nil {
			return nil, err
		}
		value.ArtifactID, value.Managed = artifact.String, managed != 0
		value.LastSeenAt, err = parseTime(seen)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) UpsertConsumerBinding(ctx context.Context, value model.ConsumerBinding) error {
	if err := value.Host.Validate(); err != nil {
		return err
	}
	if err := value.Scope.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO consumer_bindings(id,installation_id,binding_id,host,project_id,scope,config_path,config_pointer,last_seen_at)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET installation_id=excluded.installation_id,binding_id=excluded.binding_id,
		host=excluded.host,project_id=excluded.project_id,scope=excluded.scope,config_path=excluded.config_path,
		config_pointer=excluded.config_pointer,last_seen_at=excluded.last_seen_at`,
		value.ID, value.InstallationID, nullIfEmpty(value.BindingID), value.Host, nullIfEmpty(value.ProjectID), value.Scope,
		value.ConfigPath, value.ConfigPointer, timeText(value.LastSeenAt))
	return err
}

func (s *Store) ListConsumerBindings(ctx context.Context, installationID string) ([]model.ConsumerBinding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,installation_id,binding_id,host,project_id,scope,config_path,config_pointer,last_seen_at FROM consumer_bindings WHERE installation_id=? ORDER BY host,config_path,config_pointer`, installationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.ConsumerBinding{}
	for rows.Next() {
		var value model.ConsumerBinding
		var binding, project sql.NullString
		var seen string
		if err := rows.Scan(&value.ID, &value.InstallationID, &binding, &value.Host, &project, &value.Scope, &value.ConfigPath, &value.ConfigPointer, &seen); err != nil {
			return nil, err
		}
		value.BindingID, value.ProjectID = binding.String, project.String
		value.LastSeenAt, err = parseTime(seen)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) GetBundlePolicy(ctx context.Context, bundleID string) (model.BundlePolicy, error) {
	var value model.BundlePolicy
	var trusted int
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT bundle_id,mode,recipe_trusted,updated_at FROM bundle_policies WHERE bundle_id=?`, bundleID).
		Scan(&value.BundleID, &value.Mode, &trusted, &updated)
	if err != nil {
		return value, err
	}
	value.RecipeTrusted = trusted != 0
	value.UpdatedAt, err = parseTime(updated)
	return value, err
}

func (s *Store) ConfigureBundle(ctx context.Context, value model.BundlePolicy) error {
	if err := value.Mode.Validate(); err != nil {
		return err
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE bundles SET config_state='configured' WHERE id=?`, value.BundleID)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			if err != nil {
				return err
			}
			return sql.ErrNoRows
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO bundle_policies(bundle_id,mode,recipe_trusted,updated_at) VALUES(?,?,?,?)
			ON CONFLICT(bundle_id) DO UPDATE SET mode=excluded.mode,recipe_trusted=excluded.recipe_trusted,updated_at=excluded.updated_at`,
			value.BundleID, value.Mode, boolInt(value.RecipeTrusted), timeText(value.UpdatedAt))
		return err
	})
}

func (s *Store) BundleCounts(ctx context.Context) (BundleCounts, error) {
	var value BundleCounts
	err := s.db.QueryRowContext(ctx, `SELECT
		COUNT(*),
		COALESCE(SUM(config_state='configured'),0),
		COALESCE(SUM(config_state='unconfigured'),0),
		COALESCE(SUM(EXISTS(SELECT 1 FROM bundle_policies p WHERE p.bundle_id=bundles.id AND p.mode IN ('auto','manual'))),0),
		COALESCE(SUM(EXISTS(SELECT 1 FROM bundle_policies p WHERE p.bundle_id=bundles.id AND p.mode='observe')),0),
		COALESCE(SUM(lifecycle_owner='unresolved'),0)
		FROM bundles`).Scan(&value.Total, &value.Configured, &value.Unconfigured, &value.Managed, &value.Observe, &value.Unresolved)
	if err != nil {
		return value, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bundle_transactions WHERE status='failed'`).Scan(&value.FailedTransactions); err != nil {
		return value, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bundle_releases r JOIN bundles b ON b.id=r.bundle_id WHERE r.status='resolved' AND r.id<>COALESCE(b.current_release_id,'')`).Scan(&value.UpdatesAvailable); err != nil {
		return value, err
	}
	return value, nil
}

func (s *Store) ListUnfinishedBundleTransactions(ctx context.Context) ([]model.BundleTransaction, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,bundle_id,from_release_id,to_release_id,status,stage_only,error_code,error_summary,started_at,updated_at,completed_at FROM bundle_transactions WHERE status IN ('prepared','staging','activating','rolling_back') ORDER BY started_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.BundleTransaction
	for rows.Next() {
		value, scanErr := scanBundleTransaction(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) PutBundleTransaction(ctx context.Context, value model.BundleTransaction) error {
	if err := value.Status.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO bundle_transactions(id,bundle_id,from_release_id,to_release_id,status,stage_only,error_code,error_summary,started_at,updated_at,completed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, value.ID, value.BundleID, nullIfEmpty(value.FromReleaseID), nullIfEmpty(value.ToReleaseID), value.Status,
		boolInt(value.StageOnly), value.ErrorCode, value.ErrorSummary, timeText(value.StartedAt), timeText(value.UpdatedAt), nullableTimeText(value.CompletedAt))
	return err
}

func (s *Store) UpdateBundleTransaction(ctx context.Context, id string, status model.BundleTransactionStatus, code, summary string, completed *time.Time) error {
	if err := status.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE bundle_transactions SET status=?,error_code=?,error_summary=?,updated_at=?,completed_at=? WHERE id=?`,
		status, code, summary, timeText(time.Now().UTC()), nullableTimeText(completed), id)
	return err
}

func (s *Store) PutBundleTransactionStep(ctx context.Context, value model.BundleTransactionStep) error {
	if err := value.Status.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO bundle_transaction_steps(id,transaction_id,ordinal,artifact_id,installation_id,kind,status,command_json,rollback_json,before_json,after_json,error_code,error_summary,started_at,completed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, value.ID, value.TransactionID, value.Ordinal, nullIfEmpty(value.ArtifactID), nullIfEmpty(value.InstallationID),
		value.Kind, value.Status, jsonOr(value.CommandJSON, "[]"), jsonOr(value.RollbackJSON, "[]"), jsonOr(value.BeforeJSON, "{}"), jsonOr(value.AfterJSON, "{}"),
		value.ErrorCode, value.ErrorSummary, nullableTimeText(value.StartedAt), nullableTimeText(value.CompletedAt))
	return err
}

func (s *Store) UpdateBundleTransactionStep(ctx context.Context, id string, status model.BundleStepStatus, code, summary, afterJSON string, completed *time.Time) error {
	if err := status.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE bundle_transaction_steps SET status=?,error_code=?,error_summary=?,after_json=?,completed_at=? WHERE id=?`,
		status, code, summary, jsonOr(afterJSON, "{}"), nullableTimeText(completed), id)
	return err
}

func (s *Store) ListBundleTransactionSteps(ctx context.Context, transactionID string) ([]model.BundleTransactionStep, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,transaction_id,ordinal,artifact_id,installation_id,kind,status,command_json,rollback_json,before_json,after_json,error_code,error_summary,started_at,completed_at FROM bundle_transaction_steps WHERE transaction_id=? ORDER BY ordinal,id`, transactionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.BundleTransactionStep
	for rows.Next() {
		var value model.BundleTransactionStep
		var artifact, installation, started, completed sql.NullString
		if err := rows.Scan(&value.ID, &value.TransactionID, &value.Ordinal, &artifact, &installation, &value.Kind, &value.Status,
			&value.CommandJSON, &value.RollbackJSON, &value.BeforeJSON, &value.AfterJSON, &value.ErrorCode, &value.ErrorSummary, &started, &completed); err != nil {
			return nil, err
		}
		value.ArtifactID, value.InstallationID = artifact.String, installation.String
		if started.Valid {
			parsed, err := parseTime(started.String)
			if err != nil {
				return nil, err
			}
			value.StartedAt = &parsed
		}
		if completed.Valid {
			parsed, err := parseTime(completed.String)
			if err != nil {
				return nil, err
			}
			value.CompletedAt = &parsed
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) PutBundleReceipt(ctx context.Context, value model.BundleReceipt) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO bundle_receipts(id,bundle_id,transaction_id,release_id,action,status,summary_json,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		value.ID, value.BundleID, nullIfEmpty(value.TransactionID), nullIfEmpty(value.ReleaseID), value.Action, value.Status, jsonOr(value.SummaryJSON, "{}"), timeText(value.CreatedAt))
	return err
}

func (s *Store) ListBundleReceipts(ctx context.Context, bundleID string, limit int) ([]model.BundleReceipt, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT id,bundle_id,transaction_id,release_id,action,status,summary_json,created_at FROM bundle_receipts`
	args := []any{}
	if bundleID != "" {
		query += ` WHERE bundle_id=?`
		args = append(args, bundleID)
	}
	query += ` ORDER BY created_at DESC,id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.BundleReceipt{}
	for rows.Next() {
		var value model.BundleReceipt
		var transaction, release sql.NullString
		var created string
		if err := rows.Scan(&value.ID, &value.BundleID, &transaction, &release, &value.Action, &value.Status, &value.SummaryJSON, &created); err != nil {
			return nil, err
		}
		value.TransactionID, value.ReleaseID = transaction.String, release.String
		value.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) PutBundleHealthCheck(ctx context.Context, value model.BundleHealthCheck) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO bundle_health_checks(id,bundle_id,artifact_id,installation_id,name,status,summary,checked_at) VALUES(?,?,?,?,?,?,?,?)`,
		value.ID, value.BundleID, nullIfEmpty(value.ArtifactID), nullIfEmpty(value.InstallationID), value.Name, value.Status, value.Summary, timeText(value.CheckedAt))
	return err
}

func (s *Store) ListBundleHealthChecks(ctx context.Context, bundleID string, limit int) ([]model.BundleHealthCheck, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,bundle_id,artifact_id,installation_id,name,status,summary,checked_at FROM bundle_health_checks WHERE bundle_id=? ORDER BY checked_at DESC,id DESC LIMIT ?`, bundleID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.BundleHealthCheck{}
	for rows.Next() {
		var value model.BundleHealthCheck
		var artifact, installation sql.NullString
		var checked string
		if err := rows.Scan(&value.ID, &value.BundleID, &artifact, &installation, &value.Name, &value.Status, &value.Summary, &checked); err != nil {
			return nil, err
		}
		value.ArtifactID, value.InstallationID = artifact.String, installation.String
		value.CheckedAt, err = parseTime(checked)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) EnqueueBundleTask(ctx context.Context, value model.BundleTask) (bool, error) {
	if value.ID == "" || value.BundleID == "" || value.IdempotencyKey == "" || value.Kind == "" {
		return false, errors.New("store: bundle task identity is incomplete")
	}
	if value.Status == "" {
		value.Status = model.TaskPending
	}
	if err := value.Status.Validate(); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO bundle_tasks(id,bundle_id,installation_id,kind,idempotency_key,status,attempts,next_attempt_at,lease_until,error_code,error_summary,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, value.ID, value.BundleID, nullIfEmpty(value.InstallationID), value.Kind,
		value.IdempotencyKey, value.Status, value.Attempts, timeText(value.NextAttemptAt), nullableTimeText(value.LeaseUntil), value.ErrorCode, value.ErrorSummary,
		timeText(value.CreatedAt), timeText(value.UpdatedAt))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (s *Store) ClaimBundleTask(ctx context.Context, now time.Time, lease time.Duration) (model.BundleTask, error) {
	var claimed model.BundleTask
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		nowText := timeText(now)
		if _, err := tx.ExecContext(ctx, `UPDATE bundle_tasks SET status='pending',lease_until=NULL,updated_at=? WHERE status='running' AND lease_until IS NOT NULL AND lease_until<=?`, nowText, nowText); err != nil {
			return err
		}
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM bundle_tasks WHERE status='pending' AND next_attempt_at<=? ORDER BY next_attempt_at,created_at,id LIMIT 1`, nowText).Scan(&id); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE bundle_tasks SET status='running',attempts=attempts+1,lease_until=?,updated_at=? WHERE id=? AND status='pending'`, timeText(now.Add(lease)), nowText, id)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			if err != nil {
				return err
			}
			return errors.New("store: bundle task claim raced")
		}
		var installation, leaseText sql.NullString
		var next, created, updated string
		if err := tx.QueryRowContext(ctx, `SELECT id,bundle_id,installation_id,kind,idempotency_key,status,attempts,next_attempt_at,lease_until,error_code,error_summary,created_at,updated_at FROM bundle_tasks WHERE id=?`, id).
			Scan(&claimed.ID, &claimed.BundleID, &installation, &claimed.Kind, &claimed.IdempotencyKey, &claimed.Status, &claimed.Attempts, &next,
				&leaseText, &claimed.ErrorCode, &claimed.ErrorSummary, &created, &updated); err != nil {
			return err
		}
		claimed.InstallationID = installation.String
		claimed.NextAttemptAt, err = parseTime(next)
		if err != nil {
			return err
		}
		claimed.LeaseUntil, err = scanNullableTime(leaseText)
		if err != nil {
			return err
		}
		claimed.CreatedAt, err = parseTime(created)
		if err != nil {
			return err
		}
		claimed.UpdatedAt, err = parseTime(updated)
		return err
	})
	return claimed, err
}

func (s *Store) CompleteBundleTask(ctx context.Context, id string) error {
	return updateBundleTaskTerminal(ctx, s.db, id, "succeeded", "", "")
}

func (s *Store) FailBundleTask(ctx context.Context, id, code string) error {
	return updateBundleTaskTerminal(ctx, s.db, id, "failed", code, "")
}

func (s *Store) RetryBundleTask(ctx context.Context, id, code string, next time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE bundle_tasks SET status='pending',lease_until=NULL,error_code=?,error_summary='',next_attempt_at=?,updated_at=? WHERE id=? AND status='running'`, code, timeText(next), timeText(time.Now()), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func updateBundleTaskTerminal(ctx context.Context, db *sql.DB, id, status, code, summary string) error {
	result, err := db.ExecContext(ctx, `UPDATE bundle_tasks SET status=?,lease_until=NULL,error_code=?,error_summary=?,updated_at=? WHERE id=? AND status='running'`, status, code, summary, timeText(time.Now()), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func scanBundleTransaction(row bundleScanner) (model.BundleTransaction, error) {
	var value model.BundleTransaction
	var from, to, completed sql.NullString
	var stageOnly int
	var started, updated string
	if err := row.Scan(&value.ID, &value.BundleID, &from, &to, &value.Status, &stageOnly, &value.ErrorCode, &value.ErrorSummary, &started, &updated, &completed); err != nil {
		return value, err
	}
	value.FromReleaseID, value.ToReleaseID, value.StageOnly = from.String, to.String, stageOnly != 0
	var err error
	value.StartedAt, err = parseTime(started)
	if err != nil {
		return value, err
	}
	value.UpdatedAt, err = parseTime(updated)
	if err != nil {
		return value, err
	}
	if completed.Valid {
		parsed, parseErr := parseTime(completed.String)
		if parseErr != nil {
			return value, parseErr
		}
		value.CompletedAt = &parsed
	}
	return value, nil
}

func jsonOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
