package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const maxConfigBytes = 32 << 20

func defaultClaudeManagedDir() string {
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/ClaudeCode"
	}
	if runtime.GOOS == "windows" {
		if programFiles := os.Getenv("ProgramFiles"); programFiles != "" {
			return filepath.Join(programFiles, "ClaudeCode")
		}
		return `C:\Program Files\ClaudeCode`
	}
	return "/etc/claude-code"
}

func readJSONMap(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	limited := io.LimitReader(f, maxConfigBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("configuration exceeds %d bytes", maxConfigBytes)
	}
	var document map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	if document == nil {
		document = make(map[string]any)
	}
	return document, nil
}

func readTOMLMap(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	limited := io.LimitReader(f, maxConfigBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("configuration exceeds %d bytes", maxConfigBytes)
	}
	var document map[string]any
	if err := toml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if document == nil {
		document = make(map[string]any)
	}
	return document, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func asMap(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case map[string]string:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = item
		}
		return result, true
	default:
		return nil, false
	}
}

func asSlice(value any) ([]any, bool) {
	switch typed := value.(type) {
	case []any:
		return typed, true
	case []map[string]any:
		result := make([]any, len(typed))
		for i := range typed {
			result[i] = typed[i]
		}
		return result, true
	case []string:
		result := make([]any, len(typed))
		for i := range typed {
			result[i] = typed[i]
		}
		return result, true
	default:
		return nil, false
	}
}

func asString(value any) (string, bool) {
	valueString, ok := value.(string)
	return valueString, ok
}

func asBool(value any) (*bool, bool) {
	valueBool, ok := value.(bool)
	if !ok {
		return nil, false
	}
	return boolPtr(valueBool), true
}

func stringsFrom(value any) []string {
	slice, ok := asSlice(value)
	if !ok {
		if single, singleOK := asString(value); singleOK {
			return []string{single}
		}
		return nil
	}
	result := make([]string, 0, len(slice))
	for _, item := range slice {
		if text, itemOK := asString(item); itemOK {
			result = append(result, text)
		}
	}
	return result
}

func sortedMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func parseSkillFrontmatter(path, fallback string) (name, description string) {
	name = fallback
	f, err := os.Open(path)
	if err != nil {
		return name, ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(io.LimitReader(f, 128<<10))
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return name, ""
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "---" {
			break
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "name":
			if value != "" {
				name = value
			}
		case "description":
			description = value
		}
	}
	return name, description
}

func sanitizeURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

var (
	npmCoordinate = regexp.MustCompile(`^(?:@[a-z0-9._-]+/)?[a-z0-9._-]+(?:@[A-Za-z0-9*+._~^<>=|-]+)?$`)
	pythonCoord   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(?:(?:==|@)[A-Za-z0-9*+._~^<>=|-]+)?$`)
)

func runtimeSource(command string, args []string) (SourceRef, map[string]string) {
	executable := filepath.Base(strings.TrimSpace(command))
	metadata := map[string]string{}
	if executable != "" {
		metadata["executable"] = executable
	}
	switch strings.ToLower(executable) {
	case "npx":
		if pkg, version := firstRuntimePackage(args, "npm"); pkg != "" {
			return SourceRef{Kind: "npm", Locator: pkg, Package: pkg, Version: version}, metadata
		}
	case "uvx":
		if pkg, version := firstRuntimePackage(args, "python"); pkg != "" {
			return SourceRef{Kind: "pypi", Locator: pkg, Package: pkg, Version: version}, metadata
		}
	case "node", "python", "python3", "bash", "sh":
		if script := firstSafePathArg(args); script != "" {
			return SourceRef{Kind: "local", Locator: script}, metadata
		}
	}
	if filepath.IsAbs(command) {
		return SourceRef{Kind: "local", Locator: filepath.Clean(command)}, metadata
	}
	return SourceRef{Kind: "executable", Locator: executable}, metadata
}

func firstRuntimePackage(args []string, ecosystem string) (string, string) {
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			if safeRuntimeFlag(ecosystem, arg) {
				continue
			}
			// Unknown runtime flags may consume the following argument. Treat
			// the entire argv as opaque instead of risking recording a token as
			// a package coordinate.
			return "", ""
		}
		if ecosystem == "npm" && npmCoordinate.MatchString(arg) {
			return splitNPMCoordinate(arg)
		}
		if ecosystem == "python" && pythonCoord.MatchString(arg) {
			return splitPythonCoordinate(arg)
		}
		return "", ""
	}
	return "", ""
}

func safeRuntimeFlag(ecosystem, value string) bool {
	switch ecosystem {
	case "npm":
		switch value {
		case "-y", "--yes", "--quiet", "--no-install", "--ignore-existing":
			return true
		}
	case "python":
		switch value {
		case "--isolated", "--refresh", "--no-cache", "--offline", "--quiet":
			return true
		}
	}
	return false
}

func splitNPMCoordinate(raw string) (string, string) {
	if strings.HasPrefix(raw, "@") {
		slash := strings.IndexByte(raw, '/')
		if slash < 0 {
			return strings.ToLower(raw), ""
		}
		if at := strings.LastIndexByte(raw[slash+1:], '@'); at >= 0 {
			absolute := slash + 1 + at
			return strings.ToLower(raw[:absolute]), raw[absolute+1:]
		}
		return strings.ToLower(raw), ""
	}
	if at := strings.LastIndexByte(raw, '@'); at > 0 {
		return strings.ToLower(raw[:at]), raw[at+1:]
	}
	return strings.ToLower(raw), ""
}

func splitPythonCoordinate(raw string) (string, string) {
	for _, separator := range []string{"==", "@"} {
		if index := strings.Index(raw, separator); index > 0 {
			return normalizePythonPackage(raw[:index]), raw[index+len(separator):]
		}
	}
	return normalizePythonPackage(raw), ""
}

// StripRuntimeCarrierArgs converts an observed npx/uvx command into argv for
// the managed package executable. Only a recognized prefix for the exact same
// package is removed; unknown option shapes fail closed.
func StripRuntimeCarrierArgs(command string, args []string, sourceKind, packageName, executableName string) ([]string, error) {
	carrier := strings.ToLower(filepath.Base(strings.TrimSpace(command)))
	ecosystem := ""
	expectedSource := ""
	switch carrier {
	case "npx":
		ecosystem, expectedSource = "npm", "npm"
	case "uvx":
		ecosystem, expectedSource = "python", "pypi"
	default:
		return append([]string(nil), args...), nil
	}
	if sourceKind != expectedSource {
		return nil, errors.New("host: runtime carrier does not match the managed source")
	}
	for index := 0; index < len(args); index++ {
		arg := strings.TrimSpace(args[index])
		if arg == "" || arg == "--" {
			continue
		}
		packageFlag := carrier == "npx" && (arg == "--package" || arg == "-p") || carrier == "uvx" && arg == "--from"
		if packageFlag {
			if index+1 >= len(args) || !runtimePackageMatches(ecosystem, args[index+1], packageName) {
				return nil, errors.New("host: runtime carrier package does not match the managed source")
			}
			index += 2
			if index < len(args) && filepath.Base(args[index]) == filepath.Base(executableName) {
				index++
			}
			if index < len(args) && args[index] == "--" {
				index++
			}
			return append([]string(nil), args[index:]...), nil
		}
		if strings.HasPrefix(arg, "-") {
			if safeRuntimeFlag(ecosystem, arg) {
				continue
			}
			return nil, errors.New("host: runtime carrier has an unsupported option before the package")
		}
		if !runtimePackageMatches(ecosystem, arg, packageName) {
			return nil, errors.New("host: runtime carrier package does not match the managed source")
		}
		return append([]string(nil), args[index+1:]...), nil
	}
	return nil, errors.New("host: runtime carrier package argument is missing")
}

func runtimePackageMatches(ecosystem, coordinate, expected string) bool {
	if ecosystem == "npm" {
		name, _ := splitNPMCoordinate(strings.TrimSpace(coordinate))
		return strings.EqualFold(name, expected)
	}
	name, _ := splitPythonCoordinate(strings.TrimSpace(coordinate))
	return name == normalizePythonPackage(expected)
}

func normalizePythonPackage(value string) string {
	value = strings.ToLower(value)
	return regexp.MustCompile(`[-_.]+`).ReplaceAllString(value, "-")
}

func firstSafePathArg(args []string) string {
	for _, arg := range args {
		if arg == "" {
			continue
		}
		if strings.HasPrefix(arg, "-") || strings.ContainsAny(arg, "\x00\r\n") {
			return ""
		}
		if filepath.IsAbs(arg) || strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../") {
			return filepath.Clean(arg)
		}
		return ""
	}
	return ""
}

func secretRefs(config map[string]any) []SecretRef {
	var refs []SecretRef
	for _, field := range []string{
		"api_key", "apiKey", "authorization", "authorization_token",
		"client_secret", "clientSecret", "password", "token",
	} {
		if _, present := config[field]; present {
			refs = append(refs, SecretRef{Field: field, Present: true})
		}
	}
	for _, field := range []string{"env"} {
		if values, ok := asMap(config[field]); ok {
			for _, name := range sortedMapKeys(values) {
				refs = append(refs, SecretRef{Field: field, Name: name, Present: true})
			}
		}
	}
	if values, ok := asSlice(config["env_vars"]); ok {
		for _, value := range values {
			if name, nameOK := asString(value); nameOK {
				refs = append(refs, SecretRef{Field: "env_vars", Name: name, Present: true})
				continue
			}
			if object, objectOK := asMap(value); objectOK {
				if name, nameOK := asString(object["name"]); nameOK {
					refs = append(refs, SecretRef{Field: "env_vars", Name: name, Present: true})
				}
			}
		}
	}
	for _, field := range []string{"headers", "http_headers"} {
		if values, ok := asMap(config[field]); ok {
			for _, name := range sortedMapKeys(values) {
				refs = append(refs, SecretRef{Field: field, Name: name, Present: true})
			}
		}
	}
	if values, ok := asMap(config["env_http_headers"]); ok {
		for _, header := range sortedMapKeys(values) {
			envName, _ := asString(values[header])
			name := header
			if envName != "" {
				name += "<-" + envName
			}
			refs = append(refs, SecretRef{Field: "env_http_headers", Name: name, Present: true})
		}
	}
	for _, field := range []string{"bearer_token_env_var", "headersHelper", "headers_helper"} {
		if value, ok := config[field]; ok {
			name, _ := asString(value)
			if strings.Contains(strings.ToLower(field), "helper") {
				name = ""
			}
			refs = append(refs, SecretRef{Field: field, Name: name, Present: true})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Field != refs[j].Field {
			return refs[i].Field < refs[j].Field
		}
		return refs[i].Name < refs[j].Name
	})
	return refs
}

func projectChain(path string) []string {
	path = filepath.Clean(path)
	root := discoverRepoRoot(path)
	if root == "" {
		root = path
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return []string{path}
	}
	result := []string{root}
	if rel == "." {
		return result
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		result = append(result, current)
	}
	return result
}

func discoverRepoRoot(start string) string {
	current := filepath.Clean(start)
	for {
		if exists(filepath.Join(current, ".git")) {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

func resolveWithin(root, configured string) (string, bool) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return "", false
	}
	if filepath.IsAbs(configured) {
		return "", false
	}
	resolved := filepath.Clean(filepath.Join(root, configured))
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return resolved, true
}

func checkContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
