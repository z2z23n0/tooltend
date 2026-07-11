package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	semver "github.com/Masterminds/semver/v3"
	"github.com/z2z23n0/tooltend/internal/execx"
)

type NPM struct {
	Runner execx.Runner
}

func (NPM) Name() string        { return "npm" }
func (NPM) Kinds() []SourceKind { return []SourceKind{SourceNPM} }
func (NPM) Capabilities() Capabilities {
	return Capabilities{Check: true, Stage: true, ManagedAuto: true, Rollback: true}
}

func (NPM) Normalize(source Source) (Source, error) {
	return CanonicalizeSource(SourceNPM, source)
}

func (n NPM) Resolve(ctx context.Context, source Source, track Track) (Resolved, error) {
	if n.Runner == nil {
		n.Runner = execx.ExecRunner{}
	}
	spec, err := npmSpec(source.PackageName, track)
	if err != nil {
		return Resolved{}, err
	}
	result, err := n.Runner.Run(ctx, "npm", "view", spec, "version", "--json")
	if err != nil {
		return Resolved{}, errors.New("npm version lookup failed")
	}
	var version string
	if err := json.Unmarshal(result.Stdout, &version); err != nil {
		var versions []string
		if json.Unmarshal(result.Stdout, &versions) != nil || len(versions) == 0 {
			return Resolved{}, errors.New("npm returned an invalid version")
		}
		version = versions[len(versions)-1]
	}
	if version == "" {
		return Resolved{}, errors.New("npm returned an empty version")
	}
	if _, err := semver.StrictNewVersion(version); err != nil {
		return Resolved{}, errors.New("npm returned an invalid semantic version")
	}
	return Resolved{Version: version, Ref: source.PackageName + "@" + version}, nil
}

func npmSpec(packageName string, track Track) (string, error) {
	channel := track.Channel
	if channel == "" {
		channel = "stable"
	}
	switch channel {
	case "stable", "latest":
		if track.Constraint == "" {
			return packageName, nil
		}
		if _, err := semver.NewConstraint(track.Constraint); err != nil {
			return "", errors.New("invalid npm semantic version constraint")
		}
		return packageName + "@" + track.Constraint, nil
	case "semver":
		if track.Constraint == "" {
			return "", errors.New("npm semver tracking requires a constraint")
		}
		if _, err := semver.NewConstraint(track.Constraint); err != nil {
			return "", errors.New("invalid npm semantic version constraint")
		}
		return packageName + "@" + track.Constraint, nil
	case "exact":
		if track.Constraint == "" {
			return "", errors.New("exact npm tracking requires a version")
		}
		if _, err := semver.StrictNewVersion(track.Constraint); err != nil {
			return "", errors.New("invalid exact npm version")
		}
		return packageName + "@" + track.Constraint, nil
	case "main":
		if track.Constraint != "" {
			return "", errors.New("npm main tracking cannot also set a constraint")
		}
		return packageName + "@main", nil
	default:
		return "", errors.New("unsupported npm tracking channel")
	}
}

func (n NPM) Fetch(ctx context.Context, source Source, resolved Resolved, stagingDir string) (Artifact, error) {
	if n.Runner == nil {
		n.Runner = execx.ExecRunner{}
	}
	if err := os.RemoveAll(stagingDir); err != nil {
		return Artifact{}, err
	}
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return Artifact{}, err
	}
	result, err := n.Runner.Run(ctx, "npm", "install", "--prefix", stagingDir, "--ignore-scripts", "--no-audit", "--no-fund", "--package-lock=false", source.PackageName+"@"+resolved.Version)
	_ = result
	if err != nil {
		return Artifact{}, errors.New("npm isolated install failed")
	}
	packageRoot := filepath.Join(stagingDir, "node_modules", filepath.FromSlash(source.PackageName))
	if info, err := os.Stat(packageRoot); err != nil || !info.IsDir() {
		return Artifact{}, errors.New("npm isolated install did not produce the package")
	}
	return Artifact{Root: stagingDir, Integrity: resolved.Ref, Executable: filepath.Join(stagingDir, "node_modules", ".bin")}, nil
}

func (NPM) Verify(_ context.Context, source Source, resolved Resolved, artifact Artifact) error {
	packageJSON := filepath.Join(artifact.Root, "node_modules", filepath.FromSlash(source.PackageName), "package.json")
	data, err := os.ReadFile(packageJSON)
	if err != nil {
		return errors.New("npm package metadata is missing")
	}
	var metadata struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &metadata) != nil || metadata.Version != resolved.Version {
		return errors.New("npm package version does not match resolved version")
	}
	return nil
}
