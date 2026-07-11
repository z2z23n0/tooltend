package hook

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
)

const MaxInputBytes = 64 << 10

type Input struct {
	SessionID     string          `json:"session_id"`
	TurnID        string          `json:"turn_id,omitempty"`
	CWD           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	Source        string          `json:"source,omitempty"`
	ToolName      string          `json:"tool_name,omitempty"`
	ToolUseID     string          `json:"tool_use_id,omitempty"`
	ToolInput     json.RawMessage `json:"tool_input,omitempty"`
}

type ToolInput struct {
	Command string `json:"command"`
}

type InstallerIntent struct {
	Installer        string `json:"installer"`
	PackageIdentity  string `json:"package_identity"`
	RequestedVersion string `json:"requested_version,omitempty"`
}

func DecodeInput(r io.Reader) (Input, error) {
	limited := io.LimitReader(r, MaxInputBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return Input{}, err
	}
	if len(data) > MaxInputBytes {
		return Input{}, errors.New("hook input exceeds size limit")
	}
	var input Input
	if err := json.Unmarshal(data, &input); err != nil {
		return Input{}, err
	}
	return input, nil
}

func ParseInstallerIntent(input Input) (InstallerIntent, bool) {
	if input.ToolName != "Bash" && input.ToolName != "bash" {
		return InstallerIntent{}, false
	}
	var tool ToolInput
	if len(input.ToolInput) == 0 || json.Unmarshal(input.ToolInput, &tool) != nil {
		return InstallerIntent{}, false
	}
	argv, ok := literalArgv(tool.Command)
	if !ok || len(argv) < 2 {
		return InstallerIntent{}, false
	}
	return parseArgv(argv)
}

var unsafeShell = strings.NewReplacer(
	"'", "x", "\"", "x", "\\", "x", "$", "x", "`", "x",
	";", "x", "&", "x", "|", "x", "<", "x", ">", "x",
	"(", "x", ")", "x", "{", "x", "}", "x", "\n", "x", "\r", "x",
)

func literalArgv(command string) ([]string, bool) {
	if command == "" || unsafeShell.Replace(command) != command {
		return nil, false
	}
	argv := strings.Fields(command)
	if len(argv) == 0 {
		return nil, false
	}
	for _, arg := range argv {
		if strings.ContainsRune(arg, '\x00') {
			return nil, false
		}
	}
	return argv, true
}

func parseArgv(argv []string) (InstallerIntent, bool) {
	command := baseCommand(argv[0])
	switch command {
	case "npm":
		if len(argv) < 3 || (argv[1] != "install" && argv[1] != "i" && argv[1] != "add") {
			return InstallerIntent{}, false
		}
		return firstPackage("npm", argv[2:])
	case "npx":
		return firstPackage("npx", argv[1:])
	case "pipx":
		if len(argv) < 3 || argv[1] != "install" {
			return InstallerIntent{}, false
		}
		return firstPackage("pipx", argv[2:])
	case "uv":
		if len(argv) < 4 || argv[1] != "tool" || argv[2] != "install" {
			return InstallerIntent{}, false
		}
		return firstPackage("uv", argv[3:])
	case "uvx":
		return firstPackage("uvx", argv[1:])
	case "brew":
		if len(argv) < 3 || argv[1] != "install" {
			return InstallerIntent{}, false
		}
		return firstPackage("homebrew", argv[2:])
	default:
		return InstallerIntent{}, false
	}
}

func firstPackage(installer string, args []string) (InstallerIntent, bool) {
	for _, arg := range args {
		if arg == "" {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			if safeInstallerFlag(installer, arg) {
				continue
			}
			// Unknown flags may consume the following argv. Refuse the entire
			// observation rather than risking a token becoming a package name.
			return InstallerIntent{}, false
		}
		name, version, ok := packageCoordinate(installer, arg)
		if !ok {
			return InstallerIntent{}, false
		}
		return InstallerIntent{Installer: installer, PackageIdentity: name, RequestedVersion: version}, true
	}
	return InstallerIntent{}, false
}

func safeInstallerFlag(installer, value string) bool {
	switch installer {
	case "npm":
		switch value {
		case "-g", "--global", "-D", "--save-dev", "-E", "--save-exact", "--ignore-scripts", "--no-audit", "--no-fund":
			return true
		}
	case "npx":
		switch value {
		case "-y", "--yes", "--quiet", "--no-install", "--ignore-existing":
			return true
		}
	case "pipx":
		switch value {
		case "--force", "--include-deps", "--pre":
			return true
		}
	case "uv":
		switch value {
		case "--force", "--reinstall", "--upgrade", "--no-cache", "--offline", "--quiet":
			return true
		}
	case "uvx":
		switch value {
		case "--isolated", "--refresh", "--no-cache", "--offline", "--quiet":
			return true
		}
	case "homebrew":
		switch value {
		case "--formula", "--cask", "--quiet":
			return true
		}
	}
	return false
}

var (
	npmPackage = regexp.MustCompile(`^(?:@[a-z0-9._-]+/)?[a-z0-9._-]+$`)
	pyPackage  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	brewPkg    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@+._/-]*$`)
)

func packageCoordinate(installer, raw string) (string, string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "?#") {
		return "", "", false
	}
	switch installer {
	case "npm", "npx":
		name, version := splitNPMVersion(raw)
		if !npmPackage.MatchString(strings.ToLower(name)) {
			return "", "", false
		}
		return strings.ToLower(name), version, true
	case "pipx", "uv", "uvx":
		name, version := splitPythonVersion(raw)
		if !pyPackage.MatchString(name) {
			return "", "", false
		}
		return normalizePython(name), version, true
	case "homebrew":
		if !brewPkg.MatchString(raw) {
			return "", "", false
		}
		return strings.ToLower(raw), "", true
	default:
		return "", "", false
	}
}

func splitNPMVersion(raw string) (string, string) {
	if strings.HasPrefix(raw, "@") {
		slash := strings.IndexByte(raw, '/')
		if slash < 0 {
			return raw, ""
		}
		if at := strings.LastIndex(raw[slash+1:], "@"); at >= 0 {
			absolute := slash + 1 + at
			return raw[:absolute], raw[absolute+1:]
		}
		return raw, ""
	}
	if at := strings.LastIndexByte(raw, '@'); at > 0 {
		return raw[:at], raw[at+1:]
	}
	return raw, ""
}

func splitPythonVersion(raw string) (string, string) {
	for _, separator := range []string{"==", "@"} {
		if index := strings.Index(raw, separator); index > 0 {
			return raw[:index], raw[index+len(separator):]
		}
	}
	return raw, ""
}

func normalizePython(name string) string {
	name = strings.ToLower(name)
	replacer := regexp.MustCompile(`[-_.]+`)
	return replacer.ReplaceAllString(name, "-")
}

func baseCommand(path string) string {
	if index := strings.LastIndexAny(path, `/\\`); index >= 0 {
		return path[index+1:]
	}
	return path
}
