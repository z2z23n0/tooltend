package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveWithHasNoSideEffects(t *testing.T) {
	home := filepath.Join(t.TempDir(), "missing-home")
	env := map[string]string{"TOOLTEND_HOME": filepath.Join(home, "tooltend")}
	p := ResolveWith(home, func(k string) string { return env[k] })
	if _, err := os.Stat(p.DataDir); !os.IsNotExist(err) {
		t.Fatalf("ResolveWith created data dir: %v", err)
	}
	if p.ConfigFile != filepath.Join(env["TOOLTEND_HOME"], "config.toml") {
		t.Fatalf("unexpected config path %s", p.ConfigFile)
	}
}

func TestResolveWithUsesXDGAndIgnoresRelativeXDG(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(home, "cfg"),
		"XDG_STATE_HOME":  "relative-state",
		"XDG_DATA_HOME":   filepath.Join(home, "data"),
	}
	p := ResolveWith(home, func(k string) string { return env[k] })
	if p.ConfigFile != filepath.Join(home, "cfg", "tooltend", "config.toml") {
		t.Fatalf("config path = %s", p.ConfigFile)
	}
	if p.DatabaseFile != filepath.Join(home, ".local", "state", "tooltend", "state.db") {
		t.Fatalf("state path = %s", p.DatabaseFile)
	}
}

func TestSaveAtomicRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	c := Default()
	if err := SaveAtomic(path, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != c.Version || got.Notify.Mode != c.Notify.Mode {
		t.Fatalf("round trip mismatch: %#v", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestValidateRejectsUnsafeRuntimeShimDirectory(t *testing.T) {
	for _, value := range []string{"relative/bin", "/tmp/tooltend\n/bin", "/tmp/tooltend\x00/bin"} {
		c := Default()
		c.Runtime.ShimDir = value
		if err := c.Validate(); err == nil {
			t.Fatalf("unsafe shim directory accepted: %q", value)
		}
	}
	c := Default()
	c.Runtime.ShimDir = filepath.Join(t.TempDir(), "bin")
	if err := c.Validate(); err != nil {
		t.Fatalf("absolute shim directory rejected: %v", err)
	}
}
