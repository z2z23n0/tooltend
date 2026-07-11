package adapter

import "testing"

func TestNormalizeGitLocator(t *testing.T) {
	for input, want := range map[string]string{
		"git@github.com:Owner/Repo.git":       "https://github.com/Owner/Repo",
		"https://GitHub.com/Owner/Repo.git/":  "https://github.com/Owner/Repo",
		"ssh://git@github.com/Owner/Repo.git": "https://github.com/Owner/Repo",
	} {
		got, err := normalizeGitLocator(input)
		if err != nil || got != want {
			t.Fatalf("normalize %q: got %q, %v", input, got, err)
		}
	}
	if _, err := normalizeGitLocator("git://example.test/repo"); err == nil {
		t.Fatal("expected unauthenticated git protocol to be rejected")
	}
}

func TestValidateSubdir(t *testing.T) {
	for _, value := range []string{"../escape", `..\escape`, "plugins/demo\nother"} {
		if _, err := ValidateSubdir(value); err == nil {
			t.Fatalf("expected escape rejection for %q", value)
		}
	}
	if got, err := ValidateSubdir("./skills/demo"); err != nil || got != "skills/demo" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestCanonicalizeSourceMatchesManagedAdapterIdentities(t *testing.T) {
	tests := []struct {
		kind        SourceKind
		input       Source
		wantLocator string
		wantPackage string
	}{
		{SourceNPM, Source{PackageName: "@Scope/Report-CLI"}, "https://registry.npmjs.org/@scope/report-cli", "@scope/report-cli"},
		{SourcePyPI, Source{PackageName: "Python_Report"}, "https://pypi.org/project/python-report", "python-report"},
		{SourceHomebrew, Source{PackageName: "Owner/Tap/Tool"}, "owner/tap/tool", "owner/tap/tool"},
	}
	for _, test := range tests {
		canonical, err := CanonicalizeSource(test.kind, test.input)
		if err != nil {
			t.Fatalf("canonicalize %s: %v", test.kind, err)
		}
		if canonical.Kind != test.kind || canonical.Locator != test.wantLocator || canonical.PackageName != test.wantPackage {
			t.Fatalf("canonical %s=%#v", test.kind, canonical)
		}
	}
}

func TestRegistryRejectsDuplicateKinds(t *testing.T) {
	if _, err := NewRegistry(Unsupported{Kind: SourceUnknown}, Unsupported{Kind: SourceUnknown}); err == nil {
		t.Fatal("expected duplicate rejection")
	}
}

func TestRemoteMCPDropsSecretsFromLocator(t *testing.T) {
	source, err := (RemoteMCP{}).Normalize(Source{Locator: "https://example.com/mcp?token=secret#fragment"})
	if err != nil {
		t.Fatal(err)
	}
	if source.Locator != "https://example.com/mcp" {
		t.Fatalf("got %q", source.Locator)
	}
}

func TestRemoteMCPDoesNotAdvertiseVersionChecks(t *testing.T) {
	capabilities := (RemoteMCP{}).Capabilities()
	if capabilities.Check || !capabilities.RemoteOnly {
		t.Fatalf("capabilities=%+v", capabilities)
	}
}

func TestRegistryTrackConstraintsCannotBecomeAlternatePackageSources(t *testing.T) {
	if _, err := npmSpec("example", Track{Channel: "exact", Constraint: "https://evil.invalid/a.tgz"}); err == nil {
		t.Fatal("expected npm URL constraint to be rejected")
	}
	if _, err := npmSpec("example", Track{Channel: "semver", Constraint: "^1.2.0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := npmSpec("example", Track{Channel: "exact"}); err == nil {
		t.Fatal("expected empty exact npm version to be rejected")
	}
	for _, value := range []string{"1.2.3", "1.0rc1", "2!1.0+local"} {
		if !validPythonVersion(value) {
			t.Fatalf("expected Python version %q to be accepted", value)
		}
	}
	for _, value := range []string{"", "../wheel", "1.0; os_name", "https://evil.invalid/a.whl"} {
		if validPythonVersion(value) {
			t.Fatalf("expected Python version %q to be rejected", value)
		}
	}
}
