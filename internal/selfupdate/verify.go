package selfupdate

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"time"
)

const MaxManifestBytes = 1 << 20

// ReleasePublicKeyHex is embedded into release binaries with -ldflags. A dev
// build deliberately has no update authority and therefore cannot stage a
// self-update.
var ReleasePublicKeyHex string

type Envelope struct {
	KeyID     string          `json:"key_id"`
	Manifest  json.RawMessage `json:"manifest"`
	Signature string          `json:"signature"`
}

type Manifest struct {
	SchemaVersion int       `json:"schema_version"`
	Sequence      uint64    `json:"sequence"`
	Version       string    `json:"version"`
	PublishedAt   time.Time `json:"published_at"`
	Assets        []Asset   `json:"assets"`
}

type Asset struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Verified struct {
	Manifest Manifest
	Asset    Asset
}

type Verifier struct {
	Keys            map[string]ed25519.PublicKey
	CurrentSequence uint64
	OS              string
	Arch            string
}

func EmbeddedVerifier(currentSequence uint64) (Verifier, error) {
	if ReleasePublicKeyHex == "" {
		return Verifier{}, errors.New("self-update public key is not embedded in this build")
	}
	key, err := hex.DecodeString(ReleasePublicKeyHex)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return Verifier{}, errors.New("embedded self-update public key is invalid")
	}
	return Verifier{
		Keys:            map[string]ed25519.PublicKey{"tooltend-release-v1": key},
		CurrentSequence: currentSequence,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
	}, nil
}

func (v Verifier) Verify(raw []byte) (Verified, error) {
	if len(raw) == 0 || len(raw) > MaxManifestBytes {
		return Verified{}, errors.New("signed manifest has invalid size")
	}
	if err := rejectDuplicateKeys(raw); err != nil {
		return Verified{}, err
	}
	var envelope Envelope
	if err := decodeStrict(raw, &envelope); err != nil {
		return Verified{}, fmt.Errorf("decode signed manifest: %w", err)
	}
	key, ok := v.Keys[envelope.KeyID]
	if !ok || len(key) != ed25519.PublicKeySize {
		return Verified{}, errors.New("untrusted self-update key id")
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return Verified{}, errors.New("invalid self-update signature encoding")
	}
	// Nothing inside Manifest is inspected before this point. In particular, a
	// remote URL cannot influence network access before the detached signature
	// has been validated against the embedded key.
	if !ed25519.Verify(key, envelope.Manifest, signature) {
		return Verified{}, errors.New("self-update signature verification failed")
	}
	if err := rejectDuplicateKeys(envelope.Manifest); err != nil {
		return Verified{}, err
	}
	var manifest Manifest
	if err := decodeStrict(envelope.Manifest, &manifest); err != nil {
		return Verified{}, fmt.Errorf("decode verified manifest: %w", err)
	}
	if manifest.SchemaVersion != 1 || manifest.Sequence == 0 || manifest.Version == "" {
		return Verified{}, errors.New("verified manifest is incomplete")
	}
	if manifest.Sequence <= v.CurrentSequence {
		return Verified{}, errors.New("self-update manifest is stale or replayed")
	}
	osName, arch := v.OS, v.Arch
	if osName == "" {
		osName = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	for _, asset := range manifest.Assets {
		if asset.OS == osName && asset.Arch == arch {
			if err := validateAsset(asset); err != nil {
				return Verified{}, err
			}
			return Verified{Manifest: manifest, Asset: asset}, nil
		}
	}
	return Verified{}, fmt.Errorf("no signed asset for %s/%s", osName, arch)
}

func validateAsset(asset Asset) error {
	if asset.Size <= 0 || asset.Size > 512<<20 {
		return errors.New("signed asset size is invalid")
	}
	digest, err := hex.DecodeString(asset.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return errors.New("signed asset sha256 is invalid")
	}
	parsed, err := url.Parse(asset.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return errors.New("signed asset URL must be an HTTPS URL without user info")
	}
	if parsed.Fragment != "" {
		return errors.New("signed asset URL must not contain a fragment")
	}
	return nil
}

func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing JSON data")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("trailing JSON value")
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("JSON object key is not a string")
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delim)
		}
	}
	if err := walk(); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing JSON data")
	}
	return nil
}

func VerifyAsset(data []byte, asset Asset) error {
	if int64(len(data)) != asset.Size {
		return errors.New("downloaded asset size does not match signed manifest")
	}
	hash := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(hash[:]), asset.SHA256) {
		return errors.New("downloaded asset sha256 does not match signed manifest")
	}
	return nil
}
