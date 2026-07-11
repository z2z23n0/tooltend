package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func scanPluginCache(ctx context.Context, c *collector, cacheRoot string) {
	marketplaces, err := os.ReadDir(cacheRoot)
	if err != nil {
		if !os.IsNotExist(err) {
			c.warn("plugin_cache_unreadable", err.Error(), cacheRoot, "")
		}
		return
	}
	for _, marketplaceEntry := range marketplaces {
		if checkContext(ctx) != nil {
			return
		}
		if !marketplaceEntry.IsDir() {
			continue
		}
		marketplace := marketplaceEntry.Name()
		marketplaceRoot := filepath.Join(cacheRoot, marketplace)
		plugins, readErr := os.ReadDir(marketplaceRoot)
		if readErr != nil {
			c.warn("plugin_cache_unreadable", readErr.Error(), marketplaceRoot, "")
			continue
		}
		for _, pluginEntry := range plugins {
			if !pluginEntry.IsDir() {
				continue
			}
			pluginName := pluginEntry.Name()
			pluginRoot := filepath.Join(marketplaceRoot, pluginName)
			versions, versionErr := os.ReadDir(pluginRoot)
			if versionErr != nil {
				c.warn("plugin_cache_unreadable", versionErr.Error(), pluginRoot, "")
				continue
			}
			for _, versionEntry := range versions {
				if !versionEntry.IsDir() {
					continue
				}
				version := versionEntry.Name()
				root := filepath.Join(pluginRoot, version)
				source := SourceRef{Kind: "plugin_cache", Locator: marketplace + "/" + pluginName, Version: version}
				scanPluginDirectory(ctx, c, root, ScopePlugin, "", source, false)
			}
		}
	}
}

func scanPluginDirectory(ctx context.Context, c *collector, root string, scope Scope, project string, fallbackSource SourceRef, legacy bool) {
	if checkContext(ctx) != nil {
		return
	}
	manifestPath := filepath.Join(root, ".codex-plugin", "plugin.json")
	manifestRequired := c.host == Codex
	if c.host == Claude {
		manifestPath = filepath.Join(root, ".claude-plugin", "plugin.json")
		manifestRequired = false
	}
	manifest := make(map[string]any)
	if exists(manifestPath) {
		parsed, err := readJSONMap(manifestPath)
		if err != nil {
			c.warn("plugin_manifest_invalid", "plugin manifest could not be parsed", manifestPath, project)
		} else {
			manifest = parsed
			c.source(manifestPath, "json", scope, project, "plugin_manifest", 0, legacy)
		}
	} else if manifestRequired {
		c.warn("plugin_manifest_missing", "Codex plugin cache entry has no .codex-plugin/plugin.json", root, project)
	}

	name, _ := asString(manifest["name"])
	if name == "" {
		name = filepath.Base(root)
	}
	version, _ := asString(manifest["version"])
	if version == "" {
		version = fallbackSource.Version
	}
	source := fallbackSource
	if repository, ok := asString(manifest["repository"]); ok {
		if sanitized := sanitizeURL(repository); sanitized != "" {
			source = SourceRef{Kind: "git", Locator: sanitized, Version: version}
		}
	}
	if source.Kind == "" {
		source = SourceRef{Kind: "local", Locator: root, Version: version}
	}
	metadata := map[string]string{}
	if description, ok := asString(manifest["description"]); ok && description != "" {
		metadata["description"] = description
	}
	if fallbackSource.Kind == "plugin_cache" {
		metadata["cache_id"] = fallbackSource.Locator
	}
	evidencePath := manifestPath
	if !exists(manifestPath) {
		evidencePath = root
	}
	pluginEvidence := Evidence{Path: evidencePath, Format: "json", Layer: "plugin_manifest"}
	c.observe(Observation{
		Kind: ComponentPlugin, Name: name, Version: version, Path: root, Scope: scope,
		Project: project, Legacy: legacy, Source: source, Metadata: metadata,
		Evidence: []Evidence{pluginEvidence},
	}, evidencePath)

	skillPaths := manifestPaths(manifest["skills"])
	if len(skillPaths) == 0 {
		skillPaths = []string{"./skills"}
	}
	for _, configured := range skillPaths {
		resolved, ok := resolveWithin(root, configured)
		if !ok {
			c.warn("plugin_path_escape", "plugin skill path escapes the plugin root", manifestPath, project)
			continue
		}
		scanSkillDirectory(ctx, c, resolved, ScopePlugin, project, legacy, false)
	}

	if c.host == Claude {
		commands := manifestPaths(manifest["commands"])
		if len(commands) == 0 && exists(filepath.Join(root, "commands")) {
			scanLegacyCommands(ctx, c, filepath.Join(root, "commands"), ScopePlugin, project)
		} else {
			for _, commandPath := range commands {
				resolved, ok := resolveWithin(root, commandPath)
				if !ok {
					c.warn("plugin_path_escape", "plugin command path escapes the plugin root", manifestPath, project)
					continue
				}
				if info, err := os.Stat(resolved); err == nil && info.IsDir() {
					scanLegacyCommands(ctx, c, resolved, ScopePlugin, project)
				} else if exists(resolved) {
					scanLegacyCommandFile(c, resolved, project)
				}
			}
		}
	}

	scanPluginHooks(c, root, manifest, manifestPath, name, scope, project, legacy)
	scanPluginMCP(c, root, manifest, manifestPath, name, scope, project)
}

func manifestPaths(value any) []string {
	if text, ok := asString(value); ok {
		return []string{text}
	}
	return stringsFrom(value)
}

func scanLegacyCommandFile(c *collector, path, project string) {
	name, description := parseSkillFrontmatter(path, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	metadata := map[string]string{"compatibility": "legacy_command"}
	if description != "" {
		metadata["description"] = description
	}
	source := SourceRef{Kind: "local", Locator: path}
	c.observe(Observation{
		Kind: ComponentSkill, Name: name, Path: path, Scope: ScopePlugin, Project: project,
		Legacy: true, Source: source, Metadata: metadata,
		Evidence: []Evidence{{Path: path, Format: "markdown", Layer: "plugin_command"}},
	}, path)
}

func scanPluginHooks(c *collector, root string, manifest map[string]any, manifestPath, pluginName string, scope Scope, project string, legacy bool) {
	hooksValue, hasManifestHooks := manifest["hooks"]
	if hasManifestHooks {
		if inline, ok := asMap(hooksValue); ok {
			scanHooksDocument(c, inline, Evidence{Path: manifestPath, Format: "json", Layer: "plugin_hook", Pointer: "hooks"}, scope, project, legacy)
			return
		}
		if inlineList, ok := asSlice(hooksValue); ok {
			containsInline := false
			for index, entry := range inlineList {
				if inline, inlineOK := asMap(entry); inlineOK {
					containsInline = true
					scanHooksDocument(c, inline, Evidence{Path: manifestPath, Format: "json", Layer: "plugin_hook", Pointer: fmt.Sprintf("hooks/%d", index)}, scope, project, legacy)
				}
			}
			if containsInline {
				return
			}
		}
	}
	paths := manifestPaths(hooksValue)
	if len(paths) == 0 {
		paths = []string{"./hooks/hooks.json"}
	}
	for _, configured := range paths {
		path, ok := resolveWithin(root, configured)
		if !ok {
			c.warn("plugin_path_escape", "plugin hook path escapes the plugin root", manifestPath, project)
			continue
		}
		if !exists(path) {
			continue
		}
		document, err := readJSONMap(path)
		if err != nil {
			c.warn("plugin_hooks_invalid", "plugin hooks configuration could not be parsed", path, project)
			continue
		}
		c.source(path, "json", scope, project, "plugin_hooks", 0, legacy)
		scanHooksDocument(c, document, Evidence{Path: path, Format: "json", Layer: "plugin_hook", Pointer: "hooks"}, scope, project, legacy)
	}
}

func scanPluginMCP(c *collector, root string, manifest map[string]any, manifestPath, pluginName string, scope Scope, project string) {
	value, hasValue := manifest["mcpServers"]
	if hasValue {
		if inline, ok := asMap(value); ok {
			scanMCPServers(c, inline, Evidence{Path: manifestPath, Format: "json", Layer: "plugin_mcp", Pointer: "mcpServers"}, scope, project, pluginName)
			return
		}
	}
	paths := manifestPaths(value)
	if len(paths) == 0 {
		paths = []string{"./.mcp.json"}
	}
	for _, configured := range paths {
		path, ok := resolveWithin(root, configured)
		if !ok {
			c.warn("plugin_path_escape", "plugin MCP path escapes the plugin root", manifestPath, project)
			continue
		}
		if !exists(path) {
			continue
		}
		document, err := readJSONMap(path)
		if err != nil {
			c.warn("plugin_mcp_invalid", "plugin MCP configuration could not be parsed", path, project)
			continue
		}
		c.source(path, "json", scope, project, "plugin_mcp", 0, false)
		if servers, wrapped := asMap(document["mcp_servers"]); wrapped {
			scanMCPServers(c, servers, Evidence{Path: path, Format: "json", Layer: "plugin_mcp", Pointer: "mcp_servers"}, scope, project, pluginName)
		} else if servers, wrapped := asMap(document["mcpServers"]); wrapped {
			scanMCPServers(c, servers, Evidence{Path: path, Format: "json", Layer: "plugin_mcp", Pointer: "mcpServers"}, scope, project, pluginName)
		} else {
			scanMCPServers(c, document, Evidence{Path: path, Format: "json", Layer: "plugin_mcp"}, scope, project, pluginName)
		}
	}
}

func scanMarketplaceFile(ctx context.Context, c *collector, path string, scope Scope, project string, legacy bool) {
	if !exists(path) {
		return
	}
	document, err := readJSONMap(path)
	if err != nil {
		c.warn("marketplace_invalid", "plugin marketplace could not be parsed", path, project)
		return
	}
	c.source(path, "json", scope, project, "plugin_marketplace", 0, legacy)
	marketplaceName, _ := asString(document["name"])
	plugins, ok := asSlice(document["plugins"])
	if !ok {
		return
	}
	root := marketplaceRoot(path)
	for index, pluginValue := range plugins {
		if checkContext(ctx) != nil {
			return
		}
		plugin, pluginOK := asMap(pluginValue)
		if !pluginOK {
			continue
		}
		name, _ := asString(plugin["name"])
		if name == "" {
			continue
		}
		source := marketplacePluginSource(root, plugin["source"])
		version, _ := asString(plugin["version"])
		if version == "" {
			if source.Version != "" {
				version = source.Version
			}
		}
		metadata := map[string]string{"catalog_only": "true"}
		if marketplaceName != "" {
			metadata["marketplace"] = marketplaceName
		}
		if description, ok := asString(plugin["description"]); ok && description != "" {
			metadata["description"] = description
		}
		evidence := Evidence{Path: path, Format: "json", Layer: "plugin_marketplace", Pointer: fmt.Sprintf("plugins/%d", index)}
		c.observe(Observation{
			Kind: ComponentPlugin, Name: name, Version: version, Path: source.Locator,
			Scope: scope, Project: project, Legacy: legacy, Source: source,
			Metadata: metadata, Evidence: []Evidence{evidence},
		}, path)
		if source.Kind == "local" && source.Locator != "" && exists(source.Locator) {
			scanPluginDirectory(ctx, c, source.Locator, scope, project, source, legacy)
		}
	}
}

func marketplaceRoot(path string) string {
	dir := filepath.Dir(path)
	base := filepath.Base(dir)
	if base == ".claude-plugin" {
		return filepath.Dir(dir)
	}
	if base == "plugins" && filepath.Base(filepath.Dir(dir)) == ".agents" {
		return filepath.Dir(filepath.Dir(dir))
	}
	return dir
}

func marketplacePluginSource(root string, value any) SourceRef {
	if text, ok := asString(value); ok {
		if resolved, inside := resolveWithin(root, text); inside {
			return SourceRef{Kind: "local", Locator: resolved}
		}
		return SourceRef{Kind: "unknown"}
	}
	config, ok := asMap(value)
	if !ok {
		return SourceRef{Kind: "unknown"}
	}
	kind, _ := asString(config["source"])
	path, _ := asString(config["path"])
	ref, _ := asString(config["ref"])
	sha, _ := asString(config["sha"])
	if sha != "" {
		ref = sha
	}
	version, _ := asString(config["version"])
	switch kind {
	case "local", "directory", "file":
		if resolved, inside := resolveWithin(root, path); inside {
			return SourceRef{Kind: "local", Locator: resolved, Ref: ref, Version: version}
		}
	case "github":
		repo, _ := asString(config["repo"])
		if repo != "" && !strings.ContainsAny(repo, "?#@") {
			return SourceRef{Kind: "git", Locator: "https://github.com/" + strings.TrimSuffix(repo, ".git"), Ref: ref, Version: version}
		}
	case "url", "git-subdir":
		rawURL, _ := asString(config["url"])
		return SourceRef{Kind: "git", Locator: sanitizeURL(rawURL), Subdir: path, Ref: ref, Version: version}
	case "npm":
		pkg, _ := asString(config["package"])
		if npmCoordinate.MatchString(pkg) {
			return SourceRef{Kind: "npm", Locator: strings.ToLower(pkg), Package: strings.ToLower(pkg), Version: version}
		}
	default:
		if path != "" {
			if resolved, inside := resolveWithin(root, path); inside {
				return SourceRef{Kind: "local", Locator: resolved, Ref: ref, Version: version}
			}
		}
	}
	return SourceRef{Kind: "unknown", Ref: ref, Version: version}
}

func scanKnownMarketplaces(c *collector, path string) {
	if !exists(path) {
		return
	}
	document, err := readJSONMap(path)
	if err != nil {
		c.warn("known_marketplaces_invalid", "known marketplaces configuration could not be parsed", path, "")
		return
	}
	c.source(path, "json", ScopeUser, "", "known_marketplaces", 0, false)
	// Do not retain arbitrary marketplace records: older Claude versions stored
	// auth-bearing clone metadata here. Names are sufficient discovery evidence.
	for _, name := range sortedMapKeys(document) {
		source := SourceRef{Kind: "marketplace", Locator: name}
		c.observe(Observation{
			Kind: ComponentPlugin, Name: name, Scope: ScopeUser, Source: source,
			Metadata: map[string]string{"marketplace_registration": "true"},
			Evidence: []Evidence{{Path: path, Format: "json", Layer: "known_marketplaces", Pointer: name}},
		}, path)
	}
}
