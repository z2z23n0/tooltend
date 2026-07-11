package model

import (
	"strings"
	"testing"
)

func TestCandidateTransitions(t *testing.T) {
	if !CanTransitionCandidate(CandidateAvailable, CandidateStaging) {
		t.Fatal("expected available -> staging")
	}
	if CanTransitionCandidate(CandidateAvailable, CandidateActive) {
		t.Fatal("unexpected available -> active")
	}
	if err := ValidateCandidateTransition(CandidateReady, CandidateActivating); err != nil {
		t.Fatal(err)
	}
}

func TestNewID(t *testing.T) {
	id, err := NewID("cand")
	if err != nil {
		t.Fatal(err)
	}
	if len(id) < len("cand_")+20 || id[:5] != "cand_" {
		t.Fatalf("unexpected id %q", id)
	}
}

func TestActivationPointerSwitchedIsValid(t *testing.T) {
	if err := ActivationPointerSwitched.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewValidationRejectsCredentialLikeSummary(t *testing.T) {
	base := Review{ID: "review", CandidateID: "candidate", CandidateHash: strings.Repeat("a", 64), ActorType: ActorAgent, Verdict: VerdictUncertain, RiskType: "semantic_skill_change", Summary: "The wording needs another check."}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	base.Summary = "api_key = super-secret-value"
	if err := base.Validate(); err == nil {
		t.Fatal("credential-like review summary was accepted")
	}
}
