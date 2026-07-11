package safeio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	if err := AtomicWriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "two" {
		t.Fatalf("got %q", got)
	}
}

func TestValidateRelative(t *testing.T) {
	for _, invalid := range []string{"", ".", "..", "../x", "/tmp/x"} {
		if ValidateRelative(invalid) == nil {
			t.Fatalf("expected %q to be rejected", invalid)
		}
	}
	if err := ValidateRelative(filepath.Join("a", "b")); err != nil {
		t.Fatal(err)
	}
}

func TestCopyTreeRejectsSymlink(t *testing.T) {
	src := t.TempDir()
	if err := os.Symlink("outside", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(src, filepath.Join(t.TempDir(), "dst")); err == nil {
		t.Fatal("expected symlink rejection")
	}
}
