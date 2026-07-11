package kick

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Result struct {
	Started       bool `json:"started"`
	AlreadyQueued bool `json:"already_queued"`
}

func Queue(executable, stateDir string, args ...string) (Result, error) {
	if executable == "" || stateDir == "" {
		return Result{}, errors.New("kick: executable and state directory are required")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return Result{}, err
	}
	marker := filepath.Join(stateDir, "kick.pending")
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		info, statErr := os.Stat(marker)
		if statErr == nil && time.Since(info.ModTime()) < 2*time.Minute {
			return Result{AlreadyQueued: true}, nil
		}
		if removeErr := os.Remove(marker); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return Result{}, removeErr
		}
		file, err = os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	}
	if err != nil {
		return Result{}, fmt.Errorf("kick: create pending marker: %w", err)
	}
	_, writeErr := fmt.Fprintf(file, "%d\n", time.Now().UTC().Unix())
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return Result{}, errors.Join(writeErr, closeErr)
	}
	if err := startDetached(executable, args...); err != nil {
		// Keep the marker. A later SessionStart retries it after the short stale
		// window, while the daily scheduler remains an independent fallback.
		return Result{}, fmt.Errorf("kick: start one-shot worker: %w", err)
	}
	return Result{Started: true}, nil
}

func Clear(stateDir string) error {
	err := os.Remove(filepath.Join(stateDir, "kick.pending"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
