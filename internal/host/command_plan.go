package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type CommandMutationOptions struct {
	Host        Name
	ConfigPath  string
	Pointer     string
	Command     string
	ReplaceArgs bool
	Args        []string
}

type CommandSpec struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

func CommandAtPointer(configPath, pointer string) (string, error) {
	spec, err := CommandSpecAtPointer(configPath, pointer)
	return spec.Command, err
}

func CommandSpecAtPointer(configPath, pointer string) (CommandSpec, error) {
	if !filepath.IsAbs(configPath) || pointer == "" {
		return CommandSpec{}, errors.New("host: command locator is incomplete")
	}
	var (
		document map[string]any
		err      error
	)
	switch strings.TrimPrefix(strings.ToLower(filepath.Ext(configPath)), ".") {
	case "json":
		document, err = readJSONMap(configPath)
	case "toml":
		document, err = readTOMLMap(configPath)
	default:
		return CommandSpec{}, errors.New("host: unsupported command config format")
	}
	if err != nil {
		return CommandSpec{}, err
	}
	target, err := objectAtPointer(document, pointer)
	if err != nil {
		return CommandSpec{}, err
	}
	command, ok := asString(target["command"])
	if !ok || strings.TrimSpace(command) == "" {
		return CommandSpec{}, errors.New("host: config pointer has no command")
	}
	args, err := strictCommandArgs(target["args"])
	if err != nil {
		return CommandSpec{}, err
	}
	return CommandSpec{Command: command, Args: args}, nil
}

// PlanCommandMutation changes only the command field at an already observed
// Hook or MCP config pointer. Args, environment references, headers, and all
// unknown fields stay in the same in-memory document and are written back
// atomically only after the caller confirms the resulting FileMutation.
func PlanCommandMutation(ctx context.Context, options CommandMutationOptions) (FileMutation, error) {
	if err := ctx.Err(); err != nil {
		return FileMutation{}, err
	}
	if options.Host != Codex && options.Host != Claude {
		return FileMutation{}, errors.New("host: command mutation requires codex or claude")
	}
	if !filepath.IsAbs(options.ConfigPath) || options.Pointer == "" || strings.TrimSpace(options.Command) == "" || strings.ContainsAny(options.Command, "\x00\r\n") {
		return FileMutation{}, errors.New("host: command mutation locator is incomplete")
	}
	info, err := os.Stat(options.ConfigPath)
	if err != nil {
		return FileMutation{}, fmt.Errorf("host: inspect command config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return FileMutation{}, errors.New("host: command config is not a regular file")
	}
	original, err := os.ReadFile(options.ConfigPath)
	if err != nil {
		return FileMutation{}, err
	}
	format := strings.TrimPrefix(strings.ToLower(filepath.Ext(options.ConfigPath)), ".")
	var document map[string]any
	switch format {
	case "json":
		document, err = readJSONMap(options.ConfigPath)
	case "toml":
		document, err = readTOMLMap(options.ConfigPath)
	default:
		return FileMutation{}, fmt.Errorf("host: unsupported command config format %q", format)
	}
	if err != nil {
		return FileMutation{}, fmt.Errorf("host: parse command config: %w", err)
	}
	target, err := objectAtPointer(document, options.Pointer)
	if err != nil {
		return FileMutation{}, err
	}
	previous, ok := asString(target["command"])
	if !ok || strings.TrimSpace(previous) == "" {
		return FileMutation{}, errors.New("host: observed config pointer no longer contains a command")
	}
	target["command"] = options.Command
	operations := []MutationOperation{{Action: "update", Pointer: "/" + strings.TrimPrefix(options.Pointer, "/") + "/command", Description: "update managed extension command"}}
	if options.ReplaceArgs {
		if _, err := strictCommandArgs(target["args"]); err != nil {
			return FileMutation{}, err
		}
		target["args"] = append([]string(nil), options.Args...)
		operations = append(operations, MutationOperation{Action: "update", Pointer: "/" + strings.TrimPrefix(options.Pointer, "/") + "/args", Description: "remove package carrier arguments from managed extension command"})
	}
	var content []byte
	switch format {
	case "json":
		content, err = json.MarshalIndent(document, "", "  ")
		content = append(content, '\n')
	case "toml":
		content, err = toml.Marshal(document)
	}
	if err != nil {
		return FileMutation{}, fmt.Errorf("host: encode command config: %w", err)
	}
	return FileMutation{
		Path: options.ConfigPath, Format: format, Mode: info.Mode().Perm(), Exists: true,
		Changed: !bytes.Equal(original, content), BeforeSHA256: contentHash(original), AfterSHA256: contentHash(content), Content: content,
		Operations: operations,
	}, nil
}

func strictCommandArgs(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := asSlice(value)
	if !ok {
		return nil, errors.New("host: command args are not an array")
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := asString(item)
		if !ok || strings.ContainsAny(text, "\x00\r\n") {
			return nil, errors.New("host: command args contain a non-string or unsafe value")
		}
		result = append(result, text)
	}
	return result, nil
}

func objectAtPointer(document map[string]any, pointer string) (map[string]any, error) {
	segments := strings.Split(strings.Trim(pointer, "/"), "/")
	if len(segments) == 0 {
		return nil, errors.New("host: empty config pointer")
	}
	var current any = document
	for _, encoded := range segments {
		segment := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		if value, ok := asMap(current); ok {
			next, ok := value[segment]
			if !ok {
				return nil, fmt.Errorf("host: config pointer segment %q is missing", segment)
			}
			current = next
			continue
		}
		if value, ok := asSlice(current); ok {
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(value) {
				return nil, fmt.Errorf("host: config pointer index %q is invalid", segment)
			}
			current = value[index]
			continue
		}
		return nil, fmt.Errorf("host: config pointer crosses a non-container at %q", segment)
	}
	result, ok := asMap(current)
	if !ok {
		return nil, errors.New("host: config pointer does not identify an object")
	}
	return result, nil
}
