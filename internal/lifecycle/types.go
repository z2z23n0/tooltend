package lifecycle

import (
	"time"

	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/policy"
)

type UpdateOptions struct {
	Stage              bool
	Activate           bool
	Reason             string
	ExpectedRef        string
	ExpectedGeneration string
	BindGeneration     bool
}

type UpdateTarget struct {
	ComponentID      string `json:"component_id"`
	BindingID        string `json:"binding_id"`
	ResolvedRef      string `json:"resolved_ref"`
	Version          string `json:"version,omitempty"`
	ActiveGeneration string `json:"active_generation,omitempty"`
}

type UpdateResult struct {
	ComponentID string                `json:"component_id"`
	BindingID   string                `json:"binding_id"`
	ResolvedRef string                `json:"resolved_ref,omitempty"`
	Candidate   model.UpdateCandidate `json:"candidate"`
	Checked     bool                  `json:"checked"`
	Staged      bool                  `json:"staged"`
	Activated   bool                  `json:"activated"`
	NeedsReview bool                  `json:"needs_review"`
	Decision    policy.Decision       `json:"decision"`
	Warnings    []string              `json:"warnings,omitempty"`
}

type AdoptOptions struct {
	Source                 string
	Subdir                 string
	Version                string
	Executable             string
	BindingID              string
	ExpectedSourceIdentity string
	ExpectedResolvedRef    string
	ExpectedObservedHash   string
	ExpectedConfigHash     string
}

type AdoptTarget struct {
	ComponentID    string `json:"component_id"`
	BindingID      string `json:"binding_id"`
	ResolvedRef    string `json:"resolved_ref,omitempty"`
	Version        string `json:"version,omitempty"`
	Subdir         string `json:"subdir,omitempty"`
	SourceIdentity string `json:"source_identity,omitempty"`
	ObservedHash   string `json:"observed_hash,omitempty"`
	ConfigHash     string `json:"config_hash,omitempty"`
}

type AdoptResult struct {
	ComponentID string        `json:"component_id"`
	BindingID   string        `json:"binding_id"`
	Generation  string        `json:"generation"`
	TreeHash    string        `json:"tree_hash"`
	Shim        string        `json:"shim,omitempty"`
	Backup      string        `json:"backup,omitempty"`
	Baseline    bool          `json:"baseline_verified"`
	Receipt     model.Receipt `json:"receipt"`
}

type RollbackResult struct {
	ComponentID string        `json:"component_id"`
	BindingID   string        `json:"binding_id"`
	From        string        `json:"from_generation"`
	To          string        `json:"to_generation"`
	Receipt     model.Receipt `json:"receipt"`
}

type RollbackOptions struct {
	To                     string
	ExpectedFromGeneration string
}

type RollbackTarget struct {
	ComponentID    string `json:"component_id"`
	BindingID      string `json:"binding_id"`
	FromGeneration string `json:"from_generation"`
	FromRef        string `json:"from_ref"`
	ToGeneration   string `json:"to_generation"`
	ToRef          string `json:"to_ref"`
}

type RunResult struct {
	AlreadyRunning bool           `json:"already_running"`
	StartedAt      time.Time      `json:"started_at"`
	FinishedAt     time.Time      `json:"finished_at"`
	Recovered      int            `json:"recovered_activations"`
	Checked        int            `json:"checked"`
	Updated        int            `json:"updated"`
	Failed         int            `json:"failed"`
	Results        []UpdateResult `json:"results"`
}
