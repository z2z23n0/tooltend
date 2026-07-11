package lifecycle

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/adapter"
	"github.com/z2z23n0/tooltend/internal/execx"
	"github.com/z2z23n0/tooltend/internal/model"
)

func TestPythonRuntimeAdoptAndUpdateRemainExecutableAfterStagingRemoval(t *testing.T) {
	python := lifecycleCopyCapablePython(t)
	wheels := map[string]string{
		"reloc-fixture==1.0.0": writeLifecycleFixtureWheel(t, "1.0.0"),
		"reloc-fixture==2.0.0": writeLifecycleFixtureWheel(t, "2.0.0"),
	}
	provider := adapter.Python{Runner: lifecycleLocalWheelRunner{python: python, wheels: wheels}}
	registry, err := adapter.NewRegistry(provider)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newLifecycleFixture(t, model.ComponentCLI, adapter.SourcePyPI)
	fixture.service.Adapters = registry
	t.Setenv("PATH", fixture.paths.ShimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.MkdirAll(filepath.Dir(fixture.installPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.installPath, []byte("#!/bin/sh\necho native\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyAuto)

	adopted, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "pypi:reloc-fixture", Version: "1.0.0", Executable: "reloc-fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPythonRuntimeOutput(t, adopted.Shim, "reloc-fixture 1.0.0")
	assertNoRuntimeStagingArtifacts(t, fixture.paths.StagingDir)

	policy, err := fixture.database.GetPolicy(context.Background(), "binding")
	if err != nil {
		t.Fatal(err)
	}
	policy.TrackChannel, policy.Constraint = model.TrackExact, "2.0.0"
	policy.UpdatedAt = policy.UpdatedAt.Add(time.Second)
	if err := fixture.database.SetPolicy(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	updated, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Activated || updated.ResolvedRef != "reloc-fixture==2.0.0" {
		t.Fatalf("Python update was not activated: %+v", updated)
	}
	managed, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil {
		t.Fatal(err)
	}
	if managed.ActiveGenerationID == "" || managed.ActiveGenerationID == adopted.Generation {
		t.Fatalf("active generation=%q adopted=%q", managed.ActiveGenerationID, adopted.Generation)
	}
	assertPythonRuntimeOutput(t, adopted.Shim, "reloc-fixture 2.0.0")
	assertNoRuntimeStagingArtifacts(t, fixture.paths.StagingDir)
}

func assertPythonRuntimeOutput(t *testing.T, executable, expected string) {
	t.Helper()
	output, err := exec.Command(executable).CombinedOutput()
	if err != nil {
		t.Fatalf("execute managed Python runtime: %v: %s", err, output)
	}
	if strings.TrimSpace(string(output)) != expected {
		t.Fatalf("managed Python output=%q want=%q", output, expected)
	}
}

func assertNoRuntimeStagingArtifacts(t *testing.T, stagingRoot string) {
	t.Helper()
	var artifacts []string
	_ = filepath.WalkDir(stagingRoot, func(path string, entry os.DirEntry, err error) error {
		if err == nil && path != stagingRoot && !entry.IsDir() {
			artifacts = append(artifacts, path)
		}
		return nil
	})
	if len(artifacts) != 0 {
		t.Fatalf("runtime staging artifacts remain: %v", artifacts)
	}
}

type lifecycleLocalWheelRunner struct {
	python string
	wheels map[string]string
}

func (r lifecycleLocalWheelRunner) Run(ctx context.Context, name string, args ...string) (execx.Result, error) {
	if name == "python3" && len(args) >= 2 && args[0] == "-m" && args[1] == "venv" {
		name = r.python
	}
	if len(args) > 0 {
		if wheel := r.wheels[args[len(args)-1]]; wheel != "" {
			args = append([]string(nil), args...)
			args[len(args)-1] = wheel
		}
	}
	return (execx.ExecRunner{}).Run(ctx, name, args...)
}

func lifecycleCopyCapablePython(t *testing.T) string {
	t.Helper()
	candidates := []string{os.Getenv("TOOLTEND_TEST_PYTHON"), "python3", "python", "/opt/homebrew/bin/python3", "/usr/local/bin/python3"}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		resolved, err := exec.LookPath(candidate)
		if err != nil || seen[resolved] {
			continue
		}
		seen[resolved] = true
		probe := filepath.Join(t.TempDir(), "venv")
		command := exec.Command(resolved, "-m", "venv", "--copies", probe)
		if output, err := command.CombinedOutput(); err == nil {
			return resolved
		} else {
			t.Logf("Python %s cannot create copied venv: %s", resolved, strings.TrimSpace(string(output)))
		}
	}
	t.Skip("copy-capable python3 with venv support is unavailable")
	return ""
}

func writeLifecycleFixtureWheel(t *testing.T, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reloc_fixture-"+version+"-py3-none-any.whl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	distInfo := "reloc_fixture-" + version + ".dist-info"
	files := map[string]string{
		"reloc_fixture/__init__.py":    "__version__ = " + fmt.Sprintf("%q", version) + "\n",
		"reloc_fixture/cli.py":         "from . import __version__\ndef main():\n    print('reloc-fixture ' + __version__)\n",
		distInfo + "/METADATA":         "Metadata-Version: 2.1\nName: reloc-fixture\nVersion: " + version + "\n",
		distInfo + "/WHEEL":            "Wheel-Version: 1.0\nGenerator: tooltend-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		distInfo + "/entry_points.txt": "[console_scripts]\nreloc-fixture = reloc_fixture.cli:main\n",
	}
	var record strings.Builder
	for name, content := range files {
		writer, createErr := archive.Create(name)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, writeErr := writer.Write([]byte(content)); writeErr != nil {
			t.Fatal(writeErr)
		}
		fmt.Fprintf(&record, "%s,,\n", name)
	}
	recordPath := distInfo + "/RECORD"
	fmt.Fprintf(&record, "%s,,\n", recordPath)
	writer, err := archive.Create(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(record.String())); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
