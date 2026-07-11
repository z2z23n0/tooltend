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

type ReviewBundle struct {
	ID            string    `json:"id"`
	CandidateID   string    `json:"candidate_id"`
	CandidateHash string    `json:"candidate_hash"`
	ObjectHash    string    `json:"object_hash"`
	RiskTypesJSON string    `json:"risk_types_json"`
	CreatedAt     time.Time `json:"created_at"`
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
	QueuedAt      time.Time  `json:"queued_at"`
	ShownAt       *time.Time `json:"shown_at,omitempty"`
}

type Scan struct {
	ID         string     `json:"id"`
	Reason     string     `json:"reason"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}
