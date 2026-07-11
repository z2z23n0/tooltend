package adapter

import (
	"errors"
	"net/url"
	"strings"
)

// CanonicalizeSource is the single source-identity boundary shared by
// discovery persistence and lifecycle adapters. Version/ref data is excluded:
// identity is only kind + canonical locator/package/subdir.
func CanonicalizeSource(kind SourceKind, source Source) (Source, error) {
	source.Kind = kind
	switch kind {
	case SourceGit:
		locator, err := normalizeGitLocator(source.Locator)
		if err != nil {
			return Source{}, err
		}
		subdir, err := ValidateSubdir(source.Subdir)
		if err != nil {
			return Source{}, err
		}
		source.Locator, source.Subdir = locator, subdir
		return source, nil
	case SourceNPM:
		name := strings.ToLower(strings.TrimSpace(source.PackageName))
		if name == "" {
			name = strings.ToLower(strings.TrimSpace(source.Locator))
		}
		parsed, _, ok := packageCoordinate("npm", name)
		if !ok || parsed != name {
			return Source{}, errors.New("invalid npm package identity")
		}
		source.PackageName, source.Locator = name, "https://registry.npmjs.org/"+name
		return source, nil
	case SourcePyPI:
		name := source.PackageName
		if name == "" {
			name = source.Locator
		}
		name = normalizePython(name)
		if !pyPackage.MatchString(name) {
			return Source{}, errors.New("invalid Python package identity")
		}
		source.PackageName, source.Locator = name, "https://pypi.org/project/"+name
		return source, nil
	case SourceHomebrew:
		name := strings.ToLower(strings.TrimSpace(source.PackageName))
		if name == "" {
			name = strings.ToLower(strings.TrimSpace(source.Locator))
		}
		if !brewPkg.MatchString(name) {
			return Source{}, errors.New("invalid Homebrew formula identity")
		}
		source.PackageName, source.Locator = name, name
		return source, nil
	case SourceHTTP:
		parsed, err := url.Parse(strings.TrimSpace(source.Locator))
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
			return Source{}, errors.New("remote MCP locator must be an HTTPS URL without user info")
		}
		parsed.RawQuery, parsed.Fragment = "", ""
		source.Locator = parsed.String()
		return source, nil
	default:
		return Source{}, errors.New("source kind has no canonical managed identity")
	}
}
