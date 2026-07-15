package releasemanifest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/z2z23n0/tooltend/internal/selfupdate"
)

const KeyID = "tooltend-release-v1"

type Options struct {
	Version     string
	Repository  string
	AssetsDir   string
	PrivateKey  ed25519.PrivateKey
	PublishedAt time.Time
}

type Output struct {
	Envelope  []byte
	Checksums []byte
	PublicKey string
}

func Generate(options Options) (Output, error) {
	version := strings.TrimPrefix(strings.TrimSpace(options.Version), "v")
	parsed, err := semver.StrictNewVersion(version)
	if err != nil || parsed.Prerelease() != "" || parsed.Metadata() != "" {
		return Output{}, errors.New("release manifest: version must be a stable semantic version")
	}
	if err := ValidateSequenceVersion(parsed); err != nil {
		return Output{}, err
	}
	if options.Repository == "" || strings.ContainsAny(options.Repository, "\x00\r\n") || strings.Count(options.Repository, "/") != 1 {
		return Output{}, errors.New("release manifest: repository must be owner/name")
	}
	if !filepath.IsAbs(options.AssetsDir) {
		return Output{}, errors.New("release manifest: assets directory must be absolute")
	}
	if len(options.PrivateKey) != ed25519.PrivateKeySize {
		return Output{}, errors.New("release manifest: invalid Ed25519 private key")
	}
	published := options.PublishedAt.UTC()
	if published.IsZero() {
		published = time.Now().UTC()
	}
	platforms := []struct{ os, arch string }{{"darwin", "arm64"}, {"darwin", "amd64"}, {"linux", "arm64"}, {"linux", "amd64"}}
	assets := make([]selfupdate.Asset, 0, len(platforms))
	checksums := map[string]string{}
	for _, platform := range platforms {
		name := fmt.Sprintf("tooltend-%s-%s", platform.os, platform.arch)
		data, err := os.ReadFile(filepath.Join(options.AssetsDir, name))
		if err != nil {
			return Output{}, fmt.Errorf("release manifest: read %s: %w", name, err)
		}
		if len(data) == 0 {
			return Output{}, fmt.Errorf("release manifest: asset %s is empty", name)
		}
		digest := sha256.Sum256(data)
		hash := hex.EncodeToString(digest[:])
		checksums[name] = hash
		assets = append(assets, selfupdate.Asset{
			OS: platform.os, Arch: platform.arch,
			URL:    fmt.Sprintf("https://github.com/%s/releases/download/v%s/%s", options.Repository, version, name),
			SHA256: hash, Size: int64(len(data)),
		})
	}
	manifest := selfupdate.Manifest{SchemaVersion: 1, Sequence: Sequence(parsed), Version: version, PublishedAt: published, Assets: assets}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return Output{}, err
	}
	envelope := selfupdate.Envelope{KeyID: KeyID, Manifest: manifestJSON, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(options.PrivateKey, manifestJSON))}
	envelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		return Output{}, err
	}
	envelopeJSON = append(envelopeJSON, '\n')
	manifestDigest := sha256.Sum256(envelopeJSON)
	checksums["tooltend-manifest.json"] = hex.EncodeToString(manifestDigest[:])
	names := make([]string, 0, len(checksums))
	for name := range checksums {
		names = append(names, name)
	}
	sort.Strings(names)
	var checksumText strings.Builder
	for _, name := range names {
		fmt.Fprintf(&checksumText, "%s  %s\n", checksums[name], name)
	}
	publicKey := options.PrivateKey.Public().(ed25519.PublicKey)
	return Output{Envelope: envelopeJSON, Checksums: []byte(checksumText.String()), PublicKey: hex.EncodeToString(publicKey)}, nil
}

func Sequence(version *semver.Version) uint64 {
	// Keep stable SemVer ordering deterministic while leaving one million
	// patch slots per minor and one million minor slots per major.
	return version.Major()*1_000_000_000_000 + version.Minor()*1_000_000 + version.Patch() + 1
}

func ValidateSequenceVersion(version *semver.Version) error {
	const slots = uint64(1_000_000)
	if version.Minor() >= slots || version.Patch() >= slots {
		return errors.New("release manifest: minor and patch versions must be below 1000000")
	}
	tail := version.Minor()*slots + version.Patch() + 1
	if version.Major() > (^uint64(0)-tail)/(slots*slots) {
		return errors.New("release manifest: version exceeds release sequence range")
	}
	return nil
}

func DecodePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, errors.New("release manifest: private key is not valid base64")
	}
	switch len(data) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(data), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(data), nil
	default:
		return nil, errors.New("release manifest: private key must be a 32-byte seed or 64-byte private key")
	}
}
