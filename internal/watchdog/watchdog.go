package watchdog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

type Notifier interface {
	Send(context.Context, string, string) error
}

type Service struct {
	Database *store.Store
	Notifier Notifier
	Now      func() time.Time
	Enabled  bool
}

type Result struct {
	Healthy         bool                `json:"healthy"`
	Alerted         bool                `json:"alerted"`
	DesktopNotified bool                `json:"desktop_notified"`
	Reason          string              `json:"reason,omitempty"`
	LatestRun       *model.ReconcileRun `json:"latest_run,omitempty"`
}

func (s Service) Check(ctx context.Context, maxAge time.Duration) (Result, error) {
	if s.Database == nil || s.Database.DB() == nil {
		return Result{}, fmt.Errorf("watchdog: database is required")
	}
	if maxAge <= 0 {
		return Result{}, fmt.Errorf("watchdog: max age must be positive")
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	latest, err := s.Database.LatestReconcileRun(ctx)
	result := Result{}
	switch {
	case err == nil && latest.Status == "succeeded" && latest.FinishedAt != nil && !latest.FinishedAt.Before(now.Add(-maxAge)):
		result.Healthy, result.LatestRun = true, &latest
		return result, nil
	case err != nil && !isNoRows(err):
		return result, err
	case err != nil:
		result.Reason = "missing_run"
	case latest.Status == "failed":
		result.Reason, result.LatestRun = "failed_run", &latest
	case latest.Status == "running":
		result.Reason, result.LatestRun = "unfinished_run", &latest
	case latest.Status == "incomplete":
		result.Reason, result.LatestRun = "unfinished_run", &latest
	default:
		result.Reason, result.LatestRun = "stale_run", &latest
	}
	if !s.Enabled {
		return result, nil
	}
	if result.Reason == "failed_run" {
		alreadyNotified, err := s.Database.HasNotificationKindSince(ctx, "reconcile_failed:", startOfDay(now))
		if err != nil {
			return result, err
		}
		if alreadyNotified {
			return result, nil
		}
	}
	message := watchdogMessage(result.Reason)
	hash := sha256.Sum256([]byte("watchdog\x00" + now.Format("2006-01-02") + "\x00" + result.Reason))
	queued, err := s.Database.QueueNotification(ctx, model.Notification{
		CandidateHash: hex.EncodeToString(hash[:]),
		Kind:          "watchdog:" + result.Reason,
		Message:       message,
		QueuedAt:      now.UTC(),
	})
	if err != nil {
		return result, err
	}
	result.Alerted = queued
	if queued && s.Notifier != nil {
		result.DesktopNotified = s.Notifier.Send(ctx, "ToolTend", message) == nil
	}
	return result, nil
}

func watchdogMessage(reason string) string {
	switch reason {
	case "failed_run":
		return "Scheduled update failed. Run `tooltend doctor` for details."
	case "unfinished_run":
		return "Scheduled update did not finish. Run `tooltend doctor` for details."
	case "stale_run":
		return "Scheduled update has not completed recently. Run `tooltend doctor` for details."
	default:
		return "Scheduled update did not run. Run `tooltend doctor` for details."
	}
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

func startOfDay(value time.Time) time.Time {
	year, month, day := value.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, value.Location())
}
