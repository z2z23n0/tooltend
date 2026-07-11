package activation

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHashRuntimeGenerationIgnoresOnlyGeneratedCaches(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "lib", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(root, "lib", "pkg", "main.py")
	if err := os.WriteFile(module, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := HashRuntimeGeneration(root)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "lib", "pkg", "__pycache__")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "main.cpython-312.pyc"), []byte("mutable bytecode"), 0o644); err != nil {
		t.Fatal(err)
	}
	afterCache, err := HashRuntimeGeneration(root)
	if err != nil {
		t.Fatal(err)
	}
	if afterCache != before {
		t.Fatalf("runtime cache changed integrity: before=%s after=%s", before, afterCache)
	}
	if err := os.WriteFile(module, []byte("print('changed')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	afterSource, err := HashRuntimeGeneration(root)
	if err != nil {
		t.Fatal(err)
	}
	if afterSource == before {
		t.Fatal("immutable runtime file change was ignored")
	}
}
