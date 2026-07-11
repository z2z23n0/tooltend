package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type collector struct {
	host    Name
	pathEnv string
	result  Result
}

func (c *collector) source(path, format string, scope Scope, project, layer string, precedence int, legacy bool) {
	c.result.ConfigSources = append(c.result.ConfigSources, ConfigSource{
		Host: c.host, Path: path, Format: format, Scope: scope,
		Project: project, Layer: layer, Precedence: precedence, Legacy: legacy,
	})
}

func (c *collector) warn(code, message, path, project string) {
	c.result.Warnings = append(c.result.Warnings, Warning{
		Host: c.host, Code: code, Message: message, Path: path, Project: project,
	})
}

func (c *collector) observe(observation Observation, configPath string) {
	if observation.Host == "" {
		observation.Host = c.host
	}
	if observation.Key == "" {
		observation.Key = componentKey(observation.Host, observation.Kind, observation.Source, observation.Name)
	}
	if observation.Metadata != nil && len(observation.Metadata) == 0 {
		observation.Metadata = nil
	}
	c.result.Observations = append(c.result.Observations, observation)
	c.result.Bindings = append(c.result.Bindings, Binding{
		Host: observation.Host, ComponentKey: observation.Key, Scope: observation.Scope,
		Project: observation.Project, InstallPath: observation.Path, ConfigPath: configPath,
		Enabled: observation.Enabled, Legacy: observation.Legacy,
	})
}

func (c *collector) finish() Result {
	c.result.sortAndDedupe()
	return c.result
}

func scanSkillDirectory(ctx context.Context, c *collector, root string, scope Scope, project string, legacy bool, detectClaudePlugins bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			c.warn("skill_directory_unreadable", err.Error(), root, project)
		}
		return
	}
	for _, entry := range entries {
		if checkContext(ctx) != nil {
			return
		}
		path := filepath.Join(root, entry.Name())
		info, infoErr := os.Stat(path)
		if infoErr != nil {
			c.warn("skill_unreadable", infoErr.Error(), path, project)
			continue
		}
		if !info.IsDir() {
			continue
		}
		if detectClaudePlugins && exists(filepath.Join(path, ".claude-plugin", "plugin.json")) {
			scanPluginDirectory(ctx, c, path, scope, project, SourceRef{Kind: "skills_dir", Locator: path}, legacy)
			continue
		}
		skillFile := filepath.Join(path, "SKILL.md")
		if !exists(skillFile) {
			continue
		}
		name, description := parseSkillFrontmatter(skillFile, entry.Name())
		dependencies := scanExplicitDependencies(skillFile, c.pathEnv)
		realPath := path
		if evaluated, evalErr := filepath.EvalSymlinks(path); evalErr == nil {
			realPath = evaluated
		}
		source := SourceRef{Kind: "local", Locator: realPath}
		metadata := map[string]string{}
		if description != "" {
			metadata["description"] = description
		}
		if realPath != path {
			metadata["symlink_target"] = realPath
		}
		c.observe(Observation{
			Kind: ComponentSkill, Name: name, Path: path, Scope: scope, Project: project,
			Legacy: legacy, Source: source, Metadata: metadata,
			Dependencies: dependencies,
			Evidence:     []Evidence{{Path: skillFile, Format: "markdown", Layer: "skill", Pointer: "frontmatter"}},
		}, skillFile)
	}
}

func scanLegacyCommands(ctx context.Context, c *collector, root string, scope Scope, project string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			c.warn("command_directory_unreadable", err.Error(), root, project)
		}
		return
	}
	for _, entry := range entries {
		if checkContext(ctx) != nil {
			return
		}
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		name, description := parseSkillFrontmatter(path, strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())))
		metadata := map[string]string{"compatibility": "legacy_command"}
		if description != "" {
			metadata["description"] = description
		}
		source := SourceRef{Kind: "local", Locator: path}
		c.observe(Observation{
			Kind: ComponentSkill, Name: name, Path: path, Scope: scope, Project: project,
			Legacy: true, Source: source, Metadata: metadata,
			Evidence: []Evidence{{Path: path, Format: "markdown", Layer: "legacy_command", Pointer: "frontmatter"}},
		}, path)
	}
}

func scanHooksDocument(c *collector, document map[string]any, evidence Evidence, scope Scope, project string, legacy bool) {
	hooks, ok := asMap(document["hooks"])
	if !ok {
		return
	}
	for _, event := range sortedMapKeys(hooks) {
		groups, groupsOK := asSlice(hooks[event])
		if !groupsOK {
			c.warn("invalid_hook_event", "hook event must be an array", evidence.Path, project)
			continue
		}
		for groupIndex, groupValue := range groups {
			group, groupOK := asMap(groupValue)
			if !groupOK {
				continue
			}
			matcher, _ := asString(group["matcher"])
			handlers, _ := asSlice(group["hooks"])
			for handlerIndex, handlerValue := range handlers {
				handler, handlerOK := asMap(handlerValue)
				if !handlerOK {
					continue
				}
				handlerType, _ := asString(handler["type"])
				metadata := map[string]string{
					"event": event, "handler_type": handlerType,
				}
				if matcher != "" {
					metadata["matcher"] = matcher
				}
				if async, asyncOK := handler["async"].(bool); asyncOK {
					metadata["async"] = fmt.Sprintf("%t", async)
				}
				if command, commandOK := asString(handler["command"]); commandOK {
					if executable := commandExecutable(command); executable != "" {
						metadata["executable"] = executable
					}
				}
				pointer := fmt.Sprintf("%s/%s/%d/hooks/%d", evidence.Pointer, event, groupIndex, handlerIndex)
				hookEvidence := evidence
				hookEvidence.Pointer = strings.TrimPrefix(pointer, "/")
				source := SourceRef{Kind: "config", Locator: evidence.Path, Subdir: hookEvidence.Pointer}
				name := event
				if matcher != "" {
					name += ":" + matcher
				}
				c.observe(Observation{
					Kind: ComponentHook, Name: name, Path: evidence.Path, Scope: scope, Project: project,
					Legacy: legacy, Source: source, Metadata: metadata, Secrets: secretRefs(handler),
					Evidence: []Evidence{hookEvidence},
				}, evidence.Path)
			}
		}
	}
}

func commandExecutable(command string) string {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return ""
	}
	for _, field := range fields {
		candidate := strings.Trim(field, `"'`)
		if strings.Contains(candidate, "=") {
			continue
		}
		return filepath.Base(candidate)
	}
	return ""
}

func scanMCPDocument(c *collector, document map[string]any, key string, evidence Evidence, scope Scope, project string, pluginName string) {
	servers, ok := asMap(document[key])
	if !ok {
		return
	}
	scanMCPServers(c, servers, evidence, scope, project, pluginName)
}

func scanMCPServers(c *collector, servers map[string]any, evidence Evidence, scope Scope, project string, pluginName string) {
	for _, name := range sortedMapKeys(servers) {
		config, ok := asMap(servers[name])
		if !ok {
			c.warn("invalid_mcp_server", "MCP server entry must be an object", evidence.Path, project)
			continue
		}
		pointer := strings.TrimSuffix(evidence.Pointer, "/") + "/" + escapeJSONPointer(name)
		serverEvidence := evidence
		serverEvidence.Pointer = strings.TrimPrefix(pointer, "/")
		enabled := boolPtr(true)
		if configured, configuredOK := asBool(config["enabled"]); configuredOK {
			enabled = configured
		}
		transport, _ := asString(config["type"])
		urlValue, hasURL := asString(config["url"])
		command, hasCommand := asString(config["command"])
		var kind ComponentKind
		var source SourceRef
		metadata := map[string]string{}
		switch {
		case hasURL:
			kind = ComponentHTTPMCP
			if transport == "" {
				transport = "http"
				if c.host == Claude {
					c.warn("mcp_url_without_type", "Claude MCP URL entries require an explicit type", evidence.Path, project)
				}
			}
			if transport == "streamable-http" {
				transport = "http"
			}
			if transport == "sse" {
				c.warn("deprecated_mcp_transport", "SSE transport is deprecated", evidence.Path, project)
			}
			source = SourceRef{Kind: "http", Locator: sanitizeURL(urlValue)}
		case hasCommand:
			kind = ComponentStdioMCP
			if transport == "" {
				transport = "stdio"
			}
			args := stringsFrom(config["args"])
			source, metadata = runtimeSource(command, args)
		default:
			c.warn("mcp_transport_unknown", "MCP entry has neither command nor URL", evidence.Path, project)
			continue
		}
		if pluginName != "" {
			metadata["plugin"] = pluginName
		}
		observationName := name
		if pluginName != "" {
			observationName = pluginName + ":" + name
		}
		c.observe(Observation{
			Kind: kind, Name: observationName, Path: evidence.Path, Scope: scope, Project: project,
			Transport: transport, Enabled: enabled, Source: source, Metadata: metadata,
			Secrets: secretRefs(config), Evidence: []Evidence{serverEvidence},
		}, evidence.Path)
	}
}
