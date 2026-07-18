package bundle

import (
	"context"
	"testing"

	"github.com/z2z23n0/tooltend/internal/execx"
	"github.com/z2z23n0/tooltend/internal/model"
)

type resolverRunner struct {
	stdout []byte
}

func (r resolverRunner) Run(context.Context, string, ...string) (execx.Result, error) {
	return execx.Result{Stdout: r.stdout}, nil
}

func TestRunResolverAcceptsExactGitCommit(t *testing.T) {
	const resolved = "git:ABCDEF0123456789ABCDEF0123456789ABCDEF01"
	service := Service{Runner: resolverRunner{stdout: []byte(resolved + "\n")}}

	got, err := service.runResolver(context.Background(), []string{"resolver"}, model.Installation{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "git:abcdef0123456789abcdef0123456789abcdef01"; got != want {
		t.Fatalf("resolved version = %q, want %q", got, want)
	}
}
