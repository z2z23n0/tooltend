package adapter

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	semver "github.com/Masterminds/semver/v3"
	"github.com/z2z23n0/tooltend/internal/execx"
	"github.com/z2z23n0/tooltend/internal/safeio"
)

type Git struct {
	Runner execx.Runner
}

func (Git) Name() string        { return "git" }
func (Git) Kinds() []SourceKind { return []SourceKind{SourceGit} }
func (Git) Capabilities() Capabilities {
	return Capabilities{Check: true, Stage: true, ManagedAuto: true, Rollback: true}
}

func (Git) Normalize(source Source) (Source, error) {
	return CanonicalizeSource(SourceGit, source)
}

func (g Git) Resolve(ctx context.Context, source Source, track Track) (Resolved, error) {
	if g.Runner == nil {
		g.Runner = execx.ExecRunner{}
	}
	channel := track.Channel
	if channel == "" {
		channel = "stable"
	}
	if channel == "main" {
		return g.resolveRef(ctx, source.Locator, "refs/heads/main", "main")
	}
	if channel == "exact" {
		if track.Constraint == "" {
			return Resolved{}, errors.New("exact git tracking requires a ref or commit")
		}
		if looksLikeCommit(track.Constraint) {
			return Resolved{Version: track.Constraint, Ref: track.Constraint}, nil
		}
		for _, ref := range []string{"refs/tags/" + track.Constraint, "refs/tags/v" + strings.TrimPrefix(track.Constraint, "v"), "refs/heads/" + track.Constraint} {
			if resolved, err := g.resolveRef(ctx, source.Locator, ref, track.Constraint); err == nil {
				return resolved, nil
			}
		}
		return Resolved{}, errors.New("exact git ref was not found")
	}

	result, err := g.Runner.Run(ctx, "git", "ls-remote", "--tags", "--refs", source.Locator)
	if err != nil {
		return Resolved{}, errors.New("git tag lookup failed")
	}
	type candidate struct {
		version *semver.Version
		ref     string
		commit  string
	}
	var candidates []candidate
	var constraint *semver.Constraints
	if track.Constraint != "" {
		constraint, err = semver.NewConstraint(track.Constraint)
		if err != nil {
			return Resolved{}, errors.New("invalid semantic version constraint")
		}
	}
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[1], "refs/tags/") {
			continue
		}
		rawVersion := strings.TrimPrefix(strings.TrimPrefix(fields[1], "refs/tags/"), "v")
		version, parseErr := semver.NewVersion(rawVersion)
		if parseErr != nil || (channel == "stable" && version.Prerelease() != "") || (constraint != nil && !constraint.Check(version)) {
			continue
		}
		candidates = append(candidates, candidate{version: version, ref: fields[1], commit: fields[0]})
	}
	if len(candidates) == 0 {
		return Resolved{}, errors.New("no matching git release tag")
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].version.GreaterThan(candidates[j].version) })
	selected := candidates[0]
	return Resolved{Version: selected.version.Original(), Ref: selected.commit, Metadata: map[string]string{"source_ref": selected.ref}}, nil
}

func (g Git) resolveRef(ctx context.Context, locator, ref, version string) (Resolved, error) {
	result, err := g.Runner.Run(ctx, "git", "ls-remote", locator, ref)
	if err != nil {
		return Resolved{}, err
	}
	fields := strings.Fields(string(result.Stdout))
	if len(fields) < 2 || !looksLikeCommit(fields[0]) {
		return Resolved{}, errors.New("git ref was not found")
	}
	return Resolved{Version: version, Ref: fields[0], Metadata: map[string]string{"source_ref": ref}}, nil
}

func (g Git) Fetch(ctx context.Context, source Source, resolved Resolved, stagingDir string) (Artifact, error) {
	if g.Runner == nil {
		g.Runner = execx.ExecRunner{}
	}
	if !looksLikeCommit(resolved.Ref) {
		return Artifact{}, errors.New("resolved git commit is invalid")
	}
	parent := filepath.Dir(stagingDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return Artifact{}, err
	}
	clone, err := os.MkdirTemp(parent, ".tooltend-git-*")
	if err != nil {
		return Artifact{}, err
	}
	defer os.RemoveAll(clone)
	for _, command := range [][]string{
		{"init", "--quiet", clone},
		{"-C", clone, "remote", "add", "origin", source.Locator},
		{"-C", clone, "fetch", "--quiet", "--depth", "1", "origin", resolved.Ref},
		{"-C", clone, "checkout", "--quiet", "--detach", "FETCH_HEAD"},
	} {
		if _, err := g.Runner.Run(ctx, "git", command...); err != nil {
			return Artifact{}, errors.New("git staging failed")
		}
	}
	root := clone
	if source.Subdir != "" {
		root = filepath.Join(clone, filepath.FromSlash(source.Subdir))
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return Artifact{}, errors.New("git source subdirectory does not exist")
	}
	if err := os.RemoveAll(stagingDir); err != nil {
		return Artifact{}, err
	}
	if err := safeio.CopyTree(root, stagingDir); err != nil {
		return Artifact{}, fmt.Errorf("materialize git artifact: %w", err)
	}
	_ = os.RemoveAll(filepath.Join(stagingDir, ".git"))
	return Artifact{Root: stagingDir, Integrity: resolved.Ref}, nil
}

func (Git) Verify(_ context.Context, _ Source, resolved Resolved, artifact Artifact) error {
	if !looksLikeCommit(resolved.Ref) || artifact.Root == "" || artifact.Integrity != resolved.Ref {
		return errors.New("git artifact does not match resolved commit")
	}
	info, err := os.Stat(artifact.Root)
	if err != nil || !info.IsDir() {
		return errors.New("git artifact root is missing")
	}
	return nil
}

func normalizeGitLocator(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "git@") && strings.Contains(raw, ":") {
		parts := strings.SplitN(strings.TrimPrefix(raw, "git@"), ":", 2)
		raw = "https://" + strings.ToLower(parts[0]) + "/" + parts[1]
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "ssh") {
		return "", errors.New("git locator must be a supported repository URL")
	}
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimSuffix(strings.TrimSuffix(parsed.Path, "/"), ".git")
	parsed.RawQuery, parsed.Fragment, parsed.User = "", "", nil
	if parsed.Scheme == "ssh" && strings.EqualFold(parsed.Host, "github.com") {
		parsed.Scheme = "https"
	}
	return parsed.String(), nil
}

func looksLikeCommit(value string) bool {
	if len(value) < 7 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}
