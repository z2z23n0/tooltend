package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/activation"
	"github.com/z2z23n0/tooltend/internal/adapter"
	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/host"
	"github.com/z2z23n0/tooltend/internal/inventory"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/objectstore"
	"github.com/z2z23n0/tooltend/internal/store"
)

func TestManualCheckResolvesWithoutFetch(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	fixture.seedKnownSource(t, false, model.ApplyManual)
	fixture.provider.latest = "v2"

	result, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Checked || result.Staged || result.Activated {
		t.Fatalf("result = %+v", result)
	}
	if fixture.provider.resolveCalls != 1 || fixture.provider.fetchCalls != 0 {
		t.Fatalf("resolve=%d fetch=%d", fixture.provider.resolveCalls, fixture.provider.fetchCalls)
	}
	if result.Candidate.Status != model.CandidateAvailable || result.Candidate.UpstreamTreeHash != "" {
		t.Fatalf("candidate = %+v", result.Candidate)
	}
	var objectCount int
	if err := fixture.database.DB().QueryRow(`SELECT count(*) FROM objects`).Scan(&objectCount); err != nil {
		t.Fatal(err)
	}
	if objectCount != 0 {
		t.Fatalf("manual check downloaded object count=%d", objectCount)
	}
}

func TestUpdateConfirmationRejectsChangedRefBeforeFetchOrCandidateWrite(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	fixture.seedKnownSource(t, false, model.ApplyManual)
	fixture.provider.latest = "v2"
	target, err := fixture.service.ResolveUpdate(context.Background(), "component", "")
	if err != nil {
		t.Fatal(err)
	}
	fixture.provider.latest = "v3"
	if _, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{
		Stage: true, ExpectedRef: target.ResolvedRef,
	}); err == nil || !strings.Contains(err.Error(), "changed after confirmation") {
		t.Fatalf("moving ref was not rejected: %v", err)
	}
	if fixture.provider.fetchCalls != 0 {
		t.Fatalf("changed ref fetched %d artifacts", fixture.provider.fetchCalls)
	}
	var candidates int
	if err := fixture.database.DB().QueryRow(`SELECT count(*) FROM candidates`).Scan(&candidates); err != nil || candidates != 0 {
		t.Fatalf("changed ref wrote candidates=%d err=%v", candidates, err)
	}
}

func TestUpdateConfirmationRejectsChangedActiveGeneration(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyManual)
	adopted, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-plugin", Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.provider.latest = "v2"
	target, err := fixture.service.ResolveUpdate(context.Background(), "component", "")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil {
		t.Fatal(err)
	}
	binding.ActiveGenerationID = ""
	if err := fixture.database.UpsertBinding(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	resolvesBefore := fixture.provider.resolveCalls
	_, err = fixture.service.Update(context.Background(), "component", "", UpdateOptions{
		Stage: true, ExpectedRef: target.ResolvedRef, ExpectedGeneration: adopted.Generation, BindGeneration: true,
	})
	if err == nil || !strings.Contains(err.Error(), "active generation changed") {
		t.Fatalf("changed active generation was not rejected: %v", err)
	}
	if fixture.provider.resolveCalls != resolvesBefore || fixture.provider.fetchCalls != 1 { // adoption fetched v1 once
		t.Fatalf("generation race reached adapter: resolve=%d/%d fetch=%d", fixture.provider.resolveCalls, resolvesBefore, fixture.provider.fetchCalls)
	}
}

func TestHostOwnedBindingCannotBeAdopted(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	fixture.seedBindingOnly(t, false, model.ApplyIgnore)
	binding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil {
		t.Fatal(err)
	}
	binding.InstallMethod = model.HostOwnedInstallMethod(model.HostCodex)
	if err := fixture.database.UpsertBinding(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	options := AdoptOptions{Source: "https://example.test/tooltend-plugin", Version: "v1"}
	if _, err := fixture.service.ResolveAdopt(context.Background(), "component", options); err == nil || !strings.Contains(err.Error(), "owned by codex") {
		t.Fatalf("host-owned preview was not rejected: %v", err)
	}
	if _, err := fixture.service.Adopt(context.Background(), "component", options); err == nil || !strings.Contains(err.Error(), "owned by codex") {
		t.Fatalf("host-owned adoption was not rejected: %v", err)
	}
	if fixture.provider.resolveCalls != 0 || fixture.provider.fetchCalls != 0 {
		t.Fatalf("host-owned adoption reached adapter: resolve=%d fetch=%d", fixture.provider.resolveCalls, fixture.provider.fetchCalls)
	}
}

func TestNoBaselineStagesForReviewAndNeverActivates(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	fixture.seedKnownSource(t, true, model.ApplyAuto)
	fixture.provider.latest = "v2"
	background, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{Stage: true, Activate: true, Reason: "scheduled"})
	if err != nil {
		t.Fatal(err)
	}
	if background.Staged || fixture.provider.fetchCalls != 0 {
		t.Fatalf("background fork check downloaded an artifact: result=%+v fetch=%d", background, fixture.provider.fetchCalls)
	}

	result, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{Stage: true, Activate: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Staged || !result.NeedsReview || result.Activated {
		t.Fatalf("result = %+v", result)
	}
	if result.Candidate.Status != model.CandidateNeedsReview || result.Candidate.BaselineID != "" {
		t.Fatalf("candidate = %+v", result.Candidate)
	}
	if fixture.provider.fetchCalls != 1 {
		t.Fatalf("fetch calls = %d", fixture.provider.fetchCalls)
	}
	var intents, bundles int
	if err := fixture.database.DB().QueryRow(`SELECT count(*) FROM activation_intents`).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.DB().QueryRow(`SELECT count(*) FROM review_bundles WHERE candidate_id=? AND candidate_hash=?`, result.Candidate.ID, result.Candidate.CandidateHash).Scan(&bundles); err != nil {
		t.Fatal(err)
	}
	if intents != 0 || bundles != 1 {
		t.Fatalf("intents=%d bundles=%d", intents, bundles)
	}
}

func TestAdoptUpdateAndRollbackRestoresOldGeneration(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	fixture.provider.latest = "v2"
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyAuto)

	adopted, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-plugin", Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !adopted.Baseline || adopted.Generation == "" || adopted.Receipt.Action != model.ReceiptAdopt {
		t.Fatalf("adopted = %+v", adopted)
	}
	if info, err := os.Lstat(fixture.installPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("install path was not replaced by a managed symlink: info=%v err=%v", info, err)
	}

	updated, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Activated || updated.Candidate.Status != model.CandidateActive {
		t.Fatalf("updated = %+v", updated)
	}
	managed, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil {
		t.Fatal(err)
	}
	newGeneration := managed.ActiveGenerationID
	if newGeneration == "" || newGeneration == adopted.Generation {
		t.Fatalf("active generation = %q", newGeneration)
	}
	root := fixture.service.activationRoot("binding", false)
	newPath, _ := activation.GenerationPath(root, newGeneration)
	if _, err := os.Stat(newPath); err != nil {
		t.Fatal(err)
	}

	rolledBack, err := fixture.service.Rollback(context.Background(), "component", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.From != newGeneration || rolledBack.To != adopted.Generation || rolledBack.Receipt.Action != model.ReceiptRollback {
		t.Fatalf("rollback = %+v", rolledBack)
	}
	current, err := activation.Current(root)
	if err != nil || current != adopted.Generation {
		t.Fatalf("current=%q err=%v", current, err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("new generation was deleted during rollback: %v", err)
	}
	managed, err = fixture.database.GetBinding(context.Background(), "binding")
	if err != nil || managed.ActiveGenerationID != adopted.Generation {
		t.Fatalf("binding after rollback=%+v err=%v", managed, err)
	}
	var committedIntent, rollbackReceipt, updateReceipt int
	if err := fixture.database.DB().QueryRow(`SELECT count(*) FROM activation_intents WHERE phase='committed'`).Scan(&committedIntent); err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.DB().QueryRow(`SELECT count(*) FROM receipts WHERE action='rollback' AND status='succeeded'`).Scan(&rollbackReceipt); err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.DB().QueryRow(`SELECT count(*) FROM receipts WHERE action='update' AND status='succeeded'`).Scan(&updateReceipt); err != nil {
		t.Fatal(err)
	}
	if committedIntent < 2 || rollbackReceipt != 1 || updateReceipt != 1 {
		t.Fatalf("committed intents=%d rollback receipts=%d update receipts=%d", committedIntent, rollbackReceipt, updateReceipt)
	}
}

func TestCustomizedGenerationRemainsVerifiableRollbackTarget(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyAuto)
	adopted, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-plugin", Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "local.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.provider.latest = "v2"
	updated, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Activated {
		t.Fatalf("update was not activated: %+v", updated)
	}
	rolledBack, err := fixture.service.Rollback(context.Background(), "component", "", adopted.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.To != adopted.Generation {
		t.Fatalf("rollback=%+v", rolledBack)
	}
	if content, err := os.ReadFile(filepath.Join(fixture.installPath, "local.txt")); err != nil || string(content) != "keep me\n" {
		t.Fatalf("local overlay was not preserved: %q err=%v", content, err)
	}
	if content, err := os.ReadFile(filepath.Join(fixture.installPath, "plugin.txt")); err != nil || string(content) != "v1\n" {
		t.Fatalf("rollback target content=%q err=%v", content, err)
	}
}

func TestUpdateAbortsIfActiveGenerationChangesAfterSnapshotCapture(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyAuto)
	adopted, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-plugin", Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.provider.latest = "v2"
	fixture.service.SnapshotFailpoint = func(point SnapshotFailpoint) error {
		if point == SnapshotAfterCandidateCapture {
			return os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("local edit during activation\n"), 0o644)
		}
		return nil
	}
	_, err = fixture.service.Update(context.Background(), "component", "", UpdateOptions{})
	if err == nil || !strings.Contains(err.Error(), "old generation hash changed before activation") {
		t.Fatalf("update error=%v", err)
	}
	binding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil || binding.ActiveGenerationID != adopted.Generation {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	if content, err := os.ReadFile(filepath.Join(fixture.installPath, "plugin.txt")); err != nil || string(content) != "local edit during activation\n" {
		t.Fatalf("local edit=%q err=%v", content, err)
	}
}

func TestRollbackAbortsIfActiveGenerationChangesAfterSnapshotCapture(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyAuto)
	adopted, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-plugin", Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.provider.latest = "v2"
	updated, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{})
	if err != nil || !updated.Activated {
		t.Fatalf("update=%+v err=%v", updated, err)
	}
	updatedBinding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.SnapshotFailpoint = func(point SnapshotFailpoint) error {
		if point == SnapshotAfterRollbackCapture {
			return os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("local edit during rollback\n"), 0o644)
		}
		return nil
	}
	_, err = fixture.service.Rollback(context.Background(), "component", "", adopted.Generation)
	if err == nil || !strings.Contains(err.Error(), "old generation hash changed before activation") {
		t.Fatalf("rollback error=%v", err)
	}
	binding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil || binding.ActiveGenerationID != updatedBinding.ActiveGenerationID {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	if binding.ActiveGenerationID == adopted.Generation {
		t.Fatalf("rollback unexpectedly activated %s", adopted.Generation)
	}
	if content, err := os.ReadFile(filepath.Join(fixture.installPath, "plugin.txt")); err != nil || string(content) != "local edit during rollback\n" {
		t.Fatalf("local edit=%q err=%v", content, err)
	}
}

func TestUpdateRejectsArtifactThatViolatesProjectLockIntegrity(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyManual)
	adopted, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-plugin", Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := fixture.database.GetPolicy(context.Background(), "binding")
	if err != nil {
		t.Fatal(err)
	}
	policy.TrackChannel, policy.Constraint = model.TrackExact, "v2"
	policy.ExpectedIntegrity = strings.Repeat("f", 64)
	if err := fixture.database.SetPolicy(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.Update(context.Background(), "component", "", UpdateOptions{Stage: true, Activate: true})
	if err == nil || !strings.Contains(err.Error(), "project lock integrity") {
		t.Fatalf("mismatched locked artifact was not rejected: %v", err)
	}
	binding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil || binding.ActiveGenerationID != adopted.Generation {
		t.Fatalf("lock mismatch changed active generation: %+v err=%v", binding, err)
	}
}

func TestAdoptPreviewRejectsChangedLocalTreeWithoutReplacingBinding(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(fixture.installPath, "plugin.txt")
	if err := os.WriteFile(file, []byte("reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyManual)
	target, err := fixture.service.ResolveAdopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-plugin", Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if target.ObservedHash == "" || target.ResolvedRef == "" {
		t.Fatalf("preview target=%+v", target)
	}
	if entries, readErr := os.ReadDir(fixture.paths.ObjectsDir); readErr == nil && len(entries) != 0 {
		t.Fatalf("read-only preview wrote object files: %#v", entries)
	}
	if err := os.WriteFile(file, []byte("changed after preview\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-plugin", Version: "v1",
		ExpectedResolvedRef: target.ResolvedRef, ExpectedObservedHash: target.ObservedHash,
	})
	if err == nil || !strings.Contains(err.Error(), "changed after confirmation") {
		t.Fatalf("changed local tree was not rejected: %v", err)
	}
	if info, statErr := os.Lstat(fixture.installPath); statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("rejected adoption replaced binding: info=%v err=%v", info, statErr)
	}
	binding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil || binding.Managed {
		t.Fatalf("rejected adoption changed DB binding=%+v err=%v", binding, err)
	}
}

func TestScannedLocalSkillCanBeAdoptedFromExplicitGitSource(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentSkill, adapter.SourceGit)
	home := t.TempDir()
	skillPath := filepath.Join(home, ".codex", "skills", "component")
	if err := os.MkdirAll(skillPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillPath, "SKILL.md"), []byte("# v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := inventory.Scan(context.Background(), home, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inventory.Persist(context.Background(), fixture.database, report); err != nil {
		t.Fatal(err)
	}
	components, err := fixture.database.ListComponents(context.Background())
	if err != nil || len(components) != 1 {
		t.Fatalf("components=%+v err=%v", components, err)
	}
	observedSource, err := fixture.database.GetSource(context.Background(), components[0].SourceID)
	if err != nil || observedSource.Kind != model.SourceLocal {
		t.Fatalf("observed source=%+v err=%v", observedSource, err)
	}

	adopted, err := fixture.service.Adopt(context.Background(), components[0].ID, AdoptOptions{
		Source: "https://example.test/tooltend-skill", Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !adopted.Baseline {
		t.Fatalf("adoption=%+v", adopted)
	}
	managedComponent, bindings, err := fixture.service.Component(context.Background(), components[0].ID)
	if err != nil || len(bindings) != 1 || !bindings[0].Managed || bindings[0].InstallPath != skillPath {
		t.Fatalf("component=%+v bindings=%+v err=%v", managedComponent, bindings, err)
	}
	verifiedSource, err := fixture.database.GetSource(context.Background(), managedComponent.SourceID)
	if err != nil || verifiedSource.Kind != model.SourceGit || verifiedSource.Locator != "https://example.test/tooltend-skill" {
		t.Fatalf("verified source=%+v err=%v", verifiedSource, err)
	}
}

func TestInventoryGitIdentityIsReusedByAdoption(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := inventory.Report{HostResult: host.Result{
		Observations: []host.Observation{{
			Key: "observed-plugin", Host: host.Codex, Kind: host.ComponentPlugin, Name: "component",
			Path: fixture.installPath, Scope: host.ScopeUser,
			Source: host.SourceRef{Kind: "git", Locator: "https://example.test/tooltend-plugin", Package: "component", Ref: "v1"},
		}},
		Bindings: []host.Binding{{
			Host: host.Codex, ComponentKey: "observed-plugin", Scope: host.ScopeUser, InstallPath: fixture.installPath,
		}},
	}}
	if _, err := inventory.Persist(context.Background(), fixture.database, report); err != nil {
		t.Fatal(err)
	}
	components, err := fixture.database.ListComponents(context.Background())
	if err != nil || len(components) != 1 {
		t.Fatalf("components=%+v err=%v", components, err)
	}
	originalSourceID := components[0].SourceID
	adopted, err := fixture.service.Adopt(context.Background(), components[0].ID, AdoptOptions{
		Source: "https://example.test/tooltend-plugin", Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !adopted.Baseline {
		t.Fatalf("adoption=%+v", adopted)
	}
	managed, _, err := fixture.service.Component(context.Background(), components[0].ID)
	if err != nil || managed.SourceID != originalSourceID {
		t.Fatalf("managed component=%+v err=%v", managed, err)
	}
}

func TestAdoptionJournalAllowsOrdinaryPathWords(t *testing.T) {
	root := filepath.Join(t.TempDir(), "content-command-runner")
	_, err := marshalAdoptionEffects(adoptionEffects{Install: &adoptionInstallEffect{
		Path: filepath.Join(root, "content"), BackupPath: filepath.Join(root, "command.backup"),
		TempPath: filepath.Join(root, "command.link"), Target: filepath.Join(root, "current"),
		BeforeTreeHash: strings.Repeat("a", 64),
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAdoptRejectsChangingSourceSharedBySiblingBindings(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyManual)
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	identity, err := stableHash(struct {
		Kind, Locator, Package, Subdir string
	}{string(adapter.SourceGit), "https://example.test/original", "component", ""})
	if err != nil {
		t.Fatal(err)
	}
	source := model.Source{
		ID: "src_" + identity[:24], Kind: model.SourceGit, Locator: "https://example.test/original",
		PackageName: "component", IdentityHash: identity, MetadataJSON: "{}", CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.database.UpsertSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	components, err := fixture.database.ListComponents(ctx)
	if err != nil || len(components) != 1 {
		t.Fatalf("components=%+v err=%v", components, err)
	}
	component := components[0]
	component.SourceID = source.ID
	if err := fixture.database.UpsertComponent(ctx, component); err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.UpsertBinding(ctx, model.Binding{
		ID: "sibling", ComponentID: component.ID, Host: model.HostClaude, Scope: model.ScopeGlobal,
		InstallPath: filepath.Join(fixture.paths.DataDir, "sibling"), InstallMethod: "tooltend-generation",
		Managed: true, Classification: model.ClassificationClean, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = fixture.service.Adopt(ctx, component.ID, AdoptOptions{
		BindingID: "binding", Source: "https://example.test/different", Version: "v1",
	})
	if err == nil || !strings.Contains(err.Error(), "different source") {
		t.Fatalf("source migration was not rejected: %v", err)
	}
	stored, err := fixture.database.ListComponents(ctx)
	if err != nil || len(stored) != 1 || stored[0].SourceID != source.ID {
		t.Fatalf("shared component source changed: %+v err=%v", stored, err)
	}
	sibling, err := fixture.database.GetBinding(ctx, "sibling")
	if err != nil || !sibling.Managed || sibling.ComponentID != component.ID {
		t.Fatalf("sibling binding changed: %+v err=%v", sibling, err)
	}
}

func TestHookAdoptPreviewRejectsMovingRefBeforeFetch(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentHook, adapter.SourceGit)
	fixture.seedBindingOnly(t, false, model.ApplyManual)
	fixture.provider.latest = "v1"
	target, err := fixture.service.ResolveAdopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-hook", Executable: "hook.sh",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.provider.latest = "v2"
	_, err = fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-hook", Executable: "hook.sh",
		ExpectedResolvedRef: target.ResolvedRef, ExpectedConfigHash: target.ConfigHash,
	})
	if err == nil || !strings.Contains(err.Error(), "resolved ref changed") {
		t.Fatalf("moving hook ref was not rejected: %v", err)
	}
	if fixture.provider.fetchCalls != 0 {
		t.Fatalf("moving hook ref fetched %d artifacts", fixture.provider.fetchCalls)
	}
}

func TestRuntimeAdoptionCreatesStableShimAndKeepsNativeInstall(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentCLI, adapter.SourceNPM)
	t.Setenv("PATH", fixture.paths.ShimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	fixture.provider.latest = "1.0.0"
	if err := os.MkdirAll(filepath.Dir(fixture.installPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.installPath, []byte("native"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyAuto)

	result, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "npm:component", Version: "1.0.0", Executable: "component",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Baseline || result.Shim == "" {
		t.Fatalf("result = %+v", result)
	}
	if native, err := os.ReadFile(fixture.installPath); err != nil || string(native) != "native" {
		t.Fatalf("native install changed: %q err=%v", native, err)
	}
	shim, err := os.ReadFile(result.Shim)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(shim), filepath.Join(fixture.paths.RuntimesDir, "binding", "current")) {
		t.Fatalf("shim does not target stable current pointer: %s", shim)
	}
	info, err := os.Stat(result.Shim)
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("shim is not executable: info=%v err=%v", info, err)
	}
	firstGeneration := result.Generation
	fixture.provider.latest = "2.0.0"
	updated, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Activated {
		t.Fatalf("isolated runtime was not auto-activated: %+v", updated)
	}
	binding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil {
		t.Fatal(err)
	}
	if binding.ActiveGenerationID == firstGeneration {
		t.Fatalf("runtime generation did not change: %s", firstGeneration)
	}
	shimAfter, err := os.ReadFile(result.Shim)
	if err != nil || string(shimAfter) != string(shim) {
		t.Fatalf("stable shim changed across update: before=%q after=%q err=%v", shim, shimAfter, err)
	}
}

func TestRuntimeAdoptionCanAtomicallyTakeOverNativePath(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentCLI, adapter.SourceNPM)
	t.Setenv("PATH", fixture.paths.ShimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	fixture.installPath = filepath.Join(fixture.paths.ShimDir, "component")
	if err := os.MkdirAll(filepath.Dir(fixture.installPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.installPath, []byte("native"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyManual)
	result, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{Source: "npm:component", Version: "1.0.0", Executable: "component"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Backup == "" {
		t.Fatal("native executable was not retained as a rollback source")
	}
	if native, err := os.ReadFile(result.Backup); err != nil || string(native) != "native" {
		t.Fatalf("native backup=%q err=%v", native, err)
	}
	if shim, err := os.ReadFile(fixture.installPath); err != nil || !strings.Contains(string(shim), "current") {
		t.Fatalf("managed shim=%q err=%v", shim, err)
	}
}

func TestAdoptionCrashRecoveryConvergesFileHookAndRuntime(t *testing.T) {
	cases := []struct {
		name      string
		kind      model.ComponentKind
		point     AdoptionFailpoint
		committed bool
	}{
		{name: "file endpoint rolls forward", kind: model.ComponentPlugin, point: AdoptionFailAfterEndpoint, committed: true},
		{name: "hook before config rolls back", kind: model.ComponentHook, point: AdoptionFailAfterCurrent, committed: false},
		{name: "stdio config endpoint rolls forward", kind: model.ComponentStdioMCP, point: AdoptionFailAfterEndpoint, committed: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			paths := crashAdoptionPaths(root)
			if err := paths.Ensure(); err != nil {
				t.Fatal(err)
			}
			setupCrashAdoption(t, paths, test.kind)
			command := exec.Command(os.Args[0], "-test.run=^TestAdoptionCrashHelper$")
			command.Env = append(os.Environ(),
				"TOOLTEND_ADOPTION_CRASH_ROOT="+root,
				"TOOLTEND_ADOPTION_CRASH_KIND="+string(test.kind),
				"TOOLTEND_ADOPTION_CRASH_POINT="+string(test.point),
			)
			output, err := command.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 91 {
				t.Fatalf("crash helper err=%v output=%s", err, output)
			}
			database, err := store.OpenRW(paths.DatabaseFile)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			recovered, err := RecoverAdoptions(context.Background(), database, paths)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := database.GetBinding(context.Background(), "binding")
			if err != nil {
				t.Fatal(err)
			}
			var phase string
			if err := database.DB().QueryRow(`SELECT phase FROM adoption_intents WHERE binding_id='binding'`).Scan(&phase); err != nil {
				t.Fatal(err)
			}
			rootPath := filepath.Join(paths.GenerationsDir, "binding")
			if test.kind == model.ComponentStdioMCP {
				rootPath = filepath.Join(paths.RuntimesDir, "binding")
			}
			if test.committed {
				if recovered.Committed != 1 || !binding.Managed || phase != string(store.AdoptionCommitted) {
					t.Fatalf("recovered=%+v binding=%+v phase=%s", recovered, binding, phase)
				}
				if current, err := activation.Current(rootPath); err != nil || current != binding.ActiveGenerationID {
					t.Fatalf("current=%q binding=%+v err=%v", current, binding, err)
				}
				var receipts int
				if err := database.DB().QueryRow(`SELECT count(*) FROM receipts WHERE binding_id='binding' AND action='adopt' AND status='succeeded'`).Scan(&receipts); err != nil || receipts != 1 {
					t.Fatalf("receipts=%d err=%v", receipts, err)
				}
			} else {
				if recovered.RolledBack != 1 || binding.Managed || phase != string(store.AdoptionRolledBack) {
					t.Fatalf("recovered=%+v binding=%+v phase=%s", recovered, binding, phase)
				}
				if _, err := activation.Current(rootPath); !errors.Is(err, activation.ErrNoCurrent) {
					t.Fatalf("current was not cleared: %v", err)
				}
			}
			if test.kind == model.ComponentStdioMCP {
				content, err := os.ReadFile(binding.ConfigPath)
				if err != nil || !strings.Contains(string(content), "secret-value") {
					t.Fatalf("stdio secret was not preserved: %s err=%v", content, err)
				}
			}
		})
	}
}

func TestAdoptionRecoveryConflictPreservesManagedArtifacts(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentHook, adapter.SourceGit)
	fixture.seedBindingOnly(t, false, model.ApplyManual)
	binding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.AdoptionFailpoint = func(point AdoptionFailpoint) error {
		if point != AdoptionFailAfterEndpoint {
			return nil
		}
		file, err := os.OpenFile(binding.ConfigPath, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := file.WriteString("\n")
		closeErr := file.Close()
		return errors.Join(errors.New("simulated late failure"), writeErr, closeErr)
	}
	_, err = fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-hook", Version: "v1", Executable: "hook.sh",
	})
	if err == nil || !strings.Contains(err.Error(), "external state") {
		t.Fatalf("adoption error=%v", err)
	}
	stored, err := fixture.database.GetBinding(context.Background(), binding.ID)
	if err != nil || stored.Managed {
		t.Fatalf("uncommitted binding=%+v err=%v", stored, err)
	}
	var intentID, phase string
	if err := fixture.database.DB().QueryRow(`SELECT id,phase FROM adoption_intents WHERE binding_id=?`, binding.ID).Scan(&intentID, &phase); err != nil {
		t.Fatal(err)
	}
	intent, err := fixture.database.GetAdoptionIntent(context.Background(), intentID)
	if err != nil {
		t.Fatal(err)
	}
	generationID := intent.Plan.Commit.Generation.ID
	if phase != string(store.AdoptionBlocked) {
		t.Fatalf("phase=%s", phase)
	}
	root := fixture.service.activationRoot(binding.ID, false)
	if current, err := activation.Current(root); err != nil || current != generationID {
		t.Fatalf("blocked recovery removed current: current=%q generation=%q err=%v", current, generationID, err)
	}
	generationPath, _ := activation.GenerationPath(root, generationID)
	if info, err := os.Stat(generationPath); err != nil || !info.IsDir() {
		t.Fatalf("blocked recovery removed generation: info=%v err=%v", info, err)
	}
}

func TestAdoptionFinalValidationRejectsSilentTampering(t *testing.T) {
	tests := []struct {
		name   string
		kind   model.ComponentKind
		source adapter.SourceKind
		setup  func(*testing.T, *lifecycleFixture)
		adopt  AdoptOptions
		tamper func(*testing.T, *lifecycleFixture)
	}{
		{
			name: "hook config", kind: model.ComponentHook, source: adapter.SourceGit,
			setup: func(t *testing.T, fixture *lifecycleFixture) {
				fixture.seedBindingOnly(t, false, model.ApplyManual)
			},
			adopt: AdoptOptions{Source: "https://example.test/tooltend-hook", Version: "v1", Executable: "hook.sh"},
			tamper: func(t *testing.T, fixture *lifecycleFixture) {
				binding, err := fixture.database.GetBinding(context.Background(), "binding")
				if err != nil {
					t.Fatal(err)
				}
				file, err := os.OpenFile(binding.ConfigPath, os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString("\n"); err != nil {
					_ = file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "runtime shim", kind: model.ComponentCLI, source: adapter.SourceNPM,
			setup: func(t *testing.T, fixture *lifecycleFixture) {
				t.Setenv("PATH", fixture.paths.ShimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
				if err := os.MkdirAll(filepath.Dir(fixture.installPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fixture.installPath, []byte("native\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				fixture.seedBindingOnly(t, false, model.ApplyManual)
			},
			adopt: AdoptOptions{Source: "npm:component", Version: "1.0.0", Executable: "component"},
			tamper: func(t *testing.T, fixture *lifecycleFixture) {
				if err := os.WriteFile(filepath.Join(fixture.paths.ShimDir, "component"), []byte("tampered\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "generation", kind: model.ComponentPlugin, source: adapter.SourceGit,
			setup: func(t *testing.T, fixture *lifecycleFixture) {
				if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				fixture.seedBindingOnly(t, false, model.ApplyManual)
			},
			adopt: AdoptOptions{Source: "https://example.test/tooltend-plugin", Version: "v1"},
			tamper: func(t *testing.T, fixture *lifecycleFixture) {
				intents, err := fixture.database.ListPendingAdoptions(context.Background())
				if err != nil || len(intents) != 1 {
					t.Fatalf("pending intents=%+v err=%v", intents, err)
				}
				path := filepath.Join(intents[0].Plan.GenerationPath, "plugin.txt")
				if err := os.WriteFile(path, []byte("tampered\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLifecycleFixture(t, test.kind, test.source)
			test.setup(t, fixture)
			fixture.service.AdoptionFailpoint = func(point AdoptionFailpoint) error {
				if point == AdoptionFailAfterEndpoint {
					test.tamper(t, fixture)
				}
				return nil
			}
			if _, err := fixture.service.Adopt(context.Background(), "component", test.adopt); err == nil {
				t.Fatal("silently tampered adoption committed")
			}
			binding, err := fixture.database.GetBinding(context.Background(), "binding")
			if err != nil || binding.Managed {
				t.Fatalf("binding=%+v err=%v", binding, err)
			}
			var phase string
			if err := fixture.database.DB().QueryRow(`SELECT phase FROM adoption_intents WHERE binding_id='binding'`).Scan(&phase); err != nil {
				t.Fatal(err)
			}
			if phase != string(store.AdoptionBlocked) {
				t.Fatalf("phase=%s", phase)
			}
			var receipts int
			if err := fixture.database.DB().QueryRow(`SELECT count(*) FROM receipts WHERE binding_id='binding' AND action='adopt'`).Scan(&receipts); err != nil || receipts != 0 {
				t.Fatalf("receipts=%d err=%v", receipts, err)
			}
		})
	}
}

func TestStdioUnchangedConfigAfterJournalRollsBack(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentStdioMCP, adapter.SourceNPM)
	configPath := filepath.Join(filepath.Dir(fixture.installPath), "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	shimPath, err := fixture.service.runtimeShimPath(
		model.LogicalComponent{Kind: model.ComponentStdioMCP}, model.Binding{ID: "binding"}, "component",
	)
	if err != nil {
		t.Fatal(err)
	}
	configContent := "[mcp_servers.component]\ncommand = \"" + shimPath + "\"\nargs = [\"--safe\"]\nenv = { API_TOKEN = \"secret-value\" }\n"
	if err := os.WriteFile(configPath, []byte(configContent), 0o600); err != nil {
		t.Fatal(err)
	}
	canonical, err := host.PlanCommandMutation(context.Background(), host.CommandMutationOptions{
		Host: host.Codex, ConfigPath: configPath, Pointer: "mcp_servers/component", Command: shimPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, canonical.Content, 0o600); err != nil {
		t.Fatal(err)
	}
	configContent = string(canonical.Content)
	unchanged, err := host.PlanCommandMutation(context.Background(), host.CommandMutationOptions{
		Host: host.Codex, ConfigPath: configPath, Pointer: "mcp_servers/component", Command: shimPath,
	})
	if err != nil || unchanged.Changed {
		t.Fatalf("fixture config is not canonical and unchanged: changed=%t err=%v", unchanged.Changed, err)
	}
	fixture.installPath = configPath + "#mcp_servers/component"
	fixture.seedBindingOnly(t, false, model.ApplyManual)
	binding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil {
		t.Fatal(err)
	}
	binding.ConfigPath, binding.ConfigPointer = configPath, "mcp_servers/component"
	binding.ObservedVersion = "1.0.0"
	if err := fixture.database.UpsertBinding(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	fixture.service.AdoptionFailpoint = func(point AdoptionFailpoint) error {
		if point == AdoptionFailAfterJournal {
			return errors.New("stop after journal")
		}
		return nil
	}
	_, adoptErr := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "npm:component", Version: "1.0.0", Executable: "component",
	})
	if adoptErr == nil {
		t.Fatal("after-journal failpoint did not stop adoption")
	}
	recovered, err := RecoverAdoptions(context.Background(), fixture.database, fixture.paths)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.RolledBack != 1 || recovered.Blocked != 0 {
		t.Fatalf("recovery=%+v", recovered)
	}
	var phase string
	if err := fixture.database.DB().QueryRow(`SELECT phase FROM adoption_intents WHERE binding_id='binding'`).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	if phase != string(store.AdoptionRolledBack) {
		t.Fatalf("unchanged config was treated as a switched endpoint: phase=%s err=%v", phase, adoptErr)
	}
	if _, err := os.Lstat(shimPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shim survived rollback: %v", err)
	}
	root := filepath.Join(fixture.paths.RuntimesDir, "binding")
	if _, err := activation.Current(root); !errors.Is(err, activation.ErrNoCurrent) {
		t.Fatalf("current survived rollback: %v", err)
	}
	content, err := os.ReadFile(configPath)
	if err != nil || string(content) != configContent {
		t.Fatalf("config changed during rollback: %q err=%v", content, err)
	}
}

func TestAdoptionCrashHelper(t *testing.T) {
	root := os.Getenv("TOOLTEND_ADOPTION_CRASH_ROOT")
	if root == "" {
		return
	}
	kind := model.ComponentKind(os.Getenv("TOOLTEND_ADOPTION_CRASH_KIND"))
	point := AdoptionFailpoint(os.Getenv("TOOLTEND_ADOPTION_CRASH_POINT"))
	paths := crashAdoptionPaths(root)
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	objects, err := objectstore.New(paths.ObjectsDir)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(database, objects, paths)
	if err != nil {
		t.Fatal(err)
	}
	sourceKind := adapter.SourceGit
	if kind == model.ComponentStdioMCP {
		sourceKind = adapter.SourceNPM
	}
	provider := &fakeAdapter{kind: sourceKind, componentKind: kind, latest: "v1"}
	service.Adapters, err = adapter.NewRegistry(provider)
	if err != nil {
		t.Fatal(err)
	}
	service.AdoptionFailpoint = func(actual AdoptionFailpoint) error {
		if actual == point {
			os.Exit(91)
		}
		return nil
	}
	options := AdoptOptions{Source: "https://example.test/tooltend-plugin", Version: "v1"}
	if kind == model.ComponentHook {
		options.Source, options.Executable = "https://example.test/tooltend-hook", "hook.sh"
	}
	if kind == model.ComponentStdioMCP {
		options.Source, options.Version, options.Executable = "npm:component", "1.0.0", "component"
	}
	if _, err := service.Adopt(context.Background(), "component", options); err != nil {
		t.Fatal(err)
	}
	t.Fatal("adoption crash failpoint was not reached")
}

func crashAdoptionPaths(root string) config.Paths {
	state := filepath.Join(root, "state")
	data := filepath.Join(root, "data")
	return config.Paths{
		ConfigDir: filepath.Join(root, "config"), ConfigFile: filepath.Join(root, "config", "config.toml"),
		StateDir: state, DatabaseFile: filepath.Join(state, "state.db"), DataDir: data,
		ObjectsDir: filepath.Join(data, "objects"), StagingDir: filepath.Join(data, "staging"),
		GenerationsDir: filepath.Join(data, "generations"), RuntimesDir: filepath.Join(data, "runtimes"),
		ActivationLock: filepath.Join(state, "activation.lock"), ShimDir: filepath.Join(root, "bin"),
	}
}

func setupCrashAdoption(t *testing.T, paths config.Paths, kind model.ComponentKind) {
	t.Helper()
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	if err := database.UpsertComponent(context.Background(), model.LogicalComponent{
		ID: "component", Kind: kind, Name: "component", LogicalKey: "test:component", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	binding := model.Binding{
		ID: "binding", ComponentID: "component", Host: model.HostCodex, Scope: model.ScopeGlobal,
		InstallMethod: "native", Classification: model.ClassificationUnknown, LastSeenAt: now,
	}
	switch kind {
	case model.ComponentHook:
		nativeDir := filepath.Join(paths.DataDir, "native-hook")
		if err := os.MkdirAll(nativeDir, 0o700); err != nil {
			t.Fatal(err)
		}
		nativeHook := filepath.Join(nativeDir, "hook.sh")
		if err := os.WriteFile(nativeHook, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(paths.ConfigDir, "hooks.json")
		content, _ := json.Marshal(map[string]any{"hooks": []any{map[string]any{"command": nativeHook + " --safe", "timeout": 30}}})
		if err := os.WriteFile(configPath, append(content, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		binding.InstallPath = configPath + "#hooks/0"
		binding.ConfigPath, binding.ConfigPointer = configPath, "hooks/0"
	case model.ComponentStdioMCP:
		configPath := filepath.Join(paths.ConfigDir, "mcp.toml")
		content := "[mcp_servers.component]\ncommand = \"npx\"\nargs = [\"-y\", \"component@1.0.0\"]\nenv = { API_TOKEN = \"secret-value\" }\n"
		if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		binding.InstallPath = configPath + "#mcp_servers/component"
		binding.ConfigPath, binding.ConfigPointer = configPath, "mcp_servers/component"
		binding.ObservedVersion = "1.0.0"
	default:
		binding.InstallPath = filepath.Join(paths.DataDir, "native-plugin")
		if err := os.MkdirAll(binding.InstallPath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(binding.InstallPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.UpsertBinding(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	policy := model.DefaultPolicy()
	policy.BindingID, policy.ApplyMode, policy.LocalCapMode, policy.UpdatedAt = binding.ID, model.ApplyManual, model.ApplyManual, now
	if err := database.SetPolicy(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
}

func TestStdioMCPAdoptionReplacesCarrierPrefixAndPreservesSecrets(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentStdioMCP, adapter.SourceNPM)
	configPath := filepath.Join(filepath.Dir(fixture.installPath), "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	configContent := "[mcp_servers.component]\ncommand = \"npx\"\nargs = [\"-y\", \"component@1.0.0\", \"--transport\", \"stdio\"]\nenv = { API_TOKEN = \"secret-value\" }\n"
	if err := os.WriteFile(configPath, []byte(configContent), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.installPath = configPath + "#mcp_servers/component"
	fixture.seedBindingOnly(t, false, model.ApplyManual)
	binding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil {
		t.Fatal(err)
	}
	binding.ConfigPath, binding.ConfigPointer = configPath, "mcp_servers/component"
	binding.ObservedVersion = "1.0.0"
	if err := fixture.database.UpsertBinding(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{Source: "npm:component", Version: "1.0.0", Executable: "component"})
	if err != nil {
		t.Fatal(err)
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configAfter), "secret-value") || strings.Contains(string(configAfter), "component@1.0.0") || !strings.Contains(string(configAfter), result.Shim) {
		t.Fatalf("MCP config was not patched safely: %s", configAfter)
	}
	spec, err := host.CommandSpecAtPointer(configPath, "mcp_servers/component")
	if err != nil || spec.Command != result.Shim || strings.Join(spec.Args, " ") != "--transport stdio" {
		t.Fatalf("managed command=%+v err=%v", spec, err)
	}
	managed, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil || !managed.Managed || managed.TrustHash == "" {
		t.Fatalf("binding=%+v err=%v", managed, err)
	}
}

func TestReadyCandidateCannotOverwriteEditsMadeAfterStaging(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyManual)
	if _, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{Source: "https://example.test/plugin", Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	fixture.provider.latest = "v2"
	staged, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{Stage: true})
	if err != nil {
		t.Fatal(err)
	}
	if staged.Candidate.Status != model.CandidateReady {
		t.Fatalf("staged=%+v", staged)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("local edit after review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rechecked, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{Stage: true, Activate: true})
	if err != nil {
		t.Fatal(err)
	}
	if rechecked.Activated {
		t.Fatalf("changed overlay was activated: %+v", rechecked)
	}
	content, err := os.ReadFile(filepath.Join(fixture.installPath, "plugin.txt"))
	if err != nil || string(content) != "local edit after review\n" {
		t.Fatalf("local edit changed: %q err=%v", content, err)
	}
}

func TestActivationAbortsWhenPolicyIsTightenedDuringStaging(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyAuto)
	adopted, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-plugin", Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.provider.latest = "v2"
	fixture.provider.onVerify = func() error {
		policy, err := fixture.database.GetPolicy(context.Background(), "binding")
		if err != nil {
			return err
		}
		policy.ApplyMode, policy.LocalCapMode = model.ApplyManual, model.ApplyManual
		policy.UpdatedAt = policy.UpdatedAt.Add(time.Second)
		return fixture.database.SetPolicy(context.Background(), policy)
	}
	_, err = fixture.service.Update(context.Background(), "component", "", UpdateOptions{})
	if err == nil || !strings.Contains(err.Error(), "policy changed before activation") {
		t.Fatalf("activation error=%v", err)
	}
	binding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil || binding.ActiveGenerationID != adopted.Generation {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	current, err := activation.Current(fixture.service.activationRoot(binding.ID, false))
	if err != nil || current != adopted.Generation {
		t.Fatalf("current=%q err=%v", current, err)
	}
}

func TestReviewedCandidateIsReusedOnlyByContentHash(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentHook, adapter.SourceGit)
	fixture.provider.latest = "v2"
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyManual)
	adopted, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-hook", Version: "v1", Executable: "hook.sh",
	})
	if err != nil {
		t.Fatal(err)
	}
	managed, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil {
		t.Fatal(err)
	}
	if !managed.Managed || managed.InstallMethod != "tooltend-hook:hook.sh" || adopted.Shim == "" {
		t.Fatalf("managed hook = %+v adopted=%+v", managed, adopted)
	}
	payload, err := os.ReadFile(managed.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var configDocument map[string]any
	if err := json.Unmarshal(payload, &configDocument); err != nil {
		t.Fatal(err)
	}
	hook := configDocument["hooks"].([]any)[0].(map[string]any)
	command := hook["command"].(string)
	if !strings.Contains(command, filepath.Join("current", "hook.sh")) || !strings.HasSuffix(command, " --mode safe") || hook["timeout"] != float64(30) {
		t.Fatalf("hook config was not narrowly rewritten: %#v", hook)
	}

	staged, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{Stage: true})
	if err != nil {
		t.Fatal(err)
	}
	if !staged.NeedsReview || staged.Candidate.ReviewClass != model.ReviewHumanRequired {
		t.Fatalf("staged = %+v", staged)
	}
	reviewID, err := model.NewID("review")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.SubmitReview(context.Background(), model.Review{
		ID: reviewID, CandidateID: staged.Candidate.ID, CandidateHash: staged.Candidate.CandidateHash,
		ActorType: model.ActorHuman, Verdict: model.VerdictSafe, RiskType: "hook_change",
		Summary: "explicitly approved for this exact candidate", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	activated, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{Stage: true, Activate: true})
	if err != nil {
		t.Fatal(err)
	}
	if !activated.Activated || activated.Candidate.ID != staged.Candidate.ID || activated.Candidate.CandidateHash != staged.Candidate.CandidateHash {
		t.Fatalf("review was not bound to the reused candidate: staged=%+v activated=%+v", staged.Candidate, activated)
	}
}

func TestUpdateResumesEveryTransientCandidateState(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyManual)
	if _, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-plugin", Version: "v1",
	}); err != nil {
		t.Fatal(err)
	}
	fixture.provider.latest = "v2"
	staged, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{Stage: true})
	if err != nil || staged.Candidate.Status != model.CandidateReady {
		t.Fatalf("initial stage=%+v err=%v", staged, err)
	}
	for _, status := range []model.CandidateStatus{
		model.CandidateStaging, model.CandidateVerified, model.CandidateMerging, model.CandidateValidating,
	} {
		if _, err := fixture.database.DB().Exec(`UPDATE candidates SET status=?,merged_tree_hash=NULL WHERE id=?`, status, staged.Candidate.ID); err != nil {
			t.Fatal(err)
		}
		resumed, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{Stage: true})
		if err != nil {
			t.Fatalf("resume %s: %v", status, err)
		}
		if resumed.Candidate.ID != staged.Candidate.ID || resumed.Candidate.Status != model.CandidateReady {
			t.Fatalf("resume %s result=%+v", status, resumed)
		}
	}
}

func TestRollbackResumesEveryTransientCandidateState(t *testing.T) {
	for _, status := range []model.CandidateStatus{
		model.CandidateStaging, model.CandidateVerified, model.CandidateMerging, model.CandidateValidating,
	} {
		t.Run(string(status), func(t *testing.T) {
			fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
			if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			fixture.seedBindingOnly(t, false, model.ApplyAuto)
			adopted, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
				Source: "https://example.test/tooltend-plugin", Version: "v1",
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture.provider.latest = "v2"
			if updated, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{}); err != nil || !updated.Activated {
				t.Fatalf("update=%+v err=%v", updated, err)
			}
			ctx := context.Background()
			binding, err := fixture.database.GetBinding(ctx, "binding")
			if err != nil {
				t.Fatal(err)
			}
			current, err := fixture.database.GetGeneration(ctx, binding.ID, binding.ActiveGenerationID)
			if err != nil {
				t.Fatal(err)
			}
			target, err := fixture.database.GetGeneration(ctx, binding.ID, adopted.Generation)
			if err != nil {
				t.Fatal(err)
			}
			components, _ := fixture.database.ListComponents(ctx)
			source, err := fixture.database.GetSource(ctx, components[0].SourceID)
			if err != nil {
				t.Fatal(err)
			}
			baseline, err := fixture.database.LatestBaseline(ctx, binding.ID)
			if err != nil {
				t.Fatal(err)
			}
			hash, err := stableHash(candidateIdentity{
				BindingID: binding.ID, SourceID: source.ID, ResolvedRef: target.ResolvedRef,
				Upstream: target.TreeHash, Baseline: baseline.TreeHash, Rules: validationRulesVersion,
				Operation: "rollback:" + current.ID + ":" + target.ID,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.database.PutCandidate(ctx, model.UpdateCandidate{
				ID: stableCandidateID(hash), BindingID: binding.ID, SourceID: source.ID, ResolvedRef: target.ResolvedRef,
				UpstreamTreeHash: target.TreeHash, MergedTreeHash: target.TreeHash, BaselineID: baseline.ID,
				CandidateHash: hash, Status: status, ReviewClass: model.ReviewNone,
				CreatedAt: fixture.service.now(), UpdatedAt: fixture.service.now(),
			}); err != nil {
				t.Fatal(err)
			}
			rolledBack, err := fixture.service.Rollback(ctx, "component", "", adopted.Generation)
			if err != nil {
				t.Fatalf("rollback from %s: %v", status, err)
			}
			if rolledBack.To != adopted.Generation {
				t.Fatalf("rollback=%+v", rolledBack)
			}
		})
	}
}

func TestSemanticSkillReviewBundleContainsBoundedSafeEvidence(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentSkill, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "SKILL.md"), []byte("# v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyManual)
	if _, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{Source: "https://example.test/skill", Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	fixture.provider.latest = "v2"
	staged, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{Stage: true})
	if err != nil {
		t.Fatal(err)
	}
	if staged.Candidate.ReviewClass != model.ReviewSemanticSkillOnly {
		t.Fatalf("candidate=%+v", staged.Candidate)
	}
	bundle, err := fixture.database.GetReviewBundle(context.Background(), staged.Candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := fixture.service.Objects.OpenBlob(bundle.ObjectHash)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(io.LimitReader(reader, MaxReviewBundleBytes+1))
	_ = reader.Close()
	if err != nil || len(payload) > MaxReviewBundleBytes {
		t.Fatalf("bundle bytes=%d err=%v", len(payload), err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["candidate_hash"] != staged.Candidate.CandidateHash || !strings.Contains(string(payload), "# v2") {
		t.Fatalf("bundle lacks bound semantic evidence: %s", payload)
	}

	// Secret-like content must force human review and must never be embedded in
	// an agent-readable bundle.
	fixture.provider.secret = true
	fixture.provider.latest = "v3"
	secretStage, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{Stage: true})
	if err != nil {
		t.Fatal(err)
	}
	if secretStage.Candidate.ReviewClass != model.ReviewHumanRequired {
		t.Fatalf("secret candidate class=%s", secretStage.Candidate.ReviewClass)
	}
	secretBundle, err := fixture.database.GetReviewBundle(context.Background(), secretStage.Candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	secretReader, _ := fixture.service.Objects.OpenBlob(secretBundle.ObjectHash)
	secretPayload, _ := io.ReadAll(secretReader)
	_ = secretReader.Close()
	if strings.Contains(string(secretPayload), "super-secret-value") {
		t.Fatalf("review bundle leaked secret-like content: %s", secretPayload)
	}
}

type lifecycleFixture struct {
	service     *Service
	database    *store.Store
	provider    *fakeAdapter
	paths       config.Paths
	installPath string
	kind        model.ComponentKind
}

func newLifecycleFixture(t *testing.T, kind model.ComponentKind, sourceKind adapter.SourceKind) *lifecycleFixture {
	t.Helper()
	root := t.TempDir()
	paths := config.Paths{
		ConfigDir: filepath.Join(root, "config"), StateDir: filepath.Join(root, "state"),
		DatabaseFile: filepath.Join(root, "state", "state.db"), DataDir: filepath.Join(root, "data"),
		ObjectsDir: filepath.Join(root, "data", "objects"), StagingDir: filepath.Join(root, "data", "staging"),
		GenerationsDir: filepath.Join(root, "data", "generations"), RuntimesDir: filepath.Join(root, "data", "runtimes"),
		ShimDir: filepath.Join(root, "bin"),
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	objects, err := objectstore.New(paths.ObjectsDir)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(database, objects, paths)
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeAdapter{kind: sourceKind, componentKind: kind, latest: "v1"}
	registry, err := adapter.NewRegistry(provider)
	if err != nil {
		t.Fatal(err)
	}
	service.Adapters = registry
	service.Now = func() time.Time { return time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC) }
	installPath := filepath.Join(root, "native", "component")
	if kind == model.ComponentCLI {
		installPath = filepath.Join(root, "native", "bin", "component")
	}
	return &lifecycleFixture{service: service, database: database, provider: provider, paths: paths, installPath: installPath, kind: kind}
}

func (f *lifecycleFixture) seedBindingOnly(t *testing.T, managed bool, mode model.ApplyMode) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	if err := f.database.UpsertComponent(ctx, model.LogicalComponent{
		ID: "component", Kind: f.kind, Name: "component", LogicalKey: "test:component", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	binding := model.Binding{
		ID: "binding", ComponentID: "component", Host: model.HostCodex, Scope: model.ScopeGlobal,
		InstallPath: f.installPath, InstallMethod: "native", Managed: managed,
		Classification: model.ClassificationUnknown, LastSeenAt: now,
	}
	if f.kind == model.ComponentHook {
		if err := os.MkdirAll(f.installPath, 0o755); err != nil {
			t.Fatal(err)
		}
		nativeHook := filepath.Join(f.installPath, "native-hook.sh")
		if err := os.WriteFile(nativeHook, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(f.paths.ConfigDir, "hooks.json")
		payload := map[string]any{"hooks": []any{map[string]any{
			"command": nativeHook + " --mode safe", "timeout": float64(30),
		}}}
		content, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, append(content, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		binding.InstallPath = configPath + "#hooks/0"
		binding.ConfigPath = configPath
		binding.ConfigPointer = "hooks/0"
	}
	if err := f.database.UpsertBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	value := model.DefaultPolicy()
	value.BindingID, value.ApplyMode, value.LocalCapMode, value.UpdatedAt = "binding", mode, mode, now
	if err := f.database.SetPolicy(ctx, value); err != nil {
		t.Fatal(err)
	}
}

func (f *lifecycleFixture) seedKnownSource(t *testing.T, managed bool, mode model.ApplyMode) {
	t.Helper()
	f.seedBindingOnly(t, managed, mode)
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	source := model.Source{
		ID: "source", Kind: model.SourceKind(f.provider.kind), Locator: "https://example.test/tooltend-plugin",
		PackageName: "component", IdentityHash: strings.Repeat("a", 64), MetadataJSON: "{}", CreatedAt: now, UpdatedAt: now,
	}
	if err := f.database.UpsertSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	component := model.LogicalComponent{
		ID: "component", Kind: f.kind, Name: "component", SourceID: source.ID,
		LogicalKey: "test:component", CreatedAt: now, UpdatedAt: now,
	}
	if err := f.database.UpsertComponent(ctx, component); err != nil {
		t.Fatal(err)
	}
}

type fakeAdapter struct {
	kind           adapter.SourceKind
	componentKind  model.ComponentKind
	latest         string
	secret         bool
	expectedSubdir string
	onVerify       func() error
	resolveCalls   int
	fetchCalls     int
	fetchSubdirs   []string
}

func (f *fakeAdapter) Name() string                { return "fake" }
func (f *fakeAdapter) Kinds() []adapter.SourceKind { return []adapter.SourceKind{f.kind} }
func (f *fakeAdapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Check: true, Stage: true, ManagedAuto: true, Rollback: true}
}
func (f *fakeAdapter) Normalize(value adapter.Source) (adapter.Source, error) {
	value.Kind = f.kind
	if value.PackageName == "" {
		value.PackageName = "component"
	}
	return adapter.CanonicalizeSource(f.kind, value)
}
func (f *fakeAdapter) Resolve(_ context.Context, source adapter.Source, track adapter.Track) (adapter.Resolved, error) {
	f.resolveCalls++
	version := f.latest
	if track.Channel == "exact" && track.Constraint != "" {
		version = track.Constraint
	}
	if version == "" {
		return adapter.Resolved{}, errors.New("no version")
	}
	return adapter.Resolved{Version: version, Ref: source.PackageName + "@" + version}, nil
}
func (f *fakeAdapter) Fetch(_ context.Context, source adapter.Source, resolved adapter.Resolved, staging string) (adapter.Artifact, error) {
	f.fetchCalls++
	f.fetchSubdirs = append(f.fetchSubdirs, source.Subdir)
	if f.expectedSubdir != "" && source.Subdir != f.expectedSubdir {
		return adapter.Artifact{}, fmt.Errorf("unexpected source subdir %q", source.Subdir)
	}
	if err := os.RemoveAll(staging); err != nil {
		return adapter.Artifact{}, err
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return adapter.Artifact{}, err
	}
	if f.kind == adapter.SourceNPM {
		bin := filepath.Join(staging, "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			return adapter.Artifact{}, err
		}
		if err := os.WriteFile(filepath.Join(bin, "component"), []byte("#!/bin/sh\necho "+resolved.Version+"\n"), 0o755); err != nil {
			return adapter.Artifact{}, err
		}
		return adapter.Artifact{Root: staging, Executable: bin, Integrity: resolved.Ref}, nil
	}
	if f.componentKind == model.ComponentSkill {
		content := "# " + resolved.Version + "\n"
		if f.secret {
			content += "api_key = super-secret-value\n"
		}
		if err := os.WriteFile(filepath.Join(staging, "SKILL.md"), []byte(content), 0o644); err != nil {
			return adapter.Artifact{}, err
		}
		return adapter.Artifact{Root: staging, Integrity: resolved.Ref}, nil
	}
	if f.componentKind == model.ComponentHook {
		if err := os.WriteFile(filepath.Join(staging, "hook.sh"), []byte("#!/bin/sh\necho "+resolved.Version+"\n"), 0o755); err != nil {
			return adapter.Artifact{}, err
		}
		return adapter.Artifact{Root: staging, Integrity: resolved.Ref}, nil
	}
	if err := os.WriteFile(filepath.Join(staging, "plugin.txt"), []byte(resolved.Version+"\n"), 0o644); err != nil {
		return adapter.Artifact{}, err
	}
	return adapter.Artifact{Root: staging, Integrity: resolved.Ref}, nil
}
func (f *fakeAdapter) Verify(_ context.Context, _ adapter.Source, resolved adapter.Resolved, artifact adapter.Artifact) error {
	if artifact.Integrity != resolved.Ref {
		return errors.New("integrity mismatch")
	}
	if f.onVerify != nil {
		callback := f.onVerify
		f.onVerify = nil
		return callback()
	}
	return nil
}
