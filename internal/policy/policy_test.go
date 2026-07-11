package policy

import (
	"slices"
	"testing"
)

func TestDecideGitSkillAutoRequiresAllEvidence(t *testing.T) {
	input := commonAuto(AdapterGitSkill)
	if got := Decide(input); got.Mode != ModeAuto || len(got.ReasonCodes) != 0 {
		t.Fatalf("got %+v", got)
	}
	input.BaselineKnown = false
	got := Decide(input)
	if got.Mode != ModeManual || !slices.Contains(got.ReasonCodes, ReasonBaselineUnknown) {
		t.Fatalf("got %+v", got)
	}
}

func TestDecideRuntimeMustBeAdopted(t *testing.T) {
	input := commonAuto(AdapterNPM)
	input.Adopted = true
	input.RuntimeIsolated = true
	input.StableShim = true
	input.ExactVersion = true
	if got := Decide(input); got.Mode != ModeAuto {
		t.Fatalf("adopted runtime: %+v", got)
	}
	input.Adopted = false
	got := Decide(input)
	if got.Mode != ModeManual || !slices.Contains(got.ReasonCodes, ReasonNotAdopted) {
		t.Fatalf("native runtime: %+v", got)
	}
}

func TestDecideEphemeralRuntimeMustBecomePersistent(t *testing.T) {
	input := commonAuto(AdapterNPX)
	input.Adopted = true
	input.RuntimeIsolated = true
	input.StableShim = true
	input.ExactVersion = true
	got := Decide(input)
	if got.Mode != ModeManual || !slices.Contains(got.ReasonCodes, ReasonRuntimeNotPersistent) {
		t.Fatalf("got %+v", got)
	}
	input.PersistentRuntime = true
	if got := Decide(input); got.Mode != ModeAuto {
		t.Fatalf("got %+v", got)
	}
}

func TestDecideHookChangesAreManual(t *testing.T) {
	input := commonAuto(AdapterGitHook)
	input.HookContentUnchanged = true
	input.TrustUnchanged = true
	input.PermissionsUnchanged = true
	if got := Decide(input); got.Mode != ModeAuto {
		t.Fatalf("got %+v", got)
	}
	input.HookContentUnchanged = false
	input.TrustUnchanged = false
	input.PermissionsUnchanged = false
	got := Decide(input)
	for _, reason := range []ReasonCode{ReasonHookContentChanged, ReasonTrustChanged, ReasonPermissionsChanged} {
		if !slices.Contains(got.ReasonCodes, reason) {
			t.Fatalf("missing %s in %+v", reason, got)
		}
	}
}

func TestDecideManualOnlyAdapters(t *testing.T) {
	for _, adapter := range []Adapter{AdapterHTTPMCP, AdapterUnknown, "future-adapter"} {
		input := commonAuto(adapter)
		got := Decide(input)
		if got.Mode != ModeManual {
			t.Fatalf("%s: %+v", adapter, got)
		}
	}
}

func TestDecideHomebrewRequiresVerifiedDowngrade(t *testing.T) {
	input := commonAuto(AdapterHomebrew)
	got := Decide(input)
	if got.Mode != ModeManual || !slices.Contains(got.ReasonCodes, ReasonOldArtifactMissing) || !slices.Contains(got.ReasonCodes, ReasonDowngradeUnverified) {
		t.Fatalf("got %+v", got)
	}
	input.OldArtifactAvailable = true
	input.DowngradeVerified = true
	if got := Decide(input); got.Mode != ModeAuto {
		t.Fatalf("got %+v", got)
	}
}

func TestProjectPolicyCanOnlyRestrict(t *testing.T) {
	manual := ModeManual
	auto := ModeAuto
	ignore := ModeIgnore

	if mode, reason := Restrict(ModeAuto, &manual); mode != ModeManual || reason != ReasonProjectRestricted {
		t.Fatalf("auto -> manual: %s %s", mode, reason)
	}
	if mode, reason := Restrict(ModeManual, &auto); mode != ModeManual || reason != "" {
		t.Fatalf("manual cannot be raised: %s %s", mode, reason)
	}
	if mode, reason := Restrict(ModeAuto, &ignore); mode != ModeIgnore || reason != ReasonProjectRestricted {
		t.Fatalf("auto -> ignore: %s %s", mode, reason)
	}

	input := commonAuto(AdapterGitSkill)
	input.ProjectMode = &manual
	got := Decide(input)
	if got.Mode != ModeManual || !slices.Contains(got.ReasonCodes, ReasonProjectRestricted) {
		t.Fatalf("got %+v", got)
	}
}

func TestZeroValueFailsClosed(t *testing.T) {
	got := Decide(Input{})
	if got.Mode != ModeManual || !slices.Contains(got.ReasonCodes, ReasonInvalidMode) {
		t.Fatalf("got %+v", got)
	}
}

func TestIgnoreRemainsIgnore(t *testing.T) {
	auto := ModeAuto
	got := Decide(Input{LocalMode: ModeIgnore, ProjectMode: &auto})
	if got.Mode != ModeIgnore {
		t.Fatalf("got %+v", got)
	}
}

func commonAuto(adapter Adapter) Input {
	return Input{
		Adapter:              adapter,
		LocalMode:            ModeAuto,
		SourceKnown:          true,
		BaselineKnown:        true,
		ManagedBinding:       true,
		ValidationPassed:     true,
		RollbackVerified:     true,
		TrustUnchanged:       true,
		PermissionsUnchanged: true,
	}
}
