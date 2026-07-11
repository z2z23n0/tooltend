package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/model"
)

func TestHookAcknowledgementDoesNotConsumeRacingEvent(t *testing.T) {
	ctx := context.Background()
	database, _ := openWorkerStore(t)
	now := time.Now().UTC()
	first, err := database.RecordHookEvent(ctx, model.HookEvent{OccurredAt: now, Host: model.HostCodex, EventType: "PreToolUse", CorrelationHash: digestForTest("first")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordHookEvent(ctx, model.HookEvent{OccurredAt: now, Host: model.HostCodex, EventType: "PostToolUse", CorrelationHash: digestForTest("second")}); err != nil {
		t.Fatal(err)
	}
	if err := markHookSignalsProcessed(ctx, database, first, now); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := database.DB().QueryRow(`SELECT COUNT(*) FROM hook_events WHERE processed_at IS NULL`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending events = %d, want 1", pending)
	}
}
