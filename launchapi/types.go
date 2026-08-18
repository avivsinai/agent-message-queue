// Package launchapi defines AMQ's versioned public launch-intent contract.
// Runtime plans, roots, leases, bindings, journals, and backend mutation stay
// owned by internal/launch and are not represented by caller-writable types.
package launchapi

import "time"

const (
	IntentVersionV1  = 1
	RequestVersionV1 = 1
	ResultVersionV1  = 1
	SubjectSchemaV1  = 1
	SubjectSchemaV2  = 2
)

type ResultHintV1 string

const HintReprepareRecommended ResultHintV1 = "reprepare_recommended"

const (
	PrepareOutcomeReady          = "ready"
	PrepareOutcomeActionRequired = "action_required"
	PrepareOutcomeUnsupported    = "unsupported"
)

type ResumePolicy string

const (
	ResumePolicyResume   ResumePolicy = "resume"
	ResumePolicyFresh    ResumePolicy = "fresh"
	ResumePolicyDisabled ResumePolicy = "disabled"
)

type WorkingDirectoryKind string

const (
	WorkingDirectoryRelative WorkingDirectoryKind = "relative"
	WorkingDirectoryAbsolute WorkingDirectoryKind = "absolute"
)

type WakePolicy string

const (
	WakeEnabled  WakePolicy = "enabled"
	WakeDisabled WakePolicy = "disabled"
)

type InjectorMode string

const (
	InjectorAuto  InjectorMode = "auto"
	InjectorRaw   InjectorMode = "raw"
	InjectorPaste InjectorMode = "paste"
	InjectorNone  InjectorMode = "none"
)

type SymphonyEvent string

const (
	SymphonyAfterCreate  SymphonyEvent = "after_create"
	SymphonyBeforeRun    SymphonyEvent = "before_run"
	SymphonyAfterRun     SymphonyEvent = "after_run"
	SymphonyBeforeRemove SymphonyEvent = "before_remove"
)

type LaunchIntentV1 struct {
	IntentVersion int             `json:"intent_version"`
	Participants  []ParticipantV1 `json:"participants"`
}

type ParticipantV1 struct {
	Handle       string              `json:"handle"`
	Runnable     bool                `json:"runnable"`
	Executable   string              `json:"executable,omitempty"`
	Args         []string            `json:"args,omitempty"`
	InitialInput *InitialInputV1     `json:"initial_input,omitempty"`
	Cwd          *WorkingDirectoryV1 `json:"cwd,omitempty"`
	EnvOverlay   map[string]string   `json:"env_overlay,omitempty"`
	ResumePolicy ResumePolicy        `json:"resume_policy,omitempty"`
	Execution    *ExecutionOptionsV1 `json:"execution,omitempty"`
}

type WorkingDirectoryV1 struct {
	Kind WorkingDirectoryKind `json:"kind"`
	Path string               `json:"path"`
}

type ExecutionOptionsV1 struct {
	RequireWake  bool           `json:"require_wake"`
	NoGitignore  bool           `json:"no_gitignore"`
	Wake         WakeOptionsV1  `json:"wake"`
	Integrations IntegrationsV1 `json:"integrations,omitempty"`
}

type WakeOptionsV1 struct {
	Mode        WakePolicy         `json:"mode"`
	AuditReason string             `json:"audit_reason,omitempty"`
	Injector    *InjectorOptionsV1 `json:"injector,omitempty"`
}

type IntegrationsV1 struct {
	Symphony *SymphonyOptionsV1 `json:"symphony,omitempty"`
}

type InjectorOptionsV1 struct {
	Mode InjectorMode `json:"mode"`
	Via  string       `json:"via,omitempty"`
	Args []string     `json:"args,omitempty"`
}

type SymphonyOptionsV1 struct {
	Events       []SymphonyEvent `json:"events"`
	WorkspaceKey string          `json:"workspace_key,omitempty"`
}

type TargetV1 struct {
	ProjectRoot string `json:"project_root"`
	BaseRoot    string `json:"base_root,omitempty"`
	SessionRoot string `json:"session_root"`
	Session     string `json:"session"`
}

type PlacementTargetV1 string
type PlacementLayoutV1 string

const (
	PlacementCurrentWindow PlacementTargetV1 = "current_window"
	PlacementNewWindow     PlacementTargetV1 = "new_window"
	PlacementSession       PlacementTargetV1 = "session"

	PlacementColumns PlacementLayoutV1 = "columns"
	PlacementRows    PlacementLayoutV1 = "rows"
	PlacementTiled   PlacementLayoutV1 = "tiled"
)

type PlacementV1 struct {
	Target       PlacementTargetV1 `json:"target"`
	Layout       PlacementLayoutV1 `json:"layout"`
	StaggerMS    int               `json:"stagger_ms,omitempty"`
	LauncherPane string            `json:"launcher_pane,omitempty"`
}

type PlacementPreviewV1 struct {
	Requested  *PlacementV1 `json:"requested,omitempty"`
	Effective  PlacementV1  `json:"effective"`
	Supported  bool         `json:"supported"`
	ReasonCode string       `json:"reason_code,omitempty"`
}

type PrepareRequestV1 struct {
	RequestVersion int            `json:"request_version"`
	Target         TargetV1       `json:"target"`
	Launcher       string         `json:"launcher"`
	Placement      *PlacementV1   `json:"placement,omitempty"`
	Intent         LaunchIntentV1 `json:"intent"`
}

type RequiredActionKindV1 string

const (
	RequiredActionTrustConfirmation     RequiredActionKindV1 = "trust_confirmation"
	RequiredActionStaleConversation     RequiredActionKindV1 = "stale_conversation_decision"
	RequiredActionRebindConfirmation    RequiredActionKindV1 = "rebind_confirmation"
	RequiredActionUnsupportedCapability RequiredActionKindV1 = "unsupported_capability_ack"
)

type DecisionChoiceV1 string

const (
	DecisionTrustExactSubject DecisionChoiceV1 = "trust_exact_subject"
	DecisionDeny              DecisionChoiceV1 = "deny"
	DecisionFreshOnce         DecisionChoiceV1 = "fresh_once"
	DecisionAbort             DecisionChoiceV1 = "abort"
	DecisionCloseOld          DecisionChoiceV1 = "close_old"
	DecisionLeaveOld          DecisionChoiceV1 = "leave_old"
	DecisionAcceptDegraded    DecisionChoiceV1 = "accept_degraded"
)

type RequiredActionV1 struct {
	ActionID         string               `json:"action_id"`
	Kind             RequiredActionKindV1 `json:"kind"`
	Handles          []string             `json:"handles,omitempty"`
	Resources        []string             `json:"resources,omitempty"`
	AllowedDecisions []DecisionChoiceV1   `json:"allowed_decisions"`
	ReasonCode       string               `json:"reason_code"`
}

type CommandV1 struct {
	Argv       []string          `json:"argv"`
	Cwd        string            `json:"cwd"`
	EnvOverlay map[string]string `json:"env_overlay,omitempty"`
}

type RosterDriftV1 struct {
	Desired []string `json:"desired"`
	Present []string `json:"present"`
	Missing []string `json:"missing"`
	Extra   []string `json:"extra"`
}

type ParticipantPreviewV1 struct {
	Handle         string              `json:"handle"`
	Runnable       bool                `json:"runnable"`
	Provider       string              `json:"provider,omitempty"`
	Command        *CommandV1          `json:"command,omitempty"`
	ResumePolicy   ResumePolicy        `json:"resume_policy,omitempty"`
	Execution      *ExecutionOptionsV1 `json:"execution,omitempty"`
	PlannedOutcome string              `json:"planned_outcome"`
}

type PreviewV1 struct {
	Target       TargetV1                 `json:"target"`
	Backend      string                   `json:"backend"`
	Profile      string                   `json:"profile"`
	Participants []ParticipantPreviewV1   `json:"participants"`
	Roster       RosterDriftV1            `json:"roster"`
	Capabilities []ProviderCapabilitiesV1 `json:"capabilities"`
	Placement    *PlacementPreviewV1      `json:"placement,omitempty"`
}

type InitialInputKindV1 string

const (
	InitialInputArgument InitialInputKindV1 = "argument"
	InitialInputStdin    InitialInputKindV1 = "stdin"
	InitialInputFile     InitialInputKindV1 = "file"
)

const MaxInitialInputBytes = 256 * 1024

type InitialInputV1 struct {
	Kind InitialInputKindV1 `json:"kind"`
	Text string             `json:"text"`
}

type ConfigOverrideCapabilityV1 struct {
	Key           string   `json:"key"`
	AllowedValues []string `json:"allowed_values"`
}

type ProviderCapabilitiesV1 struct {
	Provider                string                       `json:"provider"`
	GrammarVersion          int                          `json:"grammar_version"`
	VerifiedProviderVersion string                       `json:"verified_provider_version"`
	AllowedArgumentForms    []string                     `json:"allowed_argument_forms"`
	ConfigOverrides         []ConfigOverrideCapabilityV1 `json:"config_overrides"`
	InitialInputKinds       []InitialInputKindV1         `json:"initial_input_kinds"`
}

type ParticipantObservationV1 struct {
	Handle       string `json:"handle"`
	Mailbox      string `json:"mailbox"`
	Runnable     bool   `json:"runnable"`
	Conversation string `json:"conversation"`
	Execution    string `json:"execution"`
	Resource     string `json:"resource"`
	ReasonCode   string `json:"reason_code,omitempty"`
}

type PlannedWriteKindV1 string

const (
	PlannedWriteCreateBaseRoot PlannedWriteKindV1 = "create_base_root"
	PlannedWriteInitialInput   PlannedWriteKindV1 = "write_initial_input"
)

type PlannedWriteV1 struct {
	WriteID string             `json:"write_id"`
	Kind    PlannedWriteKindV1 `json:"kind"`
	Path    string             `json:"path"`
	Handle  string             `json:"handle,omitempty"`
	SHA256  string             `json:"sha256,omitempty"`
}

type PrepareResultV1 struct {
	ResultVersion   int                        `json:"result_version"`
	SubjectSchema   int                        `json:"subject_schema"`
	Outcome         string                     `json:"outcome"`
	Reason          string                     `json:"reason,omitempty"`
	SubjectDigest   string                     `json:"subject_digest"`
	PlanDigest      string                     `json:"plan_digest"`
	TrustDigest     string                     `json:"trust_digest"`
	PlannedWrites   []PlannedWriteV1           `json:"planned_writes"`
	RequiredActions []RequiredActionV1         `json:"required_actions"`
	Preview         PreviewV1                  `json:"preview"`
	Observations    []ParticipantObservationV1 `json:"observations"`
}

type DecisionV1 struct {
	ActionID string           `json:"action_id"`
	Choice   DecisionChoiceV1 `json:"choice"`
}

type ApplyRequestV1 struct {
	RequestVersion int              `json:"request_version"`
	Prepare        PrepareRequestV1 `json:"prepare"`
	SubjectSchema  int              `json:"subject_schema,omitempty"`
	SubjectDigest  string           `json:"subject_digest"`
	Decisions      []DecisionV1     `json:"decisions"`
}

type EvidenceRefV1 struct {
	EvidenceVersion int       `json:"evidence_version"`
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	SHA256          string    `json:"sha256"`
	ObservedAt      time.Time `json:"observed_at"`
}

type ApplyResultV1 struct {
	ResultVersion int            `json:"result_version"`
	SubjectSchema int            `json:"subject_schema"`
	Outcome       string         `json:"outcome"`
	ReasonCode    string         `json:"reason_code,omitempty"`
	Hints         []ResultHintV1 `json:"hints,omitempty"`
	// FailureDetail is non-contract diagnostic prose for CLI stderr. It is
	// deliberately excluded from the versioned JSON DTO.
	FailureDetail string `json:"-"`
	// SubjectDigest is the pre-mutation subject that this Apply request was
	// authorized against, not a post-mutation Prepare snapshot.
	SubjectDigest string `json:"subject_digest"`
	PlanDigest    string `json:"plan_digest"`
	TrustDigest   string `json:"trust_digest"`
	// SemanticDigest is a deprecated compatibility alias of TrustDigest.
	SemanticDigest string                     `json:"semantic_digest"`
	Backend        string                     `json:"backend"`
	Profile        string                     `json:"profile"`
	Roster         RosterDriftV1              `json:"roster"`
	Observations   []ParticipantObservationV1 `json:"observations"`
	Commands       []CommandV1                `json:"commands,omitempty"`
	FollowUps      []RequiredActionV1         `json:"follow_ups,omitempty"`
	Evidence       []EvidenceRefV1            `json:"evidence,omitempty"`
}

type InspectRequestV1 struct {
	RequestVersion int      `json:"request_version"`
	Target         TargetV1 `json:"target"`
}

type FocusRequestV1 = InspectRequestV1
type CloseRequestV1 = InspectRequestV1

type LifecycleResultV1 struct {
	ResultVersion int                        `json:"result_version"`
	Outcome       string                     `json:"outcome"`
	ReasonCode    string                     `json:"reason_code,omitempty"`
	Backend       string                     `json:"backend"`
	Profile       string                     `json:"profile"`
	State         string                     `json:"state"`
	Observations  []ParticipantObservationV1 `json:"observations"`
	Evidence      []EvidenceRefV1            `json:"evidence,omitempty"`
}

type InspectResultV1 = LifecycleResultV1
type FocusResultV1 = LifecycleResultV1
type CloseResultV1 = LifecycleResultV1

type CompatibilityV1 struct {
	ContractSemver string   `json:"contract_semver"`
	IntentVersions []int    `json:"intent_versions"`
	ResultVersions []int    `json:"result_versions"`
	Features       []string `json:"features"`
}

type RequirementV1 struct {
	ContractSemver string   `json:"contract_semver"`
	IntentVersion  int      `json:"intent_version"`
	ResultVersion  int      `json:"result_version"`
	Features       []string `json:"features"`
}

type NegotiatedV1 struct {
	ContractSemver string   `json:"contract_semver"`
	IntentVersion  int      `json:"intent_version"`
	ResultVersion  int      `json:"result_version"`
	Features       []string `json:"features"`
}

const (
	FeatureBaseRoot           = "base_root"
	FeatureOnLive             = "on_live"
	FeatureWrapper            = "wrapper"
	FeaturePlacement          = "placement"
	FeatureInitialInput       = "initial_input"
	FeatureCallerContext      = "caller_context"
	FeatureExecutableIdentity = "executable_identity"
)
