package host

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

type CodexHost struct{}

func NewCodex() *CodexHost { return &CodexHost{} }

func (*CodexHost) Name() Name { return Codex }

func (h *CodexHost) Scan(ctx context.Context, options ScanOptions) (Result, error) {
	options, err := options.normalized()
	if err != nil {
		return Result{}, err
	}
	c := &collector{host: Codex, pathEnv: options.Getenv("PATH")}
	codexDir := filepath.Join(options.HomeDir, ".codex")

	h.scanTOML(ctx, c, filepath.Join(options.CodexSystemDir, "config.toml"), ScopeSystem, "", "system_config", 50, false)
	h.scanJSONHooks(c, filepath.Join(options.CodexSystemDir, "hooks.json"), ScopeSystem, "", "system_hooks", 50, false)

	userConfig := filepath.Join(codexDir, "config.toml")
	h.scanTOML(ctx, c, userConfig, ScopeUser, "", "user_config", 40, false)
	h.scanJSONHooks(c, filepath.Join(codexDir, "hooks.json"), ScopeUser, "", "user_hooks", 40, false)
	if profile := strings.TrimSpace(options.CodexProfile); profile != "" && validProfileName(profile) {
		h.scanTOML(ctx, c, filepath.Join(codexDir, profile+".config.toml"), ScopeUser, "", "profile_config", 30, false)
	} else if profile != "" {
		c.warn("invalid_profile", "Codex profile names may contain only letters, numbers, hyphens, and underscores", "", "")
	}

	for _, project := range options.Projects {
		for index, directory := range projectChain(project) {
			configPath := filepath.Join(directory, ".codex", "config.toml")
			h.scanTOML(ctx, c, configPath, ScopeProject, project, "project_config", 20-index, false)
			h.scanJSONHooks(c, filepath.Join(directory, ".codex", "hooks.json"), ScopeProject, project, "project_hooks", 20-index, false)
		}
	}

	scanSkillDirectory(ctx, c, filepath.Join(options.HomeDir, ".agents", "skills"), ScopeUser, "", false, false)
	scanSkillDirectory(ctx, c, filepath.Join(codexDir, "skills"), ScopeUser, "", true, false)
	scanSkillDirectory(ctx, c, filepath.Join(options.CodexSystemDir, "skills"), ScopeSystem, "", false, false)
	for _, project := range options.Projects {
		for _, directory := range projectChain(project) {
			scanSkillDirectory(ctx, c, filepath.Join(directory, ".agents", "skills"), ScopeProject, project, false, false)
			scanSkillDirectory(ctx, c, filepath.Join(directory, ".codex", "skills"), ScopeProject, project, true, false)
		}
	}

	scanPluginCache(ctx, c, filepath.Join(codexDir, "plugins", "cache"))
	scanMarketplaceFile(ctx, c, filepath.Join(options.HomeDir, ".agents", "plugins", "marketplace.json"), ScopeUser, "", false)
	for _, project := range options.Projects {
		root := discoverRepoRoot(project)
		if root == "" {
			root = project
		}
		scanMarketplaceFile(ctx, c, filepath.Join(root, ".agents", "plugins", "marketplace.json"), ScopeProject, root, false)
		scanMarketplaceFile(ctx, c, filepath.Join(root, ".claude-plugin", "marketplace.json"), ScopeProject, root, true)
	}

	return c.finish(), ctx.Err()
}

func (h *CodexHost) scanTOML(ctx context.Context, c *collector, path string, scope Scope, project, layer string, precedence int, legacy bool) {
	if !exists(path) {
		return
	}
	document, err := readTOMLMap(path)
	if err != nil {
		c.warn("config_invalid", "Codex configuration could not be parsed", path, project)
		return
	}
	c.source(path, "toml", scope, project, layer, precedence, legacy)
	evidence := Evidence{Path: path, Format: "toml", Layer: layer}
	scanHooksDocument(c, document, Evidence{Path: path, Format: "toml", Layer: layer, Pointer: "hooks"}, scope, project, legacy)
	scanMCPDocument(c, document, "mcp_servers", Evidence{Path: path, Format: "toml", Layer: layer, Pointer: "mcp_servers"}, scope, project, "")
	h.scanConfiguredPlugins(c, document, evidence, scope, project)
	h.scanConfiguredSkills(ctx, c, document, evidence, scope, project)
}

func (*CodexHost) scanJSONHooks(c *collector, path string, scope Scope, project, layer string, precedence int, legacy bool) {
	if !exists(path) {
		return
	}
	document, err := readJSONMap(path)
	if err != nil {
		c.warn("hooks_invalid", "Codex hooks configuration could not be parsed", path, project)
		return
	}
	c.source(path, "json", scope, project, layer, precedence, legacy)
	scanHooksDocument(c, document, Evidence{Path: path, Format: "json", Layer: layer, Pointer: "hooks"}, scope, project, legacy)
}

func (*CodexHost) scanConfiguredPlugins(c *collector, document map[string]any, evidence Evidence, scope Scope, project string) {
	plugins, ok := asMap(document["plugins"])
	if !ok {
		return
	}
	for _, id := range sortedMapKeys(plugins) {
		config, configOK := asMap(plugins[id])
		if !configOK {
			continue
		}
		enabled := boolPtr(true)
		if configured, configuredOK := asBool(config["enabled"]); configuredOK {
			enabled = configured
		}
		name, marketplace, _ := strings.Cut(id, "@")
		source := SourceRef{Kind: "plugin_config", Locator: id}
		metadata := map[string]string{}
		if marketplace != "" {
			metadata["marketplace"] = marketplace
		}
		pointer := "plugins/" + id
		c.observe(Observation{
			Kind: ComponentPlugin, Name: name, Scope: scope, Project: project,
			Enabled: enabled, Source: source, Metadata: metadata,
			Evidence: []Evidence{{Path: evidence.Path, Format: evidence.Format, Layer: evidence.Layer, Pointer: pointer}},
		}, evidence.Path)
	}
}

func (*CodexHost) scanConfiguredSkills(ctx context.Context, c *collector, document map[string]any, evidence Evidence, scope Scope, project string) {
	skills, ok := asMap(document["skills"])
	if !ok {
		return
	}
	configs, ok := asSlice(skills["config"])
	if !ok {
		return
	}
	for index, value := range configs {
		if checkContext(ctx) != nil {
			return
		}
		config, configOK := asMap(value)
		if !configOK {
			continue
		}
		path, pathOK := asString(config["path"])
		if !pathOK || !filepath.IsAbs(path) || !exists(path) {
			continue
		}
		name, description := parseSkillFrontmatter(path, filepath.Base(filepath.Dir(path)))
		enabled := boolPtr(true)
		if configured, configuredOK := asBool(config["enabled"]); configuredOK {
			enabled = configured
		}
		metadata := map[string]string{}
		if description != "" {
			metadata["description"] = description
		}
		source := SourceRef{Kind: "local", Locator: filepath.Dir(path)}
		c.observe(Observation{
			Kind: ComponentSkill, Name: name, Path: filepath.Dir(path), Scope: scope,
			Project: project, Enabled: enabled, Source: source, Metadata: metadata,
			Evidence: []Evidence{{Path: evidence.Path, Format: evidence.Format, Layer: evidence.Layer, Pointer: fmt.Sprintf("skills/config/%d", index)}},
		}, evidence.Path)
	}
}

func validProfileName(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func (*CodexHost) PlanHookInstall(ctx context.Context, options HookInstallOptions) (MutationPlan, error) {
	return planHookInstall(ctx, Codex, options)
}

var _ Host = (*CodexHost)(nil)
