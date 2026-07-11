package lifecycle

import (
	"context"
	"testing"

	"github.com/z2z23n0/tooltend/internal/adapter"
	"github.com/z2z23n0/tooltend/internal/model"
)

func TestRemoteHTTPMCPUpdateIsObservationOnly(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentHTTPMCP, adapter.SourceHTTP)
	fixture.seedKnownSource(t, false, model.ApplyManual)
	registry, err := adapter.NewRegistry(adapter.RemoteMCP{})
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.Adapters = registry
	result, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{Reason: "scheduled"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Checked || result.Candidate.ID != "" || len(result.Warnings) != 1 {
		t.Fatalf("result=%+v", result)
	}
	var notifications int
	if err := fixture.database.DB().QueryRow(`SELECT count(*) FROM notifications`).Scan(&notifications); err != nil || notifications != 0 {
		t.Fatalf("notifications=%d err=%v", notifications, err)
	}
}
