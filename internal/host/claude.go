package host

import (
	"context"
	"path/filepath"
	"strings"
)

type ClaudeHost struct{}

func NewClaude() *ClaudeHost { return &ClaudeHost{} }

func (*ClaudeHost) Name() Name { return Claude }

func (h *ClaudeHost) Scan(ctx context.Context, options ScanOptions) (Result, error) {
	options, err := options.normalized()
	if err != nil {
		return Result{}, err
	}
	c := &collector{host: Claude, pathEnv: options.Getenv("PATH")}
	claudeDir := filepath.Join(options.HomeDir, ".claude")

	h.scanSettings(c, filepath.Join(options.ClaudeManagedDir, "managed-settings.json"), ScopeSystem, "", "managed_settings", 10)
	h.scanMCPFile(c, filepath.Join(options.ClaudeManagedDir, "managed-mcp.json"), ScopeSystem, "", "managed_mcp", 10, "mcpServers")
	h.scanSettings(c, filepath.Join(claudeDir, "settings.json"), ScopeUser, "", "user_settings", 50)
	h.scanGlobalState(c, filepath.Join(options.HomeDir, ".claude.json"), options.Projects)

	for _, project := range options.Projects {
		for index, directory := range projectChain(project) {
			h.scanSettings(c, filepath.Join(directory, ".claude", "settings.json"), ScopeProject, project, "project_settings", 40-index)
			h.scanSettings(c, filepath.Join(directory, ".claude", "settings.local.json"), ScopeLocal, project, "local_settings", 30-index)
		}
		root := discoverRepoRoot(project)
		if root == "" {
			root = project
		}
		h.scanMCPFile(c, filepath.Join(root, ".mcp.json"), ScopeProject, root, "project_mcp", 40, "mcpServers")
	}

	scanSkillDirectory(ctx, c, filepath.Join(claudeDir, "skills"), ScopeUser, "", false, true)
	scanLegacyCommands(ctx, c, filepath.Join(claudeDir, "commands"), ScopeUser, "")
	for _, project := range options.Projects {
		for _, directory := range projectChain(project) {
			scanSkillDirectory(ctx, c, filepath.Join(directory, ".claude", "skills"), ScopeProject, project, false, true)
			scanLegacyCommands(ctx, c, filepath.Join(directory, ".claude", "commands"), ScopeProject, project)
		}
	}

	pluginRoot := filepath.Join(claudeDir, "plugins")
	cacheRoot := filepath.Join(pluginRoot, "cache")
	if override := strings.TrimSpace(options.Getenv("CLAUDE_CODE_PLUGIN_CACHE_DIR")); override != "" {
		if filepath.IsAbs(override) {
			cacheRoot = filepath.Clean(override)
		} else {
			c.warn("plugin_cache_override_relative", "CLAUDE_CODE_PLUGIN_CACHE_DIR must be absolute for deterministic discovery", override, "")
		}
	}
	scanPluginCache(ctx, c, cacheRoot)
	scanKnownMarketplaces(c, filepath.Join(pluginRoot, "known_marketplaces.json"))

	return c.finish(), ctx.Err()
}

func (*ClaudeHost) scanSettings(c *collector, path string, scope Scope, project, layer string, precedence int) {
	if !exists(path) {
		return
	}
	document, err := readJSONMap(path)
	if err != nil {
		c.warn("settings_invalid", "Claude settings could not be parsed", path, project)
		return
	}
	c.source(path, "json", scope, project, layer, precedence, false)
	scanHooksDocument(c, document, Evidence{Path: path, Format: "json", Layer: layer, Pointer: "hooks"}, scope, project, false)
	scanClaudeEnabledPlugins(c, document, Evidence{Path: path, Format: "json", Layer: layer}, scope, project)
	scanClaudeMarketplaces(c, document, Evidence{Path: path, Format: "json", Layer: layer}, scope, project)
}

func (*ClaudeHost) scanMCPFile(c *collector, path string, scope Scope, project, layer string, precedence int, key string) {
	if !exists(path) {
		return
	}
	document, err := readJSONMap(path)
	if err != nil {
		c.warn("mcp_config_invalid", "Claude MCP configuration could not be parsed", path, project)
		return
	}
	c.source(path, "json", scope, project, layer, precedence, false)
	scanMCPDocument(c, document, key, Evidence{Path: path, Format: "json", Layer: layer, Pointer: key}, scope, project, "")
}

func (h *ClaudeHost) scanGlobalState(c *collector, path string, selectedProjects []string) {
	if !exists(path) {
		return
	}
	document, err := readJSONMap(path)
	if err != nil {
		c.warn("global_state_invalid", "Claude global state could not be parsed", path, "")
		return
	}
	c.source(path, "json", ScopeUser, "", "global_state", 50, false)
	scanMCPDocument(c, document, "mcpServers", Evidence{Path: path, Format: "json", Layer: "user_mcp", Pointer: "mcpServers"}, ScopeUser, "", "")
	projects, ok := asMap(document["projects"])
	if !ok {
		return
	}
	selected := make(map[string]string, len(selectedProjects))
	for _, project := range selectedProjects {
		selected[filepath.Clean(project)] = project
		if root := discoverRepoRoot(project); root != "" {
			selected[filepath.Clean(root)] = project
		}
	}
	for configuredPath, projectValue := range projects {
		cleaned := filepath.Clean(configuredPath)
		selectedProject, selectedOK := selected[cleaned]
		if !selectedOK {
			continue
		}
		project, projectOK := asMap(projectValue)
		if !projectOK {
			continue
		}
		evidence := Evidence{Path: path, Format: "json", Layer: "local_mcp", Pointer: "projects/" + configuredPath + "/mcpServers"}
		scanMCPDocument(c, project, "mcpServers", evidence, ScopeLocal, selectedProject, "")
	}
}

func scanClaudeEnabledPlugins(c *collector, document map[string]any, evidence Evidence, scope Scope, project string) {
	plugins, ok := asMap(document["enabledPlugins"])
	if !ok {
		return
	}
	for _, id := range sortedMapKeys(plugins) {
		enabled, enabledOK := asBool(plugins[id])
		if !enabledOK {
			continue
		}
		name, marketplace, _ := strings.Cut(id, "@")
		metadata := map[string]string{}
		if marketplace != "" {
			metadata["marketplace"] = marketplace
		}
		source := SourceRef{Kind: "plugin_config", Locator: id}
		c.observe(Observation{
			Kind: ComponentPlugin, Name: name, Scope: scope, Project: project,
			Enabled: enabled, Source: source, Metadata: metadata,
			Evidence: []Evidence{{Path: evidence.Path, Format: evidence.Format, Layer: evidence.Layer, Pointer: "enabledPlugins/" + id}},
		}, evidence.Path)
	}
}

func scanClaudeMarketplaces(c *collector, document map[string]any, evidence Evidence, scope Scope, project string) {
	marketplaces, ok := asMap(document["extraKnownMarketplaces"])
	if !ok {
		return
	}
	for _, name := range sortedMapKeys(marketplaces) {
		entry, entryOK := asMap(marketplaces[name])
		if !entryOK {
			continue
		}
		source := SourceRef{Kind: "marketplace", Locator: name}
		if configured, configuredOK := entry["source"]; configuredOK {
			parsed := marketplacePluginSource(filepath.Dir(evidence.Path), configured)
			if parsed.Kind != "unknown" {
				source = parsed
			}
		}
		c.observe(Observation{
			Kind: ComponentPlugin, Name: name, Scope: scope, Project: project,
			Source: source, Metadata: map[string]string{"marketplace_registration": "true"},
			Evidence: []Evidence{{Path: evidence.Path, Format: evidence.Format, Layer: evidence.Layer, Pointer: "extraKnownMarketplaces/" + name}},
		}, evidence.Path)
	}
}

func (*ClaudeHost) PlanHookInstall(ctx context.Context, options HookInstallOptions) (MutationPlan, error) {
	return planHookInstall(ctx, Claude, options)
}

var _ Host = (*ClaudeHost)(nil)
