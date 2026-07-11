package objectstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureVerifyMaterializeTree(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "readme.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := New(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	hash, manifest, err := store.CaptureTree(context.Background(), root, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 3 {
		t.Fatalf("entries = %d", len(manifest.Entries))
	}
	if err := store.VerifyTree(hash); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "generation")
	if err := store.MaterializeTree(context.Background(), hash, dest); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "readme.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("content = %q", b)
	}
	info, err := os.Stat(filepath.Join(dest, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestCaptureRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("../../secret", filepath.Join(root, "bad")); err != nil {
		t.Fatal(err)
	}
	store, _ := New(filepath.Join(t.TempDir(), "objects"))
	if _, _, err := store.CaptureTree(context.Background(), root, CaptureOptions{}); err == nil {
		t.Fatal("expected unsafe symlink rejection")
	}
}

func TestNewDoesNotCreateRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	if _, err := New(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("New created root: %v", err)
	}
}

func TestFingerprintTreeMatchesCaptureWithoutWriting(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	objects := filepath.Join(t.TempDir(), "objects")
	store, err := New(objects)
	if err != nil {
		t.Fatal(err)
	}
	previewHash, err := store.FingerprintTree(context.Background(), root, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(objects); !os.IsNotExist(err) {
		t.Fatalf("fingerprint created object store: %v", err)
	}
	capturedHash, _, err := store.CaptureTree(context.Background(), root, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if previewHash != capturedHash {
		t.Fatalf("preview=%s captured=%s", previewHash, capturedHash)
	}
}
