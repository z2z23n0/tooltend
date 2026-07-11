package adapter

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/z2z23n0/tooltend/internal/execx"
	"github.com/z2z23n0/tooltend/internal/objectstore"
)

func TestPythonFetchProducesRelocatableRuntime(t *testing.T) {
	python := copyCapablePython(t)
	wheel := writeFixtureWheel(t, "1.0.0")
	runner := localWheelRunner{python: python, wheels: map[string]string{"reloc-fixture==1.0.0": wheel}}
	provider := Python{Runner: runner}
	source, err := provider.Normalize(Source{Kind: SourcePyPI, PackageName: "reloc-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	resolved := Resolved{Version: "1.0.0", Ref: "reloc-fixture==1.0.0"}
	staging := filepath.Join(t.TempDir(), "staging")
	artifact, err := provider.Fetch(context.Background(), source, resolved, staging)
	if err != nil {
		t.Fatal(err)
	}
	assertPythonRuntimeMetadataRelocatable(t, filepath.Join(staging, "venv"))
	if err := provider.Verify(context.Background(), source, resolved, artifact); err != nil {
		t.Fatal(err)
	}
	objects, err := objectstore.New(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	hash, _, err := objects.CaptureTree(context.Background(), artifact.Root, objectstore.CaptureOptions{})
	if err != nil {
		t.Fatalf("capture relocatable Python runtime: %v", err)
	}
	final := filepath.Join(t.TempDir(), "generation")
	if err := objects.MaterializeTree(context.Background(), hash, final); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(staging); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(final, "venv", "bin", "reloc-fixture"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("execute materialized console script: %v: %s", err, output)
	}
	if strings.TrimSpace(string(output)) != "reloc-fixture 1.0.0" {
		t.Fatalf("console output=%q", output)
	}
}

func assertPythonRuntimeMetadataRelocatable(t *testing.T, venv string) {
	t.Helper()
	roots := pythonRuntimeRoots(venv)
	config, err := os.ReadFile(filepath.Join(venv, "pyvenv.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if containsPythonRuntimeRoot(config, roots) || bytes.Contains(config, []byte("command =")) {
		t.Fatalf("Python metadata remains staging-bound: %s", config)
	}
	entries, err := os.ReadDir(filepath.Join(venv, "bin"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxPythonLauncherSize {
			continue
		}
		data, err := os.ReadFile(filepath.Join(venv, "bin", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if containsPythonRuntimeRoot(data, roots) {
			t.Fatalf("Python bin entry %s remains staging-bound", entry.Name())
		}
	}
}

func TestRelocatePythonLauncherRecognizesPipShebangForms(t *testing.T) {
	root := "/tmp/tooltend-stage/venv"
	for name, input := range map[string]string{
		"direct":  "#!" + root + "/bin/python3\nfrom package import main\nmain()\n",
		"distlib": "#!/bin/sh\n'''exec' '" + root + "/bin/python3' \"$0\" \"$@\"\n' '''\nfrom package import main\nmain()\n",
	} {
		t.Run(name, func(t *testing.T) {
			output, changed, err := relocatePythonLauncher([]byte(input), []string{root})
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if bytes.Contains(output, []byte(root)) || !bytes.Contains(output, []byte("$(/usr/bin/dirname \"$0\")/python")) || !bytes.Contains(output, []byte("from package import main")) {
				t.Fatalf("unexpected relocated launcher:\n%s", output)
			}
		})
	}
	if _, _, err := relocatePythonLauncher([]byte("#!/bin/sh\nexec "+root+"/bin/python3 script.py\n"), []string{root}); err == nil {
		t.Fatal("unrecognized staging-bound launcher was accepted")
	}
}

type localWheelRunner struct {
	python string
	wheels map[string]string
}

func (r localWheelRunner) Run(ctx context.Context, name string, args ...string) (execx.Result, error) {
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

func copyCapablePython(t *testing.T) string {
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

func writeFixtureWheel(t *testing.T, version string) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "reloc_fixture-"+version+"-py3-none-any.whl")
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
