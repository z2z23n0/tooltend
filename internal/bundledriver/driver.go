package bundledriver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	semver "github.com/Masterminds/semver/v3"

	"github.com/z2z23n0/tooltend/internal/execx"
	"github.com/z2z23n0/tooltend/internal/safeio"
)

const maxDownloadBytes = 256 << 20

type Driver struct {
	Runner execx.Runner
	Client *http.Client
	Out    io.Writer
	GOOS   string
	GOARCH string
}

func (d Driver) Execute(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("bundle driver action is required")
	}
	switch args[0] {
	case "npm-resolve":
		if len(args) != 2 {
			return errors.New("npm-resolve requires package")
		}
		return d.npmResolve(ctx, args[1])
	case "npm-stage":
		if len(args) != 6 {
			return errors.New("npm-stage requires package, version, previous version, stage, and path")
		}
		return d.npmStage(ctx, args[1], args[2], args[3], args[4], args[5])
	case "npm-activate":
		if len(args) != 2 {
			return errors.New("npm-activate requires stage")
		}
		return d.npmInstallArchive(ctx, filepath.Join(args[1], "target.tgz"))
	case "npm-rollback":
		if len(args) != 4 {
			return errors.New("npm-rollback requires package, version, and stage")
		}
		return d.npmRollback(ctx, args[1], args[2], args[3])
	case "github-resolve":
		if len(args) != 2 {
			return errors.New("github-resolve requires repository")
		}
		return d.githubResolve(ctx, args[1])
	case "github-stage":
		if len(args) != 7 {
			return errors.New("github-stage requires repository, binary, version, previous version, stage, and path")
		}
		return d.githubStage(ctx, args[1], args[2], args[3], args[4], args[5], args[6])
	case "github-activate":
		if len(args) != 3 {
			return errors.New("github-activate requires stage and path")
		}
		return replacePath(filepath.Join(args[1], "next"), args[2])
	case "github-rollback":
		if len(args) != 6 {
			return errors.New("github-rollback requires repository, binary, version, stage, and path")
		}
		return d.githubRollback(ctx, args[1], args[2], args[3], args[4], args[5])
	case "git-resolve":
		if len(args) != 2 {
			return errors.New("git-resolve requires repository URL")
		}
		return d.gitResolve(ctx, args[1])
	case "git-release-resolve":
		if len(args) != 3 {
			return errors.New("git-release-resolve requires repository URL and GitHub repository")
		}
		return d.gitReleaseResolve(ctx, args[1], args[2])
	case "git-npm-release-resolve":
		if len(args) != 3 {
			return errors.New("git-npm-release-resolve requires repository URL and npm package")
		}
		return d.gitNPMReleaseResolve(ctx, args[1], args[2])
	case "git-stage":
		if len(args) != 7 {
			return errors.New("git-stage requires repository, subdirectory, ref, previous ref, stage, and path")
		}
		return d.gitStage(ctx, args[1], args[2], args[3], args[4], args[5], args[6])
	case "git-activate":
		if len(args) != 3 {
			return errors.New("git-activate requires stage and path")
		}
		if err := replacePath(filepath.Join(args[1], "next"), args[2]); err != nil {
			return err
		}
		return detachSkillLock(args[2])
	case "git-rollback":
		if len(args) != 6 {
			return errors.New("git-rollback requires repository, subdirectory, ref, stage, and path")
		}
		return d.gitRollback(ctx, args[1], args[2], args[3], args[4], args[5])
	case "skill-health":
		if len(args) != 2 {
			return errors.New("skill-health requires path")
		}
		return skillHealth(args[1])
	case "binary-health":
		if len(args) < 2 {
			return errors.New("binary-health requires path")
		}
		_, err := d.runner().Run(ctx, args[1], args[2:]...)
		if err != nil {
			return errors.New("managed binary health check failed")
		}
		return nil
	case "mainline-hooks-stage":
		if len(args) != 3 {
			return errors.New("mainline-hooks-stage requires project and stage")
		}
		return stageMainlineHooks(args[1], args[2])
	case "mainline-hooks-activate":
		if len(args) != 2 {
			return errors.New("mainline-hooks-activate requires project")
		}
		return d.runMainlineHooks(ctx, args[1], "install")
	case "mainline-hooks-rollback":
		if len(args) != 3 {
			return errors.New("mainline-hooks-rollback requires project and stage")
		}
		if restored, err := restoreMainlineHooks(args[1], args[2]); err != nil || restored {
			return err
		}
		return d.runMainlineHooks(ctx, args[1], "install")
	case "mainline-hooks-health":
		if len(args) != 2 {
			return errors.New("mainline-hooks-health requires project")
		}
		return d.runMainlineHooks(ctx, args[1], "status")
	default:
		return fmt.Errorf("unsupported bundle driver action %q", args[0])
	}
}

func (d Driver) runner() execx.Runner {
	if d.Runner != nil {
		return d.Runner
	}
	return execx.ExecRunner{}
}

func (d Driver) output(value string) error {
	w := d.Out
	if w == nil {
		w = os.Stdout
	}
	_, err := fmt.Fprintln(w, value)
	return err
}

func (d Driver) npmResolve(ctx context.Context, packageName string) error {
	version, err := d.npmVersion(ctx, packageName)
	if err != nil {
		return err
	}
	return d.output(version)
}

func (d Driver) npmVersion(ctx context.Context, packageName string) (string, error) {
	result, err := d.runner().Run(ctx, "npm", "view", packageName, "version", "--json")
	if err != nil {
		return "", errors.New("npm version lookup failed")
	}
	var version string
	if json.Unmarshal(result.Stdout, &version) != nil {
		var versions []string
		if json.Unmarshal(result.Stdout, &versions) != nil || len(versions) == 0 {
			return "", errors.New("npm returned an invalid version")
		}
		version = versions[len(versions)-1]
	}
	if _, err := semver.StrictNewVersion(version); err != nil {
		return "", errors.New("npm returned an invalid semantic version")
	}
	return version, nil
}

func (d Driver) npmStage(ctx context.Context, packageName, version, previous, stage, path string) error {
	if _, err := semver.StrictNewVersion(version); err != nil {
		return errors.New("npm target version is invalid")
	}
	if err := resetStage(stage); err != nil {
		return err
	}
	if err := d.npmPack(ctx, packageName, version, stage, "target.tgz"); err != nil {
		return err
	}
	if _, err := semver.StrictNewVersion(strings.TrimPrefix(previous, "v")); err == nil {
		if err := d.npmPack(ctx, packageName, strings.TrimPrefix(previous, "v"), stage, "previous.tgz"); err != nil {
			return err
		}
	}
	return backupPath(path, filepath.Join(stage, "previous-installation"))
}

func (d Driver) npmPack(ctx context.Context, packageName, version, stage, target string) error {
	result, err := d.runner().Run(ctx, "npm", "pack", packageName+"@"+version, "--json", "--pack-destination", stage)
	if err != nil {
		return errors.New("npm package staging failed")
	}
	var records []struct {
		Filename string `json:"filename"`
	}
	if json.Unmarshal(result.Stdout, &records) != nil || len(records) != 1 || filepath.Base(records[0].Filename) != records[0].Filename {
		return errors.New("npm package staging returned invalid metadata")
	}
	return os.Rename(filepath.Join(stage, records[0].Filename), filepath.Join(stage, target))
}

func (d Driver) npmInstallArchive(ctx context.Context, archive string) error {
	if info, err := os.Stat(archive); err != nil || !info.Mode().IsRegular() {
		return errors.New("staged npm package is missing")
	}
	if _, err := d.runner().Run(ctx, "npm", "install", "--global", "--no-audit", "--no-fund", archive); err != nil {
		return errors.New("npm package activation failed")
	}
	return nil
}

func (d Driver) npmRollback(ctx context.Context, packageName, version, stage string) error {
	archive := filepath.Join(stage, "previous.tgz")
	if info, err := os.Stat(archive); err == nil && info.Mode().IsRegular() {
		return d.npmInstallArchive(ctx, archive)
	}
	version = strings.TrimPrefix(version, "v")
	if _, err := semver.StrictNewVersion(version); err != nil {
		return errors.New("npm rollback version is unavailable")
	}
	if _, err := d.runner().Run(ctx, "npm", "install", "--global", "--no-audit", "--no-fund", packageName+"@"+version); err != nil {
		return errors.New("npm package rollback failed")
	}
	return nil
}

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

func (d Driver) githubResolve(ctx context.Context, repository string) error {
	release, err := d.getRelease(ctx, repository, "latest")
	if err != nil {
		return err
	}
	version := strings.TrimPrefix(strings.TrimSpace(release.TagName), "v")
	if _, err := semver.StrictNewVersion(version); err != nil {
		return errors.New("GitHub latest release is not a stable semantic version")
	}
	return d.output(version)
}

func (d Driver) githubStage(ctx context.Context, repository, binary, version, _ string, stage, path string) error {
	version = strings.TrimPrefix(version, "v")
	if _, err := semver.StrictNewVersion(version); err != nil {
		return errors.New("GitHub release version is invalid")
	}
	if err := resetStage(stage); err != nil {
		return err
	}
	release, err := d.getRelease(ctx, repository, "tags/v"+version)
	if err != nil {
		release, err = d.getRelease(ctx, repository, "tags/"+version)
	}
	if err != nil {
		return err
	}
	asset, checksums, err := selectReleaseAssets(release.Assets, d.goos(), d.goarch())
	if err != nil {
		return err
	}
	archive, err := d.download(ctx, asset)
	if err != nil {
		return err
	}
	checksumData, err := d.download(ctx, checksums)
	if err != nil {
		return err
	}
	if err := verifyChecksum(asset.Name, archive, checksumData); err != nil {
		return err
	}
	if err := extractTarBinary(archive, binary, filepath.Join(stage, "next")); err != nil {
		return err
	}
	return backupPath(path, filepath.Join(stage, "previous"))
}

func (d Driver) githubRollback(ctx context.Context, repository, binary, version, stage, path string) error {
	previous := filepath.Join(stage, "previous")
	if info, err := os.Stat(previous); err == nil && info.Mode().IsRegular() {
		return replacePath(previous, path)
	}
	if _, err := semver.StrictNewVersion(strings.TrimPrefix(version, "v")); err != nil {
		return errors.New("GitHub rollback version is unavailable")
	}
	temporary, err := os.MkdirTemp(filepath.Dir(path), ".tooltend-github-rollback-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	if err := d.githubStage(ctx, repository, binary, version, "", temporary, path); err != nil {
		return err
	}
	return replacePath(filepath.Join(temporary, "next"), path)
}

func (d Driver) getRelease(ctx context.Context, repository, endpoint string) (githubRelease, error) {
	if strings.Count(repository, "/") != 1 || strings.ContainsAny(repository, "\x00\r\n?#") {
		return githubRelease{}, errors.New("GitHub repository identity is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repository+"/releases/"+endpoint, nil)
	if err != nil {
		return githubRelease{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "tooltend-bundle-driver")
	if token := d.githubToken(ctx); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := d.client().Do(request)
	if err != nil {
		return githubRelease{}, errors.New("GitHub release lookup failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return githubRelease{}, fmt.Errorf("GitHub release lookup failed with status %d", response.StatusCode)
	}
	var release githubRelease
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if decoder.Decode(&release) != nil || release.TagName == "" {
		return githubRelease{}, errors.New("GitHub release metadata is invalid")
	}
	return release, nil
}

func (d Driver) githubToken(ctx context.Context) string {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(name)); validToken(token) {
			return token
		}
	}
	result, err := d.runner().Run(ctx, "gh", "auth", "token")
	if err != nil {
		return ""
	}
	token := strings.TrimSpace(string(result.Stdout))
	if !validToken(token) {
		return ""
	}
	return token
}

func validToken(value string) bool {
	return value != "" && len(value) <= 4096 && !strings.ContainsAny(value, "\x00\r\n \t")
}

func (d Driver) download(ctx context.Context, asset githubAsset) ([]byte, error) {
	if asset.Size <= 0 || asset.Size > maxDownloadBytes || !strings.HasPrefix(asset.URL, "https://github.com/") {
		return nil, errors.New("GitHub release asset metadata is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "tooltend-bundle-driver")
	response, err := d.client().Do(request)
	if err != nil {
		return nil, errors.New("GitHub release asset download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub release asset download failed with status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDownloadBytes+1))
	if err != nil || int64(len(data)) > maxDownloadBytes {
		return nil, errors.New("GitHub release asset exceeded the download limit")
	}
	if int64(len(data)) != asset.Size {
		return nil, errors.New("GitHub release asset size mismatch")
	}
	return data, nil
}

func (d Driver) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (d Driver) goos() string {
	if d.GOOS != "" {
		return d.GOOS
	}
	return runtime.GOOS
}

func (d Driver) goarch() string {
	if d.GOARCH != "" {
		return d.GOARCH
	}
	return runtime.GOARCH
}

func selectReleaseAssets(assets []githubAsset, goos, goarch string) (githubAsset, githubAsset, error) {
	osTokens := map[string][]string{"darwin": {"darwin", "apple-darwin"}, "linux": {"linux", "unknown-linux"}}[goos]
	archTokens := map[string][]string{"arm64": {"arm64", "aarch64"}, "amd64": {"amd64", "x86_64"}}[goarch]
	if len(osTokens) == 0 || len(archTokens) == 0 {
		return githubAsset{}, githubAsset{}, errors.New("platform is not supported by the bundle release driver")
	}
	var candidates []githubAsset
	var checksums githubAsset
	for _, asset := range assets {
		name := strings.ToLower(asset.Name)
		if name == "checksums.txt" {
			checksums = asset
			continue
		}
		if strings.HasSuffix(name, ".tar.gz") && containsAny(name, osTokens) && containsAny(name, archTokens) {
			candidates = append(candidates, asset)
		}
	}
	if len(candidates) != 1 || checksums.Name == "" {
		return githubAsset{}, githubAsset{}, errors.New("release does not contain one matching archive and checksums.txt")
	}
	return candidates[0], checksums, nil
}

func containsAny(value string, candidates []string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func verifyChecksum(name string, data, checksums []byte) error {
	expected := ""
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimPrefix(fields[len(fields)-1], "*") == name {
			expected = strings.ToLower(fields[0])
			break
		}
	}
	if len(expected) != sha256.Size*2 {
		return errors.New("release checksum entry is missing")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != expected {
		return errors.New("release checksum verification failed")
	}
	return nil
}

func extractTarBinary(archive []byte, binary, target string) error {
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return errors.New("release archive is not valid gzip")
	}
	defer reader.Close()
	tr := tar.NewReader(reader)
	for {
		header, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return errors.New("release archive is invalid")
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(filepath.Clean(header.Name)) != binary {
			continue
		}
		if header.Size <= 0 || header.Size > 128<<20 {
			return errors.New("release binary size is invalid")
		}
		data, readErr := io.ReadAll(io.LimitReader(tr, header.Size+1))
		if readErr != nil || int64(len(data)) != header.Size {
			return errors.New("release binary is truncated")
		}
		return safeio.AtomicWriteFile(target, data, 0o755)
	}
	return errors.New("release archive does not contain the expected binary")
}

func (d Driver) gitResolve(ctx context.Context, repository string) error {
	result, err := d.runner().Run(ctx, "git", "ls-remote", repository, "HEAD")
	if err != nil {
		return errors.New("git source lookup failed")
	}
	fields := strings.Fields(string(result.Stdout))
	if len(fields) < 2 || !commitHash(fields[0]) {
		return errors.New("git source did not resolve to an exact commit")
	}
	return d.output("git:" + strings.ToLower(fields[0]))
}

func (d Driver) gitReleaseResolve(ctx context.Context, repository, githubRepository string) error {
	release, err := d.getRelease(ctx, githubRepository, "latest")
	if err != nil {
		return err
	}
	tag := strings.TrimSpace(release.TagName)
	if tag == "" || strings.ContainsAny(tag, "\x00\r\n") {
		return errors.New("GitHub release tag is invalid")
	}
	for _, ref := range []string{"refs/tags/" + tag + "^{}", "refs/tags/" + tag} {
		result, resolveErr := d.runner().Run(ctx, "git", "ls-remote", repository, ref)
		if resolveErr != nil {
			continue
		}
		fields := strings.Fields(string(result.Stdout))
		if len(fields) >= 2 && commitHash(fields[0]) {
			return d.output("git:" + strings.ToLower(fields[0]))
		}
	}
	return errors.New("GitHub release tag did not resolve to an exact git commit")
}

func (d Driver) gitNPMReleaseResolve(ctx context.Context, repository, packageName string) error {
	version, err := d.npmVersion(ctx, packageName)
	if err != nil {
		return err
	}
	for _, tag := range []string{"v" + version, version} {
		for _, ref := range []string{"refs/tags/" + tag + "^{}", "refs/tags/" + tag} {
			result, resolveErr := d.runner().Run(ctx, "git", "ls-remote", repository, ref)
			if resolveErr != nil {
				continue
			}
			fields := strings.Fields(string(result.Stdout))
			if len(fields) >= 2 && commitHash(fields[0]) {
				return d.output("git:" + strings.ToLower(fields[0]))
			}
		}
	}
	return errors.New("npm version did not resolve to an exact git release tag")
}

func (d Driver) gitStage(ctx context.Context, repository, subdir, ref, _ string, stage, path string) error {
	commit := strings.TrimPrefix(ref, "git:")
	if !commitHash(commit) {
		return errors.New("git target ref is invalid")
	}
	cleanSubdir, err := sourceSubdir(subdir)
	if err != nil {
		return err
	}
	if err := resetStage(stage); err != nil {
		return err
	}
	clone := filepath.Join(stage, "repository")
	commands := [][]string{
		{"init", "--quiet", clone},
		{"-C", clone, "remote", "add", "origin", repository},
		{"-C", clone, "fetch", "--quiet", "--depth", "1", "origin", commit},
		{"-C", clone, "checkout", "--quiet", "--detach", "FETCH_HEAD"},
	}
	for _, command := range commands {
		if _, err := d.runner().Run(ctx, "git", command...); err != nil {
			return errors.New("git skill staging failed")
		}
	}
	root := clone
	if cleanSubdir != "." {
		root = filepath.Join(clone, filepath.FromSlash(cleanSubdir))
	}
	if err := copySkillTree(root, filepath.Join(stage, "next")); err != nil {
		return err
	}
	if err := skillHealth(filepath.Join(stage, "next")); err != nil {
		return err
	}
	if err := backupPath(path, filepath.Join(stage, "previous")); err != nil {
		return err
	}
	return backupSkillLock(path, stage)
}

func (d Driver) gitRollback(ctx context.Context, repository, subdir, ref, stage, path string) error {
	previous := filepath.Join(stage, "previous")
	if info, err := os.Stat(previous); err == nil && info.IsDir() {
		if err := replacePath(previous, path); err != nil {
			return err
		}
		return restoreSkillLock(path, stage)
	}
	if !commitHash(strings.TrimPrefix(ref, "git:")) {
		return errors.New("git rollback ref is unavailable")
	}
	temporary, err := os.MkdirTemp(filepath.Dir(path), ".tooltend-git-rollback-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	if err := d.gitStage(ctx, repository, subdir, ref, "", temporary, path); err != nil {
		return err
	}
	if err := replacePath(filepath.Join(temporary, "next"), path); err != nil {
		return err
	}
	return detachSkillLock(path)
}

func commitHash(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func sourceSubdir(value string) (string, error) {
	value = filepath.ToSlash(filepath.Clean(strings.TrimSpace(value)))
	if value == "" {
		value = "."
	}
	if filepath.IsAbs(value) || value == ".." || strings.HasPrefix(value, "../") {
		return "", errors.New("git skill subdirectory is invalid")
	}
	return value, nil
}

func resetStage(stage string) error {
	if !filepath.IsAbs(stage) || filepath.Clean(stage) == string(filepath.Separator) {
		return errors.New("bundle stage path must be an absolute non-root path")
	}
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	return os.MkdirAll(stage, 0o700)
}

func backupPath(path, destination string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return copyPath(path, destination, false)
}

func replacePath(source, destination string) error {
	if !filepath.IsAbs(destination) || filepath.Clean(destination) == string(filepath.Separator) {
		return errors.New("managed installation path must be an absolute non-root path")
	}
	if _, err := os.Lstat(source); err != nil {
		return errors.New("staged installation is missing")
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(parent, ".tooltend-next-*")
	if err != nil {
		return err
	}
	_ = os.Remove(temporary)
	defer os.RemoveAll(temporary)
	if err := copyPath(source, temporary, false); err != nil {
		return err
	}
	backup := temporary + ".old"
	hadDestination := false
	if _, err := os.Lstat(destination); err == nil {
		hadDestination = true
		if err := os.Rename(destination, backup); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		if hadDestination {
			_ = os.Rename(backup, destination)
		}
		return err
	}
	return os.RemoveAll(backup)
}

func copySkillTree(source, destination string) error {
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		return errors.New("git skill source directory is missing")
	}
	return copyPath(source, destination, true)
}

func copyPath(source, destination string, skipGit bool) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		if filepath.IsAbs(target) || strings.HasPrefix(filepath.Clean(target), "..") {
			return errors.New("source contains an unsafe symbolic link")
		}
		return os.Symlink(target, destination)
	}
	if info.IsDir() {
		if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if skipGit && entry.Name() == ".git" {
				continue
			}
			if err := copyPath(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name()), skipGit); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return errors.New("source contains an unsupported filesystem entry")
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return safeio.AtomicWriteFile(destination, data, info.Mode().Perm())
}

func skillHealth(path string) error {
	info, err := os.Stat(filepath.Join(path, "SKILL.md"))
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 4<<20 {
		return errors.New("managed skill is missing a valid SKILL.md")
	}
	return nil
}

func skillLockPath(path string) string {
	parent := filepath.Dir(path)
	if filepath.Base(parent) != "skills" || filepath.Base(filepath.Dir(parent)) != ".agents" {
		return ""
	}
	return filepath.Join(filepath.Dir(parent), ".skill-lock.json")
}

func backupSkillLock(path, stage string) error {
	lock := skillLockPath(path)
	if lock == "" {
		return nil
	}
	manifest := map[string]bool{"existed": false}
	if info, err := os.Stat(lock); err == nil && info.Mode().IsRegular() {
		manifest["existed"] = true
		if err := copyPath(lock, filepath.Join(stage, "skill-lock.previous"), false); err != nil {
			return err
		}
	}
	data, _ := json.Marshal(manifest)
	return safeio.AtomicWriteFile(filepath.Join(stage, "skill-lock.json"), data, 0o600)
}

func detachSkillLock(path string) error {
	lock := skillLockPath(path)
	if lock == "" {
		return nil
	}
	data, err := os.ReadFile(lock)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var document struct {
		Version   int                        `json:"version"`
		Skills    map[string]json.RawMessage `json:"skills"`
		Dismissed map[string]json.RawMessage `json:"dismissed"`
	}
	if json.Unmarshal(data, &document) != nil || document.Skills == nil {
		return errors.New("npx skills lock file is invalid")
	}
	delete(document.Skills, filepath.Base(path))
	updated, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	updated = append(updated, '\n')
	return safeio.AtomicWriteFile(lock, updated, 0o600)
}

func restoreSkillLock(path, stage string) error {
	manifestData, err := os.ReadFile(filepath.Join(stage, "skill-lock.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var manifest map[string]bool
	if json.Unmarshal(manifestData, &manifest) != nil {
		return errors.New("skill lock rollback metadata is invalid")
	}
	lock := skillLockPath(path)
	if lock == "" {
		return nil
	}
	if !manifest["existed"] {
		if err := os.Remove(lock); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return copyPath(filepath.Join(stage, "skill-lock.previous"), lock, false)
}

var mainlineHookFiles = []string{".claude/settings.json", ".codex/config.toml", ".codex/hooks.json", ".cursor/hooks.json"}

func stageMainlineHooks(project, stage string) error {
	if !filepath.IsAbs(project) {
		return errors.New("mainline hook project path must be absolute")
	}
	if err := resetStage(stage); err != nil {
		return err
	}
	existed := map[string]bool{}
	for _, relative := range mainlineHookFiles {
		source := filepath.Join(project, filepath.FromSlash(relative))
		if info, err := os.Stat(source); err == nil && info.Mode().IsRegular() {
			existed[relative] = true
			if err := copyPath(source, filepath.Join(stage, "previous", filepath.FromSlash(relative)), false); err != nil {
				return err
			}
		}
	}
	data, _ := json.Marshal(existed)
	return safeio.AtomicWriteFile(filepath.Join(stage, "manifest.json"), data, 0o600)
}

func restoreMainlineHooks(project, stage string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(stage, "manifest.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var existed map[string]bool
	if json.Unmarshal(data, &existed) != nil {
		return false, errors.New("mainline hook rollback metadata is invalid")
	}
	for _, relative := range mainlineHookFiles {
		target := filepath.Join(project, filepath.FromSlash(relative))
		if existed[relative] {
			if err := copyPath(filepath.Join(stage, "previous", filepath.FromSlash(relative)), target, false); err != nil {
				return false, err
			}
		} else if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return true, nil
}

func (d Driver) runMainlineHooks(ctx context.Context, project, action string) error {
	if !filepath.IsAbs(project) {
		return errors.New("mainline hook project path must be absolute")
	}
	runner := d.runner()
	if value, ok := runner.(execx.ExecRunner); ok {
		value.Dir = project
		runner = value
	}
	if _, err := runner.Run(ctx, "mainline", "hooks", action); err != nil {
		return errors.New("mainline hook command failed")
	}
	return nil
}
