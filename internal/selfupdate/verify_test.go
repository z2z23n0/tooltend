package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func signedEnvelope(t *testing.T, manifest []byte) ([]byte, ed25519.PublicKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	envelope := Envelope{KeyID: "test", Manifest: manifest, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifest))}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return raw, publicKey
}

func TestVerifierChecksSignatureBeforeAsset(t *testing.T) {
	assetData := []byte("binary")
	hash := sha256.Sum256(assetData)
	manifest, _ := json.Marshal(Manifest{
		SchemaVersion: 1,
		Sequence:      2,
		Version:       "1.0.0",
		PublishedAt:   time.Now().UTC(),
		Assets:        []Asset{{OS: "test-os", Arch: "test-arch", URL: "https://example.com/tooltend", SHA256: hex.EncodeToString(hash[:]), Size: int64(len(assetData))}},
	})
	raw, key := signedEnvelope(t, manifest)
	verifier := Verifier{Keys: map[string]ed25519.PublicKey{"test": key}, CurrentSequence: 1, OS: "test-os", Arch: "test-arch"}
	verified, err := verifier.Verify(raw)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Manifest.Version != "1.0.0" || VerifyAsset(assetData, verified.Asset) != nil {
		t.Fatalf("invalid verified result: %#v", verified)
	}
	raw[len(raw)/2] ^= 1
	if _, err := verifier.Verify(raw); err == nil {
		t.Fatal("expected tampered envelope to fail")
	}
}

func TestVerifierRejectsReplayAndDuplicateKeys(t *testing.T) {
	if err := rejectDuplicateKeys([]byte(`{"key_id":"a","key_id":"b","manifest":{},"signature":"x"}`)); err == nil {
		t.Fatal("expected duplicate key rejection")
	}
	manifest := []byte(`{"schema_version":1,"sequence":1,"version":"1","published_at":"2026-01-01T00:00:00Z","assets":[]}`)
	raw, key := signedEnvelope(t, manifest)
	verifier := Verifier{Keys: map[string]ed25519.PublicKey{"test": key}, CurrentSequence: 1, OS: "x", Arch: "y"}
	if _, err := verifier.Verify(raw); err == nil {
		t.Fatal("expected replay rejection")
	}
}

func TestEmbeddedSignatureCapabilityDoesNotExposeKeyMaterial(t *testing.T) {
	original := ReleasePublicKeyHex
	t.Cleanup(func() { ReleasePublicKeyHex = original })
	ReleasePublicKeyHex = ""
	if value := EmbeddedSignatureCapability(); value.Embedded || value.Valid || value.KeyID == "" {
		t.Fatalf("development capability = %+v", value)
	}
	ReleasePublicKeyHex = strings.Repeat("ab", ed25519.PublicKeySize)
	if value := EmbeddedSignatureCapability(); !value.Embedded || !value.Valid {
		t.Fatalf("release capability = %+v", value)
	}
}
