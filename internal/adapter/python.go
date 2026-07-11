package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/z2z23n0/tooltend/internal/execx"
)

type Python struct {
	Runner execx.Runner
	Client *http.Client
}

func (Python) Name() string        { return "python" }
func (Python) Kinds() []SourceKind { return []SourceKind{SourcePyPI} }
func (Python) Capabilities() Capabilities {
	return Capabilities{Check: true, Stage: true, ManagedAuto: true, Rollback: true}
}

func (Python) Normalize(source Source) (Source, error) {
	return CanonicalizeSource(SourcePyPI, source)
}

func (p Python) Resolve(ctx context.Context, source Source, track Track) (Resolved, error) {
	channel := track.Channel
	if channel == "" {
		channel = "stable"
	}
	if channel == "exact" {
		if !validPythonVersion(track.Constraint) {
			return Resolved{}, errors.New("exact Python tracking requires a safe PEP 440 version")
		}
		return Resolved{Version: track.Constraint, Ref: source.PackageName + "==" + track.Constraint}, nil
	}
	if channel != "stable" && channel != "latest" {
		return Resolved{}, errors.New("Python v1 supports stable, latest, or exact tracking")
	}
	if track.Constraint != "" {
		return Resolved{}, errors.New("Python range constraints require an exact lock in v1")
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	endpoint := "https://pypi.org/pypi/" + url.PathEscape(source.PackageName) + "/json"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Resolved{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return Resolved{}, errors.New("PyPI version lookup failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Resolved{}, fmt.Errorf("PyPI version lookup returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	decoder := json.NewDecoder(response.Body)
	if decoder.Decode(&payload) != nil || payload.Info.Version == "" {
		return Resolved{}, errors.New("PyPI returned invalid package metadata")
	}
	if !validPythonVersion(payload.Info.Version) {
		return Resolved{}, errors.New("PyPI returned an unsafe package version")
	}
	return Resolved{Version: payload.Info.Version, Ref: source.PackageName + "==" + payload.Info.Version}, nil
}

var pythonVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.!+_-]{0,127}$`)

func validPythonVersion(value string) bool {
	return pythonVersion.MatchString(value) && !strings.Contains(value, "..")
}

func (p Python) Fetch(ctx context.Context, source Source, resolved Resolved, stagingDir string) (Artifact, error) {
	if p.Runner == nil {
		p.Runner = execx.ExecRunner{}
	}
	if err := os.RemoveAll(stagingDir); err != nil {
		return Artifact{}, err
	}
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return Artifact{}, err
	}
	venv := filepath.Join(stagingDir, "venv")
	if _, err := p.Runner.Run(ctx, "python3", "-m", "venv", "--copies", venv); err != nil {
		return Artifact{}, errors.New("create relocatable isolated Python environment failed")
	}
	if err := removePythonActivationScripts(venv); err != nil {
		return Artifact{}, err
	}
	pip := filepath.Join(venv, "bin", "python")
	if _, err := p.Runner.Run(ctx, pip, "-m", "pip", "install", "--disable-pip-version-check", "--no-input", "--only-binary=:all:", source.PackageName+"=="+resolved.Version); err != nil {
		return Artifact{}, errors.New("install exact Python package failed")
	}
	if err := makePythonRuntimeRelocatable(venv); err != nil {
		return Artifact{}, err
	}
	return Artifact{Root: stagingDir, Integrity: resolved.Ref, Executable: filepath.Join(venv, "bin")}, nil
}

func (p Python) Verify(ctx context.Context, source Source, resolved Resolved, artifact Artifact) error {
	if p.Runner == nil {
		p.Runner = execx.ExecRunner{}
	}
	python := filepath.Join(artifact.Root, "venv", "bin", "python")
	result, err := p.Runner.Run(ctx, python, "-c", "import importlib.metadata; print(importlib.metadata.version("+pythonLiteral(source.PackageName)+"))")
	if err != nil || strings.TrimSpace(string(result.Stdout)) != resolved.Version {
		return errors.New("Python package version does not match resolved version")
	}
	return nil
}

func pythonLiteral(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

const maxPythonLauncherSize = 1 << 20

var pythonActivationScripts = []string{"activate", "activate.csh", "activate.fish", "Activate.ps1"}
var pythonVersionedPipLauncher = regexp.MustCompile(`^pip3(?:\.[0-9]+)+$`)

func removePythonActivationScripts(venv string) error {
	for _, name := range pythonActivationScripts {
		if err := os.Remove(filepath.Join(venv, "bin", name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove non-relocatable Python activation script: %w", err)
		}
	}
	return nil
}

func makePythonRuntimeRelocatable(venv string) error {
	bin := filepath.Join(venv, "bin")
	python := filepath.Join(bin, "python")
	info, err := os.Lstat(python)
	if err != nil {
		return fmt.Errorf("inspect copied Python interpreter: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&0o111 == 0 {
		return errors.New("copied Python environment did not produce a regular executable interpreter")
	}
	for _, entry := range []string{"pip", "pip3"} {
		if err := os.Remove(filepath.Join(bin, entry)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove mutable Python package installer: %w", err)
		}
	}
	entries, err := os.ReadDir(bin)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if pythonVersionedPipLauncher.MatchString(entry.Name()) {
			if err := os.Remove(filepath.Join(bin, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove mutable Python package installer: %w", err)
			}
		}
	}
	roots := pythonRuntimeRoots(venv)
	if err := rewritePythonLaunchers(bin, roots); err != nil {
		return err
	}
	configPath := filepath.Join(venv, "pyvenv.cfg")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read Python environment metadata: %w", err)
	}
	lines := strings.Split(string(configData), "\n")
	filtered := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "command =") {
			continue
		}
		filtered = append(filtered, line)
	}
	configData = []byte(strings.Join(filtered, "\n"))
	if containsPythonRuntimeRoot(configData, roots) {
		return errors.New("Python environment metadata remains bound to staging")
	}
	if err := os.WriteFile(configPath, configData, 0o644); err != nil {
		return fmt.Errorf("rewrite Python environment metadata: %w", err)
	}
	return nil
}

func pythonRuntimeRoots(venv string) []string {
	values := []string{filepath.Clean(venv)}
	if resolved, err := filepath.EvalSymlinks(venv); err == nil && resolved != values[0] {
		values = append(values, filepath.Clean(resolved))
	}
	return values
}

func rewritePythonLaunchers(bin string, roots []string) error {
	entries, err := os.ReadDir(bin)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(bin, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > maxPythonLauncherSize {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rewritten, changed, err := relocatePythonLauncher(data, roots)
		if err != nil {
			return fmt.Errorf("rewrite Python launcher %s: %w", entry.Name(), err)
		}
		if !changed {
			continue
		}
		if err := os.WriteFile(path, rewritten, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

func relocatePythonLauncher(data []byte, roots []string) ([]byte, bool, error) {
	firstEnd := bytes.IndexByte(data, '\n')
	if firstEnd < 0 {
		if containsPythonRuntimeRoot(data, roots) {
			return nil, false, errors.New("staging-bound launcher has no script body")
		}
		return data, false, nil
	}
	bodyOffset := -1
	first := data[:firstEnd]
	if bytes.HasPrefix(first, []byte("#!")) && containsPythonRuntimeRoot(first, roots) {
		bodyOffset = firstEnd + 1
	} else if bytes.Equal(first, []byte("#!/bin/sh")) {
		secondRelative := bytes.IndexByte(data[firstEnd+1:], '\n')
		if secondRelative >= 0 {
			secondEnd := firstEnd + 1 + secondRelative
			thirdRelative := bytes.IndexByte(data[secondEnd+1:], '\n')
			if thirdRelative >= 0 {
				thirdEnd := secondEnd + 1 + thirdRelative
				second, third := data[firstEnd+1:secondEnd], data[secondEnd+1:thirdEnd]
				if bytes.Contains(second, []byte("'''exec'")) && containsPythonRuntimeRoot(second, roots) && bytes.Equal(bytes.TrimSpace(third), []byte("' '''")) {
					bodyOffset = thirdEnd + 1
				}
			}
		}
	}
	if bodyOffset < 0 {
		if containsPythonRuntimeRoot(data, roots) {
			return nil, false, errors.New("unrecognized staging-bound launcher")
		}
		return data, false, nil
	}
	launcher := []byte("#!/bin/sh\n'''exec' \"$(/usr/bin/dirname \"$0\")/python\" \"$0\" \"$@\"\n' '''\n")
	result := make([]byte, 0, len(launcher)+len(data)-bodyOffset)
	result = append(result, launcher...)
	result = append(result, data[bodyOffset:]...)
	if containsPythonRuntimeRoot(result, roots) {
		return nil, false, errors.New("rewritten launcher remains bound to staging")
	}
	return result, true, nil
}

func containsPythonRuntimeRoot(data []byte, roots []string) bool {
	for _, root := range roots {
		if bytes.Contains(data, []byte(root)) {
			return true
		}
	}
	return false
}
