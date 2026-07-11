package inventory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverProjectsUsesBoundedAgentIndexesWithoutWalkingHome(t *testing.T) {
	home := t.TempDir()
	current := filepath.Join(home, "work", "current")
	codexProject := filepath.Join(home, "work", "codex")
	claudeProject := filepath.Join(home, "work", "claude")
	for _, path := range []string{current, codexProject, claudeProject} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	codexConfig := `[projects."` + codexProject + `"]
trust_level = "trusted"
`
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(codexConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeState := `{"projects":{"` + claudeProject + `":{"allowedTools":[]}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claudeState), 0o600); err != nil {
		t.Fatal(err)
	}
	candidates := DiscoverProjects(home, current, 20)
	if len(candidates) != 3 {
		t.Fatalf("candidates=%#v", candidates)
	}
	if candidates[0].Path != current || !candidates[0].Current {
		t.Fatalf("current candidate=%#v", candidates[0])
	}
	sources := map[string]bool{}
	for _, candidate := range candidates {
		sources[candidate.Source] = true
	}
	if !sources["codex_config"] || !sources["claude_state"] {
		t.Fatalf("sources=%#v", sources)
	}
}
