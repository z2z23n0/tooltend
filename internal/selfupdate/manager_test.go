package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/safeio"
)

type mapFetcher map[string][]byte

func (f mapFetcher) Fetch(_ context.Context, url string, limit int64) ([]byte, error) {
	data, ok := f[url]
	if !ok {
		return nil, errors.New("missing URL")
	}
	if int64(len(data)) > limit {
		return nil, errors.New("too large")
	}
	return append([]byte(nil), data...), nil
}

type fetcherFunc func(context.Context, string, int64) ([]byte, error)

func (f fetcherFunc) Fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	return f(ctx, url, limit)
}

func releaseFixture(t *testing.T, sequence uint64, version string, binary []byte) ([]byte, ed25519.PublicKey) {
	t.Helper()
	hash := sha256.Sum256(binary)
	manifest, err := json.Marshal(Manifest{
		SchemaVersion: 1,
		Sequence:      sequence,
		Version:       version,
		PublishedAt:   time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC),
		Assets: []Asset{{
			OS: "test-os", Arch: "test-arch", URL: "https://example.test/tooltend",
			SHA256: hex.EncodeToString(hash[:]), Size: int64(len(binary)),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return signedEnvelope(t, manifest)
}

func TestManagerStagesAndAtomicallyAppliesRelease(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	executable := filepath.Join(root, "tooltend")
	if err := os.WriteFile(executable, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBinary := []byte("new-binary")
	envelope, key := releaseFixture(t, 2, "1.2.0", newBinary)
	fetcher := mapFetcher{
		"https://example.test/manifest": envelope,
		"https://example.test/tooltend": newBinary,
	}
	manager := Manager{
		StateDir: state, Executable: executable, ManifestURL: "https://example.test/manifest", Fetcher: fetcher,
		Verifier: func(sequence uint64) (Verifier, error) {
			return Verifier{Keys: map[string]ed25519.PublicKey{"test": key}, CurrentSequence: sequence, OS: "test-os", Arch: "test-arch"}, nil
		},
	}
	pending, err := manager.StageRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pending.Version != "1.2.0" {
		t.Fatalf("pending = %#v", pending)
	}
	if got, _ := os.ReadFile(executable); string(got) != "old-binary" {
		t.Fatalf("staging changed live executable: %q", got)
	}
	result, err := manager.ApplyPending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.AlreadyLive {
		t.Fatalf("result = %#v", result)
	}
	if got, _ := os.ReadFile(executable); string(got) != string(newBinary) {
		t.Fatalf("live binary = %q", got)
	}
	if got, _ := os.ReadFile(executable + ".previous"); string(got) != "old-binary" {
		t.Fatalf("previous binary = %q", got)
	}
	if sequence, err := readSequence(state); err != nil || sequence != 2 {
		t.Fatalf("sequence=%d err=%v", sequence, err)
	}
	if _, err := os.Stat(pendingFile(state)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending metadata still exists: %v", err)
	}
}

func TestPreparedReleaseBindsPreviewToExactManifest(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "tooltend")
	if err := os.WriteFile(executable, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	binary := []byte("release-one")
	envelope, key := releaseFixture(t, 1, "1.0.0", binary)
	fetcher := mapFetcher{"https://example.test/manifest": envelope, "https://example.test/tooltend": binary}
	manager := Manager{
		StateDir: filepath.Join(root, "state"), Executable: executable, ManifestURL: "https://example.test/manifest", Fetcher: fetcher,
		Verifier: func(sequence uint64) (Verifier, error) {
			return Verifier{Keys: map[string]ed25519.PublicKey{"test": key}, CurrentSequence: sequence, OS: "test-os", Arch: "test-arch"}, nil
		},
	}
	prepared, err := manager.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A release feed can move between preview and confirmation. Removing the
	// manifest URL proves StagePrepared does not fetch it a second time.
	delete(fetcher, "https://example.test/manifest")
	pending, err := manager.StagePrepared(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Version != "1.0.0" || pending.Sequence != 1 {
		t.Fatalf("pending=%#v", pending)
	}
}

func TestStagePreparedSerializesConcurrentManagers(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	executable := filepath.Join(root, "tooltend")
	if err := os.WriteFile(executable, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	binary := []byte("release-one")
	envelope, key := releaseFixture(t, 1, "1.0.0", binary)
	base := Manager{
		StateDir: state, Executable: executable, ManifestURL: "https://example.test/manifest",
		Fetcher: mapFetcher{"https://example.test/manifest": envelope, "https://example.test/tooltend": binary},
		Verifier: func(sequence uint64) (Verifier, error) {
			return Verifier{Keys: map[string]ed25519.PublicKey{"test": key}, CurrentSequence: sequence, OS: "test-os", Arch: "test-arch"}, nil
		},
	}
	prepared, err := base.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	firstFetching := make(chan struct{})
	releaseFirst := make(chan struct{})
	first := base
	first.Fetcher = fetcherFunc(func(ctx context.Context, _ string, _ int64) ([]byte, error) {
		close(firstFetching)
		select {
		case <-releaseFirst:
			return append([]byte(nil), binary...), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	var secondFetches atomic.Int32
	secondContended := make(chan struct{}, 1)
	second := base
	second.Fetcher = fetcherFunc(func(context.Context, string, int64) ([]byte, error) {
		secondFetches.Add(1)
		return nil, errors.New("second stage unexpectedly downloaded the same release")
	})
	second.onLockContended = func(operation string) {
		if operation == "stage" {
			select {
			case secondContended <- struct{}{}:
			default:
			}
		}
	}

	type stageResult struct {
		pending Pending
		err     error
	}
	firstDone := make(chan stageResult, 1)
	go func() {
		pending, stageErr := first.StagePrepared(context.Background(), prepared)
		firstDone <- stageResult{pending: pending, err: stageErr}
	}()
	waitForTestSignal(t, firstFetching, "first stage fetch")
	secondDone := make(chan stageResult, 1)
	go func() {
		pending, stageErr := second.StagePrepared(context.Background(), prepared)
		secondDone <- stageResult{pending: pending, err: stageErr}
	}()
	waitForTestSignal(t, secondContended, "second stage lock contention")
	close(releaseFirst)

	firstResult := <-firstDone
	secondResult := <-secondDone
	if firstResult.err != nil || secondResult.err != nil {
		t.Fatalf("first err=%v second err=%v", firstResult.err, secondResult.err)
	}
	if firstResult.pending.BinaryPath != secondResult.pending.BinaryPath || firstResult.pending.EnvelopePath != secondResult.pending.EnvelopePath || firstResult.pending.SHA256 != secondResult.pending.SHA256 {
		t.Fatalf("staged releases differ: first=%+v second=%+v", firstResult.pending, secondResult.pending)
	}
	if calls := secondFetches.Load(); calls != 0 {
		t.Fatalf("second manager fetched %d assets instead of reusing the pending release", calls)
	}
}

func TestApplyPendingSerializesDifferentStateDirsForSameExecutable(t *testing.T) {
	root := t.TempDir()
	firstState := filepath.Join(root, "state-one")
	secondState := filepath.Join(root, "state-two")
	executable := filepath.Join(root, "tooltend")
	oldBinary := []byte("the-real-old-binary")
	newBinary := []byte("the-new-binary")
	if err := os.WriteFile(executable, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	envelope, key := releaseFixture(t, 5, "5.0.0", newBinary)
	base := Manager{
		StateDir: firstState, Executable: executable, ManifestURL: "https://example.test/manifest",
		Fetcher: mapFetcher{"https://example.test/manifest": envelope, "https://example.test/tooltend": newBinary},
		Verifier: func(sequence uint64) (Verifier, error) {
			return Verifier{Keys: map[string]ed25519.PublicKey{"test": key}, CurrentSequence: sequence, OS: "test-os", Arch: "test-arch"}, nil
		},
	}
	if _, err := base.StageRelease(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondBase := base
	secondBase.StateDir = secondState
	if _, err := secondBase.StageRelease(context.Background()); err != nil {
		t.Fatal(err)
	}

	firstReadLive := make(chan struct{})
	releaseFirst := make(chan struct{})
	first := base
	first.afterApplyLiveHash = func() {
		close(firstReadLive)
		<-releaseFirst
	}
	secondContended := make(chan struct{}, 1)
	second := secondBase
	second.onLockContended = func(operation string) {
		if operation == "apply" {
			select {
			case secondContended <- struct{}{}:
			default:
			}
		}
	}

	type applyOutcome struct {
		result ApplyResult
		err    error
	}
	firstDone := make(chan applyOutcome, 1)
	go func() {
		result, applyErr := first.ApplyPending(context.Background())
		firstDone <- applyOutcome{result: result, err: applyErr}
	}()
	waitForTestSignal(t, firstReadLive, "first apply live hash")
	secondDone := make(chan applyOutcome, 1)
	go func() {
		result, applyErr := second.ApplyPending(context.Background())
		secondDone <- applyOutcome{result: result, err: applyErr}
	}()
	waitForTestSignal(t, secondContended, "second apply lock contention")
	close(releaseFirst)

	firstOutcome := <-firstDone
	secondOutcome := <-secondDone
	if firstOutcome.err != nil || secondOutcome.err != nil {
		t.Fatalf("first err=%v second err=%v", firstOutcome.err, secondOutcome.err)
	}
	if !firstOutcome.result.Applied || !secondOutcome.result.Applied || !secondOutcome.result.AlreadyLive {
		t.Fatalf("first=%+v second=%+v", firstOutcome.result, secondOutcome.result)
	}
	if got, err := os.ReadFile(executable); err != nil || string(got) != string(newBinary) {
		t.Fatalf("live binary=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(executable + ".previous"); err != nil || string(got) != string(oldBinary) {
		t.Fatalf("previous binary=%q err=%v", got, err)
	}
	for _, state := range []string{firstState, secondState} {
		if sequence, err := readSequence(state); err != nil || sequence != 5 {
			t.Fatalf("state=%s sequence=%d err=%v", state, sequence, err)
		}
		if _, err := os.Stat(pendingFile(state)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("state=%s pending metadata still exists: %v", state, err)
		}
	}
}

func waitForTestSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func TestManagerRejectsTamperedPendingBinary(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "tooltend")
	if err := os.WriteFile(executable, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	binary := []byte("signed")
	envelope, key := releaseFixture(t, 1, "1.0.0", binary)
	manager := Manager{
		StateDir: filepath.Join(root, "state"), Executable: executable, ManifestURL: "https://example.test/manifest",
		Fetcher: mapFetcher{"https://example.test/manifest": envelope, "https://example.test/tooltend": binary},
		Verifier: func(sequence uint64) (Verifier, error) {
			return Verifier{Keys: map[string]ed25519.PublicKey{"test": key}, CurrentSequence: sequence, OS: "test-os", Arch: "test-arch"}, nil
		},
	}
	pending, err := manager.StageRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending.BinaryPath, []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ApplyPending(context.Background()); err == nil {
		t.Fatal("expected tampered pending binary to fail")
	}
	if got, _ := os.ReadFile(executable); string(got) != "old" {
		t.Fatalf("live executable changed: %q", got)
	}
}

func TestApplyPendingRecoversCrashAfterSequenceCommit(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	executable := filepath.Join(root, "tooltend")
	oldBinary := []byte("old")
	newBinary := []byte("new-live-binary")
	if err := os.WriteFile(executable, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	envelope, key := releaseFixture(t, 7, "7.0.0", newBinary)
	manager := Manager{
		StateDir: state, Executable: executable, ManifestURL: "https://example.test/manifest",
		Fetcher: mapFetcher{"https://example.test/manifest": envelope, "https://example.test/tooltend": newBinary},
		Verifier: func(sequence uint64) (Verifier, error) {
			return Verifier{Keys: map[string]ed25519.PublicKey{"test": key}, CurrentSequence: sequence, OS: "test-os", Arch: "test-arch"}, nil
		},
	}
	pending, err := manager.StageRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, newBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := safeio.AtomicWriteFile(sequenceFile(state), []byte("7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := manager.ApplyPending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || !result.AlreadyLive || result.Version != pending.Version {
		t.Fatalf("result=%+v", result)
	}
	for _, path := range []string{pendingFile(state), pending.BinaryPath, pending.EnvelopePath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pending artifact still exists at %s: %v", path, err)
		}
	}
}

func TestStageRejectsUnsafeSignedVersion(t *testing.T) {
	verified := Verified{Manifest: Manifest{Version: "../../escape"}, Asset: Asset{URL: "https://example.test/a", Size: 1, SHA256: string(make([]byte, 64))}}
	if _, err := Stage(context.Background(), mapFetcher{"https://example.test/a": {0}}, verified, t.TempDir()); err == nil {
		t.Fatal("expected unsafe version to fail before writing")
	}
}

func TestManagerDefersToHomebrew(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Cellar", "tooltend", "1.0.0", "bin")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "tooltend")
	if err := os.WriteFile(executable, []byte("brew"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := (Manager{StateDir: t.TempDir(), Executable: executable}).StageRelease(context.Background())
	if !errors.Is(err, ErrHomebrewManaged) {
		t.Fatalf("got %v", err)
	}
}
