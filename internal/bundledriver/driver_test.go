package bundledriver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/z2z23n0/tooltend/internal/safeio"
)

func TestSelectReleaseAssetsSupportsGoAndRustNames(t *testing.T) {
	tests := []struct {
		name   string
		asset  string
		goos   string
		goarch string
	}{
		{name: "go", asset: "mainline_0.5.0_darwin_arm64.tar.gz", goos: "darwin", goarch: "arm64"},
		{name: "rust", asset: "xsearch-x86_64-unknown-linux-gnu.tar.gz", goos: "linux", goarch: "amd64"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected, checksums, err := selectReleaseAssets([]githubAsset{
				{Name: test.asset, URL: "https://github.com/example/repo/releases/download/v1/" + test.asset, Size: 10},
				{Name: "checksums.txt", URL: "https://github.com/example/repo/releases/download/v1/checksums.txt", Size: 10},
			}, test.goos, test.goarch)
			if err != nil {
				t.Fatal(err)
			}
			if selected.Name != test.asset || checksums.Name != "checksums.txt" {
				t.Fatalf("selected = %#v, checksums = %#v", selected, checksums)
			}
		})
	}
}

func TestVerifyChecksumRejectsTampering(t *testing.T) {
	data := []byte("release")
	digest := sha256.Sum256(data)
	checksums := []byte(hex.EncodeToString(digest[:]) + "  tool.tar.gz\n")
	if err := verifyChecksum("tool.tar.gz", data, checksums); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksum("tool.tar.gz", []byte("tampered"), checksums); err == nil {
		t.Fatal("expected tampered asset rejection")
	}
}

func TestGitSkillActivationDetachesAndCompensationRestoresSkillLock(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".agents", "skills", "mainline")
	stage := filepath.Join(home, "stage")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := safeio.AtomicWriteFile(filepath.Join(path, "SKILL.md"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(home, ".agents", ".skill-lock.json")
	lock := map[string]any{"version": 3, "skills": map[string]any{"mainline": map[string]any{"source": "mainline-org/mainline"}}, "dismissed": map[string]any{}}
	lockData, _ := json.Marshal(lock)
	if err := safeio.AtomicWriteFile(lockPath, lockData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stage, "next"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := safeio.AtomicWriteFile(filepath.Join(stage, "next", "SKILL.md"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := backupPath(path, filepath.Join(stage, "previous")); err != nil {
		t.Fatal(err)
	}
	if err := backupSkillLock(path, stage); err != nil {
		t.Fatal(err)
	}
	driver := Driver{}
	if err := driver.Execute(context.Background(), []string{"git-activate", stage, path}); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(path, "SKILL.md"), "new")
	updatedLock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(updatedLock) == string(lockData) {
		t.Fatal("npx skills lock entry was not detached")
	}
	if err := driver.Execute(context.Background(), []string{"git-rollback", "unused", ".", "", stage, path}); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(path, "SKILL.md"), "old")
	restoredLock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	var restored struct {
		Skills map[string]json.RawMessage `json:"skills"`
	}
	if json.Unmarshal(restoredLock, &restored) != nil || restored.Skills["mainline"] == nil {
		t.Fatal("npx skills lock was not restored during compensation")
	}
}

func TestMainlineHookBackupRestoresRemovedAndExistingFiles(t *testing.T) {
	project := t.TempDir()
	stage := filepath.Join(t.TempDir(), "stage")
	existing := filepath.Join(project, ".codex", "hooks.json")
	if err := safeio.AtomicWriteFile(existing, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := stageMainlineHooks(project, stage); err != nil {
		t.Fatal(err)
	}
	if err := safeio.AtomicWriteFile(existing, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(project, ".cursor", "hooks.json")
	if err := safeio.AtomicWriteFile(created, []byte("created"), 0o644); err != nil {
		t.Fatal(err)
	}
	restored, err := restoreMainlineHooks(project, stage)
	if err != nil || !restored {
		t.Fatalf("restored = %t, err = %v", restored, err)
	}
	assertFileContent(t, existing, "old")
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("new hook file still exists: %v", err)
	}
}

func assertFileContent(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != expected {
		t.Fatalf("%s = %q, want %q", path, data, expected)
	}
}
