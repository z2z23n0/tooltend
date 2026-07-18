package notify

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDarwinBundleMetadataMatchesConstants(t *testing.T) {
	if !strings.Contains(darwinInfoPlist, "<string>"+DarwinBundleID+"</string>") {
		t.Fatalf("Info.plist does not contain bundle id %q", DarwinBundleID)
	}
	if len(darwinNotifierSource) == 0 || !strings.Contains(string(darwinNotifierSource), "UNUserNotificationCenter") {
		t.Fatal("native notifier source is missing")
	}
}

func TestCheckDarwinAuthorizationRunsInstalledHelper(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only notifier")
	}
	home := t.TempDir()
	executable := DarwinNotifierExecutable(home)
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("helper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(DarwinAppPath(home), "Contents", "Info.plist"), []byte("<string>"+DarwinBundleID+"</string>"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	if err := CheckDarwinAuthorization(context.Background(), home, runner); err != nil {
		t.Fatal(err)
	}
	if runner.name != executable || len(runner.args) != 1 || runner.args[0] != "--check" {
		t.Fatalf("authorization check = %s %#v", runner.name, runner.args)
	}
}
