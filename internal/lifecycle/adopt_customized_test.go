package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/z2z23n0/tooltend/internal/adapter"
	"github.com/z2z23n0/tooltend/internal/model"
)

func TestCustomizedExactAdoptionKeepsVerifiedBaselineAndLocalOverlay(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "local.txt"), []byte("keep local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyAuto)
	adopted, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-plugin", Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !adopted.Baseline {
		t.Fatalf("adoption=%+v", adopted)
	}
	binding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil || binding.Classification != model.ClassificationCustomized {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	baseline, err := fixture.database.LatestBaseline(context.Background(), binding.ID)
	if err != nil || baseline.TreeHash == adopted.TreeHash {
		t.Fatalf("baseline=%+v adopted=%+v err=%v", baseline, adopted, err)
	}

	fixture.provider.latest = "v2"
	updated, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Activated {
		t.Fatalf("update=%+v", updated)
	}
	if content, err := os.ReadFile(filepath.Join(fixture.installPath, "plugin.txt")); err != nil || string(content) != "v2\n" {
		t.Fatalf("upstream file=%q err=%v", content, err)
	}
	if content, err := os.ReadFile(filepath.Join(fixture.installPath, "local.txt")); err != nil || string(content) != "keep local\n" {
		t.Fatalf("local overlay=%q err=%v", content, err)
	}
}
