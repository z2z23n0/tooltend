package lockfile

import (
	"errors"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTryIsNonBlocking(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("unix lock test")
	}
	path := filepath.Join(t.TempDir(), "activation.lock")
	first, err := Try(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := Try(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("got %v", err)
	}
}
