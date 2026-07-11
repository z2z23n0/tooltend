package adapter

import (
	"regexp"
	"strings"
)

var (
	npmPackage = regexp.MustCompile(`^(?:@[a-z0-9._-]+/)?[a-z0-9._-]+$`)
	pyPackage  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	brewPkg    = regexp.MustCompile(`^[a-z0-9][a-z0-9@+._/-]*$`)
)

func packageCoordinate(installer, raw string) (string, string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "?#") {
		return "", "", false
	}
	switch installer {
	case "npm", "npx":
		name, version := splitNPMVersion(raw)
		name = strings.ToLower(name)
		return name, version, npmPackage.MatchString(name)
	case "pipx", "uv", "uvx", "pypi":
		name, version := splitPythonVersion(raw)
		name = normalizePython(name)
		return name, version, pyPackage.MatchString(name)
	case "homebrew":
		name := strings.ToLower(raw)
		return name, "", brewPkg.MatchString(name)
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
		if relative := strings.LastIndex(raw[slash+1:], "@"); relative >= 0 {
			index := slash + 1 + relative
			return raw[:index], raw[index+1:]
		}
		return raw, ""
	}
	if index := strings.LastIndexByte(raw, '@'); index > 0 {
		return raw[:index], raw[index+1:]
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
	name = strings.ToLower(strings.TrimSpace(name))
	return regexp.MustCompile(`[-_.]+`).ReplaceAllString(name, "-")
}
