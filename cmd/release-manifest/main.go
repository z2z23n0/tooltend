package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/z2z23n0/tooltend/internal/releasemanifest"
)

func main() {
	var version, repository, assetsDir, output, checksums, published string
	var publicOnly bool
	var sequenceOnly bool
	flag.StringVar(&version, "version", "", "stable semantic version")
	flag.StringVar(&repository, "repository", "z2z23n0/tooltend", "GitHub owner/name")
	flag.StringVar(&assetsDir, "assets-dir", "", "directory containing release binaries")
	flag.StringVar(&output, "output", "tooltend-manifest.json", "signed envelope output")
	flag.StringVar(&checksums, "checksums", "checksums.txt", "checksum output")
	flag.StringVar(&published, "published-at", "", "RFC3339 publication timestamp")
	flag.BoolVar(&publicOnly, "public-key", false, "print the release public key and exit")
	flag.BoolVar(&sequenceOnly, "sequence", false, "print the deterministic release sequence and exit")
	flag.Parse()
	if sequenceOnly {
		parsed, parseErr := semver.StrictNewVersion(strings.TrimPrefix(strings.TrimSpace(version), "v"))
		if parseErr != nil || parsed.Prerelease() != "" || parsed.Metadata() != "" {
			fatal(fmt.Errorf("--version must be a stable semantic version"))
		}
		if err := releasemanifest.ValidateSequenceVersion(parsed); err != nil {
			fatal(err)
		}
		fmt.Println(releasemanifest.Sequence(parsed))
		return
	}
	privateKey, err := releasemanifest.DecodePrivateKey(os.Getenv("TOOLTEND_RELEASE_PRIVATE_KEY_B64"))
	if err != nil {
		fatal(err)
	}
	if publicOnly {
		fmt.Println(hex.EncodeToString(privateKey.Public().(ed25519.PublicKey)))
		return
	}
	when := time.Now().UTC()
	if published != "" {
		when, err = time.Parse(time.RFC3339, published)
		if err != nil {
			fatal(err)
		}
	}
	if assetsDir == "" {
		fatal(fmt.Errorf("--assets-dir is required"))
	}
	assetsDir, err = filepath.Abs(assetsDir)
	if err != nil {
		fatal(err)
	}
	result, err := releasemanifest.Generate(releasemanifest.Options{Version: version, Repository: repository, AssetsDir: assetsDir, PrivateKey: privateKey, PublishedAt: when})
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(output, result.Envelope, 0o600); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(checksums, result.Checksums, 0o600); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "release-manifest:", err)
	os.Exit(1)
}
