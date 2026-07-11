package adapter

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/z2z23n0/tooltend/internal/execx"
)

type Homebrew struct {
	Runner execx.Runner
}

func (Homebrew) Name() string        { return "homebrew" }
func (Homebrew) Kinds() []SourceKind { return []SourceKind{SourceHomebrew} }
func (Homebrew) Capabilities() Capabilities {
	return Capabilities{Check: true, Stage: false, ManagedAuto: false, Rollback: false}
}

func (Homebrew) Normalize(source Source) (Source, error) {
	return CanonicalizeSource(SourceHomebrew, source)
}

func (h Homebrew) Resolve(ctx context.Context, source Source, _ Track) (Resolved, error) {
	if h.Runner == nil {
		h.Runner = execx.ExecRunner{}
	}
	result, err := h.Runner.Run(ctx, "brew", "info", "--json=v2", source.PackageName)
	if err != nil {
		return Resolved{}, errors.New("Homebrew version lookup failed")
	}
	var payload struct {
		Formulae []struct {
			Versions struct {
				Stable string `json:"stable"`
			} `json:"versions"`
		} `json:"formulae"`
		Casks []struct {
			Version string `json:"version"`
		} `json:"casks"`
	}
	if json.Unmarshal(result.Stdout, &payload) != nil {
		return Resolved{}, errors.New("Homebrew returned invalid metadata")
	}
	version := ""
	if len(payload.Formulae) > 0 {
		version = payload.Formulae[0].Versions.Stable
	} else if len(payload.Casks) > 0 {
		version = payload.Casks[0].Version
	}
	if version == "" {
		return Resolved{}, errors.New("Homebrew package was not found")
	}
	return Resolved{Version: version, Ref: source.PackageName + "@" + version}, nil
}

func (Homebrew) Fetch(context.Context, Source, Resolved, string) (Artifact, error) {
	return Artifact{}, errors.New("Homebrew stays manual because reliable isolated rollback is unavailable")
}

func (Homebrew) Verify(context.Context, Source, Resolved, Artifact) error {
	return errors.New("Homebrew artifact verification is unavailable")
}
