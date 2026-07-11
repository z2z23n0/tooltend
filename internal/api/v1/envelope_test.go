package v1

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteStableEnvelope(t *testing.T) {
	var out bytes.Buffer
	if err := Write(&out, Success("status", map[string]any{"ready": true})); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{`"schema_version":1`, `"command":"status"`, `"ok":true`, `"warnings":[]`} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q missing from %s", want, got)
		}
	}
}

func TestValidateFailure(t *testing.T) {
	if err := (Envelope{SchemaVersion: 1, Command: "x", OK: false}).Validate(); err == nil {
		t.Fatal("expected missing error to fail")
	}
}
