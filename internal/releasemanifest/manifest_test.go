package releasemanifest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/z2z23n0/tooltend/internal/selfupdate"
)

func TestGenerateProducesVerifiableFourPlatformManifest(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, name := range []string{"tooltend-darwin-arm64", "tooltend-darwin-amd64", "tooltend-linux-arm64", "tooltend-linux-amd64"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("binary-"+name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Generate(Options{Version: "v0.2.0", Repository: "z2z23n0/tooltend", AssetsDir: dir, PrivateKey: privateKey, PublishedAt: time.Unix(1, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if result.PublicKey != hex.EncodeToString(publicKey) {
		t.Fatal("generated public key does not match signer")
	}
	verifier := selfupdate.Verifier{Keys: map[string]ed25519.PublicKey{KeyID: publicKey}, OS: "darwin", Arch: "arm64"}
	verified, err := verifier.Verify(result.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Manifest.Version != "0.2.0" || len(verified.Manifest.Assets) != 4 {
		t.Fatalf("unexpected manifest: %#v", verified.Manifest)
	}
	tampered := append([]byte(nil), result.Envelope...)
	tampered[len(tampered)/2] ^= 1
	if _, err := verifier.Verify(tampered); err == nil {
		t.Fatal("tampered manifest was accepted")
	}
}

func TestSequenceFollowsStableSemver(t *testing.T) {
	versions := []string{"0.1.999999", "0.2.0", "1.0.0", "1.0.1"}
	var previous uint64
	for _, raw := range versions {
		version := semver.MustParse(raw)
		if err := ValidateSequenceVersion(version); err != nil {
			t.Fatal(err)
		}
		current := Sequence(version)
		if current <= previous {
			t.Fatalf("release sequence does not preserve semver ordering: %s = %d after %d", raw, current, previous)
		}
		previous = current
	}
	if err := ValidateSequenceVersion(semver.MustParse("0.1000000.0")); err == nil {
		t.Fatal("oversized minor version was accepted")
	}
}
