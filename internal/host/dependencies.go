package host

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxDependencyDocumentBytes = 256 << 10

var packageToken = regexp.MustCompile(`^(?:@[a-zA-Z0-9._-]+/)?[a-zA-Z0-9][a-zA-Z0-9._/-]*(?:@[a-zA-Z0-9*+._~^<>=|-]+)?$`)
var dependencyConstraint = regexp.MustCompile(`^[a-zA-Z0-9*+._~^<>=!|-]*$`)

var dependencyCarriers = map[string]struct{}{
	"bash": {}, "brew": {}, "bun": {}, "cargo": {}, "cd": {}, "curl": {}, "env": {}, "git": {}, "gh": {},
	"go": {}, "make": {}, "node": {}, "npm": {}, "npx": {}, "pip": {}, "pip3": {}, "pipx": {}, "python": {},
	"python3": {}, "sh": {}, "sudo": {}, "uv": {}, "uvx": {}, "wget": {}, "zsh": {},
}

// scanExplicitDependencies reads only the bounded SKILL.md itself. It accepts
// package-manager forms with an explicit package, plus literal commands in a
// shell code block. Generic carrier invocations are deliberately excluded.
func scanExplicitDependencies(path, pathEnv string) []DependencyRef {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	reader := bufio.NewScanner(io.LimitReader(file, maxDependencyDocumentBytes))
	reader.Buffer(make([]byte, 16<<10), 256<<10)
	inShellFence := false
	lineNumber := 0
	seen := map[string]struct{}{}
	var result []DependencyRef
	add := func(identity, constraint string, source SourceRef, executable, carrier string) {
		identity = strings.TrimSpace(identity)
		if identity == "" {
			return
		}
		key := identity + "\x00" + constraint + "\x00" + source.Kind + "\x00" + source.Package
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		installPath := locateDependencyExecutable(executable, pathEnv)
		result = append(result, DependencyRef{
			PackageIdentity: identity, Constraint: constraint, Source: source,
			Executable: executable, InstallPath: installPath, Carrier: carrier,
			EvidencePath: path, EvidenceLine: lineNumber,
		})
	}
	for reader.Scan() {
		lineNumber++
		line := strings.TrimSpace(reader.Text())
		if strings.HasPrefix(line, "```") {
			language := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "```")))
			if inShellFence {
				inShellFence = false
			} else {
				inShellFence = language == "sh" || language == "bash" || language == "shell" || language == "zsh" || language == "console"
			}
			continue
		}
		fields := shellLikeFields(line)
		if len(fields) == 0 {
			continue
		}
		commandIndex := 0
		if fields[0] == "$" || fields[0] == ">" {
			commandIndex++
		}
		for commandIndex < len(fields) && strings.Contains(fields[commandIndex], "=") {
			commandIndex++
		}
		if commandIndex >= len(fields) {
			continue
		}
		command := strings.ToLower(filepath.Base(fields[commandIndex]))
		args := fields[commandIndex+1:]
		if identity, constraint, source, executable, ok := explicitPackageDependency(command, args); ok {
			add(identity, constraint, source, executable, command)
			continue
		}
		if !inShellFence || strings.Contains(fields[commandIndex], "/") || strings.HasPrefix(fields[commandIndex], "-") {
			continue
		}
		if _, carrier := dependencyCarriers[command]; carrier {
			continue
		}
		if packageToken.MatchString(command) {
			add("cli:"+command, "", SourceRef{Kind: "unknown", Package: command}, command, "direct")
		}
	}
	return result
}

func explicitPackageDependency(command string, args []string) (string, string, SourceRef, string, bool) {
	kind := ""
	executable := ""
	switch command {
	case "npx":
		kind = "npm"
	case "uvx":
		kind = "pypi"
	case "pipx":
		if len(args) == 0 || args[0] != "install" {
			return "", "", SourceRef{}, "", false
		}
		kind, args = "pypi", args[1:]
	case "uv":
		if len(args) < 2 || args[0] != "tool" || args[1] != "install" {
			return "", "", SourceRef{}, "", false
		}
		kind, args = "pypi", args[2:]
	case "npm":
		if len(args) == 0 || (args[0] != "install" && args[0] != "i") {
			return "", "", SourceRef{}, "", false
		}
		kind, args = "npm", args[1:]
	case "brew":
		if len(args) == 0 || args[0] != "install" {
			return "", "", SourceRef{}, "", false
		}
		kind, args = "homebrew", args[1:]
	default:
		return "", "", SourceRef{}, "", false
	}
	packageSpec := ""
	for index := 0; index < len(args); index++ {
		arg := strings.Trim(strings.TrimSpace(args[index]), `"'`)
		if arg == "--package" || arg == "-p" || command == "uvx" && arg == "--from" {
			if index+1 < len(args) {
				packageSpec = strings.Trim(args[index+1], `"'`)
				for _, candidate := range args[index+2:] {
					candidate = strings.Trim(strings.TrimSpace(candidate), `"'`)
					if candidate != "" && !strings.HasPrefix(candidate, "-") {
						executable = candidate
						break
					}
				}
			}
			break
		}
		if strings.HasPrefix(arg, "-") {
			if !safeDependencyFlag(command, arg) {
				return "", "", SourceRef{}, "", false
			}
			continue
		}
		if arg == "" {
			continue
		}
		packageSpec = arg
		break
	}
	if packageSpec == "" || strings.Contains(packageSpec, "..") {
		return "", "", SourceRef{}, "", false
	}
	name, constraint := splitPackageConstraint(kind, packageSpec)
	if name == "" || !packageToken.MatchString(name) || !dependencyConstraint.MatchString(constraint) {
		return "", "", SourceRef{}, "", false
	}
	if executable == "" {
		executable = dependencyExecutableName(name)
	}
	if _, carrier := dependencyCarriers[strings.ToLower(executable)]; carrier {
		executable = ""
	}
	version := constraint
	if kind == "pypi" {
		version = strings.TrimPrefix(version, "==")
	}
	return kind + ":" + strings.ToLower(name), constraint, SourceRef{Kind: kind, Package: name, Version: version}, executable, true
}

func safeDependencyFlag(command, value string) bool {
	allowed := map[string]map[string]struct{}{
		"npx": {
			"-y": {}, "--yes": {}, "--quiet": {}, "--no-install": {}, "--ignore-existing": {},
		},
		"uvx": {
			"--isolated": {}, "--refresh": {}, "--no-cache": {}, "--offline": {}, "--quiet": {},
		},
		"pipx": {
			"--force": {}, "--include-deps": {}, "--quiet": {}, "--verbose": {},
		},
		"uv": {
			"--force": {}, "--reinstall": {}, "--upgrade": {}, "--no-cache": {}, "--offline": {}, "--quiet": {},
		},
		"npm": {
			"-g": {}, "--global": {}, "-D": {}, "--save-dev": {}, "-E": {}, "--save-exact": {},
			"--ignore-scripts": {}, "--no-audit": {}, "--no-fund": {},
		},
		"brew": {
			"--cask": {}, "--formula": {}, "--force": {}, "--quiet": {}, "--verbose": {},
		},
	}
	_, ok := allowed[command][value]
	return ok
}

func dependencyExecutableName(packageName string) string {
	name := filepath.Base(strings.TrimSpace(packageName))
	if name == "." || name == string(filepath.Separator) || strings.ContainsAny(name, "/\\\x00\r\n") {
		return ""
	}
	return name
}

func locateDependencyExecutable(name, pathEnv string) string {
	name = strings.TrimSpace(name)
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00\r\n") {
		return ""
	}
	if _, carrier := dependencyCarriers[strings.ToLower(name)]; carrier {
		return ""
	}
	for _, directory := range filepath.SplitList(pathEnv) {
		if !filepath.IsAbs(directory) {
			continue
		}
		candidate := filepath.Join(filepath.Clean(directory), name)
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			continue
		}
		return candidate
	}
	return ""
}

func splitPackageConstraint(kind, value string) (string, string) {
	if kind == "npm" {
		if strings.HasPrefix(value, "@") {
			if slash := strings.IndexByte(value, '/'); slash > 0 {
				if at := strings.LastIndex(value[slash+1:], "@"); at >= 0 {
					absolute := slash + 1 + at
					return value[:absolute], value[absolute+1:]
				}
			}
			return value, ""
		}
		if at := strings.LastIndexByte(value, '@'); at > 0 {
			return value[:at], value[at+1:]
		}
	}
	if kind == "pypi" {
		for _, separator := range []string{"==", ">=", "<=", "~=", "!=", ">", "<"} {
			if index := strings.Index(value, separator); index > 0 {
				return value[:index], value[index:]
			}
		}
		if index := strings.LastIndexByte(value, '@'); index > 0 {
			return value[:index], value[index+1:]
		}
	}
	return value, ""
}

func shellLikeFields(line string) []string {
	if line == "" || strings.HasPrefix(line, "#") {
		return nil
	}
	line = strings.TrimPrefix(line, "$ ")
	return strings.Fields(line)
}
