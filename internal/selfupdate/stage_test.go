package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

type staticFetcher []byte

func (f staticFetcher) Fetch(context.Context, string, int64) ([]byte, error) {
	return append([]byte(nil), f...), nil
}

func TestStageRejectsTamperedAssetBeforeWritingPendingBinary(t *testing.T) {
	want := []byte("signed release binary")
	hash := sha256.Sum256(want)
	verified := Verified{
		Manifest: Manifest{Version: "1.0.0"},
		Asset:    Asset{URL: "https://example.test/tooltend", SHA256: hex.EncodeToString(hash[:]), Size: int64(len(want))},
	}
	state := t.TempDir()
	tampered := append([]byte(nil), want...)
	tampered[0] ^= 1
	if _, err := Stage(context.Background(), staticFetcher(tampered), verified, state); err == nil {
		t.Fatal("expected tampered release to be rejected")
	}
	if _, err := os.Stat(filepath.Join(state, "self-update")); !os.IsNotExist(err) {
		t.Fatalf("tampered release reached staging: %v", err)
	}
}
