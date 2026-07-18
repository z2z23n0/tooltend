package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/z2z23n0/tooltend/internal/model"
)

func (s *Store) BeginReconcileRun(ctx context.Context, value model.ReconcileRun) error {
	if value.ID == "" || value.Status != "running" {
		return errors.New("store: reconcile run must start in running state")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO reconcile_runs(id,reason,status,started_at,finished_at,error_code,summary_json) VALUES(?,?,?,?,NULL,'','{}')`, value.ID, value.Reason, value.Status, timeText(value.StartedAt))
	return err
}

func (s *Store) FinishReconcileRun(ctx context.Context, id, status, errorCode, summaryJSON string, finishedAt time.Time) error {
	if status != "succeeded" && status != "incomplete" && status != "failed" {
		return fmt.Errorf("store: invalid reconcile run status %q", status)
	}
	if summaryJSON == "" {
		summaryJSON = "{}"
	}
	result, err := s.db.ExecContext(ctx, `UPDATE reconcile_runs SET status=?,finished_at=?,error_code=?,summary_json=? WHERE id=? AND status='running'`, status, timeText(finishedAt), errorCode, summaryJSON, id)
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

func (s *Store) LatestReconcileRun(ctx context.Context) (model.ReconcileRun, error) {
	var value model.ReconcileRun
	var started string
	var finished sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,reason,status,started_at,finished_at,error_code,summary_json FROM reconcile_runs ORDER BY COALESCE(finished_at,started_at) DESC,started_at DESC LIMIT 1`).Scan(
		&value.ID, &value.Reason, &value.Status, &started, &finished, &value.ErrorCode, &value.SummaryJSON,
	)
	if err != nil {
		return model.ReconcileRun{}, err
	}
	value.StartedAt, err = parseTime(started)
	if err != nil {
		return model.ReconcileRun{}, err
	}
	if finished.Valid {
		parsed, parseErr := parseTime(finished.String)
		if parseErr != nil {
			return model.ReconcileRun{}, parseErr
		}
		value.FinishedAt = &parsed
	}
	return value, nil
}
