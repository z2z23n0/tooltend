package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/z2z23n0/tooltend/internal/activation"
	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/objectstore"
	"github.com/z2z23n0/tooltend/internal/safeio"
	"github.com/z2z23n0/tooltend/internal/store"
)

const adoptionPlanVersion = 1

type AdoptionFailpoint string

const (
	AdoptionFailAfterJournal  AdoptionFailpoint = "after_journal"
	AdoptionFailAfterCurrent  AdoptionFailpoint = "after_current"
	AdoptionFailAfterEndpoint AdoptionFailpoint = "after_endpoint"
)

type adoptionEffects struct {
	Install *adoptionInstallEffect `json:"install,omitempty"`
	Shim    *adoptionShimEffect    `json:"shim,omitempty"`
	Config  *adoptionConfigEffect  `json:"config,omitempty"`
}

type adoptionInstallEffect struct {
	Path           string `json:"path"`
	BackupPath     string `json:"backup_path"`
	TempPath       string `json:"temp_path"`
	Target         string `json:"target"`
	BeforeTreeHash string `json:"before_tree_hash"`
}

type adoptionShimEffect struct {
	Path         string `json:"path"`
	BackupPath   string `json:"backup_path,omitempty"`
	BeforeExists bool   `json:"before_exists"`
	BeforeHash   string `json:"before_hash,omitempty"`
	AfterHash    string `json:"after_hash"`
}

type adoptionConfigEffect struct {
	Path          string `json:"path"`
	Pointer       string `json:"pointer"`
	BeforeHash    string `json:"before_hash"`
	AfterHash     string `json:"after_hash"`
	ManagedTarget string `json:"managed_target"`
	Changed       bool   `json:"changed"`
}

type adoptionEffectState int

const (
	effectBefore adoptionEffectState = iota
	effectPartial
	effectAfter
)

var errAdoptionRecoveryConflict = errors.New("lifecycle: adoption recovery found external state")

type AdoptionRecoveryResult struct {
	Committed  int `json:"committed"`
	RolledBack int `json:"rolled_back"`
	Blocked    int `json:"blocked"`
}

func (r AdoptionRecoveryResult) Total() int { return r.Committed + r.RolledBack }

// RecoverAdoptions converges every unfinished adoption while the caller owns
// activation.lock. A conflict is recorded as blocked and left operational;
// unrelated bindings continue to recover.
func RecoverAdoptions(ctx context.Context, database *store.Store, paths config.Paths) (AdoptionRecoveryResult, error) {
	if database == nil {
		return AdoptionRecoveryResult{}, errors.New("lifecycle: adoption recovery database is required")
	}
	intents, err := database.ListPendingAdoptions(ctx)
	if err != nil {
		return AdoptionRecoveryResult{}, err
	}
	objects, err := objectstore.New(paths.ObjectsDir)
	if err != nil {
		return AdoptionRecoveryResult{}, err
	}
	var result AdoptionRecoveryResult
	for _, intent := range intents {
		action, recoverErr := recoverAdoptionIntent(ctx, database, objects, paths, intent)
		if errors.Is(recoverErr, errAdoptionRecoveryConflict) || errors.Is(recoverErr, store.ErrAdoptionStateChanged) {
			if markErr := database.MarkAdoptionBlocked(ctx, intent.ID, "adoption_recovery_conflict", time.Now().UTC()); markErr != nil {
				return result, errors.Join(recoverErr, markErr)
			}
			result.Blocked++
			continue
		}
		if recoverErr != nil {
			return result, fmt.Errorf("lifecycle: recover adoption %s: %w", intent.ID, recoverErr)
		}
		switch action {
		case store.AdoptionCommitted:
			result.Committed++
		case store.AdoptionRolledBack:
			result.RolledBack++
		}
	}
	return result, nil
}

func recoverOneAdoption(ctx context.Context, database *store.Store, paths config.Paths, id string) (store.AdoptionPhase, error) {
	intent, err := database.GetAdoptionIntent(ctx, id)
	if err != nil {
		return "", err
	}
	objects, err := objectstore.New(paths.ObjectsDir)
	if err != nil {
		return "", err
	}
	phase, err := recoverAdoptionIntent(ctx, database, objects, paths, intent)
	if errors.Is(err, errAdoptionRecoveryConflict) || errors.Is(err, store.ErrAdoptionStateChanged) {
		markErr := database.MarkAdoptionBlocked(ctx, id, "adoption_recovery_conflict", time.Now().UTC())
		return store.AdoptionBlocked, errors.Join(err, markErr)
	}
	return phase, err
}

func recoverAdoptionIntent(ctx context.Context, database *store.Store, objects *objectstore.Store, paths config.Paths, intent store.AdoptionIntent) (store.AdoptionPhase, error) {
	if intent.Phase == store.AdoptionCommitted || intent.Phase == store.AdoptionRolledBack {
		return intent.Phase, nil
	}
	effects, err := decodeAndValidateAdoptionEffects(intent, paths)
	if err != nil {
		return "", errors.Join(errAdoptionRecoveryConflict, err)
	}
	finalState, err := finalAdoptionEffectState(ctx, objects, intent.Plan, effects)
	if err != nil {
		return "", err
	}
	if finalState == effectAfter {
		if err := ensureAdoptionEffectsAfter(ctx, objects, effects); err != nil {
			return "", err
		}
		if err := ensureAdoptionCurrentAfter(intent.Plan); err != nil {
			return "", err
		}
		if err := database.MarkAdoptionSwitched(ctx, intent.ID, time.Now().UTC()); err != nil {
			return "", err
		}
		if err := database.FinalizeAdoption(ctx, intent.ID, time.Now().UTC()); err != nil {
			return "", err
		}
		return store.AdoptionCommitted, nil
	}
	if err := rollbackAdoptionEffects(ctx, objects, effects); err != nil {
		return "", err
	}
	if err := rollbackAdoptionCurrent(intent.Plan); err != nil {
		return "", err
	}
	if err := os.RemoveAll(intent.Plan.GenerationPath); err != nil {
		return "", err
	}
	if err := database.MarkAdoptionRolledBack(ctx, intent.ID, time.Now().UTC()); err != nil {
		return "", err
	}
	return store.AdoptionRolledBack, nil
}

func decodeAndValidateAdoptionEffects(intent store.AdoptionIntent, paths config.Paths) (adoptionEffects, error) {
	plan := intent.Plan
	if plan.Version != adoptionPlanVersion || plan.Kind != intent.Kind || plan.Commit.Binding.ID != intent.BindingID {
		return adoptionEffects{}, errors.New("journal plan identity mismatch")
	}
	expectedRoot := filepath.Join(paths.GenerationsDir, intent.BindingID)
	if plan.Runtime {
		expectedRoot = filepath.Join(paths.RuntimesDir, intent.BindingID)
	}
	if filepath.Clean(plan.Root) != filepath.Clean(expectedRoot) {
		return adoptionEffects{}, errors.New("journal activation root is outside the binding root")
	}
	expectedGeneration, err := activation.GenerationPath(expectedRoot, plan.Commit.Generation.ID)
	if err != nil || filepath.Clean(plan.GenerationPath) != filepath.Clean(expectedGeneration) {
		return adoptionEffects{}, errors.New("journal generation path mismatch")
	}
	var effects adoptionEffects
	if err := json.Unmarshal(plan.EffectsJSON, &effects); err != nil {
		return effects, err
	}
	expectedBinding := plan.Commit.ExpectedBinding
	switch intent.Kind {
	case store.AdoptionFile:
		if effects.Install == nil || effects.Shim != nil || effects.Config != nil {
			return effects, errors.New("file adoption has invalid effects")
		}
	case store.AdoptionHook:
		if effects.Config == nil || effects.Install != nil || effects.Shim != nil {
			return effects, errors.New("hook adoption has invalid effects")
		}
	case store.AdoptionRuntimeCLI:
		if effects.Shim == nil || effects.Install != nil || effects.Config != nil {
			return effects, errors.New("CLI runtime adoption has invalid effects")
		}
	case store.AdoptionRuntimeStdio:
		if effects.Shim == nil || effects.Config == nil || effects.Install != nil {
			return effects, errors.New("stdio runtime adoption has invalid effects")
		}
	default:
		return effects, errors.New("unknown adoption kind")
	}
	if effects.Install != nil {
		effect := effects.Install
		if filepath.Clean(effect.Path) != filepath.Clean(expectedBinding.InstallPath) ||
			filepath.Clean(effect.Target) != filepath.Join(expectedRoot, "current") ||
			filepath.Clean(effect.BackupPath) != filepath.Clean(adoptionBackupPath(effect.Path, intent.ID)) ||
			filepath.Clean(effect.TempPath) != filepath.Clean(adoptionInstallTempPath(effect.Path, intent.ID)) || effect.BeforeTreeHash == "" {
			return effects, errors.New("install adoption effect path mismatch")
		}
	}
	if effects.Shim != nil {
		effect := effects.Shim
		if !filepath.IsAbs(effect.Path) || filepath.Clean(filepath.Dir(effect.Path)) != filepath.Clean(paths.ShimDir) || effect.AfterHash == "" {
			return effects, errors.New("shim adoption effect path mismatch")
		}
		if effect.BeforeExists {
			if effect.BeforeHash == "" || filepath.Clean(effect.BackupPath) != filepath.Clean(adoptionBackupPath(effect.Path, intent.ID)) {
				return effects, errors.New("shim adoption backup mismatch")
			}
		} else if effect.BeforeHash != "" || effect.BackupPath != "" {
			return effects, errors.New("new shim effect contains a backup")
		}
	}
	if effects.Config != nil {
		effect := effects.Config
		if filepath.Clean(effect.Path) != filepath.Clean(expectedBinding.ConfigPath) || effect.Pointer != expectedBinding.ConfigPointer ||
			effect.BeforeHash == "" || effect.AfterHash == "" || !filepath.IsAbs(effect.ManagedTarget) {
			return effects, errors.New("config adoption effect mismatch")
		}
		if effect.Changed == (effect.BeforeHash == effect.AfterHash) {
			return effects, errors.New("config adoption change marker mismatch")
		}
		if intent.Kind == store.AdoptionRuntimeStdio && effects.Shim != nil && filepath.Clean(effect.ManagedTarget) != filepath.Clean(effects.Shim.Path) {
			return effects, errors.New("stdio config does not target its shim")
		}
	}
	return effects, nil
}

func finalAdoptionEffectState(ctx context.Context, objects *objectstore.Store, plan store.AdoptionJournalPlan, effects adoptionEffects) (adoptionEffectState, error) {
	// An unchanged config is a validation guard, not an applied endpoint. In a
	// stdio adoption the shim is therefore the final observable mutation; in a
	// hook adoption the generation pointer itself is the only mutation.
	if effects.Config != nil && effects.Config.Changed {
		return configEffectState(*effects.Config)
	}
	if effects.Install != nil {
		return installEffectState(ctx, objects, *effects.Install)
	}
	if effects.Shim != nil {
		return shimEffectState(*effects.Shim)
	}
	current, err := activation.Current(plan.Root)
	if errors.Is(err, activation.ErrNoCurrent) {
		return effectBefore, nil
	}
	if err != nil {
		return effectBefore, err
	}
	if current != plan.Commit.Generation.ID {
		return effectBefore, errors.Join(errAdoptionRecoveryConflict, fmt.Errorf("current points to %q", current))
	}
	return effectAfter, nil
}

func ensureAdoptionEffectsAfter(ctx context.Context, objects *objectstore.Store, effects adoptionEffects) error {
	if effects.Install != nil {
		state, err := installEffectState(ctx, objects, *effects.Install)
		if err != nil || state != effectAfter {
			return errors.Join(errAdoptionRecoveryConflict, err, errors.New("install endpoint is not fully switched"))
		}
	}
	if effects.Shim != nil {
		state, err := shimEffectState(*effects.Shim)
		if err != nil || state != effectAfter {
			return errors.Join(errAdoptionRecoveryConflict, err, errors.New("shim endpoint is not fully switched"))
		}
	}
	if effects.Config != nil {
		state, err := configEffectState(*effects.Config)
		if err != nil || state != effectAfter {
			return errors.Join(errAdoptionRecoveryConflict, err, errors.New("config endpoint is not fully switched"))
		}
	}
	return nil
}

func rollbackAdoptionEffects(ctx context.Context, objects *objectstore.Store, effects adoptionEffects) error {
	if effects.Config != nil {
		state, err := configEffectState(*effects.Config)
		if err != nil {
			return err
		}
		if effects.Config.Changed && state != effectBefore {
			return errors.Join(errAdoptionRecoveryConflict, errors.New("config no longer matches the pre-adoption state"))
		}
	}
	if effects.Shim != nil {
		if err := rollbackShimEffect(*effects.Shim); err != nil {
			return err
		}
	}
	if effects.Install != nil {
		if err := rollbackInstallEffect(ctx, objects, *effects.Install); err != nil {
			return err
		}
	}
	return nil
}

func ensureAdoptionCurrentAfter(plan store.AdoptionJournalPlan) error {
	current, err := activation.Current(plan.Root)
	if errors.Is(err, activation.ErrNoCurrent) {
		if err := verifyAdoptionGeneration(plan); err != nil {
			return err
		}
		return activation.SwitchCurrent(plan.Root, plan.Commit.Generation.ID)
	}
	if err != nil {
		return err
	}
	if current != plan.Commit.Generation.ID {
		return errors.Join(errAdoptionRecoveryConflict, fmt.Errorf("current points to %q", current))
	}
	return verifyAdoptionGeneration(plan)
}

func verifyAdoptionGeneration(plan store.AdoptionJournalPlan) error {
	var actual string
	var err error
	if plan.Runtime {
		actual, err = activation.HashRuntimeGeneration(plan.GenerationPath)
	} else {
		actual, err = activation.HashGeneration(plan.GenerationPath)
	}
	if err != nil {
		return errors.Join(errAdoptionRecoveryConflict, err)
	}
	if actual != plan.GenerationHash {
		return errors.Join(errAdoptionRecoveryConflict, errors.New("adoption generation hash mismatch"))
	}
	return nil
}

func rollbackAdoptionCurrent(plan store.AdoptionJournalPlan) error {
	current, err := activation.Current(plan.Root)
	if errors.Is(err, activation.ErrNoCurrent) {
		return nil
	}
	if err != nil {
		return err
	}
	if current != plan.Commit.Generation.ID {
		return errors.Join(errAdoptionRecoveryConflict, fmt.Errorf("current points to %q", current))
	}
	return activation.ClearCurrent(plan.Root)
}

func installEffectState(ctx context.Context, objects *objectstore.Store, effect adoptionInstallEffect) (adoptionEffectState, error) {
	if err := validateInstallTemp(effect); err != nil {
		return effectBefore, err
	}
	pathInfo, pathErr := os.Lstat(effect.Path)
	backupInfo, backupErr := os.Lstat(effect.BackupPath)
	if pathErr == nil && pathInfo.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(effect.Path)
		if err != nil || filepath.Clean(target) != filepath.Clean(effect.Target) || backupErr != nil || !backupInfo.IsDir() || backupInfo.Mode()&os.ModeSymlink != 0 {
			return effectBefore, errors.Join(errAdoptionRecoveryConflict, err, errors.New("managed install symlink or backup changed"))
		}
		if err := verifyDirectoryFingerprint(ctx, objects, effect.BackupPath, effect.BeforeTreeHash); err != nil {
			return effectBefore, err
		}
		return effectAfter, nil
	}
	if errors.Is(pathErr, fs.ErrNotExist) && backupErr == nil && backupInfo.IsDir() && backupInfo.Mode()&os.ModeSymlink == 0 {
		if err := verifyDirectoryFingerprint(ctx, objects, effect.BackupPath, effect.BeforeTreeHash); err != nil {
			return effectBefore, err
		}
		return effectPartial, nil
	}
	if pathErr == nil && pathInfo.IsDir() && pathInfo.Mode()&os.ModeSymlink == 0 && errors.Is(backupErr, fs.ErrNotExist) {
		if err := verifyDirectoryFingerprint(ctx, objects, effect.Path, effect.BeforeTreeHash); err != nil {
			return effectBefore, err
		}
		return effectBefore, nil
	}
	return effectBefore, errors.Join(errAdoptionRecoveryConflict, errors.New("install endpoint is neither before nor after"))
}

func rollbackInstallEffect(ctx context.Context, objects *objectstore.Store, effect adoptionInstallEffect) error {
	state, err := installEffectState(ctx, objects, effect)
	if err != nil {
		return err
	}
	if err := removeInstallTemp(effect); err != nil {
		return err
	}
	if state == effectBefore {
		return nil
	}
	if state == effectAfter {
		if err := os.Remove(effect.Path); err != nil {
			return err
		}
	}
	if err := os.Rename(effect.BackupPath, effect.Path); err != nil {
		return err
	}
	return safeio.SyncDir(filepath.Dir(effect.Path))
}

func validateInstallTemp(effect adoptionInstallEffect) error {
	info, err := os.Lstat(effect.TempPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return errors.Join(errAdoptionRecoveryConflict, err, errors.New("install temporary path changed"))
	}
	target, err := os.Readlink(effect.TempPath)
	if err != nil || filepath.Clean(target) != filepath.Clean(effect.Target) {
		return errors.Join(errAdoptionRecoveryConflict, err, errors.New("install temporary symlink changed"))
	}
	return nil
}

func removeInstallTemp(effect adoptionInstallEffect) error {
	if err := validateInstallTemp(effect); err != nil {
		return err
	}
	if err := os.Remove(effect.TempPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func shimEffectState(effect adoptionShimEffect) (adoptionEffectState, error) {
	pathHash, pathExists, err := regularFileHash(effect.Path)
	if err != nil {
		return effectBefore, err
	}
	backupHash, backupExists, err := regularFileHash(effect.BackupPath)
	if err != nil && effect.BackupPath != "" {
		return effectBefore, err
	}
	if pathExists && pathHash == effect.AfterHash {
		if effect.BeforeExists && (!backupExists || backupHash != effect.BeforeHash) {
			return effectBefore, errors.Join(errAdoptionRecoveryConflict, errors.New("native shim backup changed"))
		}
		if !effect.BeforeExists && backupExists {
			return effectBefore, errors.Join(errAdoptionRecoveryConflict, errors.New("unexpected shim backup exists"))
		}
		return effectAfter, nil
	}
	if effect.BeforeExists {
		if pathExists && pathHash == effect.BeforeHash && !backupExists {
			return effectBefore, nil
		}
		if !pathExists && backupExists && backupHash == effect.BeforeHash {
			return effectPartial, nil
		}
	} else if !pathExists && !backupExists {
		return effectBefore, nil
	}
	return effectBefore, errors.Join(errAdoptionRecoveryConflict, errors.New("shim endpoint is neither before nor after"))
}

func rollbackShimEffect(effect adoptionShimEffect) error {
	state, err := shimEffectState(effect)
	if err != nil {
		return err
	}
	if state == effectBefore {
		return nil
	}
	if state == effectAfter {
		if err := os.Remove(effect.Path); err != nil {
			return err
		}
	}
	if effect.BeforeExists {
		if err := os.Rename(effect.BackupPath, effect.Path); err != nil {
			return err
		}
	}
	return safeio.SyncDir(filepath.Dir(effect.Path))
}

func configEffectState(effect adoptionConfigEffect) (adoptionEffectState, error) {
	hash, exists, err := regularFileHash(effect.Path)
	if err != nil {
		return effectBefore, err
	}
	if !exists {
		return effectBefore, errors.Join(errAdoptionRecoveryConflict, errors.New("host config disappeared"))
	}
	if !effect.Changed && hash == effect.AfterHash && effect.BeforeHash == effect.AfterHash {
		return effectAfter, nil
	}
	switch hash {
	case effect.BeforeHash:
		return effectBefore, nil
	case effect.AfterHash:
		return effectAfter, nil
	default:
		return effectBefore, errors.Join(errAdoptionRecoveryConflict, errors.New("host config was edited during adoption"))
	}
}

func verifyDirectoryFingerprint(ctx context.Context, objects *objectstore.Store, path, expected string) error {
	actual, err := objects.FingerprintTree(ctx, path, objectstore.CaptureOptions{})
	if err != nil {
		return errors.Join(errAdoptionRecoveryConflict, err)
	}
	if actual != expected {
		return errors.Join(errAdoptionRecoveryConflict, errors.New("adoption directory backup hash mismatch"))
	}
	return nil
}

func regularFileHash(path string) (string, bool, error) {
	if path == "" {
		return "", false, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", true, errors.Join(errAdoptionRecoveryConflict, fmt.Errorf("%s is not a regular file", path))
	}
	file, err := os.Open(path)
	if err != nil {
		return "", true, err
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", true, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), true, nil
}

func adoptionBackupPath(path, intentID string) string {
	return filepath.Clean(path) + ".tooltend-adopt-" + intentID + ".backup"
}

func adoptionInstallTempPath(path, intentID string) string {
	return filepath.Join(filepath.Dir(path), ".tooltend-adopt-"+intentID+".link")
}

func planShimEffect(path string, content []byte, nativePath, intentID string) (adoptionShimEffect, error) {
	effect := adoptionShimEffect{Path: filepath.Clean(path)}
	digest := sha256.Sum256(content)
	effect.AfterHash = hex.EncodeToString(digest[:])
	hash, exists, err := regularFileHash(path)
	if err != nil {
		return effect, err
	}
	if !exists {
		return effect, nil
	}
	if nativePath == "" || filepath.Clean(nativePath) != filepath.Clean(path) {
		return effect, fmt.Errorf("lifecycle: refusing to replace unrelated existing shim path %s", path)
	}
	effect.BeforeExists = true
	effect.BeforeHash = hash
	effect.BackupPath = adoptionBackupPath(path, intentID)
	if _, err := os.Lstat(effect.BackupPath); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			return effect, errors.New("lifecycle: native executable adoption backup already exists")
		}
		return effect, err
	}
	return effect, nil
}

func applyShimEffect(effect adoptionShimEffect, content []byte) error {
	if effect.BeforeExists {
		if err := os.Rename(effect.Path, effect.BackupPath); err != nil {
			return err
		}
		if err := safeio.SyncDir(filepath.Dir(effect.Path)); err != nil {
			return err
		}
	}
	if err := safeio.AtomicWriteFile(effect.Path, content, 0o755); err != nil {
		return err
	}
	hash, exists, err := regularFileHash(effect.Path)
	if err != nil || !exists || hash != effect.AfterHash {
		return errors.Join(err, errors.New("lifecycle: managed shim hash mismatch after write"))
	}
	return nil
}

func applyInstallEffect(effect adoptionInstallEffect) error {
	if err := os.Symlink(effect.Target, effect.TempPath); err != nil {
		return err
	}
	if err := os.Rename(effect.Path, effect.BackupPath); err != nil {
		return err
	}
	if err := os.Rename(effect.TempPath, effect.Path); err != nil {
		return err
	}
	return safeio.SyncDir(filepath.Dir(effect.Path))
}

func marshalAdoptionEffects(effects adoptionEffects) (json.RawMessage, error) {
	encoded, err := json.Marshal(effects)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}
