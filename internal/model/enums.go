package model

import "fmt"

type ComponentKind string

const (
	ComponentSkill    ComponentKind = "skill"
	ComponentPlugin   ComponentKind = "plugin"
	ComponentHook     ComponentKind = "hook"
	ComponentStdioMCP ComponentKind = "stdio_mcp"
	ComponentHTTPMCP  ComponentKind = "http_mcp"
	ComponentCLI      ComponentKind = "cli"
)

type SourceKind string

const (
	SourceGit      SourceKind = "git"
	SourceNPM      SourceKind = "npm"
	SourcePyPI     SourceKind = "pypi"
	SourceHomebrew SourceKind = "homebrew"
	SourceHTTP     SourceKind = "http"
	SourceLocal    SourceKind = "local"
	SourceUnknown  SourceKind = "unknown"
)

type HostKind string

const (
	HostCodex  HostKind = "codex"
	HostClaude HostKind = "claude"
	HostSystem HostKind = "system"
)

type ScopeKind string

const (
	ScopeGlobal  ScopeKind = "global"
	ScopeProject ScopeKind = "project"
)

type Classification string

const (
	ClassificationClean      Classification = "clean"
	ClassificationCustomized Classification = "customized"
	ClassificationDetached   Classification = "detached"
	ClassificationUnknown    Classification = "unknown"
)

type TrackChannel string

const (
	TrackStable TrackChannel = "stable"
	TrackLatest TrackChannel = "latest"
	TrackMain   TrackChannel = "main"
	TrackSemver TrackChannel = "semver"
	TrackExact  TrackChannel = "exact"
)

type ApplyMode string

const (
	ApplyAuto   ApplyMode = "auto"
	ApplyManual ApplyMode = "manual"
	ApplyIgnore ApplyMode = "ignore"
)

type NotifyMode string

const (
	NotifyAll      NotifyMode = "all"
	NotifyFailures NotifyMode = "failures"
	NotifyNone     NotifyMode = "none"
)

type TrustLevel string

const (
	TrustUnknown  TrustLevel = "unknown"
	TrustLocal    TrustLevel = "local"
	TrustVerified TrustLevel = "verified"
	TrustPinned   TrustLevel = "pinned"
)

type ObjectKind string

const (
	ObjectBlob   ObjectKind = "blob"
	ObjectTree   ObjectKind = "tree"
	ObjectBundle ObjectKind = "bundle"
)

type CandidateStatus string

const (
	CandidateAvailable   CandidateStatus = "available"
	CandidateStaging     CandidateStatus = "staging"
	CandidateVerified    CandidateStatus = "verified"
	CandidateMerging     CandidateStatus = "merging"
	CandidateValidating  CandidateStatus = "validating"
	CandidateNeedsReview CandidateStatus = "needs_review"
	CandidateReady       CandidateStatus = "ready"
	CandidateActivating  CandidateStatus = "activating"
	CandidateActive      CandidateStatus = "active"
	CandidateRolledBack  CandidateStatus = "rolled_back"
	CandidateFailed      CandidateStatus = "failed"
	CandidateSuperseded  CandidateStatus = "superseded"
)

type ReviewClass string

const (
	ReviewNone              ReviewClass = "none"
	ReviewSemanticSkillOnly ReviewClass = "semantic_skill_only"
	ReviewHumanRequired     ReviewClass = "human_required"
)

type ReviewVerdict string

const (
	VerdictSafe      ReviewVerdict = "safe"
	VerdictConflict  ReviewVerdict = "conflict"
	VerdictUncertain ReviewVerdict = "uncertain"
)

type ActorType string

const (
	ActorHuman ActorType = "human"
	ActorAgent ActorType = "agent"
)

type GenerationState string

const (
	GenerationPrepared GenerationState = "prepared"
	GenerationActive   GenerationState = "active"
	GenerationInactive GenerationState = "inactive"
	GenerationFailed   GenerationState = "failed"
	GenerationOriginal GenerationState = "original"
)

type ActivationPhase string

const (
	ActivationPrepared        ActivationPhase = "prepared"
	ActivationPointerSwitched ActivationPhase = "pointer_switched"
	ActivationCommitted       ActivationPhase = "committed"
	ActivationRolledBack      ActivationPhase = "rolled_back"
)

type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskRunning   TaskStatus = "running"
	TaskSucceeded TaskStatus = "succeeded"
	TaskFailed    TaskStatus = "failed"
)

type ReceiptAction string

const (
	ReceiptUpdate   ReceiptAction = "update"
	ReceiptRollback ReceiptAction = "rollback"
	ReceiptAdopt    ReceiptAction = "adopt"
)

type ReceiptStatus string

const (
	ReceiptSucceeded  ReceiptStatus = "succeeded"
	ReceiptRolledBack ReceiptStatus = "rolled_back"
	ReceiptFailed     ReceiptStatus = "failed"
)

// LifecycleOwner identifies the authority that is allowed to mutate an
// installation. Only tooltend and delegated owners may ever execute update
// commands; every other owner is observation-only.
type LifecycleOwner string

const (
	LifecycleToolTend        LifecycleOwner = "tooltend"
	LifecycleDelegated       LifecycleOwner = "delegated"
	LifecycleHostOwned       LifecycleOwner = "host-owned"
	LifecycleAppOwned        LifecycleOwner = "app-owned"
	LifecycleWorkspaceLinked LifecycleOwner = "workspace-linked"
	LifecycleUnresolved      LifecycleOwner = "unresolved"
)

type BundlePolicyMode string

const (
	BundlePolicyAuto    BundlePolicyMode = "auto"
	BundlePolicyManual  BundlePolicyMode = "manual"
	BundlePolicyObserve BundlePolicyMode = "observe"
	BundlePolicyIgnore  BundlePolicyMode = "ignore"
)

type BundleConfigState string

const (
	BundleUnconfigured BundleConfigState = "unconfigured"
	BundleConfigured   BundleConfigState = "configured"
)

type BundleConfidence string

const (
	BundleConfidenceHigh       BundleConfidence = "high"
	BundleConfidenceMedium     BundleConfidence = "medium"
	BundleConfidenceLow        BundleConfidence = "low"
	BundleConfidenceUnresolved BundleConfidence = "unresolved"
)

type ArtifactKind string

const (
	ArtifactCLI    ArtifactKind = "cli"
	ArtifactSkill  ArtifactKind = "skill"
	ArtifactHook   ArtifactKind = "hook"
	ArtifactApp    ArtifactKind = "app"
	ArtifactConfig ArtifactKind = "config"
	ArtifactBinary ArtifactKind = "embedded_binary"
	ArtifactPlugin ArtifactKind = "plugin"
	ArtifactMCP    ArtifactKind = "mcp"
)

type BundleTransactionStatus string

const (
	BundleTransactionPrepared    BundleTransactionStatus = "prepared"
	BundleTransactionStaging     BundleTransactionStatus = "staging"
	BundleTransactionActivating  BundleTransactionStatus = "activating"
	BundleTransactionCommitted   BundleTransactionStatus = "committed"
	BundleTransactionRollingBack BundleTransactionStatus = "rolling_back"
	BundleTransactionRolledBack  BundleTransactionStatus = "rolled_back"
	BundleTransactionFailed      BundleTransactionStatus = "failed"
)

type BundleStepStatus string

const (
	BundleStepPending      BundleStepStatus = "pending"
	BundleStepStaged       BundleStepStatus = "staged"
	BundleStepActivating   BundleStepStatus = "activating"
	BundleStepActivated    BundleStepStatus = "activated"
	BundleStepHealthy      BundleStepStatus = "healthy"
	BundleStepCompensating BundleStepStatus = "compensating"
	BundleStepCompensated  BundleStepStatus = "compensated"
	BundleStepFailed       BundleStepStatus = "failed"
)

func validateEnum[T ~string](name string, value T, allowed ...T) error {
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("model: invalid %s %q", name, value)
}

func (v ComponentKind) Validate() error {
	return validateEnum("component kind", v, ComponentSkill, ComponentPlugin, ComponentHook, ComponentStdioMCP, ComponentHTTPMCP, ComponentCLI)
}
func (v SourceKind) Validate() error {
	return validateEnum("source kind", v, SourceGit, SourceNPM, SourcePyPI, SourceHomebrew, SourceHTTP, SourceLocal, SourceUnknown)
}
func (v HostKind) Validate() error {
	return validateEnum("host kind", v, HostCodex, HostClaude, HostSystem)
}
func (v ScopeKind) Validate() error { return validateEnum("scope", v, ScopeGlobal, ScopeProject) }
func (v Classification) Validate() error {
	return validateEnum("classification", v, ClassificationClean, ClassificationCustomized, ClassificationDetached, ClassificationUnknown)
}
func (v TrackChannel) Validate() error {
	return validateEnum("track channel", v, TrackStable, TrackLatest, TrackMain, TrackSemver, TrackExact)
}
func (v ApplyMode) Validate() error {
	return validateEnum("apply mode", v, ApplyAuto, ApplyManual, ApplyIgnore)
}
func (v NotifyMode) Validate() error {
	return validateEnum("notify mode", v, NotifyAll, NotifyFailures, NotifyNone)
}
func (v TrustLevel) Validate() error {
	return validateEnum("trust level", v, TrustUnknown, TrustLocal, TrustVerified, TrustPinned)
}
func (v CandidateStatus) Validate() error {
	return validateEnum("candidate status", v, CandidateAvailable, CandidateStaging, CandidateVerified, CandidateMerging, CandidateValidating, CandidateNeedsReview, CandidateReady, CandidateActivating, CandidateActive, CandidateRolledBack, CandidateFailed, CandidateSuperseded)
}
func (v ReviewClass) Validate() error {
	return validateEnum("review class", v, ReviewNone, ReviewSemanticSkillOnly, ReviewHumanRequired)
}
func (v ReviewVerdict) Validate() error {
	return validateEnum("review verdict", v, VerdictSafe, VerdictConflict, VerdictUncertain)
}
func (v ActorType) Validate() error { return validateEnum("actor type", v, ActorHuman, ActorAgent) }
func (v GenerationState) Validate() error {
	return validateEnum("generation state", v, GenerationPrepared, GenerationActive, GenerationInactive, GenerationFailed, GenerationOriginal)
}
func (v ActivationPhase) Validate() error {
	return validateEnum("activation phase", v, ActivationPrepared, ActivationPointerSwitched, ActivationCommitted, ActivationRolledBack)
}
func (v TaskStatus) Validate() error {
	return validateEnum("task status", v, TaskPending, TaskRunning, TaskSucceeded, TaskFailed)
}
func (v ReceiptAction) Validate() error {
	return validateEnum("receipt action", v, ReceiptUpdate, ReceiptRollback, ReceiptAdopt)
}
func (v ReceiptStatus) Validate() error {
	return validateEnum("receipt status", v, ReceiptSucceeded, ReceiptRolledBack, ReceiptFailed)
}
func (v LifecycleOwner) Validate() error {
	return validateEnum("lifecycle owner", v, LifecycleToolTend, LifecycleDelegated, LifecycleHostOwned, LifecycleAppOwned, LifecycleWorkspaceLinked, LifecycleUnresolved)
}
func (v BundlePolicyMode) Validate() error {
	return validateEnum("bundle policy", v, BundlePolicyAuto, BundlePolicyManual, BundlePolicyObserve, BundlePolicyIgnore)
}
func (v BundleConfigState) Validate() error {
	return validateEnum("bundle config state", v, BundleUnconfigured, BundleConfigured)
}
func (v BundleConfidence) Validate() error {
	return validateEnum("bundle confidence", v, BundleConfidenceHigh, BundleConfidenceMedium, BundleConfidenceLow, BundleConfidenceUnresolved)
}
func (v ArtifactKind) Validate() error {
	return validateEnum("artifact kind", v, ArtifactCLI, ArtifactSkill, ArtifactHook, ArtifactApp, ArtifactConfig, ArtifactBinary, ArtifactPlugin, ArtifactMCP)
}
func (v BundleTransactionStatus) Validate() error {
	return validateEnum("bundle transaction status", v, BundleTransactionPrepared, BundleTransactionStaging, BundleTransactionActivating, BundleTransactionCommitted, BundleTransactionRollingBack, BundleTransactionRolledBack, BundleTransactionFailed)
}
func (v BundleStepStatus) Validate() error {
	return validateEnum("bundle step status", v, BundleStepPending, BundleStepStaged, BundleStepActivating, BundleStepActivated, BundleStepHealthy, BundleStepCompensating, BundleStepCompensated, BundleStepFailed)
}
