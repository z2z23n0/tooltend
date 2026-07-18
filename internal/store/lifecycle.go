package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/z2z23n0/tooltend/internal/model"
)

type PendingReview struct {
	ComponentID   string                `json:"component_id"`
	ComponentName string                `json:"component_name"`
	BindingID     string                `json:"binding_id"`
	Candidate     model.UpdateCandidate `json:"candidate"`
	Bundle        *model.ReviewBundle   `json:"bundle,omitempty"`
}

type TaskCounts struct {
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

func (s *Store) PutCandidate(ctx context.Context, value model.UpdateCandidate) error {
	if err := value.Status.Validate(); err != nil {
		return err
	}
	if err := value.ReviewClass.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	if value.CreatedAt.IsZero() {
		value.CreatedAt = now
	}
	if value.UpdatedAt.IsZero() {
		value.UpdatedAt = now
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO candidates(id,binding_id,source_id,resolved_ref,upstream_tree_hash,baseline_id,overlay_id,merged_tree_hash,candidate_hash,status,review_class,failure_code,failure_summary,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET resolved_ref=excluded.resolved_ref,upstream_tree_hash=excluded.upstream_tree_hash,
		baseline_id=excluded.baseline_id,overlay_id=excluded.overlay_id,merged_tree_hash=excluded.merged_tree_hash,
		status=excluded.status,review_class=excluded.review_class,failure_code=excluded.failure_code,failure_summary=excluded.failure_summary,updated_at=excluded.updated_at
		WHERE candidates.candidate_hash=excluded.candidate_hash AND candidates.binding_id=excluded.binding_id AND candidates.source_id=excluded.source_id`,
		value.ID, value.BindingID, value.SourceID, value.ResolvedRef, nullIfEmpty(value.UpstreamTreeHash), nullIfEmpty(value.BaselineID), nullIfEmpty(value.OverlayID), nullIfEmpty(value.MergedTreeHash), value.CandidateHash, value.Status, value.ReviewClass, value.FailureCode, value.FailureSummary, timeText(value.CreatedAt), timeText(value.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: put candidate: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("store: candidate identity or hash is immutable")
	}
	return nil
}

func (s *Store) GetCandidate(ctx context.Context, id string) (model.UpdateCandidate, error) {
	return scanCandidate(s.db.QueryRowContext(ctx, `SELECT id,binding_id,source_id,resolved_ref,upstream_tree_hash,baseline_id,overlay_id,merged_tree_hash,candidate_hash,status,review_class,failure_code,failure_summary,created_at,updated_at FROM candidates WHERE id=?`, id))
}

func scanCandidate(row *sql.Row) (model.UpdateCandidate, error) {
	var value model.UpdateCandidate
	var upstream, baseline, overlay, merged sql.NullString
	var created, updated string
	err := row.Scan(&value.ID, &value.BindingID, &value.SourceID, &value.ResolvedRef, &upstream, &baseline, &overlay, &merged, &value.CandidateHash, &value.Status, &value.ReviewClass, &value.FailureCode, &value.FailureSummary, &created, &updated)
	if err != nil {
		return value, err
	}
	value.UpstreamTreeHash = upstream.String
	value.BaselineID = baseline.String
	value.OverlayID = overlay.String
	value.MergedTreeHash = merged.String
	value.CreatedAt, err = parseTime(created)
	if err != nil {
		return value, err
	}
	value.UpdatedAt, err = parseTime(updated)
	return value, err
}

func (s *Store) ListCandidates(ctx context.Context, bindingID string, status model.CandidateStatus) ([]model.UpdateCandidate, error) {
	query := `SELECT id,binding_id,source_id,resolved_ref,upstream_tree_hash,baseline_id,overlay_id,merged_tree_hash,candidate_hash,status,review_class,failure_code,failure_summary,created_at,updated_at FROM candidates WHERE 1=1`
	args := []any{}
	if bindingID != "" {
		query += ` AND binding_id=?`
		args = append(args, bindingID)
	}
	if status != "" {
		query += ` AND status=?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.UpdateCandidate{}
	for rows.Next() {
		var value model.UpdateCandidate
		var upstream, baseline, overlay, merged sql.NullString
		var created, updated string
		if err := rows.Scan(&value.ID, &value.BindingID, &value.SourceID, &value.ResolvedRef, &upstream, &baseline, &overlay, &merged, &value.CandidateHash, &value.Status, &value.ReviewClass, &value.FailureCode, &value.FailureSummary, &created, &updated); err != nil {
			return nil, err
		}
		value.UpstreamTreeHash = upstream.String
		value.BaselineID = baseline.String
		value.OverlayID = overlay.String
		value.MergedTreeHash = merged.String
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

func (s *Store) TransitionCandidate(ctx context.Context, id string, to model.CandidateStatus, failureCode, failureSummary string) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		var from model.CandidateStatus
		if err := tx.QueryRowContext(ctx, `SELECT status FROM candidates WHERE id=?`, id).Scan(&from); err != nil {
			return err
		}
		if err := model.ValidateCandidateTransition(from, to); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE candidates SET status=?,failure_code=?,failure_summary=?,updated_at=? WHERE id=? AND status=?`, to, failureCode, failureSummary, timeText(time.Now()), id, from)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return errors.New("store: concurrent candidate transition")
		}
		return nil
	})
}

// ResetCandidateForRetry resumes a deterministic pipeline after a process
// stopped in a transient pre-activation state. An activating candidate is
// deliberately excluded because it must be recovered through its journal.
func (s *Store) ResetCandidateForRetry(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE candidates SET status='available',merged_tree_hash=NULL,review_class='none',failure_code='',failure_summary='',updated_at=?
		WHERE id=? AND status IN ('staging','verified','merging','validating')
		AND NOT EXISTS (
			SELECT 1 FROM activation_intents ai WHERE ai.candidate_id=candidates.id AND ai.phase IN ('prepared','pointer_switched')
		)`, timeText(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: reset candidate for retry: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("store: candidate is not safely retryable")
	}
	return nil
}

func (s *Store) PutReviewBundle(ctx context.Context, value model.ReviewBundle) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		var hash string
		if err := tx.QueryRowContext(ctx, `SELECT candidate_hash FROM candidates WHERE id=?`, value.CandidateID).Scan(&hash); err != nil {
			return err
		}
		if hash != value.CandidateHash {
			return errors.New("store: review bundle candidate hash mismatch")
		}
		if value.RiskTypesJSON == "" {
			value.RiskTypesJSON = "[]"
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO review_bundles(id,candidate_id,candidate_hash,object_hash,risk_types_json,created_at) VALUES(?,?,?,?,?,?)
			ON CONFLICT(candidate_id) DO UPDATE SET id=excluded.id,candidate_hash=excluded.candidate_hash,object_hash=excluded.object_hash,risk_types_json=excluded.risk_types_json,created_at=excluded.created_at`,
			value.ID, value.CandidateID, value.CandidateHash, value.ObjectHash, value.RiskTypesJSON, timeText(value.CreatedAt))
		return err
	})
}

// TransitionCandidateWithReviewBundle makes needs_review and its required
// bundle visible atomically. Immutable object bytes/records may be prepared
// first, but no reader can observe a terminal review state without the row it
// needs to inspect and submit a verdict against.
func (s *Store) TransitionCandidateWithReviewBundle(ctx context.Context, id, failureCode, failureSummary string, value model.ReviewBundle) error {
	if value.ID == "" || value.CandidateID != id || value.CandidateHash == "" || value.ObjectHash == "" {
		return errors.New("store: review bundle identity is incomplete")
	}
	if value.RiskTypesJSON == "" {
		value.RiskTypesJSON = "[]"
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		var from model.CandidateStatus
		var candidateHash string
		if err := tx.QueryRowContext(ctx, `SELECT status,candidate_hash FROM candidates WHERE id=?`, id).Scan(&from, &candidateHash); err != nil {
			return err
		}
		if candidateHash != value.CandidateHash {
			return errors.New("store: review bundle candidate hash mismatch")
		}
		if err := model.ValidateCandidateTransition(from, model.CandidateNeedsReview); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_bundles(id,candidate_id,candidate_hash,object_hash,risk_types_json,created_at) VALUES(?,?,?,?,?,?)
			ON CONFLICT(candidate_id) DO UPDATE SET id=excluded.id,candidate_hash=excluded.candidate_hash,object_hash=excluded.object_hash,risk_types_json=excluded.risk_types_json,created_at=excluded.created_at`,
			value.ID, value.CandidateID, value.CandidateHash, value.ObjectHash, value.RiskTypesJSON, timeText(value.CreatedAt)); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE candidates SET status='needs_review',failure_code=?,failure_summary=?,updated_at=? WHERE id=? AND status=?`,
			failureCode, failureSummary, timeText(time.Now()), id, from)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return errors.New("store: concurrent candidate transition")
		}
		return nil
	})
}

func (s *Store) GetReviewBundle(ctx context.Context, candidateID string) (model.ReviewBundle, error) {
	var value model.ReviewBundle
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id,candidate_id,candidate_hash,object_hash,risk_types_json,created_at FROM review_bundles WHERE candidate_id=?`, candidateID).
		Scan(&value.ID, &value.CandidateID, &value.CandidateHash, &value.ObjectHash, &value.RiskTypesJSON, &created)
	if err != nil {
		return value, err
	}
	value.CreatedAt, err = parseTime(created)
	return value, err
}

func (s *Store) ListReviews(ctx context.Context, candidateID string) ([]model.Review, error) {
	query := `SELECT id,candidate_id,candidate_hash,actor_type,verdict,risk_type,summary,created_at FROM reviews`
	args := []any{}
	if candidateID != "" {
		query += ` WHERE candidate_id=?`
		args = append(args, candidateID)
	}
	query += ` ORDER BY created_at DESC,id DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.Review{}
	for rows.Next() {
		var value model.Review
		var created string
		if err := rows.Scan(&value.ID, &value.CandidateID, &value.CandidateHash, &value.ActorType, &value.Verdict, &value.RiskType, &value.Summary, &created); err != nil {
			return nil, err
		}
		value.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) ListPendingReviews(ctx context.Context, componentID string) ([]PendingReview, error) {
	components, err := s.ListComponents(ctx)
	if err != nil {
		return nil, err
	}
	componentNames := make(map[string]string, len(components))
	for _, component := range components {
		componentNames[component.ID] = component.Name
	}
	bindings, err := s.ListBindings(ctx, componentID)
	if err != nil {
		return nil, err
	}
	result := []PendingReview{}
	for _, binding := range bindings {
		candidates, err := s.ListCandidates(ctx, binding.ID, model.CandidateNeedsReview)
		if err != nil {
			return nil, err
		}
		for _, candidate := range candidates {
			item := PendingReview{ComponentID: binding.ComponentID, ComponentName: componentNames[binding.ComponentID], BindingID: binding.ID, Candidate: candidate}
			bundle, bundleErr := s.GetReviewBundle(ctx, candidate.ID)
			if bundleErr == nil {
				item.Bundle = &bundle
			} else if !errors.Is(bundleErr, sql.ErrNoRows) {
				return nil, bundleErr
			}
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].Candidate.CreatedAt.Equal(result[j].Candidate.CreatedAt) {
			return result[i].Candidate.CreatedAt.After(result[j].Candidate.CreatedAt)
		}
		return result[i].Candidate.ID < result[j].Candidate.ID
	})
	return result, nil
}

func (s *Store) SubmitReview(ctx context.Context, value model.Review) error {
	if err := value.Validate(); err != nil {
		return err
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		var hash string
		var status model.CandidateStatus
		var class model.ReviewClass
		if err := tx.QueryRowContext(ctx, `SELECT candidate_hash,status,review_class FROM candidates WHERE id=?`, value.CandidateID).Scan(&hash, &status, &class); err != nil {
			return err
		}
		if hash != value.CandidateHash {
			return errors.New("store: review candidate hash mismatch")
		}
		if value.ActorType == model.ActorAgent && value.Verdict == model.VerdictSafe && class != model.ReviewSemanticSkillOnly {
			return errors.New("store: agent cannot approve this review class")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO reviews(id,candidate_id,candidate_hash,actor_type,verdict,risk_type,summary,created_at) VALUES(?,?,?,?,?,?,?,?)`, value.ID, value.CandidateID, value.CandidateHash, value.ActorType, value.Verdict, value.RiskType, value.Summary, timeText(value.CreatedAt)); err != nil {
			return err
		}
		if value.Verdict == model.VerdictSafe && status == model.CandidateNeedsReview {
			_, err := tx.ExecContext(ctx, `UPDATE candidates SET status='ready',updated_at=? WHERE id=? AND candidate_hash=? AND status='needs_review'`, timeText(time.Now()), value.CandidateID, value.CandidateHash)
			return err
		}
		return nil
	})
}

func (s *Store) PrepareActivation(ctx context.Context, intent model.ActivationIntent) error {
	if intent.Phase == "" {
		intent.Phase = model.ActivationPrepared
	}
	if intent.Phase != model.ActivationPrepared {
		return errors.New("store: new activation intent must be prepared")
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		var status model.CandidateStatus
		if err := tx.QueryRowContext(ctx, `SELECT status FROM candidates WHERE id=? AND binding_id=?`, intent.CandidateID, intent.BindingID).Scan(&status); err != nil {
			return err
		}
		if err := model.ValidateCandidateTransition(status, model.CandidateActivating); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO activation_intents(id,binding_id,candidate_id,old_generation_id,new_generation_id,expected_pointer,phase,error_code,started_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, intent.ID, intent.BindingID, intent.CandidateID, nullIfEmpty(intent.OldGenerationID), intent.NewGenerationID, intent.ExpectedPointer, intent.Phase, intent.ErrorCode, timeText(intent.StartedAt), timeText(intent.UpdatedAt)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE candidates SET status='activating',updated_at=? WHERE id=? AND status=?`, timeText(time.Now()), intent.CandidateID, status)
		return err
	})
}

func (s *Store) UpdateActivationPhase(ctx context.Context, id string, phase model.ActivationPhase, errorCode string) error {
	if err := phase.Validate(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE activation_intents SET phase=?,error_code=?,updated_at=? WHERE id=?`, phase, errorCode, timeText(time.Now()), id)
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

func (s *Store) ListPreparedActivations(ctx context.Context) ([]model.ActivationIntent, error) {
	return s.ListUnfinishedActivations(ctx)
}

func (s *Store) ListUnfinishedActivations(ctx context.Context) ([]model.ActivationIntent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,binding_id,candidate_id,old_generation_id,new_generation_id,expected_pointer,phase,error_code,started_at,updated_at FROM activation_intents WHERE phase IN ('prepared','pointer_switched') ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.ActivationIntent{}
	for rows.Next() {
		var value model.ActivationIntent
		var old sql.NullString
		var started, updated string
		if err := rows.Scan(&value.ID, &value.BindingID, &value.CandidateID, &old, &value.NewGenerationID, &value.ExpectedPointer, &value.Phase, &value.ErrorCode, &started, &updated); err != nil {
			return nil, err
		}
		value.OldGenerationID = old.String
		value.StartedAt, err = parseTime(started)
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

func (s *Store) PutReceipt(ctx context.Context, value model.Receipt) error {
	if err := value.Action.Validate(); err != nil {
		return err
	}
	if err := value.Status.Validate(); err != nil {
		return err
	}
	if value.SummaryJSON == "" {
		value.SummaryJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO receipts(id,binding_id,candidate_id,action,old_generation_id,new_generation_id,from_ref,to_ref,candidate_hash,status,summary_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, value.ID, value.BindingID, nullIfEmpty(value.CandidateID), value.Action, nullIfEmpty(value.OldGenerationID), nullIfEmpty(value.NewGenerationID), value.FromRef, value.ToRef, value.CandidateHash, value.Status, value.SummaryJSON, timeText(value.CreatedAt))
	return err
}

func (s *Store) ListReceipts(ctx context.Context, bindingID string, limit int) ([]model.Receipt, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT id,binding_id,candidate_id,action,old_generation_id,new_generation_id,from_ref,to_ref,candidate_hash,status,summary_json,created_at FROM receipts`
	args := []any{}
	if bindingID != "" {
		query += ` WHERE binding_id=?`
		args = append(args, bindingID)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.Receipt{}
	for rows.Next() {
		var value model.Receipt
		var candidate, old, new sql.NullString
		var created string
		if err := rows.Scan(&value.ID, &value.BindingID, &candidate, &value.Action, &old, &new, &value.FromRef, &value.ToRef, &value.CandidateHash, &value.Status, &value.SummaryJSON, &created); err != nil {
			return nil, err
		}
		value.CandidateID = candidate.String
		value.OldGenerationID = old.String
		value.NewGenerationID = new.String
		value.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) EnqueueTask(ctx context.Context, value model.Task) (bool, error) {
	if value.Status == "" {
		value.Status = model.TaskPending
	}
	if value.Status != model.TaskPending {
		return false, errors.New("store: new task must be pending")
	}
	now := time.Now().UTC()
	if value.NextAttemptAt.IsZero() {
		value.NextAttemptAt = now
	}
	if value.CreatedAt.IsZero() {
		value.CreatedAt = now
	}
	if value.UpdatedAt.IsZero() {
		value.UpdatedAt = now
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO tasks(id,kind,binding_id,candidate_id,idempotency_key,status,attempts,next_attempt_at,lease_until,error_code,error_summary,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, value.ID, value.Kind, nullIfEmpty(value.BindingID), nullIfEmpty(value.CandidateID), value.IdempotencyKey, value.Status, value.Attempts, timeText(value.NextAttemptAt), nullableTimeText(value.LeaseUntil), value.ErrorCode, value.ErrorSummary, timeText(value.CreatedAt), timeText(value.UpdatedAt))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (s *Store) CountTasks(ctx context.Context) (TaskCounts, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status,count(*) FROM tasks GROUP BY status`)
	if err != nil {
		return TaskCounts{}, err
	}
	defer rows.Close()
	var result TaskCounts
	for rows.Next() {
		var status model.TaskStatus
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return TaskCounts{}, err
		}
		switch status {
		case model.TaskPending:
			result.Pending = count
		case model.TaskRunning:
			result.Running = count
		case model.TaskSucceeded:
			result.Succeeded = count
		case model.TaskFailed:
			result.Failed = count
		default:
			return TaskCounts{}, fmt.Errorf("store: invalid task status %q", status)
		}
	}
	return result, rows.Err()
}

func (s *Store) ClaimTask(ctx context.Context, now time.Time, lease time.Duration) (model.Task, error) {
	var claimed model.Task
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		nowText := timeText(now)
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='pending',lease_until=NULL,updated_at=? WHERE status='running' AND lease_until IS NOT NULL AND lease_until<=?`, nowText, nowText); err != nil {
			return err
		}
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE status='pending' AND next_attempt_at<=? ORDER BY CASE WHEN kind='adopt_runtime_auto' THEN 0 ELSE 1 END,next_attempt_at,created_at,id LIMIT 1`, nowText).Scan(&id); err != nil {
			return err
		}
		leaseUntil := now.Add(lease)
		result, err := tx.ExecContext(ctx, `UPDATE tasks SET status='running',attempts=attempts+1,lease_until=?,updated_at=? WHERE id=? AND status='pending'`, timeText(leaseUntil), nowText, id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return errors.New("store: task claim raced")
		}
		var binding, candidate, leaseText sql.NullString
		var next, created, updated string
		err = tx.QueryRowContext(ctx, `SELECT id,kind,binding_id,candidate_id,idempotency_key,status,attempts,next_attempt_at,lease_until,error_code,error_summary,created_at,updated_at FROM tasks WHERE id=?`, id).Scan(&claimed.ID, &claimed.Kind, &binding, &candidate, &claimed.IdempotencyKey, &claimed.Status, &claimed.Attempts, &next, &leaseText, &claimed.ErrorCode, &claimed.ErrorSummary, &created, &updated)
		if err != nil {
			return err
		}
		claimed.BindingID = binding.String
		claimed.CandidateID = candidate.String
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

func (s *Store) CompleteTask(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='succeeded',lease_until=NULL,error_code='',error_summary='',updated_at=? WHERE id=? AND status='running'`, timeText(time.Now()), id)
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

func (s *Store) RetryTask(ctx context.Context, id, code, summary string, next time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='pending',lease_until=NULL,error_code=?,error_summary=?,next_attempt_at=?,updated_at=? WHERE id=? AND status='running'`, code, summary, timeText(next), timeText(time.Now()), id)
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

func (s *Store) FailTask(ctx context.Context, id, code, summary string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='failed',lease_until=NULL,error_code=?,error_summary=?,updated_at=? WHERE id=? AND status='running'`, code, summary, timeText(time.Now()), id)
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

func (s *Store) RecordHookEvent(ctx context.Context, value model.HookEvent) (int64, error) {
	if err := value.Host.Validate(); err != nil {
		return 0, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO hook_events(occurred_at,host,event_type,project_id,installer,package_identity,requested_version,correlation_hash,processed_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(correlation_hash) DO NOTHING`, timeText(value.OccurredAt), value.Host, value.EventType, nullIfEmpty(value.ProjectID), value.Installer, value.PackageIdentity, value.RequestedVersion, nullIfEmpty(value.CorrelationHash), nullableTimeText(value.ProcessedAt))
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	return id, err
}

func (s *Store) QueueNotification(ctx context.Context, value model.Notification) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO notifications(candidate_hash,kind,message,queued_at,shown_at) VALUES(?,?,?,?,?) ON CONFLICT(candidate_hash,kind) DO NOTHING`, value.CandidateHash, value.Kind, value.Message, timeText(value.QueuedAt), nullableTimeText(value.ShownAt))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (s *Store) HasNotificationKindSince(ctx context.Context, kindPrefix string, since time.Time) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notifications WHERE kind LIKE ? AND queued_at>=?)`, kindPrefix+"%", timeText(since)).Scan(&exists)
	return exists == 1, err
}

func (s *Store) TakeNotifications(ctx context.Context, limit int) ([]model.Notification, error) {
	if limit <= 0 {
		limit = 20
	}
	result := []model.Notification{}
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT candidate_hash,kind,message,queued_at FROM notifications WHERE shown_at IS NULL ORDER BY queued_at LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var value model.Notification
			var queued string
			if err := rows.Scan(&value.CandidateHash, &value.Kind, &value.Message, &queued); err != nil {
				return err
			}
			value.QueuedAt, err = parseTime(queued)
			if err != nil {
				return err
			}
			result = append(result, value)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		now := timeText(time.Now())
		for i := range result {
			if _, err := tx.ExecContext(ctx, `UPDATE notifications SET shown_at=? WHERE candidate_hash=? AND kind=? AND shown_at IS NULL`, now, result[i].CandidateHash, result[i].Kind); err != nil {
				return err
			}
			parsed, err := parseTime(now)
			if err != nil {
				return err
			}
			result[i].ShownAt = &parsed
		}
		return nil
	})
	return result, err
}

func (s *Store) BeginScan(ctx context.Context, value model.Scan) error {
	if value.Status == "" {
		value.Status = "running"
	}
	if value.Status != "running" {
		return errors.New("store: new scan must be running")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO scans(id,reason,status,started_at,finished_at) VALUES(?,?,?,?,NULL)`, value.ID, value.Reason, value.Status, timeText(value.StartedAt))
	return err
}

func (s *Store) FinishScan(ctx context.Context, id, status string) error {
	if status != "succeeded" && status != "failed" {
		return fmt.Errorf("store: invalid scan status %q", status)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE scans SET status=?,finished_at=? WHERE id=? AND status='running'`, status, timeText(time.Now()), id)
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
