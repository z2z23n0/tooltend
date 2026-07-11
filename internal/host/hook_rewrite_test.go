package host

import "testing"

func TestRewriteHookCommandPreservesLiteralArgs(t *testing.T) {
	for original, want := range map[string]string{
		"/old/hook --mode safe":                  "'/managed/hook' --mode safe",
		"python3 '/old/hook.py' --mode safe":     "'/managed/hook' --mode safe",
		"ENV=value /bin/sh /old/hook.sh --quiet": "ENV=value '/managed/hook' --quiet",
	} {
		got, err := RewriteHookCommand(original, "/managed/hook")
		if err != nil || got != want {
			t.Fatalf("rewrite %q: got %q want %q err=%v", original, got, want, err)
		}
	}
	if _, err := RewriteHookCommand("/old/hook | tee /tmp/log", "/managed/hook"); err == nil {
		t.Fatal("expected complex shell hook to be rejected")
	}
}
