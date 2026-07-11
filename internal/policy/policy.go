// Package policy computes fail-closed effective update policy decisions.
package policy

import "slices"

type Mode string

const (
	ModeAuto   Mode = "auto"
	ModeManual Mode = "manual"
	ModeIgnore Mode = "ignore"
)

type Adapter string

const (
	AdapterGitSkill  Adapter = "git_skill"
	AdapterGitPlugin Adapter = "git_plugin"
	AdapterGitHook   Adapter = "git_hook"
	AdapterNPM       Adapter = "npm"
	AdapterNPX       Adapter = "npx"
	AdapterPipx      Adapter = "pipx"
	AdapterUV        Adapter = "uv"
	AdapterUVX       Adapter = "uvx"
	AdapterHomebrew  Adapter = "homebrew"
	AdapterHTTPMCP   Adapter = "http_mcp"
	AdapterUnknown   Adapter = "unknown"
)

type ReasonCode string

const (
	ReasonRequestedManual      ReasonCode = "requested_manual"
	ReasonRequestedIgnore      ReasonCode = "requested_ignore"
	ReasonInvalidMode          ReasonCode = "invalid_mode"
	ReasonProjectRestricted    ReasonCode = "project_restricted"
	ReasonAdapterManualOnly    ReasonCode = "adapter_manual_only"
	ReasonUnknownAdapter       ReasonCode = "unknown_adapter"
	ReasonSourceUnknown        ReasonCode = "source_unknown"
	ReasonBaselineUnknown      ReasonCode = "baseline_unknown"
	ReasonBindingUnmanaged     ReasonCode = "binding_unmanaged"
	ReasonValidationMissing    ReasonCode = "validation_missing"
	ReasonRollbackUnverified   ReasonCode = "rollback_unverified"
	ReasonNotAdopted           ReasonCode = "not_adopted"
	ReasonRuntimeNotIsolated   ReasonCode = "runtime_not_isolated"
	ReasonShimUnstable         ReasonCode = "shim_unstable"
	ReasonVersionNotExact      ReasonCode = "version_not_exact"
	ReasonRuntimeNotPersistent ReasonCode = "runtime_not_persistent"
	ReasonHookContentChanged   ReasonCode = "hook_content_changed"
	ReasonTrustChanged         ReasonCode = "trust_changed"
	ReasonPermissionsChanged   ReasonCode = "permissions_changed"
	ReasonOldArtifactMissing   ReasonCode = "old_artifact_missing"
	ReasonDowngradeUnverified  ReasonCode = "downgrade_unverified"
)

// Input contains positive evidence. Its zero value intentionally cannot
// authorize automatic updates.
type Input struct {
	Adapter Adapter `json:"adapter"`

	// LocalMode is the machine-local policy. ProjectMode may only make it
	// stricter; it can never grant automatic-update authority.
	LocalMode   Mode  `json:"local_mode"`
	ProjectMode *Mode `json:"project_mode,omitempty"`

	SourceKnown       bool `json:"source_known"`
	BaselineKnown     bool `json:"baseline_known"`
	ManagedBinding    bool `json:"managed_binding"`
	ValidationPassed  bool `json:"validation_passed"`
	RollbackVerified  bool `json:"rollback_verified"`
	Adopted           bool `json:"adopted"`
	RuntimeIsolated   bool `json:"runtime_isolated"`
	StableShim        bool `json:"stable_shim"`
	ExactVersion      bool `json:"exact_version"`
	PersistentRuntime bool `json:"persistent_runtime"`

	HookContentUnchanged bool `json:"hook_content_unchanged"`
	TrustUnchanged       bool `json:"trust_unchanged"`
	PermissionsUnchanged bool `json:"permissions_unchanged"`

	OldArtifactAvailable bool `json:"old_artifact_available"`
	DowngradeVerified    bool `json:"downgrade_verified"`
}

type Decision struct {
	Mode        Mode         `json:"mode"`
	ReasonCodes []ReasonCode `json:"reason_codes,omitempty"`
}

// Decide returns automatic mode only when every adapter-specific safety fact
// is explicitly present. Unknown and malformed input is manual.
func Decide(input Input) Decision {
	effective, restrictionReason := Restrict(input.LocalMode, input.ProjectMode)
	switch effective {
	case ModeIgnore:
		reasons := []ReasonCode{ReasonRequestedIgnore}
		if restrictionReason != "" {
			reasons = append(reasons, restrictionReason)
		}
		return Decision{Mode: ModeIgnore, ReasonCodes: unique(reasons)}
	case ModeManual:
		var reasons []ReasonCode
		if input.LocalMode == ModeManual {
			reasons = append(reasons, ReasonRequestedManual)
		}
		if restrictionReason != "" {
			reasons = append(reasons, restrictionReason)
		}
		if !validMode(input.LocalMode) {
			reasons = append(reasons, ReasonInvalidMode)
		}
		if len(reasons) == 0 {
			reasons = append(reasons, ReasonRequestedManual)
		}
		return Decision{Mode: ModeManual, ReasonCodes: unique(reasons)}
	case ModeAuto:
		// Continue through capability checks.
	default:
		return Decision{Mode: ModeManual, ReasonCodes: []ReasonCode{ReasonInvalidMode}}
	}

	var reasons []ReasonCode
	requireCommon := func() {
		if !input.SourceKnown {
			reasons = append(reasons, ReasonSourceUnknown)
		}
		if !input.BaselineKnown {
			reasons = append(reasons, ReasonBaselineUnknown)
		}
		if !input.ManagedBinding {
			reasons = append(reasons, ReasonBindingUnmanaged)
		}
		if !input.ValidationPassed {
			reasons = append(reasons, ReasonValidationMissing)
		}
		if !input.RollbackVerified {
			reasons = append(reasons, ReasonRollbackUnverified)
		}
	}
	requireAdoptedRuntime := func(persistent bool) {
		requireCommon()
		if !input.Adopted {
			reasons = append(reasons, ReasonNotAdopted)
		}
		if !input.RuntimeIsolated {
			reasons = append(reasons, ReasonRuntimeNotIsolated)
		}
		if !input.StableShim {
			reasons = append(reasons, ReasonShimUnstable)
		}
		if !input.ExactVersion {
			reasons = append(reasons, ReasonVersionNotExact)
		}
		if persistent && !input.PersistentRuntime {
			reasons = append(reasons, ReasonRuntimeNotPersistent)
		}
	}

	switch input.Adapter {
	case AdapterGitSkill, AdapterGitPlugin:
		requireCommon()
	case AdapterGitHook:
		requireCommon()
		if !input.HookContentUnchanged {
			reasons = append(reasons, ReasonHookContentChanged)
		}
		if !input.TrustUnchanged {
			reasons = append(reasons, ReasonTrustChanged)
		}
		if !input.PermissionsUnchanged {
			reasons = append(reasons, ReasonPermissionsChanged)
		}
	case AdapterNPM, AdapterPipx, AdapterUV:
		requireAdoptedRuntime(false)
	case AdapterNPX, AdapterUVX:
		requireAdoptedRuntime(true)
	case AdapterHomebrew:
		requireCommon()
		if !input.OldArtifactAvailable {
			reasons = append(reasons, ReasonOldArtifactMissing)
		}
		if !input.DowngradeVerified {
			reasons = append(reasons, ReasonDowngradeUnverified)
		}
	case AdapterHTTPMCP:
		reasons = append(reasons, ReasonAdapterManualOnly)
	case AdapterUnknown, "":
		reasons = append(reasons, ReasonUnknownAdapter)
	default:
		reasons = append(reasons, ReasonUnknownAdapter)
	}

	if restrictionReason != "" {
		reasons = append(reasons, restrictionReason)
	}
	reasons = unique(reasons)
	if len(reasons) != 0 {
		return Decision{Mode: ModeManual, ReasonCodes: reasons}
	}
	return Decision{Mode: ModeAuto}
}

// Restrict applies a project policy monotonically. Restrictiveness is
// auto < manual < ignore. Invalid local policy fails closed to manual;
// invalid project input cannot grant authority and also restricts to manual.
func Restrict(local Mode, project *Mode) (Mode, ReasonCode) {
	if !validMode(local) {
		return ModeManual, ReasonInvalidMode
	}
	if project == nil {
		return local, ""
	}
	if !validMode(*project) {
		if local == ModeIgnore {
			return ModeIgnore, ReasonInvalidMode
		}
		return ModeManual, ReasonInvalidMode
	}
	if rank(*project) > rank(local) {
		return *project, ReasonProjectRestricted
	}
	return local, ""
}

func rank(mode Mode) int {
	switch mode {
	case ModeAuto:
		return 0
	case ModeManual:
		return 1
	case ModeIgnore:
		return 2
	default:
		return 1
	}
}

func validMode(mode Mode) bool {
	return mode == ModeAuto || mode == ModeManual || mode == ModeIgnore
}

func unique(input []ReasonCode) []ReasonCode {
	result := make([]ReasonCode, 0, len(input))
	for _, reason := range input {
		if reason != "" && !slices.Contains(result, reason) {
			result = append(result, reason)
		}
	}
	return result
}
