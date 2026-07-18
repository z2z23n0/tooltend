package watchdog

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

type recordingNotifier struct {
	calls int
}

func (n *recordingNotifier) Send(context.Context, string, string) error {
	n.calls++
	return nil
}

func TestMissingRunAlertsOnlyOncePerDay(t *testing.T) {
	database, err := store.OpenRW(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Date(2026, 7, 18, 20, 30, 0, 0, time.Local)
	notifier := &recordingNotifier{}
	service := Service{Database: database, Notifier: notifier, Now: func() time.Time { return now }, Enabled: true}
	first, err := service.Check(context.Background(), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Check(context.Background(), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if first.Healthy || !first.Alerted || !first.DesktopNotified || second.Alerted || notifier.calls != 1 {
		t.Fatalf("first=%#v second=%#v calls=%d", first, second, notifier.calls)
	}
}

func TestRecentSuccessfulRunIsHealthy(t *testing.T) {
	database, err := store.OpenRW(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Date(2026, 7, 18, 20, 30, 0, 0, time.UTC)
	if err := database.BeginReconcileRun(context.Background(), model.ReconcileRun{ID: "run", Reason: "scheduled", Status: "running", StartedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := database.FinishReconcileRun(context.Background(), "run", "succeeded", "", `{}`, now.Add(-59*time.Minute)); err != nil {
		t.Fatal(err)
	}
	result, err := (Service{Database: database, Enabled: true, Now: func() time.Time { return now }}).Check(context.Background(), 2*time.Hour)
	if err != nil || !result.Healthy || result.Alerted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestFailedRunDoesNotDuplicateReconcileFailureAlert(t *testing.T) {
	database, err := store.OpenRW(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 20, 30, 0, 0, time.Local)
	if err := database.BeginReconcileRun(ctx, model.ReconcileRun{ID: "run", Reason: "scheduled", Status: "running", StartedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := database.FinishReconcileRun(ctx, "run", "failed", "task_failed", `{}`, now.Add(-59*time.Minute)); err != nil {
		t.Fatal(err)
	}
	queued, err := database.QueueNotification(ctx, model.Notification{
		CandidateHash: strings.Repeat("f", 64), Kind: "reconcile_failed:task_failed", Message: "update failed", QueuedAt: now.Add(-58 * time.Minute),
	})
	if err != nil || !queued {
		t.Fatalf("queue=%v err=%v", queued, err)
	}
	notifier := &recordingNotifier{}
	result, err := (Service{Database: database, Notifier: notifier, Enabled: true, Now: func() time.Time { return now }}).Check(ctx, 2*time.Hour)
	if err != nil || result.Healthy || result.Alerted || result.DesktopNotified || notifier.calls != 0 {
		t.Fatalf("result=%#v calls=%d err=%v", result, notifier.calls, err)
	}
}
