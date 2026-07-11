package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/z2z23n0/tooltend/internal/safeio"
)

const defaultHookTimeoutSeconds = 5

type HookInstallOptions struct {
	HomeDir    string
	Project    string
	BinaryPath string
	Scope      Scope
}

type MutationOperation struct {
	Action      string `json:"action"`
	Pointer     string `json:"pointer"`
	Description string `json:"description"`
}

// FileMutation is a read-only proposal. PlanHookInstall never creates a
// directory or writes this content; the caller must preview and apply it using
// the normal confirmed mutation path.
type FileMutation struct {
	Path         string              `json:"path"`
	Format       string              `json:"format"`
	Mode         os.FileMode         `json:"mode"`
	Exists       bool                `json:"exists"`
	Changed      bool                `json:"changed"`
	BeforeSHA256 string              `json:"before_sha256,omitempty"`
	AfterSHA256  string              `json:"after_sha256"`
	Content      []byte              `json:"content"`
	Operations   []MutationOperation `json:"operations,omitempty"`
}

type MutationPlan struct {
	Host      Name           `json:"host"`
	Mutations []FileMutation `json:"mutations"`
	Warnings  []Warning      `json:"warnings,omitempty"`
}

// ApplyMutation commits a previously inspected hook mutation only when the
// target still has the exact existence and content hash shown in the preview.
// This prevents an init/doctor confirmation from overwriting concurrent user
// edits made after planning.
func ApplyMutation(mutation FileMutation) error {
	if mutation.Path == "" || !filepath.IsAbs(mutation.Path) {
		return errors.New("host: mutation target must be absolute")
	}
	current, err := os.ReadFile(mutation.Path)
	switch {
	case mutation.Exists && err != nil:
		return fmt.Errorf("host: hook target changed after preview: %w", err)
	case !mutation.Exists && err == nil:
		return errors.New("host: hook target was created after preview")
	case !mutation.Exists && !os.IsNotExist(err):
		return fmt.Errorf("host: inspect hook target before apply: %w", err)
	case mutation.Exists && contentHash(current) != mutation.BeforeSHA256:
		return errors.New("host: hook target content changed after preview")
	}
	if contentHash(mutation.Content) != mutation.AfterSHA256 {
		return errors.New("host: planned hook content hash is invalid")
	}
	return safeio.AtomicWriteFile(mutation.Path, mutation.Content, mutation.Mode)
}

type hookSpecification struct {
	event   string
	matcher string
	async   bool
}

func planHookInstall(ctx context.Context, host Name, options HookInstallOptions) (MutationPlan, error) {
	if err := ctx.Err(); err != nil {
		return MutationPlan{}, err
	}
	if host != Codex && host != Claude {
		return MutationPlan{}, fmt.Errorf("host: unsupported hook host %q", host)
	}

	home, err := hookHome(options.HomeDir)
	if err != nil {
		return MutationPlan{}, err
	}
	binary, err := validatedHookBinary(options.BinaryPath)
	if err != nil {
		return MutationPlan{}, err
	}
	target, scope, err := hookTarget(host, home, options.Project, options.Scope)
	if err != nil {
		return MutationPlan{}, err
	}

	document, original, mode, existed, err := readHookTarget(target)
	if err != nil {
		return MutationPlan{}, err
	}
	updated := document
	operations, err := ensureToolTendHooks(updated, host, binary)
	if err != nil {
		return MutationPlan{}, fmt.Errorf("host: plan hook mutation for %s: %w", target, err)
	}

	content := original
	if len(operations) > 0 {
		content, err = json.MarshalIndent(updated, "", "  ")
		if err != nil {
			return MutationPlan{}, fmt.Errorf("host: encode hook plan for %s: %w", target, err)
		}
		content = append(content, '\n')
	}
	if !existed && len(content) == 0 {
		content = []byte("{}\n")
	}

	beforeHash := ""
	if existed {
		beforeHash = contentHash(original)
	}
	mutation := FileMutation{
		Path: target, Format: "json", Mode: mode, Exists: existed,
		Changed:      !existed || !bytes.Equal(original, content),
		BeforeSHA256: beforeHash, AfterSHA256: contentHash(content),
		Content: content, Operations: operations,
	}
	plan := MutationPlan{Host: host, Mutations: []FileMutation{mutation}}
	if scope == ScopeLocal {
		plan.Warnings = append(plan.Warnings, Warning{
			Host: host, Code: "local_hook_config_uncommitted",
			Message: "local hook configuration is machine-specific and should remain uncommitted",
			Path:    target, Project: filepath.Clean(options.Project),
		})
	}
	return plan, nil
}

func hookHome(configured string) (string, error) {
	if strings.TrimSpace(configured) == "" {
		detected, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("host: determine home directory: %w", err)
		}
		configured = detected
	}
	return cleanAbsolute(configured)
}

func validatedHookBinary(configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return "", errors.New("host: hook executable path is required")
	}
	if strings.ContainsAny(configured, "\x00\r\n") {
		return "", errors.New("host: hook executable path contains invalid characters")
	}
	if !filepath.IsAbs(configured) {
		return "", errors.New("host: hook executable path must be absolute")
	}
	return filepath.Clean(configured), nil
}

func hookTarget(host Name, home, project string, configuredScope Scope) (string, Scope, error) {
	scope := configuredScope
	if scope == "" {
		scope = ScopeUser
	}
	switch scope {
	case ScopeUser:
		if host == Codex {
			return filepath.Join(home, ".codex", "hooks.json"), scope, nil
		}
		return filepath.Join(home, ".claude", "settings.json"), scope, nil
	case ScopeProject, ScopeLocal:
		if host == Codex && scope == ScopeLocal {
			return "", "", errors.New("host: Codex does not define a local hook settings layer")
		}
		if strings.TrimSpace(project) == "" {
			return "", "", errors.New("host: project path is required for project-scoped hooks")
		}
		cleaned, err := cleanAbsolute(project)
		if err != nil {
			return "", "", err
		}
		if host == Codex {
			return filepath.Join(cleaned, ".codex", "hooks.json"), scope, nil
		}
		filename := "settings.json"
		if scope == ScopeLocal {
			filename = "settings.local.json"
		}
		return filepath.Join(cleaned, ".claude", filename), scope, nil
	default:
		return "", "", fmt.Errorf("host: unsupported hook scope %q", scope)
	}
}

func readHookTarget(path string) (map[string]any, []byte, os.FileMode, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil, 0o600, false, nil
		}
		return nil, nil, 0, false, fmt.Errorf("host: inspect hook target %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, 0, false, fmt.Errorf("host: hook target %s is not a regular file", path)
	}
	document, err := readJSONMap(path)
	if err != nil {
		return nil, nil, 0, false, fmt.Errorf("host: parse hook target %s: %w", path, err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, 0, false, fmt.Errorf("host: read hook target %s: %w", path, err)
	}
	return document, original, info.Mode().Perm(), true, nil
}

func ensureToolTendHooks(document map[string]any, host Name, binary string) ([]MutationOperation, error) {
	hooksValue, hasHooks := document["hooks"]
	hooks, ok := asMap(hooksValue)
	if hasHooks && !ok {
		return nil, errors.New("hooks field must be an object")
	}
	if !hasHooks {
		hooks = make(map[string]any)
		document["hooks"] = hooks
	}
	specifications := []hookSpecification{
		{event: "SessionStart", matcher: "startup|resume|clear|compact"},
		{event: "PreToolUse", matcher: toolMatcher(host), async: host == Claude},
		{event: "PostToolUse", matcher: toolMatcher(host), async: host == Claude},
	}
	var operations []MutationOperation
	for _, specification := range specifications {
		pointer, action, err := ensureToolTendHook(hooks, host, binary, specification)
		if err != nil {
			return nil, err
		}
		if action == "" {
			continue
		}
		operations = append(operations, MutationOperation{
			Action: action, Pointer: pointer,
			Description: fmt.Sprintf("%s ToolTend %s hook", action, specification.event),
		})
	}
	return operations, nil
}

func ensureToolTendHook(hooks map[string]any, host Name, binary string, specification hookSpecification) (string, string, error) {
	desiredCommand := hookCommand(binary, host, specification.event)
	groupsValue, hasEvent := hooks[specification.event]
	groups, groupsOK := asSlice(groupsValue)
	if hasEvent && !groupsOK {
		return "", "", fmt.Errorf("hook event %s must be an array", specification.event)
	}
	if !hasEvent {
		groups = []any{}
	}

	for groupIndex, groupValue := range groups {
		group, ok := asMap(groupValue)
		if !ok {
			continue
		}
		matcher, _ := asString(group["matcher"])
		if matcher != specification.matcher {
			continue
		}
		handlers, ok := asSlice(group["hooks"])
		if !ok {
			continue
		}
		for handlerIndex, handlerValue := range handlers {
			handler, ok := asMap(handlerValue)
			if !ok || !isToolTendHook(handler, desiredCommand) {
				continue
			}
			changed := normalizeToolTendHandler(handler, host, specification, desiredCommand)
			pointer := fmt.Sprintf("/hooks/%s/%d/hooks/%d", escapeJSONPointer(specification.event), groupIndex, handlerIndex)
			if changed {
				return pointer, "update", nil
			}
			return pointer, "", nil
		}
	}

	desiredHandler := map[string]any{
		"type": "command", "command": desiredCommand,
		"timeout": json.Number(fmt.Sprintf("%d", defaultHookTimeoutSeconds)),
	}
	if specification.async {
		desiredHandler["async"] = true
	}

	for groupIndex, groupValue := range groups {
		group, ok := asMap(groupValue)
		if !ok {
			continue
		}
		matcher, _ := asString(group["matcher"])
		if matcher != specification.matcher {
			continue
		}
		handlers, handlersOK := asSlice(group["hooks"])
		if !handlersOK {
			continue
		}
		group["hooks"] = append(handlers, desiredHandler)
		hooks[specification.event] = groups
		return fmt.Sprintf("/hooks/%s/%d/hooks/%d", escapeJSONPointer(specification.event), groupIndex, len(handlers)), "append", nil
	}

	groups = append(groups, map[string]any{
		"matcher": specification.matcher,
		"hooks":   []any{desiredHandler},
	})
	hooks[specification.event] = groups
	return fmt.Sprintf("/hooks/%s/%d", escapeJSONPointer(specification.event), len(groups)-1), "append", nil
}

func normalizeToolTendHandler(handler map[string]any, host Name, specification hookSpecification, desiredCommand string) bool {
	changed := setMapValue(handler, "type", "command")
	changed = setMapValue(handler, "command", desiredCommand) || changed
	changed = setMapValue(handler, "timeout", json.Number(fmt.Sprintf("%d", defaultHookTimeoutSeconds))) || changed
	if _, exists := handler["args"]; exists {
		delete(handler, "args")
		changed = true
	}
	if host == Claude && specification.async {
		changed = setMapValue(handler, "async", true) || changed
	} else if _, exists := handler["async"]; exists {
		delete(handler, "async")
		changed = true
	}
	return changed
}

func setMapValue(values map[string]any, key string, desired any) bool {
	current, exists := values[key]
	if exists && fmt.Sprint(current) == fmt.Sprint(desired) {
		return false
	}
	values[key] = desired
	return true
}

func isToolTendHook(handler map[string]any, desiredCommand string) bool {
	command, ok := asString(handler["command"])
	if !ok {
		return false
	}
	if command == desiredCommand {
		return true
	}
	oldBinary, oldHost, oldEvent, oldOK := parseManagedHookCommand(command)
	newBinary, newHost, newEvent, newOK := parseManagedHookCommand(desiredCommand)
	return oldOK && newOK && oldHost == newHost && oldEvent == newEvent && filepath.Base(oldBinary) == filepath.Base(newBinary)
}

// parseManagedHookCommand accepts only the exact command grammar emitted by
// hookCommand. This lets a reinstall update an old absolute ToolTend path
// without treating an arbitrary user shell command as ToolTend-owned.
func parseManagedHookCommand(command string) (binary, hostName, event string, ok bool) {
	const marker = " hook --host "
	index := strings.Index(command, marker)
	if index <= 0 || strings.Index(command[index+len(marker):], marker) >= 0 {
		return "", "", "", false
	}
	prefix, tail := command[:index], command[index+len(marker):]
	hostName, event, found := strings.Cut(tail, " --event ")
	if !found || hostName == "" || event == "" || strings.ContainsAny(hostName+event, " \t\r\n") {
		return "", "", "", false
	}
	binary, ok = decodeShellQuotedPath(prefix)
	if !ok || !filepath.IsAbs(binary) {
		return "", "", "", false
	}
	return filepath.Clean(binary), hostName, event, true
}

func decodeShellQuotedPath(value string) (string, bool) {
	if len(value) < 2 || value[0] != '\'' || value[len(value)-1] != '\'' || strings.ContainsRune(value, '\x00') {
		return "", false
	}
	inner := value[1 : len(value)-1]
	const escapedQuote = `'"'"'`
	inner = strings.ReplaceAll(inner, escapedQuote, "\x00")
	if strings.ContainsRune(inner, '\'') || strings.ContainsAny(inner, "\r\n") {
		return "", false
	}
	inner = strings.ReplaceAll(inner, "\x00", "'")
	if strings.ContainsAny(inner, "\r\n") {
		return "", false
	}
	return inner, true
}

func toolMatcher(host Name) string {
	if host == Claude {
		return "Bash|Edit|Write|mcp__.*"
	}
	return "Bash|apply_patch|mcp__.*"
}

func hookCommand(binary string, host Name, event string) string {
	return shellQuote(binary) + " hook --host " + string(host) + " --event " + event
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func escapeJSONPointer(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	return strings.ReplaceAll(value, "/", "~1")
}

func contentHash(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
