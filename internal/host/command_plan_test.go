package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestCommandMutationPreservesMCPArgsAndSecretFields(t *testing.T) {
	for _, fixture := range []struct {
		name, file, pointer, content string
	}{
		{name: "json", file: "settings.json", pointer: "mcpServers/server~1one", content: `{"mcpServers":{"server/one":{"command":"npx","args":["-y","pkg@1"],"env":{"API_TOKEN":"secret-value"}}},"unknown":true}`},
		{name: "toml", file: "config.toml", pointer: "mcp_servers/server", content: "[mcp_servers.server]\ncommand = \"uvx\"\nargs = [\"pkg==1\"]\nenv = { API_TOKEN = \"secret-value\" }\n\n[unknown]\nkeep = true\n"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fixture.file)
			if err := os.WriteFile(path, []byte(fixture.content), 0o600); err != nil {
				t.Fatal(err)
			}
			mutation, err := PlanCommandMutation(context.Background(), CommandMutationOptions{Host: Codex, ConfigPath: path, Pointer: fixture.pointer, Command: "/home/user/.local/bin/server"})
			if err != nil {
				t.Fatal(err)
			}
			if !mutation.Changed || strings.Contains(string(mutation.Content), "<redacted>") {
				t.Fatalf("mutation=%#v", mutation)
			}
			if err := ApplyMutation(mutation); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if fixture.name == "json" {
				if err := json.Unmarshal(data, &document); err != nil {
					t.Fatal(err)
				}
			} else if err := toml.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			target, err := objectAtPointer(document, fixture.pointer)
			if err != nil {
				t.Fatal(err)
			}
			if target["command"] != "/home/user/.local/bin/server" || !strings.Contains(string(data), "secret-value") || !strings.Contains(string(data), "pkg") {
				t.Fatalf("config fields were not preserved: %s", data)
			}
		})
	}
}

func TestCommandMutationCanReplaceOnlyCarrierArgs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := "[mcp_servers.server]\ncommand = \"npx\"\nargs = [\"-y\", \"pkg@1.0.0\", \"--transport\", \"stdio\"]\nenv = { API_TOKEN = \"secret-value\" }\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	mutation, err := PlanCommandMutation(context.Background(), CommandMutationOptions{
		Host: Codex, ConfigPath: path, Pointer: "mcp_servers/server", Command: "/managed/pkg",
		ReplaceArgs: true, Args: []string{"--transport", "stdio"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mutation.Content), "pkg@1.0.0") || !strings.Contains(string(mutation.Content), "secret-value") {
		t.Fatalf("mutation=%s", mutation.Content)
	}
	if err := ApplyMutation(mutation); err != nil {
		t.Fatal(err)
	}
	spec, err := CommandSpecAtPointer(path, "mcp_servers/server")
	if err != nil || spec.Command != "/managed/pkg" || strings.Join(spec.Args, " ") != "--transport stdio" {
		t.Fatalf("spec=%+v err=%v", spec, err)
	}
}

func TestStripRuntimeCarrierArgs(t *testing.T) {
	tests := []struct {
		name, command, source, packageName, executable string
		args, want                                     []string
	}{
		{"npx", "npx", "npm", "@scope/server", "server", []string{"-y", "@scope/server@1.2.3", "--port", "42"}, []string{"--port", "42"}},
		{"uvx", "uvx", "pypi", "example-server", "example-server", []string{"--offline", "example_server==1.2.3", "--stdio"}, []string{"--stdio"}},
		{"npx package flag", "npx", "npm", "pkg", "pkg-cli", []string{"--package", "pkg@1.0.0", "pkg-cli", "--safe"}, []string{"--safe"}},
		{"direct executable", "/usr/local/bin/server", "npm", "pkg", "server", []string{"--safe"}, []string{"--safe"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := StripRuntimeCarrierArgs(test.command, test.args, test.source, test.packageName, test.executable)
			if err != nil || strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
				t.Fatalf("got=%q want=%q err=%v", got, test.want, err)
			}
		})
	}
	if _, err := StripRuntimeCarrierArgs("npx", []string{"--registry", "evil", "pkg"}, "npm", "pkg", "pkg"); err == nil {
		t.Fatal("unknown carrier option was accepted")
	}
	if _, err := StripRuntimeCarrierArgs("uvx", []string{"other==1"}, "pypi", "pkg", "pkg"); err == nil {
		t.Fatal("mismatched package was accepted")
	}
}
