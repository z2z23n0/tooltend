package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

func TestDiscoverDeduplicatesPhysicalInstallAndReadsPackageMetadata(t *testing.T) {
	database, err := store.OpenRW(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	packageRoot := filepath.Join(t.TempDir(), "node_modules", "@it", "oa-skills")
	if err := os.MkdirAll(filepath.Join(packageRoot, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "package.json"), []byte(`{"name":"@it/oa-skills","version":"1.2.3"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(packageRoot, "bin", "oa-skills")
	if err := os.WriteFile(physical, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkOne := filepath.Join(t.TempDir(), "oa-skills")
	linkTwo := filepath.Join(t.TempDir(), "oa-skills")
	if err := os.Symlink(physical, linkOne); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physical, linkTwo); err != nil {
		t.Fatal(err)
	}
	source := model.Source{ID: "source", Kind: model.SourceNPM, Locator: "https://registry.npmjs.org/@it/oa-skills", PackageName: "@it/oa-skills", IdentityHash: "source-hash", CreatedAt: now, UpdatedAt: now}
	if err := database.UpsertSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	component := model.LogicalComponent{ID: "component", Kind: model.ComponentCLI, Name: "@it/oa-skills", SourceID: source.ID, LogicalKey: "oa-skills", CreatedAt: now, UpdatedAt: now}
	if err := database.UpsertComponent(ctx, component); err != nil {
		t.Fatal(err)
	}
	for _, value := range []model.Binding{
		{ID: "codex", ComponentID: component.ID, Host: model.HostCodex, Scope: model.ScopeGlobal, InstallPath: linkOne, ObservedVersion: "latest", Classification: model.ClassificationClean, LastSeenAt: now},
		{ID: "claude", ComponentID: component.ID, Host: model.HostClaude, Scope: model.ScopeGlobal, InstallPath: linkTwo, ObservedVersion: "latest", Classification: model.ClassificationClean, LastSeenAt: now},
	} {
		if err := database.UpsertBinding(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Discover(ctx, database, DiscoverOptions{HomeDir: t.TempDir(), Executable: "/missing/tooltend", LookupPath: func(command string) (string, error) {
		if command == "oa-skills" {
			return linkOne, nil
		}
		return "", os.ErrNotExist
	}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if result.Bundles == 0 {
		t.Fatal("no bundle discovered")
	}
	bundleValue, err := database.GetBundleBySlug(ctx, "citadel")
	if err != nil {
		t.Fatal(err)
	}
	installations, err := database.ListInstallations(ctx, bundleValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(installations) != 1 || installations[0].ObservedVersion != "1.2.3" || installations[0].PackageIdentity != "@it/oa-skills" {
		t.Fatalf("installations = %#v", installations)
	}
	consumers, err := database.ListConsumerBindings(ctx, installations[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(consumers) != 2 {
		t.Fatalf("consumers = %#v", consumers)
	}
}

func TestDiscoverMainlineHooksUsesSelectedRepositories(t *testing.T) {
	database, err := store.OpenRW(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	selected := t.TempDir()
	ignored := t.TempDir()
	for _, root := range []string{selected, ignored} {
		if err := os.MkdirAll(filepath.Join(root, ".mainline"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".mainline", "config.toml"), []byte("[hooks]\nenabled=true\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if err := database.UpsertProject(ctx, model.Project{ID: "selected", RootPath: selected, RootFingerprint: "selected", Selected: true, DiscoveredVia: "test", LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertProject(ctx, model.Project{ID: "ignored", RootPath: ignored, RootFingerprint: "ignored", Selected: false, DiscoveredVia: "test", LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	matches, err := discoverMainlineHooks(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].path != selected || matches[0].packageIdentity != "mainline-hooks" {
		t.Fatalf("matches = %#v", matches)
	}
}

func TestDiscoverPrunesStaleProbeWhenBindingProvidesRicherEvidence(t *testing.T) {
	database, err := store.OpenRW(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	firstSeen := time.Now().UTC().Add(-time.Minute)
	packageRoot := filepath.Join(t.TempDir(), "node_modules", "@it", "oa-skills")
	if err := os.MkdirAll(filepath.Join(packageRoot, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "package.json"), []byte(`{"name":"@it/oa-skills","version":"2.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(packageRoot, "bin", "oa-skills")
	if err := os.WriteFile(physical, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	lookup := func(command string) (string, error) {
		if command == "oa-skills" {
			return physical, nil
		}
		return "", os.ErrNotExist
	}
	if _, err := Discover(ctx, database, DiscoverOptions{HomeDir: t.TempDir(), Executable: "/missing/tooltend", LookupPath: lookup, Now: func() time.Time { return firstSeen }}); err != nil {
		t.Fatal(err)
	}
	bundleValue, err := database.GetBundleBySlug(ctx, "citadel")
	if err != nil {
		t.Fatal(err)
	}
	installations, err := database.ListInstallations(ctx, bundleValue.ID)
	if err != nil || len(installations) != 1 || installations[0].SourceIdentity != "" || installations[0].PackageIdentity != "@it/oa-skills" || installations[0].ObservedVersion != "2.0.0" {
		t.Fatalf("probe installations = %#v err=%v", installations, err)
	}

	now := firstSeen.Add(time.Minute)
	source := model.Source{ID: "source", Kind: model.SourceNPM, Locator: "https://registry.npmjs.org/@it/oa-skills", PackageName: "@it/oa-skills", IdentityHash: "source-hash", CreatedAt: now, UpdatedAt: now}
	if err := database.UpsertSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	component := model.LogicalComponent{ID: "component", Kind: model.ComponentCLI, Name: "@it/oa-skills", SourceID: source.ID, LogicalKey: "oa-skills", CreatedAt: now, UpdatedAt: now}
	if err := database.UpsertComponent(ctx, component); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertBinding(ctx, model.Binding{ID: "codex", ComponentID: component.ID, Host: model.HostCodex, Scope: model.ScopeGlobal, InstallPath: physical, Classification: model.ClassificationClean, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	result, err := Discover(ctx, database, DiscoverOptions{HomeDir: t.TempDir(), Executable: "/missing/tooltend", LookupPath: lookup, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installations, err = database.ListInstallations(ctx, bundleValue.ID)
	if err != nil || len(installations) != 1 || installations[0].SourceIdentity != "source-hash" {
		t.Fatalf("merged installations = %#v err=%v", installations, err)
	}
	if result.Pruned == 0 {
		t.Fatal("stale probe installation was not pruned")
	}
}

func TestBuiltinRecipesDoNotTreatSkillsAsCLIsOrDerivedHooks(t *testing.T) {
	catalog, err := LoadCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	mainlineRecipe, ok := catalog.Get("mainline")
	if !ok {
		t.Fatal("mainline recipe not found")
	}
	mainlineSkill := observedInstallation{component: model.LogicalComponent{Name: "mainline", Kind: model.ComponentSkill}, dependencies: []model.Dependency{{PackageIdentity: "cli:mainline"}}}
	for _, artifact := range mainlineRecipe.Artifacts {
		if (artifact.Key == "cli" || artifact.Key == "hooks") && artifactMatches(artifact, mainlineSkill) {
			t.Fatalf("mainline skill matched %s artifact", artifact.Key)
		}
	}
	sherlogRecipe, ok := catalog.Get("sherlog")
	if !ok {
		t.Fatal("sherlog recipe not found")
	}
	sherlogSkill := observedInstallation{component: model.LogicalComponent{Name: "sherlog", Kind: model.ComponentSkill}}
	for _, artifact := range sherlogRecipe.Artifacts {
		if artifact.Key == "cli" && artifactMatches(artifact, sherlogSkill) {
			t.Fatal("sherlog skill matched CLI artifact")
		}
	}
}

func TestRecipeRejectsShellStrings(t *testing.T) {
	data := []byte(`
schema = "bundle-recipe-v1"
id = "unsafe"
version = "1"
name = "Unsafe"
owner = "delegated"
confidence = "high"
[[artifacts]]
key = "cli"
name = "CLI"
kind = "cli"
driver = "test"
required = true
activate_argv = ["sh", "-c", "echo ok | curl example.test"]
`)
	if _, err := decodeRecipe(data, "local"); err == nil {
		t.Fatal("unsafe shell recipe was accepted")
	}
}

func TestHostOwnedPluginEvidenceCannotBecomeDelegatedByName(t *testing.T) {
	value := observedInstallation{component: model.LogicalComponent{Name: "mainline", Kind: model.ComponentSkill}, binding: model.Binding{
		Host: model.HostCodex, InstallMethod: model.HostOwnedInstallMethod(model.HostCodex), InstallPath: "/Users/test/.codex/plugins/cache/example/mainline",
	}}
	if !observedHostOwned(value) {
		t.Fatal("host-owned plugin evidence was not protected from delegated recipe matching")
	}
}

func TestLoadSkillSourceEvidenceUsesPackageManagerRecordsWithoutInventingVersions(t *testing.T) {
	home := t.TempDir()
	agents := filepath.Join(home, ".agents")
	if err := os.MkdirAll(agents, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := `{"version":3,"skills":{"mainline":{"source":"mainline-org/mainline","sourceType":"github","sourceUrl":"https://github.com/mainline-org/mainline.git","skillPath":"skills/mainline/SKILL.md","skillFolderHash":"abc123","installedAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-02T00:00:00Z"}}}`
	if err := os.WriteFile(filepath.Join(agents, ".skill-lock.json"), []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeSkills := filepath.Join(home, ".claude", "skills")
	itu := filepath.Join(claudeSkills, "itu-context7")
	if err := os.MkdirAll(itu, 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("skill body\n")
	digest := sha256.Sum256(content)
	if err := os.WriteFile(filepath.Join(itu, "SKILL.md"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := "skill-manifest-version: 1\nskill-id: signed-id\n\nfiles:\n  " + hex.EncodeToString(digest[:]) + "  SKILL.md\n"
	if err := os.WriteFile(filepath.Join(itu, "skill.manifest"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(itu, "skill.sig"), []byte("signature"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := `{"skillName":"itu-context7","sourceType":"mt-name","sourceId":"itu-context7","env":"prod","installedAt":"2026-01-03T00:00:00Z","targetDir":"` + claudeSkills + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(claudeSkills, ".mtskills-source.jsonl"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}

	evidence := loadSkillSourceEvidence(home)
	if evidence["mainline"].SourceIdentity != "https://github.com/mainline-org/mainline.git#skills/mainline/SKILL.md" || evidence["mainline"].ObservedHash != "abc123" {
		t.Fatalf("mainline evidence = %#v", evidence["mainline"])
	}
	ituEvidence := evidence["itu-context7"]
	if ituEvidence.SourceIdentity != "mtskills:prod:itu-context7" || ituEvidence.ObservedHash != "signed-id" {
		t.Fatalf("itu evidence = %#v", ituEvidence)
	}
	if ituEvidence.Metadata["manifest_integrity_valid"] != true || ituEvidence.Metadata["signature_present"] != true {
		t.Fatalf("itu metadata = %#v", ituEvidence.Metadata)
	}
	if _, exists := ituEvidence.Metadata["source_version"]; exists {
		t.Fatalf("non-semver source evidence became an installed version: %#v", ituEvidence.Metadata)
	}
}
