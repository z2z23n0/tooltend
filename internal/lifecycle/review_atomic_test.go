package lifecycle

import (
	"context"
	"testing"

	"github.com/z2z23n0/tooltend/internal/adapter"
	"github.com/z2z23n0/tooltend/internal/model"
)

func TestReviewBundleFailureLeavesCandidateRetryable(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	fixture.seedKnownSource(t, true, model.ApplyAuto)
	fixture.provider.latest = "v2"
	if _, err := fixture.database.DB().Exec(`CREATE TRIGGER fail_review_bundle BEFORE INSERT ON review_bundles BEGIN SELECT RAISE(ABORT, 'bundle failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{Stage: true}); err == nil {
		t.Fatal("expected review bundle transaction failure")
	}
	candidates, err := fixture.database.ListCandidates(context.Background(), "binding", "")
	if err != nil {
		t.Fatal(err)
	}
	var staged model.UpdateCandidate
	for _, candidate := range candidates {
		if candidate.UpstreamTreeHash != "" {
			staged = candidate
		}
	}
	if staged.ID == "" || staged.Status != model.CandidateMerging {
		t.Fatalf("candidate after failed bundle=%+v all=%+v", staged, candidates)
	}
	var bundles int
	if err := fixture.database.DB().QueryRow(`SELECT count(*) FROM review_bundles WHERE candidate_id=?`, staged.ID).Scan(&bundles); err != nil || bundles != 0 {
		t.Fatalf("bundles=%d err=%v", bundles, err)
	}
	if _, err := fixture.database.DB().Exec(`DROP TRIGGER fail_review_bundle`); err != nil {
		t.Fatal(err)
	}
	resumed, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{Stage: true})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Candidate.ID != staged.ID || resumed.Candidate.Status != model.CandidateNeedsReview || !resumed.NeedsReview {
		t.Fatalf("resumed=%+v", resumed)
	}
	if _, err := fixture.database.GetReviewBundle(context.Background(), staged.ID); err != nil {
		t.Fatal(err)
	}
}
