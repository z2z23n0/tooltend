package adapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/z2z23n0/tooltend/internal/execx"
)

type monorepoGitRunner struct{}

func (monorepoGitRunner) Run(_ context.Context, name string, args ...string) (execx.Result, error) {
	if name == "git" && len(args) >= 3 && args[0] == "-C" && args[2] == "checkout" {
		root := args[1]
		for path, content := range map[string]string{
			filepath.Join(root, "plugins", "demo", "plugin.txt"):   "selected\n",
			filepath.Join(root, "plugins", "other", "ignored.txt"): "ignored\n",
		} {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return execx.Result{}, err
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				return execx.Result{}, err
			}
		}
	}
	return execx.Result{}, nil
}

func TestGitFetchMaterializesOnlyCanonicalSubdir(t *testing.T) {
	provider := Git{Runner: monorepoGitRunner{}}
	source, err := provider.Normalize(Source{
		Kind: SourceGit, Locator: "https://example.test/monorepo.git", Subdir: "./plugins/demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if source.Subdir != "plugins/demo" {
		t.Fatalf("canonical subdir=%q", source.Subdir)
	}
	stage := filepath.Join(t.TempDir(), "stage")
	artifact, err := provider.Fetch(context.Background(), source, Resolved{Ref: "0123456789abcdef"}, stage)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(artifact.Root, "plugin.txt"))
	if err != nil || string(content) != "selected\n" {
		t.Fatalf("selected content=%q err=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(artifact.Root, "plugins", "other", "ignored.txt")); !os.IsNotExist(err) {
		t.Fatalf("sibling monorepo content leaked into artifact: %v", err)
	}
}
