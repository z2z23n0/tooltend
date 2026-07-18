package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

type Policy struct {
	BindingID         string       `json:"binding_id"`
	TrackChannel      TrackChannel `json:"track_channel"`
	Constraint        string       `json:"constraint,omitempty"`
	ExpectedIntegrity string       `json:"expected_integrity,omitempty"`
	ApplyMode         ApplyMode    `json:"apply_mode"`
	NotifyMode        NotifyMode   `json:"notify_mode"`
	LocalCapMode      ApplyMode    `json:"local_cap_mode"`
	UpdatedAt         time.Time    `json:"updated_at"`
}

func DefaultPolicy() Policy {
	return Policy{TrackChannel: TrackStable, ApplyMode: ApplyAuto, NotifyMode: NotifyFailures, LocalCapMode: ApplyAuto}
}

func (p Policy) Validate() error {
	if err := p.TrackChannel.Validate(); err != nil {
		return err
	}
	if err := p.ApplyMode.Validate(); err != nil {
		return err
	}
	if err := p.NotifyMode.Validate(); err != nil {
		return err
	}
	if err := p.LocalCapMode.Validate(); err != nil {
		return err
	}
	if p.ExpectedIntegrity != "" {
		if p.TrackChannel != TrackExact {
			return errors.New("policy: expected integrity requires exact tracking")
		}
		decoded, err := hex.DecodeString(p.ExpectedIntegrity)
		if err != nil || len(decoded) != sha256.Size {
			return errors.New("policy: expected integrity must be a SHA-256 hash")
		}
	}
	return nil
}

type Project struct {
	ID              string    `json:"id"`
	RootPath        string    `json:"root_path"`
	RootFingerprint string    `json:"root_fingerprint"`
	Selected        bool      `json:"selected"`
	DiscoveredVia   string    `json:"discovered_via,omitempty"`
	LastSeenAt      time.Time `json:"last_seen_at"`
}

type Source struct {
	ID           string     `json:"id"`
	Kind         SourceKind `json:"kind"`
	Locator      string     `json:"locator"`
	Subdir       string     `json:"subdir,omitempty"`
	PackageName  string     `json:"package_name,omitempty"`
	IdentityHash string     `json:"identity_hash"`
	MetadataJSON string     `json:"metadata_json,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type SourceTrust struct {
	SourceID   string     `json:"source_id"`
	Level      TrustLevel `json:"level"`
	ApprovedBy string     `json:"approved_by,omitempty"`
	ApprovedAt time.Time  `json:"approved_at"`
}

type LogicalComponent struct {
	ID         string        `json:"id"`
	Kind       ComponentKind `json:"kind"`
	Name       string        `json:"name"`
	SourceID   string        `json:"source_id,omitempty"`
	LogicalKey string        `json:"logical_key"`
	CreatedAt  time.Time     `json:"created_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
}

type Binding struct {
	ID                 string         `json:"id"`
	ComponentID        string         `json:"component_id"`
	Host               HostKind       `json:"host"`
	ProjectID          string         `json:"project_id,omitempty"`
	Scope              ScopeKind      `json:"scope"`
	InstallPath        string         `json:"install_path"`
	ConfigPath         string         `json:"config_path,omitempty"`
	ConfigPointer      string         `json:"config_pointer,omitempty"`
	InstallMethod      string         `json:"install_method,omitempty"`
	Managed            bool           `json:"managed"`
	Classification     Classification `json:"classification"`
	ObservedHash       string         `json:"observed_hash,omitempty"`
	ObservedVersion    string         `json:"observed_version,omitempty"`
	TrustHash          string         `json:"trust_hash,omitempty"`
	ActiveGenerationID string         `json:"active_generation_id,omitempty"`
	LastSeenAt         time.Time      `json:"last_seen_at"`
}

type ObjectRecord struct {
	Hash         string     `json:"hash"`
	Kind         ObjectKind `json:"kind"`
	Size         int64      `json:"size"`
	RelativePath string     `json:"relative_path"`
	VerifiedAt   time.Time  `json:"verified_at"`
	CreatedAt    time.Time  `json:"created_at"`
}

type Baseline struct {
	ID          string    `json:"id"`
	BindingID   string    `json:"binding_id"`
	SourceID    string    `json:"source_id"`
	ResolvedRef string    `json:"resolved_ref"`
	TreeHash    string    `json:"tree_hash"`
	CreatedAt   time.Time `json:"created_at"`
}

type Overlay struct {
	ID         string    `json:"id"`
	BindingID  string    `json:"binding_id"`
	BaselineID string    `json:"baseline_id"`
	TreeHash   string    `json:"tree_hash"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

type Dependency struct {
	ID              string `json:"id"`
	FromComponentID string `json:"from_component_id"`
	ToComponentID   string `json:"to_component_id,omitempty"`
	PackageIdentity string `json:"package_identity"`
	Constraint      string `json:"constraint,omitempty"`
	EvidencePath    string `json:"evidence_path"`
	EvidenceLine    int    `json:"evidence_line,omitempty"`
	Explicit        bool   `json:"explicit"`
}

type Generation struct {
	ID            string          `json:"id"`
	BindingID     string          `json:"binding_id"`
	CandidateID   string          `json:"candidate_id,omitempty"`
	ResolvedRef   string          `json:"resolved_ref"`
	TreeHash      string          `json:"tree_hash"`
	IntegrityHash string          `json:"integrity_hash,omitempty"`
	State         GenerationState `json:"state"`
	CreatedAt     time.Time       `json:"created_at"`
	ActivatedAt   *time.Time      `json:"activated_at,omitempty"`
}

type UpdateCandidate struct {
	ID               string          `json:"id"`
	BindingID        string          `json:"binding_id"`
	SourceID         string          `json:"source_id"`
	ResolvedRef      string          `json:"resolved_ref"`
	UpstreamTreeHash string          `json:"upstream_tree_hash"`
	BaselineID       string          `json:"baseline_id,omitempty"`
	OverlayID        string          `json:"overlay_id,omitempty"`
	MergedTreeHash   string          `json:"merged_tree_hash,omitempty"`
	CandidateHash    string          `json:"candidate_hash"`
	Status           CandidateStatus `json:"status"`
	ReviewClass      ReviewClass     `json:"review_class"`
	FailureCode      string          `json:"failure_code,omitempty"`
	FailureSummary   string          `json:"failure_summary,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type ReviewPacket struct {
	ID            string    `json:"id"`
	CandidateID   string    `json:"candidate_id"`
	CandidateHash string    `json:"candidate_hash"`
	ObjectHash    string    `json:"object_hash"`
	RiskTypesJSON string    `json:"risk_types_json"`
	CreatedAt     time.Time `json:"created_at"`
}

// ReviewBundle is kept as a source-compatible alias for the v0.1 component
// review API. The product-level Bundle model is intentionally separate.
type ReviewBundle = ReviewPacket

type Bundle struct {
	ID               string            `json:"id"`
	Slug             string            `json:"slug"`
	Name             string            `json:"name"`
	RecipeID         string            `json:"recipe_id"`
	RecipeVersion    string            `json:"recipe_version"`
	RecipeSource     string            `json:"recipe_source"`
	Owner            LifecycleOwner    `json:"lifecycle_owner"`
	ConfigState      BundleConfigState `json:"config_state"`
	Confidence       BundleConfidence  `json:"confidence"`
	CurrentReleaseID string            `json:"current_release_id,omitempty"`
	MetadataJSON     string            `json:"metadata_json,omitempty"`
	DiscoveredAt     time.Time         `json:"discovered_at"`
	LastSeenAt       time.Time         `json:"last_seen_at"`
}

type BundleRelease struct {
	ID           string    `json:"id"`
	BundleID     string    `json:"bundle_id"`
	Version      string    `json:"version"`
	ResolvedRef  string    `json:"resolved_ref,omitempty"`
	ManifestJSON string    `json:"manifest_json"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
}

type BundleArtifact struct {
	ID           string       `json:"id"`
	BundleID     string       `json:"bundle_id"`
	ReleaseID    string       `json:"release_id,omitempty"`
	RecipeKey    string       `json:"recipe_key"`
	Kind         ArtifactKind `json:"kind"`
	Name         string       `json:"name"`
	Ordinal      int          `json:"ordinal"`
	Required     bool         `json:"required"`
	Driver       string       `json:"driver"`
	MetadataJSON string       `json:"metadata_json"`
}

type Installation struct {
	ID              string         `json:"id"`
	BundleID        string         `json:"bundle_id"`
	ArtifactID      string         `json:"artifact_id,omitempty"`
	Driver          string         `json:"driver"`
	Path            string         `json:"path"`
	PackageIdentity string         `json:"package_identity,omitempty"`
	SourceIdentity  string         `json:"source_identity,omitempty"`
	ObservedVersion string         `json:"observed_version,omitempty"`
	ObservedHash    string         `json:"observed_hash,omitempty"`
	Owner           LifecycleOwner `json:"lifecycle_owner"`
	Managed         bool           `json:"managed"`
	MetadataJSON    string         `json:"metadata_json"`
	LastSeenAt      time.Time      `json:"last_seen_at"`
}

type ConsumerBinding struct {
	ID             string    `json:"id"`
	InstallationID string    `json:"installation_id"`
	BindingID      string    `json:"binding_id,omitempty"`
	Host           HostKind  `json:"host"`
	ProjectID      string    `json:"project_id,omitempty"`
	Scope          ScopeKind `json:"scope"`
	ConfigPath     string    `json:"config_path,omitempty"`
	ConfigPointer  string    `json:"config_pointer,omitempty"`
	LastSeenAt     time.Time `json:"last_seen_at"`
}

type BundlePolicy struct {
	BundleID      string           `json:"bundle_id"`
	Mode          BundlePolicyMode `json:"mode"`
	RecipeTrusted bool             `json:"recipe_trusted"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

type BundleTransaction struct {
	ID            string                  `json:"id"`
	BundleID      string                  `json:"bundle_id"`
	FromReleaseID string                  `json:"from_release_id,omitempty"`
	ToReleaseID   string                  `json:"to_release_id,omitempty"`
	Status        BundleTransactionStatus `json:"status"`
	StageOnly     bool                    `json:"stage_only"`
	ErrorCode     string                  `json:"error_code,omitempty"`
	ErrorSummary  string                  `json:"error_summary,omitempty"`
	StartedAt     time.Time               `json:"started_at"`
	UpdatedAt     time.Time               `json:"updated_at"`
	CompletedAt   *time.Time              `json:"completed_at,omitempty"`
}

type BundleTransactionStep struct {
	ID             string           `json:"id"`
	TransactionID  string           `json:"transaction_id"`
	Ordinal        int              `json:"ordinal"`
	ArtifactID     string           `json:"artifact_id,omitempty"`
	InstallationID string           `json:"installation_id,omitempty"`
	Kind           string           `json:"kind"`
	Status         BundleStepStatus `json:"status"`
	CommandJSON    string           `json:"command_json"`
	RollbackJSON   string           `json:"rollback_json"`
	BeforeJSON     string           `json:"before_json"`
	AfterJSON      string           `json:"after_json"`
	ErrorCode      string           `json:"error_code,omitempty"`
	ErrorSummary   string           `json:"error_summary,omitempty"`
	StartedAt      *time.Time       `json:"started_at,omitempty"`
	CompletedAt    *time.Time       `json:"completed_at,omitempty"`
}

type BundleReceipt struct {
	ID            string    `json:"id"`
	BundleID      string    `json:"bundle_id"`
	TransactionID string    `json:"transaction_id,omitempty"`
	ReleaseID     string    `json:"release_id,omitempty"`
	Action        string    `json:"action"`
	Status        string    `json:"status"`
	SummaryJSON   string    `json:"summary_json"`
	CreatedAt     time.Time `json:"created_at"`
}

type BundleHealthCheck struct {
	ID             string    `json:"id"`
	BundleID       string    `json:"bundle_id"`
	ArtifactID     string    `json:"artifact_id,omitempty"`
	InstallationID string    `json:"installation_id,omitempty"`
	Name           string    `json:"name"`
	Status         string    `json:"status"`
	Summary        string    `json:"summary,omitempty"`
	CheckedAt      time.Time `json:"checked_at"`
}

type BundleTask struct {
	ID             string     `json:"id"`
	BundleID       string     `json:"bundle_id"`
	InstallationID string     `json:"installation_id,omitempty"`
	Kind           string     `json:"kind"`
	IdempotencyKey string     `json:"idempotency_key"`
	Status         TaskStatus `json:"status"`
	Attempts       int        `json:"attempts"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	LeaseUntil     *time.Time `json:"lease_until,omitempty"`
	ErrorCode      string     `json:"error_code,omitempty"`
	ErrorSummary   string     `json:"error_summary,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type Review struct {
	ID            string        `json:"id"`
	CandidateID   string        `json:"candidate_id"`
	CandidateHash string        `json:"candidate_hash"`
	ActorType     ActorType     `json:"actor_type"`
	Verdict       ReviewVerdict `json:"verdict"`
	RiskType      string        `json:"risk_type"`
	Summary       string        `json:"summary"`
	CreatedAt     time.Time     `json:"created_at"`
}

type ActivationIntent struct {
	ID              string          `json:"id"`
	BindingID       string          `json:"binding_id"`
	CandidateID     string          `json:"candidate_id"`
	OldGenerationID string          `json:"old_generation_id,omitempty"`
	NewGenerationID string          `json:"new_generation_id"`
	ExpectedPointer string          `json:"expected_pointer"`
	Phase           ActivationPhase `json:"phase"`
	ErrorCode       string          `json:"error_code,omitempty"`
	StartedAt       time.Time       `json:"started_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type Receipt struct {
	ID              string        `json:"id"`
	BindingID       string        `json:"binding_id"`
	CandidateID     string        `json:"candidate_id,omitempty"`
	Action          ReceiptAction `json:"action"`
	OldGenerationID string        `json:"old_generation_id,omitempty"`
	NewGenerationID string        `json:"new_generation_id,omitempty"`
	FromRef         string        `json:"from_ref,omitempty"`
	ToRef           string        `json:"to_ref,omitempty"`
	CandidateHash   string        `json:"candidate_hash,omitempty"`
	Status          ReceiptStatus `json:"status"`
	SummaryJSON     string        `json:"summary_json,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
}

type Task struct {
	ID             string     `json:"id"`
	Kind           string     `json:"kind"`
	BindingID      string     `json:"binding_id,omitempty"`
	CandidateID    string     `json:"candidate_id,omitempty"`
	IdempotencyKey string     `json:"idempotency_key"`
	Status         TaskStatus `json:"status"`
	Attempts       int        `json:"attempts"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	LeaseUntil     *time.Time `json:"lease_until,omitempty"`
	ErrorCode      string     `json:"error_code,omitempty"`
	ErrorSummary   string     `json:"error_summary,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type HookEvent struct {
	ID               int64      `json:"id"`
	OccurredAt       time.Time  `json:"occurred_at"`
	Host             HostKind   `json:"host"`
	EventType        string     `json:"event_type"`
	ProjectID        string     `json:"project_id,omitempty"`
	Installer        string     `json:"installer,omitempty"`
	PackageIdentity  string     `json:"package_identity,omitempty"`
	RequestedVersion string     `json:"requested_version,omitempty"`
	CorrelationHash  string     `json:"correlation_hash,omitempty"`
	ProcessedAt      *time.Time `json:"processed_at,omitempty"`
}

type Notification struct {
	CandidateHash string     `json:"candidate_hash"`
	Kind          string     `json:"kind"`
	Message       string     `json:"message,omitempty"`
	QueuedAt      time.Time  `json:"queued_at"`
	ShownAt       *time.Time `json:"shown_at,omitempty"`
}

type ReconcileRun struct {
	ID          string     `json:"id"`
	Reason      string     `json:"reason"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	ErrorCode   string     `json:"error_code,omitempty"`
	SummaryJSON string     `json:"summary_json"`
}

type Scan struct {
	ID         string     `json:"id"`
	Reason     string     `json:"reason"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}
