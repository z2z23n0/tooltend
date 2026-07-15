package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/z2z23n0/tooltend/internal/lockfile"
	"github.com/z2z23n0/tooltend/internal/safeio"
)

const DefaultManifestURL = "https://github.com/z2z23n0/tooltend/releases/latest/download/tooltend-manifest.json"

var ErrHomebrewManaged = errors.New("self-update is managed by Homebrew; run brew upgrade tooltend")

type VerifierFactory func(currentSequence uint64) (Verifier, error)

type Manager struct {
	StateDir        string
	Executable      string
	ManifestURL     string
	Fetcher         Fetcher
	Verifier        VerifierFactory
	Now             func() time.Time
	CurrentVersion  string
	CurrentSequence uint64

	// Test seams for deterministic concurrency barriers. Production managers
	// leave both callbacks nil.
	onLockContended    func(operation string)
	afterApplyLiveHash func()
}

type Pending struct {
	Version      string    `json:"version"`
	Sequence     uint64    `json:"sequence"`
	BinaryPath   string    `json:"binary_path"`
	EnvelopePath string    `json:"envelope_path"`
	SHA256       string    `json:"sha256"`
	Size         int64     `json:"size"`
	StagedAt     time.Time `json:"staged_at"`
}

type Status struct {
	CurrentVersion  string   `json:"current_version"`
	CurrentSequence uint64   `json:"current_sequence"`
	InstallMethod   string   `json:"install_method"`
	Pending         *Pending `json:"pending,omitempty"`
}

type ApplyResult struct {
	Applied     bool   `json:"applied"`
	Version     string `json:"version,omitempty"`
	Previous    string `json:"previous_path,omitempty"`
	AlreadyLive bool   `json:"already_live,omitempty"`
}

type PreparedRelease struct {
	Verified Verified `json:"verified"`
	Envelope []byte   `json:"-"`
}

func (m Manager) Status() (Status, error) {
	sequence, err := m.currentSequence()
	if err != nil {
		return Status{}, err
	}
	executable, err := m.executable()
	if err != nil {
		return Status{}, err
	}
	method := "direct"
	if isHomebrewExecutable(executable) {
		method = "homebrew"
	}
	pending, err := readPending(m.StateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Status{}, err
	}
	return Status{CurrentVersion: m.CurrentVersion, CurrentSequence: sequence, InstallMethod: method, Pending: pending}, nil
}

// Check fetches and verifies only the signed release manifest. It does not
// download an executable or mutate local state.
func (m Manager) Check(ctx context.Context) (Verified, error) {
	prepared, err := m.Prepare(ctx)
	if err != nil {
		return Verified{}, err
	}
	return prepared.Verified, nil
}

// Prepare binds a later confirmed stage operation to the exact signed
// envelope shown in its preview. It is read-only and does not fetch the asset.
func (m Manager) Prepare(ctx context.Context) (PreparedRelease, error) {
	sequence, err := m.currentSequence()
	if err != nil {
		return PreparedRelease{}, err
	}
	verifier, err := m.verifier(sequence)
	if err != nil {
		return PreparedRelease{}, err
	}
	raw, err := m.fetcher().Fetch(ctx, m.manifestURL(), MaxManifestBytes)
	if err != nil {
		return PreparedRelease{}, fmt.Errorf("self-update: fetch signed manifest: %w", err)
	}
	verified, err := verifier.Verify(raw)
	if err != nil {
		return PreparedRelease{}, err
	}
	return PreparedRelease{Verified: verified, Envelope: raw}, nil
}

// StageRelease verifies the envelope before using its asset URL, downloads
// the selected asset, verifies the signed size and checksum, and records all
// evidence needed to repeat verification on the next invocation.
func (m Manager) StageRelease(ctx context.Context) (Pending, error) {
	executable, err := m.executable()
	if err != nil {
		return Pending{}, err
	}
	if isHomebrewExecutable(executable) {
		return Pending{}, ErrHomebrewManaged
	}
	prepared, err := m.Prepare(ctx)
	if err != nil {
		return Pending{}, err
	}
	return m.StagePrepared(ctx, prepared)
}

// StagePrepared stages precisely the release represented by a prior preview;
// it never refetches the manifest. The signed envelope is reverified against
// the current sequence immediately before its asset URL is used.
func (m Manager) StagePrepared(ctx context.Context, prepared PreparedRelease) (pending Pending, err error) {
	executable, err := m.executable()
	if err != nil {
		return Pending{}, err
	}
	if isHomebrewExecutable(executable) {
		return Pending{}, ErrHomebrewManaged
	}
	lock, err := m.acquireOperationLock(ctx, "stage")
	if err != nil {
		return Pending{}, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	return m.stagePreparedLocked(ctx, prepared)
}

func (m Manager) stagePreparedLocked(ctx context.Context, prepared PreparedRelease) (Pending, error) {
	sequence, err := m.currentSequence()
	if err != nil {
		return Pending{}, err
	}
	verifier, err := m.verifier(sequence)
	if err != nil {
		return Pending{}, err
	}
	if len(prepared.Envelope) == 0 {
		return Pending{}, errors.New("self-update: prepared release has no signed envelope")
	}
	verified, err := verifier.Verify(prepared.Envelope)
	if err != nil {
		return Pending{}, err
	}
	if prepared.Verified.Manifest.Sequence != 0 && (prepared.Verified.Manifest.Sequence != verified.Manifest.Sequence ||
		prepared.Verified.Manifest.Version != verified.Manifest.Version || prepared.Verified.Asset.URL != verified.Asset.URL ||
		prepared.Verified.Asset.SHA256 != verified.Asset.SHA256 || prepared.Verified.Asset.Size != verified.Asset.Size) {
		return Pending{}, errors.New("self-update: prepared release metadata does not match its signed envelope")
	}
	if existing, pendingErr := readPending(m.StateDir); pendingErr == nil {
		if !samePendingRelease(*existing, verified) {
			return Pending{}, errors.New("self-update: a different signed release is already pending")
		}
		if err := verifyPendingPrepared(*existing, prepared, verified); err != nil {
			return Pending{}, err
		}
		return *existing, nil
	} else if !errors.Is(pendingErr, os.ErrNotExist) {
		return Pending{}, pendingErr
	}
	binaryPath, err := Stage(ctx, m.fetcher(), verified, m.StateDir)
	if err != nil {
		return Pending{}, err
	}
	pendingDir := filepath.Join(m.StateDir, "self-update")
	envelopePath := filepath.Join(pendingDir, "tooltend-"+verified.Manifest.Version+".manifest.json")
	if err := safeio.AtomicWriteFile(envelopePath, prepared.Envelope, 0o600); err != nil {
		_ = os.Remove(binaryPath)
		return Pending{}, err
	}
	now := time.Now().UTC()
	if m.Now != nil {
		now = m.Now().UTC()
	}
	pending := Pending{
		Version: verified.Manifest.Version, Sequence: verified.Manifest.Sequence,
		BinaryPath: binaryPath, EnvelopePath: envelopePath,
		SHA256: verified.Asset.SHA256, Size: verified.Asset.Size, StagedAt: now,
	}
	encoded, err := json.Marshal(pending)
	if err != nil {
		return Pending{}, err
	}
	if err := safeio.AtomicWriteFile(pendingFile(m.StateDir), encoded, 0o600); err != nil {
		return Pending{}, err
	}
	return pending, nil
}

// ApplyPending re-verifies the signed envelope and staged bytes, creates a
// sibling backup of the currently running executable, then atomically renames
// the new binary over it. If a crash happened after replacement, comparing
// the live checksum makes the retry idempotent and preserves the old backup.
func (m Manager) ApplyPending(ctx context.Context) (result ApplyResult, err error) {
	// Preserve the side-effect-free no-pending path used by ordinary commands
	// before ToolTend has created its state directories. The metadata is read
	// again only after the lock is held.
	if _, statErr := os.Lstat(pendingFile(m.StateDir)); errors.Is(statErr, os.ErrNotExist) {
		return ApplyResult{}, nil
	} else if statErr != nil {
		return ApplyResult{}, statErr
	}
	lock, err := m.acquireOperationLock(ctx, "apply")
	if err != nil {
		return ApplyResult{}, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	return m.applyPendingLocked(ctx)
}

func (m Manager) applyPendingLocked(ctx context.Context) (ApplyResult, error) {
	pending, err := readPending(m.StateDir)
	if errors.Is(err, os.ErrNotExist) {
		return ApplyResult{}, nil
	}
	if err != nil {
		return ApplyResult{}, err
	}
	executable, err := m.executable()
	if err != nil {
		return ApplyResult{}, err
	}
	if isHomebrewExecutable(executable) {
		return ApplyResult{}, ErrHomebrewManaged
	}
	sequence, err := m.currentSequence()
	if err != nil {
		return ApplyResult{}, err
	}
	if pending.Sequence < sequence {
		return ApplyResult{}, errors.New("self-update: pending release is stale")
	}
	if pending.Sequence == sequence {
		// finalizePending persists the anti-rollback sequence before deleting
		// pending artifacts. A crash in that cleanup window is therefore a
		// completed replacement, not a replay. Only accept it when the live
		// executable still has the exact staged checksum, then finish cleanup
		// idempotently without requiring the possibly already removed files.
		if err := ctx.Err(); err != nil {
			return ApplyResult{}, err
		}
		liveHash, hashErr := fileSHA256(executable)
		if hashErr != nil {
			return ApplyResult{}, hashErr
		}
		if m.afterApplyLiveHash != nil {
			m.afterApplyLiveHash()
		}
		if !strings.EqualFold(liveHash, pending.SHA256) {
			return ApplyResult{}, errors.New("self-update: committed sequence does not match the live executable")
		}
		if err := finalizePending(m.StateDir, pending); err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{
			Applied: true, Version: pending.Version, Previous: executable + ".previous", AlreadyLive: true,
		}, nil
	}
	verifier, err := m.verifier(sequence)
	if err != nil {
		return ApplyResult{}, err
	}
	raw, err := os.ReadFile(pending.EnvelopePath)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("self-update: read pending manifest: %w", err)
	}
	verified, err := verifier.Verify(raw)
	if err != nil {
		return ApplyResult{}, err
	}
	if verified.Manifest.Version != pending.Version || verified.Manifest.Sequence != pending.Sequence || verified.Asset.SHA256 != pending.SHA256 || verified.Asset.Size != pending.Size {
		return ApplyResult{}, errors.New("self-update: pending metadata does not match signed manifest")
	}
	data, err := os.ReadFile(pending.BinaryPath)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("self-update: read pending binary: %w", err)
	}
	if err := VerifyAsset(data, verified.Asset); err != nil {
		return ApplyResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ApplyResult{}, err
	}
	liveHash, err := fileSHA256(executable)
	if err != nil {
		return ApplyResult{}, err
	}
	if m.afterApplyLiveHash != nil {
		m.afterApplyLiveHash()
	}
	previous := executable + ".previous"
	if strings.EqualFold(liveHash, verified.Asset.SHA256) {
		if err := finalizePending(m.StateDir, pending); err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{Applied: true, Version: pending.Version, Previous: previous, AlreadyLive: true}, nil
	}
	info, err := os.Lstat(executable)
	if err != nil {
		return ApplyResult{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ApplyResult{}, errors.New("self-update: direct executable is not a regular file")
	}
	parent := filepath.Dir(executable)
	newFile, err := os.CreateTemp(parent, ".tooltend-update-*")
	if err != nil {
		return ApplyResult{}, err
	}
	newPath := newFile.Name()
	defer os.Remove(newPath)
	if err := newFile.Chmod(0o755); err != nil {
		_ = newFile.Close()
		return ApplyResult{}, err
	}
	if _, err := newFile.Write(data); err != nil {
		_ = newFile.Close()
		return ApplyResult{}, err
	}
	if err := newFile.Sync(); err != nil {
		_ = newFile.Close()
		return ApplyResult{}, err
	}
	if err := newFile.Close(); err != nil {
		return ApplyResult{}, err
	}
	backupTemp := previous + ".tmp"
	_ = os.Remove(backupTemp)
	if err := linkOrCopy(executable, backupTemp, info.Mode().Perm()); err != nil {
		return ApplyResult{}, fmt.Errorf("self-update: preserve previous binary: %w", err)
	}
	if err := os.Rename(backupTemp, previous); err != nil {
		_ = os.Remove(backupTemp)
		return ApplyResult{}, err
	}
	if err := os.Rename(newPath, executable); err != nil {
		return ApplyResult{}, fmt.Errorf("self-update: replace executable: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return ApplyResult{}, err
	}
	if err := finalizePending(m.StateDir, pending); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Applied: true, Version: pending.Version, Previous: previous}, nil
}

func (m Manager) acquireOperationLock(ctx context.Context, operation string) (*lockfile.Lock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	executable, err := m.executable()
	if err != nil {
		return nil, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	// The live executable is the resource protected by this lock. Keeping the
	// lock beside it makes independent --state-dir invocations serialize before
	// either can replace the binary or overwrite its .previous backup.
	path := filepath.Join(filepath.Dir(executable), "."+filepath.Base(executable)+".self-update.lock")
	contended := false
	for {
		lock, err := lockfile.Try(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, lockfile.ErrLocked) {
			return nil, fmt.Errorf("self-update: acquire operation lock: %w", err)
		}
		if !contended {
			contended = true
			if m.onLockContended != nil {
				m.onLockContended(operation)
			}
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func samePendingRelease(pending Pending, verified Verified) bool {
	return pending.Version == verified.Manifest.Version && pending.Sequence == verified.Manifest.Sequence &&
		strings.EqualFold(pending.SHA256, verified.Asset.SHA256) && pending.Size == verified.Asset.Size
}

func verifyPendingPrepared(pending Pending, prepared PreparedRelease, verified Verified) error {
	envelope, err := os.ReadFile(pending.EnvelopePath)
	if err != nil {
		return fmt.Errorf("self-update: read existing pending manifest: %w", err)
	}
	if !bytes.Equal(envelope, prepared.Envelope) {
		return errors.New("self-update: pending release does not match the confirmed signed envelope")
	}
	binary, err := os.ReadFile(pending.BinaryPath)
	if err != nil {
		return fmt.Errorf("self-update: read existing pending binary: %w", err)
	}
	if err := VerifyAsset(binary, verified.Asset); err != nil {
		return fmt.Errorf("self-update: verify existing pending binary: %w", err)
	}
	return nil
}

func (m Manager) verifier(sequence uint64) (Verifier, error) {
	if m.Verifier != nil {
		return m.Verifier(sequence)
	}
	return EmbeddedVerifier(sequence)
}

func (m Manager) fetcher() Fetcher {
	if m.Fetcher != nil {
		return m.Fetcher
	}
	return HTTPFetcher{}
}

func (m Manager) manifestURL() string {
	if m.ManifestURL != "" {
		return m.ManifestURL
	}
	return DefaultManifestURL
}

func (m Manager) executable() (string, error) {
	value := m.Executable
	if value == "" {
		var err error
		value, err = os.Executable()
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(value) {
		return "", errors.New("self-update: executable path must be absolute")
	}
	return filepath.Clean(value), nil
}

func pendingFile(stateDir string) string {
	return filepath.Join(stateDir, "self-update", "pending.json")
}

func readPending(stateDir string) (*Pending, error) {
	data, err := os.ReadFile(pendingFile(stateDir))
	if err != nil {
		return nil, err
	}
	var pending Pending
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pending); err != nil {
		return nil, fmt.Errorf("self-update: decode pending metadata: %w", err)
	}
	if !safeVersion(pending.Version) || pending.Sequence == 0 || pending.Size <= 0 || pending.BinaryPath == "" || pending.EnvelopePath == "" || len(pending.SHA256) != sha256.Size*2 {
		return nil, errors.New("self-update: pending metadata is incomplete")
	}
	root := filepath.Clean(filepath.Join(stateDir, "self-update"))
	for _, value := range []string{pending.BinaryPath, pending.EnvelopePath} {
		cleaned := filepath.Clean(value)
		relative, err := filepath.Rel(root, cleaned)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, errors.New("self-update: pending path escapes state directory")
		}
	}
	return &pending, nil
}

func sequenceFile(stateDir string) string { return filepath.Join(stateDir, "self-update", "sequence") }

func (m Manager) currentSequence() (uint64, error) {
	stored, err := readSequence(m.StateDir)
	if err != nil {
		return 0, err
	}
	if m.CurrentSequence > stored {
		return m.CurrentSequence, nil
	}
	return stored, nil
}

func readSequence(stateDir string) (uint64, error) {
	data, err := os.ReadFile(sequenceFile(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	sequence, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, errors.New("self-update: stored sequence is invalid")
	}
	return sequence, nil
}

func finalizePending(stateDir string, pending *Pending) error {
	if err := safeio.AtomicWriteFile(sequenceFile(stateDir), []byte(strconv.FormatUint(pending.Sequence, 10)+"\n"), 0o600); err != nil {
		return err
	}
	for _, value := range []string{pendingFile(stateDir), pending.BinaryPath, pending.EnvelopePath} {
		if err := os.Remove(value); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func linkOrCopy(source, destination string, mode os.FileMode) error {
	if err := os.Link(source, destination); err == nil {
		return nil
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func isHomebrewExecutable(executable string) bool {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		resolved = executable
	}
	resolved = filepath.ToSlash(resolved)
	return strings.Contains(resolved, "/Cellar/") || strings.Contains(resolved, "/Homebrew/Cellar/")
}
