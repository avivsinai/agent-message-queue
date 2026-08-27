package launch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// DefaultLaunchStateDir returns the existing platform-specific trust-state
// root without creating it.
func DefaultLaunchStateDir() (string, error) {
	if value := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); value != "" {
		return filepath.Join(value, "amq"), nil
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "amq", "state"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "amq"), nil
}

const preparePlaceholderUUID = "00000000-0000-4000-8000-000000000000"
const MaxInitialInputBytes = 256 * 1024

const (
	PrepareOutcomeReady          = "ready"
	PrepareOutcomeActionRequired = "action_required"
	PrepareOutcomeUnsupported    = "unsupported"
	SubjectSchemaV1              = 1
	SubjectSchemaV2              = 2
)

type RequiredActionKind string

const (
	ActionTrustConfirmation     RequiredActionKind = "trust_confirmation"
	ActionStaleConversation     RequiredActionKind = "stale_conversation_decision"
	ActionRebindConfirmation    RequiredActionKind = "rebind_confirmation"
	ActionUnsupportedCapability RequiredActionKind = "unsupported_capability_ack"
)

type PrepareTarget struct {
	ProjectRoot string `json:"project_root"`
	BaseRoot    string `json:"base_root,omitempty"`
	SessionRoot string `json:"session_root"`
	Session     string `json:"session"`
}

type PlannedWriteKind string

const (
	PlannedWriteCreateBaseRoot PlannedWriteKind = "create_base_root"
	PlannedWriteInitialInput   PlannedWriteKind = "write_initial_input"
)

type PlannedWrite struct {
	WriteID string           `json:"write_id"`
	Kind    PlannedWriteKind `json:"kind"`
	Path    string           `json:"path"`
	Handle  string           `json:"handle,omitempty"`
	SHA256  string           `json:"sha256,omitempty"`
}

type PrepareExecutionOptions struct {
	RequireWake          bool     `json:"require_wake"`
	NoGitignore          bool     `json:"no_gitignore"`
	Named                bool     `json:"named,omitempty"`
	WakeMode             string   `json:"wake_mode"`
	AuditReason          string   `json:"audit_reason,omitempty"`
	InjectorMode         string   `json:"injector_mode,omitempty"`
	InjectorVia          string   `json:"injector_via,omitempty"`
	InjectorArgs         []string `json:"injector_args,omitempty"`
	SymphonyEvents       []string `json:"symphony_events,omitempty"`
	SymphonyWorkspaceKey string   `json:"symphony_workspace_key,omitempty"`
}

type PrepareParticipant struct {
	Handle       string
	Runnable     bool
	Provider     string
	Executable   string
	Args         []string
	BypassArgs   []string
	Cwd          string
	EnvOverlay   map[string]string
	ResumePolicy ResumePolicy
	Execution    PrepareExecutionOptions
	InitialInput *PrepareInitialInput
	OnLive       string
	Wrapper      *Wrapper
}

type PrepareInitialInput struct {
	Kind InitialInputKind
	Text string
}

type PrepareRequest struct {
	Target        PrepareTarget
	Launcher      string
	IntentDigest  string
	Participants  []PrepareParticipant
	SubjectSchema int
	Placement     *Placement
	CallerContext map[string]string
}

type AdapterFactory func(provider, executable string) HarnessAdapter

type PrepareDependencies struct {
	Backends     map[string]Backend
	Preferences  []string
	AdapterFor   AdapterFactory
	AMQPath      string
	TrustStore   *TrustStore
	HostIdentity string
}

type PrepareRequiredAction struct {
	ActionID         string             `json:"action_id"`
	Kind             RequiredActionKind `json:"kind"`
	Handles          []string           `json:"handles,omitempty"`
	Resources        []string           `json:"resources,omitempty"`
	AllowedDecisions []string           `json:"allowed_decisions"`
	ReasonCode       string             `json:"reason_code"`
}

type PrepareCommand struct {
	Argv       []string          `json:"argv"`
	Cwd        string            `json:"cwd"`
	EnvOverlay map[string]string `json:"env_overlay,omitempty"`
}

type PreparedParticipant struct {
	Handle         string                  `json:"handle"`
	Runnable       bool                    `json:"runnable"`
	Provider       string                  `json:"provider,omitempty"`
	Command        *PrepareCommand         `json:"command,omitempty"`
	ResumePolicy   ResumePolicy            `json:"resume_policy,omitempty"`
	Execution      PrepareExecutionOptions `json:"execution,omitempty"`
	PlannedOutcome string                  `json:"planned_outcome"`
	CwdIdentity    string                  `json:"cwd_identity,omitempty"`
	OnLive         string                  `json:"on_live,omitempty"`
	Executable     *ConsultedExecutable    `json:"executable_identity,omitempty"`
	Wrapper        *ConsultedExecutable    `json:"wrapper_executable_identity,omitempty"`
}

type PrepareRoster struct {
	Desired []string `json:"desired"`
	Present []string `json:"present"`
	Missing []string `json:"missing"`
	Extra   []string `json:"extra"`
}

type PrepareObservation struct {
	Handle                     string `json:"handle"`
	Mailbox                    string `json:"mailbox"`
	Runnable                   bool   `json:"runnable"`
	Conversation               string `json:"conversation"`
	ConversationIdentityDigest string `json:"conversation_identity_digest"`
	Execution                  string `json:"execution"`
	ExecutionIdentityDigest    string `json:"execution_identity_digest"`
	Resource                   string `json:"resource"`
	ReasonCode                 string `json:"reason_code,omitempty"`
	Disposition                string `json:"disposition,omitempty"`
	StartMode                  string `json:"start_mode,omitempty"`
}

type PrepareResult struct {
	Outcome             string
	Reason              string
	SubjectSchema       int
	SubjectDigest       string
	PlanDigest          string
	TrustDigest         string
	BaseAuthorityDigest string
	PlannedWrites       []PlannedWrite
	Target              PrepareTarget
	Backend             string
	Profile             string
	Participants        []PreparedParticipant
	Roster              PrepareRoster
	RequiredActions     []PrepareRequiredAction
	Observations        []PrepareObservation
	Placement           PlacementPreview
	CallerContext       map[string]string
}

type prepareTargetState struct {
	target          PrepareTarget
	projectIdentity string
	sessionIdentity string
	projectRoot     *fsq.DeliveryRoot
	baseRoot        *fsq.DeliveryRoot
	sessionRoot     *fsq.DeliveryRoot
	mailboxConfig   *fsq.MailboxConfigAuthorization
	baseAuthority   *explicitBaseAuthority
}

var ErrPrepareAuthorityDrift = errors.New("prepare authority drift")

type prepareAuthorityDriftError struct{ err error }

func (err *prepareAuthorityDriftError) Error() string { return err.err.Error() }
func (err *prepareAuthorityDriftError) Unwrap() error { return err.err }
func (err *prepareAuthorityDriftError) Is(target error) bool {
	return target == ErrPrepareAuthorityDrift || errors.Is(err.err, target)
}

func prepareAuthorityDrift(err error) error {
	if err == nil {
		return nil
	}
	return &prepareAuthorityDriftError{err: err}
}

func isPrepareAuthorityDrift(err error) bool {
	return errors.Is(err, ErrPrepareAuthorityDrift) || errors.Is(err, fsq.ErrDeliveryRootChanged)
}

func (state *prepareTargetState) close() {
	if state == nil {
		return
	}
	if state.mailboxConfig != nil {
		_ = state.mailboxConfig.Close()
	}
	if state.sessionRoot != nil {
		_ = state.sessionRoot.Close()
	}
	if state.baseRoot != nil {
		_ = state.baseRoot.Close()
	}
	if state.baseAuthority != nil {
		state.baseAuthority.close()
	}
	if state.projectRoot != nil {
		_ = state.projectRoot.Close()
	}
}

func (state *prepareTargetState) verify() error {
	if state.mailboxConfig != nil {
		if err := state.mailboxConfig.Verify(); err != nil {
			return prepareAuthorityDrift(fmt.Errorf("mailbox config changed during Prepare: %w", err))
		}
	}
	if err := state.projectRoot.VerifyBase(); err != nil {
		return prepareAuthorityDrift(fmt.Errorf("project root changed during Prepare: %w", err))
	}
	if state.baseAuthority != nil {
		if err := state.baseAuthority.verify(state.projectRoot, state.baseRoot); err != nil {
			return prepareAuthorityDrift(fmt.Errorf("base-root authority changed during Prepare: %w", err))
		}
	} else {
		if err := state.baseRoot.VerifyBase(); err != nil {
			return prepareAuthorityDrift(fmt.Errorf("session base root changed during Prepare: %w", err))
		}
	}
	if state.sessionRoot != nil {
		if err := state.sessionRoot.VerifyBase(); err != nil {
			return prepareAuthorityDrift(fmt.Errorf("session root changed during Prepare: %w", err))
		}
		return nil
	}
	if state.baseRoot == nil {
		return nil
	}
	child, err := state.baseRoot.OpenDirectChild(state.target.Session)
	if err == nil {
		_ = child.Close()
		return prepareAuthorityDrift(fmt.Errorf("session root appeared during Prepare"))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return prepareAuthorityDrift(err)
	}
	return nil
}

type prepareBindingObservation struct {
	Present    bool           `json:"present"`
	Binding    *BindingRecord `json:"binding,omitempty"`
	Inspection string         `json:"inspection"`
	Evidence   string         `json:"evidence,omitempty"`
}

type prepareSubject struct {
	Version         int                       `json:"version"`
	IntentDigest    string                    `json:"intent_digest"`
	PlanDigest      string                    `json:"plan_digest"`
	TrustDigest     string                    `json:"trust_digest"`
	Target          PrepareTarget             `json:"target"`
	ProjectIdentity string                    `json:"project_identity"`
	SessionIdentity string                    `json:"session_identity"`
	BaseAuthority   string                    `json:"base_authority,omitempty"`
	PlannedWrites   *[]PlannedWrite           `json:"planned_writes,omitempty"`
	Backend         string                    `json:"backend"`
	Profile         string                    `json:"profile"`
	Detection       DetectResult              `json:"detection"`
	Binding         prepareBindingObservation `json:"binding"`
	Roster          PrepareRoster             `json:"roster"`
	Actions         []PrepareRequiredAction   `json:"actions"`
	Participants    []PreparedParticipant     `json:"participants"`
	Observations    []PrepareObservation      `json:"observations"`
	Placement       *PlacementPreview         `json:"placement,omitempty"`
	CallerContext   map[string]string         `json:"caller_context,omitempty"`
}

// Prepare compiles public caller intent and inspects current launch state. It
// deliberately has no write-capable dependency: no lease, Create, Close,
// Focus, trust replacement, journal writer, or mailbox repair callback is
// reachable from this function.
func Prepare(ctx context.Context, request PrepareRequest, dependencies PrepareDependencies) (PrepareResult, error) {
	ctx, subjectSchema, dependencies, err := validatePrepareRequest(ctx, request, dependencies)
	if err != nil {
		return PrepareResult{}, err
	}
	state, err := openPrepareTarget(request.Target)
	if err != nil {
		if reason := baseRootRefusalReason(err); reason != "" {
			return prepareBaseRootRefusal(request, subjectSchema, reason)
		}
		return PrepareResult{}, err
	}
	defer state.close()
	return prepareAtTarget(ctx, request, dependencies, state, subjectSchema)
}

func validatePrepareRequest(ctx context.Context, request PrepareRequest, dependencies PrepareDependencies) (context.Context, int, PrepareDependencies, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, PrepareDependencies{}, err
	}
	if !validDigest(request.IntentDigest) {
		return nil, 0, PrepareDependencies{}, fmt.Errorf("invalid intent digest")
	}
	if len(request.Participants) == 0 {
		return nil, 0, PrepareDependencies{}, fmt.Errorf("prepare requires at least one participant")
	}
	subjectSchema, err := normalizeSubjectSchema(request.SubjectSchema)
	if err != nil {
		return nil, 0, PrepareDependencies{}, err
	}
	if request.Placement != nil {
		if subjectSchema == SubjectSchemaV1 {
			return nil, 0, PrepareDependencies{}, fmt.Errorf("placement requires subject schema %d", SubjectSchemaV2)
		}
		if err := request.Placement.Validate(); err != nil {
			return nil, 0, PrepareDependencies{}, err
		}
	}
	for _, participant := range request.Participants {
		switch participant.OnLive {
		case "", OnLiveRefuse:
		case OnLiveKeep:
			if subjectSchema == SubjectSchemaV1 {
				return nil, 0, PrepareDependencies{}, fmt.Errorf("on_live keep requires subject schema %d", SubjectSchemaV2)
			}
		default:
			return nil, 0, PrepareDependencies{}, fmt.Errorf("invalid on_live %q", participant.OnLive)
		}
	}
	if len(request.CallerContext) > 0 && subjectSchema == SubjectSchemaV1 {
		return nil, 0, PrepareDependencies{}, fmt.Errorf("caller_context requires subject schema %d", SubjectSchemaV2)
	}
	if err := ValidateCallerContext(request.CallerContext); err != nil {
		return nil, 0, PrepareDependencies{}, err
	}
	amqPath, err := resolveLaunchAMQExecutable(dependencies.AMQPath, request.Target.ProjectRoot)
	if err != nil {
		return nil, 0, PrepareDependencies{}, err
	}
	dependencies.AMQPath = amqPath
	return ctx, subjectSchema, dependencies, nil
}

func prepareAtTarget(ctx context.Context, request PrepareRequest, dependencies PrepareDependencies, state *prepareTargetState, subjectSchema int) (PrepareResult, error) {
	binding, bindingObservation, err := inspectPrepareBinding(state.sessionRoot)
	if err != nil {
		return PrepareResult{}, err
	}
	backendName, _, detect, err := selectPrepareBackend(request.Launcher, binding, dependencies, state.projectRoot)
	if err != nil {
		return PrepareResult{}, err
	}
	var boundDetect DetectResult
	if binding != nil {
		boundBackend := dependencies.Backends[binding.Backend]
		if boundBackend == nil {
			bindingObservation.Inspection = "backend_unavailable"
		} else {
			if binding.Backend == backendName {
				boundDetect = detect
			} else {
				boundDetect = boundBackend.Detect()
			}
			if err := boundDetect.Validate(); err != nil {
				return PrepareResult{}, err
			}
			switch {
			case prepareBindingForeign(*binding, boundDetect, dependencies.HostIdentity):
				bindingObservation.Inspection = "foreign"
			case !boundDetect.Available:
				bindingObservation.Inspection = "backend_unavailable"
			default:
				bindingObservation, err = inspectBoundBackend(boundBackend, *binding, state.sessionRoot, bindingObservation)
			}
		}
		if err != nil {
			return PrepareResult{}, err
		}
	}

	if state.sessionRoot != nil {
		authorization, inventory, authorizationErr := fsq.OpenMailboxConfigAuthorization(state.sessionRoot)
		switch {
		case authorizationErr == nil:
			state.mailboxConfig = authorization
		case inventory.ActiveConfigStatus != string(fsq.MailboxPathMissing):
			return PrepareResult{}, fmt.Errorf("pin mailbox config: %w", authorizationErr)
		}
	}
	roster, mailboxStates, err := inspectPrepareRoster(state.sessionRoot, state.mailboxConfig, request.Participants)
	if err != nil {
		return PrepareResult{}, err
	}
	result := PrepareResult{
		SubjectSchema: subjectSchema, Target: state.target, Backend: backendName, Roster: roster,
		Participants:  make([]PreparedParticipant, 0, len(request.Participants)),
		Observations:  make([]PrepareObservation, 0, len(request.Participants)+len(roster.Extra)),
		CallerContext: cloneCallerContext(request.CallerContext),
	}
	baseAuthorityDigest := ""
	if state.baseAuthority != nil {
		baseAuthorityDigest = state.baseAuthority.authorityDigest
		result.BaseAuthorityDigest = baseAuthorityDigest
		if state.baseAuthority.baseMissing {
			write := PlannedWrite{Kind: PlannedWriteCreateBaseRoot, Path: state.target.BaseRoot}
			write.WriteID, err = digestCanonical(struct {
				Kind      PlannedWriteKind `json:"kind"`
				Target    PrepareTarget    `json:"target"`
				Authority string           `json:"authority"`
			}{write.Kind, state.target, baseAuthorityDigest})
			if err != nil {
				return PrepareResult{}, err
			}
			result.PlannedWrites = []PlannedWrite{write}
		}
	}
	if detect.Profile.Backend != "" {
		result.Profile = detect.Profile.Identity()
	}
	placement, err := ResolvePlacement(backendName, request.Placement)
	if err != nil {
		return PrepareResult{}, err
	}
	result.Placement = placement
	placementUnsupported := request.Placement != nil && !placement.Supported

	plan := Plan{Version: PlanVersion, Agents: []AgentPlan{}}
	for _, participant := range request.Participants {
		prepared, observation, agentPlan, actions, participantErr := prepareOneParticipant(participant, state, dependencies, mailboxStates[participant.Handle], subjectSchema)
		if participantErr != nil {
			return PrepareResult{}, participantErr
		}
		if placementUnsupported && prepared.Runnable {
			prepared.PlannedOutcome = "unsupported"
		}
		result.Participants = append(result.Participants, prepared)
		result.Observations = append(result.Observations, observation)
		if !placementUnsupported {
			result.RequiredActions = append(result.RequiredActions, actions...)
			if agentPlan != nil {
				plan.Agents = append(plan.Agents, *agentPlan)
			}
		}
	}
	applyBindingResources(result.Observations, binding)
	if err := appendExtraObservations(&result, roster.Extra, mailboxStates, binding, state.sessionRoot); err != nil {
		return PrepareResult{}, err
	}

	if !placementUnsupported {
		result.RequiredActions = append(result.RequiredActions, prepareRebindActions(binding, bindingObservation, boundDetect, backendName, result.Profile, dependencies.HostIdentity)...)
		for i := range result.RequiredActions {
			result.RequiredActions[i].ActionID, err = prepareActionID(result.RequiredActions[i])
			if err != nil {
				return PrepareResult{}, err
			}
		}
		slices.SortFunc(result.RequiredActions, comparePrepareActions)
	}

	result.PlanDigest, err = PreparePlanDigest(plan)
	if err != nil {
		return PrepareResult{}, err
	}
	trustPlanDigest, err := PrepareTrustPlanDigest(plan)
	if err != nil {
		return PrepareResult{}, err
	}
	result.TrustDigest, err = PrepareTrustDigestWithAuthority(trustPlanDigest, state.target.Session, state.target.SessionRoot, state.sessionIdentity, baseAuthorityDigest, onLiveKeepHandles(request.Participants))
	if err != nil {
		return PrepareResult{}, err
	}
	if !placementUnsupported && detect.Available && len(plan.Agents) > 0 {
		trusted, trustErr := loadPrepareTrust(dependencies.TrustStore, result.TrustDigest)
		if trustErr != nil {
			return PrepareResult{}, trustErr
		}
		if !trusted {
			handles := make([]string, 0, len(plan.Agents))
			for _, agent := range plan.Agents {
				handles = append(handles, agent.Handle)
			}
			action := PrepareRequiredAction{
				Kind: ActionTrustConfirmation, Handles: handles, Resources: []string{result.TrustDigest},
				AllowedDecisions: []string{"trust_exact_subject", "deny"}, ReasonCode: "untrusted_config_digest",
			}
			action.ActionID, err = prepareActionID(action)
			if err != nil {
				return PrepareResult{}, err
			}
			result.RequiredActions = append(result.RequiredActions, action)
			slices.SortFunc(result.RequiredActions, comparePrepareActions)
		}
	}

	switch {
	case placementUnsupported:
		result.Outcome, result.Reason = PrepareOutcomeUnsupported, PlacementUnsupportedReason
	case firstInitialInputUnsupported(result.Observations) != "":
		result.Outcome, result.Reason = PrepareOutcomeUnsupported, firstInitialInputUnsupported(result.Observations)
	case len(result.RequiredActions) > 0:
		result.Outcome, result.Reason = PrepareOutcomeActionRequired, result.RequiredActions[0].ReasonCode
	case !detect.Available:
		result.Outcome, result.Reason = PrepareOutcomeUnsupported, "launcher_not_available"
	case bindingObservation.Present && (bindingObservation.Inspection == string(InspectUnknown) || bindingObservation.Inspection == "backend_unavailable"):
		result.Outcome, result.Reason = PrepareOutcomeActionRequired, "binding_inspection_unavailable"
	default:
		result.Outcome = PrepareOutcomeReady
	}
	if err := state.verify(); err != nil {
		return PrepareResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return PrepareResult{}, err
	}
	subject := prepareSubject{
		Version: subjectSchema, IntentDigest: request.IntentDigest, PlanDigest: result.PlanDigest, TrustDigest: result.TrustDigest,
		Target: state.target, ProjectIdentity: state.projectIdentity, SessionIdentity: state.sessionIdentity,
		BaseAuthority: baseAuthorityDigest,
		Backend:       result.Backend, Profile: result.Profile, Detection: detect, Binding: bindingObservation, Roster: result.Roster,
		Actions: result.RequiredActions, Participants: result.Participants, Observations: result.Observations,
	}
	if subjectSchema == SubjectSchemaV2 {
		setPrepareSubjectV2Fields(&subject, result.PlannedWrites, result.Placement, request.CallerContext)
	}
	result.SubjectDigest, err = digestCanonical(subject)
	if err != nil {
		return PrepareResult{}, err
	}
	if state.mailboxConfig != nil {
		_ = state.mailboxConfig.Close()
		state.mailboxConfig = nil
	}
	return result, nil
}

func setPrepareSubjectV2Fields(subject *prepareSubject, plannedWrites []PlannedWrite, placement PlacementPreview, callerContext map[string]string) {
	writes := slices.Clone(plannedWrites)
	if writes == nil {
		writes = []PlannedWrite{}
	}
	subject.PlannedWrites = &writes
	subject.Placement = &placement
	subject.CallerContext = cloneCallerContext(callerContext)
}

func prepareBaseRootRefusal(request PrepareRequest, subjectSchema int, reason string) (PrepareResult, error) {
	result := PrepareResult{
		Outcome: PrepareOutcomeUnsupported, Reason: reason, SubjectSchema: subjectSchema,
		Target: request.Target, Backend: request.Launcher,
		Participants:  make([]PreparedParticipant, 0, len(request.Participants)),
		Observations:  make([]PrepareObservation, 0, len(request.Participants)),
		PlannedWrites: []PlannedWrite{}, RequiredActions: []PrepareRequiredAction{},
	}
	for _, participant := range request.Participants {
		result.Roster.Desired = append(result.Roster.Desired, participant.Handle)
		result.Roster.Missing = append(result.Roster.Missing, participant.Handle)
		result.Participants = append(result.Participants, PreparedParticipant{
			Handle: participant.Handle, Runnable: participant.Runnable, Provider: participant.Provider,
			ResumePolicy: participant.ResumePolicy, PlannedOutcome: "unsupported",
		})
		result.Observations = append(result.Observations, PrepareObservation{
			Handle: participant.Handle, Mailbox: "unknown", Runnable: participant.Runnable,
			Conversation: "none", Execution: "none", Resource: "none", ReasonCode: reason,
		})
	}
	slices.Sort(result.Roster.Desired)
	slices.Sort(result.Roster.Missing)
	var err error
	result.PlanDigest, err = digestCanonical(struct {
		Version int           `json:"version"`
		Reason  string        `json:"reason"`
		Target  PrepareTarget `json:"target"`
		Intent  string        `json:"intent_digest"`
	}{1, reason, request.Target, request.IntentDigest})
	if err != nil {
		return PrepareResult{}, err
	}
	result.TrustDigest, err = digestCanonical(struct {
		Version int           `json:"version"`
		Reason  string        `json:"reason"`
		Target  PrepareTarget `json:"target"`
	}{1, reason, request.Target})
	if err != nil {
		return PrepareResult{}, err
	}
	result.SubjectDigest, err = digestCanonical(struct {
		Version int    `json:"version"`
		Plan    string `json:"plan_digest"`
		Trust   string `json:"trust_digest"`
	}{subjectSchema, result.PlanDigest, result.TrustDigest})
	return result, err
}

func normalizeSubjectSchema(schema int) (int, error) {
	if schema == 0 {
		return SubjectSchemaV2, nil
	}
	if schema != SubjectSchemaV1 && schema != SubjectSchemaV2 {
		return 0, fmt.Errorf("unsupported subject schema %d", schema)
	}
	return schema, nil
}

func openPrepareTarget(target PrepareTarget) (*prepareTargetState, error) {
	if strings.TrimSpace(target.ProjectRoot) == "" || strings.TrimSpace(target.SessionRoot) == "" || strings.TrimSpace(target.Session) == "" {
		return nil, fmt.Errorf("prepare target is incomplete")
	}
	project, err := resolvedPath(target.ProjectRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve project root: %w", err)
	}
	projectSnapshot, err := fsq.SnapshotDeliveryRoot(project)
	if err != nil {
		return nil, err
	}
	projectRoot, err := fsq.OpenDeliveryRoot(project, projectSnapshot)
	if err != nil {
		return nil, err
	}
	projectIdentity, err := fsq.StableTreeIdentityInfo(projectRoot.FileInfo())
	if err != nil {
		_ = projectRoot.Close()
		return nil, err
	}

	if target.BaseRoot != "" {
		if project != target.ProjectRoot {
			_ = projectRoot.Close()
			return nil, fmt.Errorf("%s: project_root must be canonical", baseRootRelationInvalid)
		}
		authority, baseRoot, authorityErr := openExplicitBaseAuthority(projectRoot, target)
		if authorityErr != nil {
			_ = projectRoot.Close()
			return nil, authorityErr
		}
		state := &prepareTargetState{
			target:          PrepareTarget{ProjectRoot: project, BaseRoot: target.BaseRoot, SessionRoot: target.SessionRoot, Session: target.Session},
			projectIdentity: projectIdentity, projectRoot: projectRoot, baseRoot: baseRoot, baseAuthority: authority,
		}
		if baseRoot == nil {
			state.sessionIdentity = "intended-base-child:" + authority.parentIdentity + ":" + authority.baseName + ":" + target.Session
			return state, nil
		}
		sessionRoot, openErr := baseRoot.OpenDirectChild(target.Session)
		if openErr == nil {
			exact, exactErr := hasExactDirectChildName(baseRoot, target.Session)
			if exactErr != nil || !exact {
				_ = sessionRoot.Close()
				state.close()
				if exactErr != nil {
					return nil, exactErr
				}
				return nil, fmt.Errorf("%s: session uses an alternate direct-child spelling", baseRootRelationInvalid)
			}
			state.sessionRoot = sessionRoot
			state.sessionIdentity, openErr = fsq.StableTreeIdentityInfo(sessionRoot.FileInfo())
			if openErr != nil {
				state.close()
				return nil, openErr
			}
			return state, nil
		}
		if !errors.Is(openErr, os.ErrNotExist) {
			state.close()
			return nil, openErr
		}
		baseIdentity, identityErr := fsq.StableTreeIdentityInfo(baseRoot.FileInfo())
		if identityErr != nil {
			state.close()
			return nil, identityErr
		}
		state.sessionIdentity = "intended-child:" + baseIdentity + ":" + target.Session
		return state, nil
	}

	sessionPath := filepath.Clean(target.SessionRoot)
	if filepath.Base(sessionPath) != target.Session || filepath.Dir(sessionPath) == sessionPath {
		_ = projectRoot.Close()
		return nil, fmt.Errorf("session root must be the direct child named %q", target.Session)
	}
	base, err := resolvedPath(filepath.Dir(sessionPath))
	if err != nil {
		_ = projectRoot.Close()
		return nil, fmt.Errorf("resolve session base root: %w", err)
	}
	canonicalSession := filepath.Join(base, target.Session)
	baseSnapshot, err := fsq.SnapshotDeliveryRoot(base)
	if err != nil {
		_ = projectRoot.Close()
		return nil, err
	}
	baseRoot, err := fsq.OpenDeliveryRoot(base, baseSnapshot)
	if err != nil {
		_ = projectRoot.Close()
		return nil, err
	}
	state := &prepareTargetState{
		target:          PrepareTarget{ProjectRoot: project, SessionRoot: canonicalSession, Session: target.Session},
		projectIdentity: projectIdentity, projectRoot: projectRoot, baseRoot: baseRoot,
	}
	sessionRoot, err := baseRoot.OpenDirectChild(target.Session)
	if err == nil {
		state.sessionRoot = sessionRoot
		state.sessionIdentity, err = fsq.StableTreeIdentityInfo(sessionRoot.FileInfo())
		if err != nil {
			state.close()
			return nil, err
		}
		return state, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		state.close()
		return nil, err
	}
	baseIdentity, identityErr := fsq.StableTreeIdentityInfo(baseRoot.FileInfo())
	if identityErr != nil {
		state.close()
		return nil, identityErr
	}
	state.sessionIdentity = "intended-child:" + baseIdentity + ":" + target.Session
	return state, nil
}

func inspectPrepareBinding(root *fsq.DeliveryRoot) (*BindingRecord, prepareBindingObservation, error) {
	observation := prepareBindingObservation{Inspection: "absent"}
	if root == nil {
		return nil, observation, nil
	}
	binding, err := LoadBinding(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, observation, nil
	}
	if err != nil {
		return nil, observation, err
	}
	observation.Present, observation.Binding = true, &binding
	observation.Inspection = "not_inspected"
	return &binding, observation, nil
}

func inspectBoundBackend(backend Backend, binding BindingRecord, root *fsq.DeliveryRoot, observation prepareBindingObservation) (prepareBindingObservation, error) {
	inspection, err := backend.Inspect(InspectRequest{Binding: binding, Root: root})
	if err != nil {
		return observation, err
	}
	observation.Inspection, observation.Evidence = string(inspection.Status), inspection.Evidence
	return observation, nil
}

func selectPrepareBackend(requested string, binding *BindingRecord, dependencies PrepareDependencies, projectRoot *fsq.DeliveryRoot) (string, Backend, DetectResult, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		requested = LauncherAuto
	}
	selected := requested
	if selected == LauncherAuto && binding != nil {
		selected = binding.Backend
	}
	preferences := slices.Clone(dependencies.Preferences)
	if selected == LauncherAuto && len(preferences) == 0 {
		var err error
		preferences, err = loadPreparePreferences(projectRoot)
		if err != nil {
			return "", nil, DetectResult{}, err
		}
	}
	if selected == LauncherAuto {
		for _, name := range prependInsideSurfacePreference(preferences) {
			backend := dependencies.Backends[name]
			if backend == nil {
				continue
			}
			detect := backend.Detect()
			if err := detect.Validate(); err != nil {
				return name, backend, detect, err
			}
			if detect.Available {
				return name, backend, detect, nil
			}
		}
		return LauncherAuto, nil, DetectResult{}, nil
	}
	backend := dependencies.Backends[selected]
	if backend == nil {
		return selected, nil, DetectResult{}, nil
	}
	detect := backend.Detect()
	if err := detect.Validate(); err != nil {
		return selected, backend, detect, err
	}
	return selected, backend, detect, nil
}

func loadPreparePreferences(projectRoot *fsq.DeliveryRoot) ([]string, error) {
	const localConfigPath = ".amq/launch.local.json"
	data, err := projectRoot.ReadRegularNoFollow(localConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return []string{LauncherCommands}, nil
	}
	if err != nil {
		return nil, err
	}
	config, err := ParseLocalConfig(localConfigPath, data)
	if err != nil {
		return nil, err
	}
	return slices.Clone(config.LauncherPreference), nil
}

func inspectPrepareRoster(root *fsq.DeliveryRoot, authorization *fsq.MailboxConfigAuthorization, participants []PrepareParticipant) (PrepareRoster, map[string]string, error) {
	roster := PrepareRoster{Desired: []string{}, Present: []string{}, Missing: []string{}, Extra: []string{}}
	states := make(map[string]string, len(participants))
	for _, participant := range participants {
		roster.Desired = append(roster.Desired, participant.Handle)
		states[participant.Handle] = "missing"
	}
	slices.Sort(roster.Desired)
	if root == nil {
		roster.Missing = slices.Clone(roster.Desired)
		return roster, states, nil
	}
	var inventory fsq.MailboxInventory
	var err error
	if authorization != nil {
		inventory, err = fsq.InspectMailboxLayoutWithAuthorization(root, authorization)
	} else {
		inventory, err = fsq.InspectMailboxLayout(root)
	}
	if err != nil {
		return PrepareRoster{}, nil, err
	}
	desired := make(map[string]struct{}, len(roster.Desired))
	for _, handle := range roster.Desired {
		desired[handle] = struct{}{}
	}
	for _, mailbox := range inventory.Mailboxes {
		state := "invalid"
		if mailbox.Status == "ok" {
			state = "present"
		}
		states[mailbox.Handle] = state
		if _, ok := desired[mailbox.Handle]; ok {
			if state == "present" {
				roster.Present = append(roster.Present, mailbox.Handle)
			} else {
				roster.Missing = append(roster.Missing, mailbox.Handle)
			}
		} else {
			roster.Extra = append(roster.Extra, mailbox.Handle)
		}
	}
	for _, handle := range roster.Desired {
		if states[handle] == "missing" {
			roster.Missing = append(roster.Missing, handle)
		}
	}
	slices.Sort(roster.Present)
	slices.Sort(roster.Missing)
	slices.Sort(roster.Extra)
	return roster, states, nil
}

func prepareOneParticipant(participant PrepareParticipant, state *prepareTargetState, dependencies PrepareDependencies, mailbox string, subjectSchema int) (PreparedParticipant, PrepareObservation, *AgentPlan, []PrepareRequiredAction, error) {
	preparedExecution := participant.Execution
	if subjectSchema == SubjectSchemaV1 {
		preparedExecution.Named = false
	}
	prepared := PreparedParticipant{
		Handle: participant.Handle, Runnable: participant.Runnable, Provider: participant.Provider,
		ResumePolicy: participant.ResumePolicy, Execution: preparedExecution, PlannedOutcome: "participant_only",
	}
	if participant.OnLive == OnLiveKeep {
		prepared.OnLive = OnLiveKeep
	}
	observation := PrepareObservation{
		Handle: participant.Handle, Mailbox: mailbox, Runnable: participant.Runnable,
		Conversation: "none", ConversationIdentityDigest: "absent",
		Execution: "none", ExecutionIdentityDigest: "absent", Resource: "none",
	}
	if state.sessionRoot != nil {
		if ticket, err := LoadExecutionTicket(state.sessionRoot, participant.Handle); err == nil {
			observation.Execution = string(ticket.State)
			observation.ExecutionIdentityDigest, err = digestCanonical(ticket)
			if err != nil {
				return prepared, observation, nil, nil, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return prepared, observation, nil, nil, err
		}
	}
	conversation, hasConversation, err := loadPrepareConversation(state.sessionRoot, participant.Handle)
	if err != nil {
		return prepared, observation, nil, nil, err
	}
	if hasConversation {
		observation.Conversation = string(conversation.State)
		observation.ConversationIdentityDigest, err = digestCanonical(conversation)
		if err != nil {
			return prepared, observation, nil, nil, err
		}
	}
	if !participant.Runnable {
		return prepared, observation, nil, nil, nil
	}
	if subjectSchema == SubjectSchemaV2 && strings.TrimSpace(participant.Executable) != "" {
		consulted, execErr := ResolveConsultedExecutable(participant.Executable)
		if execErr != nil {
			return prepared, observation, nil, nil, fmt.Errorf("identify executable for %s: %w", participant.Handle, execErr)
		}
		prepared.Executable = &consulted
	}
	if reason := prepareInitialInputUnsupported(participant); reason != "" {
		prepared.PlannedOutcome = "unsupported"
		observation.ReasonCode = reason
		return prepared, observation, nil, nil, nil
	}
	if err := validateWrapperFileForProject(participant.Wrapper, state.target.ProjectRoot); err != nil {
		return prepared, observation, nil, nil, fmt.Errorf("wrapper for %s: %w", participant.Handle, err)
	}
	if subjectSchema == SubjectSchemaV2 && participant.Wrapper != nil {
		identity, err := ProbeExecutableIdentity(participant.Wrapper.Executable)
		if err != nil {
			return prepared, observation, nil, nil, fmt.Errorf("identify wrapper for %s: %w", participant.Handle, err)
		}
		raw, err := MarshalExecutableIdentity(identity)
		if err != nil {
			return prepared, observation, nil, nil, err
		}
		prepared.Wrapper = &ConsultedExecutable{
			Requested: participant.Wrapper.Executable,
			Consulted: participant.Wrapper.Executable,
			Identity:  raw,
		}
	}
	if dependencies.AdapterFor == nil {
		return prepared, observation, nil, nil, fmt.Errorf("prepare adapter factory is missing")
	}

	cwd, err := resolvePrepareCwd(state.target.ProjectRoot, participant.Cwd)
	if err != nil {
		return prepared, observation, nil, nil, err
	}
	prepared.CwdIdentity, err = fsq.StableTreeIdentity(cwd)
	if err != nil {
		return prepared, observation, nil, nil, fmt.Errorf("identify cwd for %s: %w", participant.Handle, err)
	}
	adapter := dependencies.AdapterFor(participant.Provider, participant.Executable)
	if adapter == nil {
		return prepared, observation, nil, nil, fmt.Errorf("provider %q has no adapter", participant.Provider)
	}

	execution := participant.Execution
	// V1 subjects must reproduce the pre-naming plan for existing callers
	// (amq-squad); naming is a V2 capability.
	execution.Named = execution.Named && subjectSchema == SubjectSchemaV2 && supportsManagedPlanNaming(participant.Provider)
	base := PlanRequest{
		Handle: participant.Handle, Session: state.target.Session, ProjectRoot: state.target.ProjectRoot, SessionRoot: state.target.SessionRoot,
		AMQExecutable: dependencies.AMQPath, Cwd: cwd, AllowExternalCwd: true,
		LaunchNonce: preparePlaceholderUUID, Named: execution.Named, ResumePolicy: participant.ResumePolicy,
		CommittedArgs: slices.Clone(participant.Args), BypassArgs: slices.Clone(participant.BypassArgs), EnvOverlay: cloneEnv(participant.EnvOverlay),
	}
	if participant.InitialInput != nil {
		digest := initialInputDigest(participant.InitialInput.Text)
		base.InitialInput = &InitialInputRequest{Kind: participant.InitialInput.Kind, Value: participant.InitialInput.Text, SHA256: digest}
	}
	var plan AgentPlan
	var actions []PrepareRequiredAction
	useResume := participant.ResumePolicy == ResumeEnabled && hasConversation && conversation.State == CaptureReady
	if useResume {
		plan, err = adapter.PlanResume(ResumeRequest{PlanRequest: base, Conversation: conversation.Identity})
		prepared.PlannedOutcome = "resume"
	} else {
		if participant.ResumePolicy == ResumeEnabled && hasConversation {
			actions = append(actions, PrepareRequiredAction{
				Kind: ActionStaleConversation, Handles: []string{participant.Handle},
				AllowedDecisions: []string{"fresh_once", "abort"}, ReasonCode: ReasonStaleConversation,
			})
			if conversation.State == CapturePending {
				prepared.PlannedOutcome = "capture_pending"
				return prepared, observation, nil, actions, nil
			}
		}
		base.ResumePolicy = participant.ResumePolicy
		plan, err = adapter.PlanFresh(base)
		prepared.PlannedOutcome = "fresh"
		if participant.ResumePolicy == ResumeDisabled {
			prepared.PlannedOutcome = "fresh_without_continuation"
		}
	}
	if err != nil {
		return prepared, observation, nil, actions, err
	}
	execution.Named = execution.Named && !useResume
	plan, err = applyWrapper(plan, participant.Wrapper)
	if err != nil {
		return prepared, observation, nil, actions, fmt.Errorf("wrapper for %s: %w", participant.Handle, err)
	}
	plan.Execution = clonePrepareExecutionOptions(&execution)
	prepared.Command = previewStaticCommand(plan)
	return prepared, observation, &plan, actions, nil
}

func initialInputDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func prepareInitialInputUnsupported(participant PrepareParticipant) string {
	if participant.InitialInput == nil {
		return ""
	}
	if len([]byte(participant.InitialInput.Text)) > MaxInitialInputBytes {
		return "initial_input_too_large"
	}
	if participant.InitialInput.Kind != InitialInputArgument || participant.Provider == CursorProvider {
		return "initial_input_unsupported"
	}
	return ""
}

func firstInitialInputUnsupported(observations []PrepareObservation) string {
	for _, observation := range observations {
		if observation.ReasonCode == "initial_input_too_large" || observation.ReasonCode == "initial_input_unsupported" {
			return observation.ReasonCode
		}
	}
	return ""
}

func clonePrepareExecutionOptions(options *PrepareExecutionOptions) *PrepareExecutionOptions {
	if options == nil {
		return nil
	}
	cloned := *options
	cloned.InjectorArgs = slices.Clone(options.InjectorArgs)
	cloned.SymphonyEvents = slices.Clone(options.SymphonyEvents)
	return &cloned
}

func resolvePrepareCwd(projectRoot, cwd string) (string, error) {
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(projectRoot, cwd)
	}
	resolved, err := resolvedPath(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat working directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working directory is not a directory")
	}
	return resolved, nil
}

func loadPrepareConversation(root *fsq.DeliveryRoot, handle string) (ConversationRecord, bool, error) {
	if root == nil {
		return ConversationRecord{}, false, nil
	}
	record, err := LoadConversation(root, handle)
	if errors.Is(err, os.ErrNotExist) {
		return ConversationRecord{}, false, nil
	}
	return record, err == nil, err
}

func previewStaticCommand(plan AgentPlan) *PrepareCommand {
	argv := slices.Clone(plan.Argv)
	for _, dynamic := range plan.DynamicArgv {
		switch dynamic.Kind {
		case DynamicArgLaunchNonce:
			argv[dynamic.Index] = "${launch_nonce}"
		case DynamicArgConversationID:
			argv[dynamic.Index] = "${conversation_id}"
		}
	}
	return &PrepareCommand{Argv: argv, Cwd: plan.Cwd, EnvOverlay: cloneEnv(plan.EnvOverlay)}
}

func prepareRebindActions(binding *BindingRecord, observation prepareBindingObservation, boundDetect DetectResult, selectedBackend, selectedProfile, hostIdentity string) []PrepareRequiredAction {
	if binding == nil {
		return nil
	}
	foreign := prepareBindingForeign(*binding, boundDetect, hostIdentity)
	incompatible := binding.Backend != selectedBackend || binding.Profile != selectedProfile
	if !foreign && !incompatible {
		return nil
	}
	allowed := []string{"close_old", "leave_old", "abort"}
	reason := "rebind_confirmation_required"
	if foreign {
		allowed, reason = []string{"leave_old", "abort"}, "foreign_binding"
	}
	resources := make([]string, 0, len(binding.Resources.Resources))
	for _, resource := range binding.Resources.Resources {
		resources = append(resources, resource.OpaqueID)
	}
	slices.Sort(resources)
	return []PrepareRequiredAction{{
		Kind: ActionRebindConfirmation, Resources: resources, AllowedDecisions: allowed, ReasonCode: reason,
	}}
}

func prepareBindingForeign(binding BindingRecord, detect DetectResult, hostIdentity string) bool {
	if hostIdentity == "" || detect.InstanceIdentity == "" {
		return true
	}
	if binding.HostIdentity != hostIdentity {
		return true
	}
	return binding.InstanceIdentity != detect.InstanceIdentity
}

func applyBindingResources(observations []PrepareObservation, binding *BindingRecord) {
	if binding == nil {
		return
	}
	resources := make(map[string]string, len(binding.Resources.Resources))
	for _, resource := range binding.Resources.Resources {
		if resource.Agent != "" {
			resources[resource.Agent] = resource.OpaqueID
		}
	}
	for i := range observations {
		if resource := resources[observations[i].Handle]; resource != "" {
			observations[i].Resource = resource
		}
	}
}

func appendExtraObservations(result *PrepareResult, extras []string, mailboxStates map[string]string, binding *BindingRecord, root *fsq.DeliveryRoot) error {
	for _, handle := range extras {
		observation := PrepareObservation{
			Handle: handle, Mailbox: mailboxStates[handle], Conversation: "none", ConversationIdentityDigest: "absent",
			Execution: "none", ExecutionIdentityDigest: "absent", Resource: "none", ReasonCode: "extra_mailbox_preserved",
		}
		if conversation, present, err := loadPrepareConversation(root, handle); err != nil {
			return err
		} else if present {
			observation.Conversation = string(conversation.State)
			observation.ConversationIdentityDigest, err = digestCanonical(conversation)
			if err != nil {
				return err
			}
		}
		if root != nil {
			if ticket, err := LoadExecutionTicket(root, handle); err == nil {
				observation.Execution = string(ticket.State)
				observation.ExecutionIdentityDigest, err = digestCanonical(ticket)
				if err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if binding != nil {
			for _, resource := range binding.Resources.Resources {
				if resource.Agent == handle {
					observation.Resource = resource.OpaqueID
					break
				}
			}
		}
		result.Observations = append(result.Observations, observation)
	}
	return nil
}

func loadPrepareTrust(store *TrustStore, digest string) (bool, error) {
	if store == nil {
		return false, nil
	}
	_, trusted, err := store.LoadForDigest(digest)
	return trusted, err
}

func prepareActionID(action PrepareRequiredAction) (string, error) {
	action.ActionID = ""
	return digestCanonical(struct {
		Version          int                `json:"version"`
		Kind             RequiredActionKind `json:"kind"`
		Handles          []string           `json:"handles"`
		Resources        []string           `json:"resources"`
		AllowedDecisions []string           `json:"allowed_decisions"`
		ReasonCode       string             `json:"reason_code"`
	}{1, action.Kind, action.Handles, action.Resources, action.AllowedDecisions, action.ReasonCode})
}

func comparePrepareActions(left, right PrepareRequiredAction) int {
	if value := strings.Compare(string(left.Kind), string(right.Kind)); value != 0 {
		return value
	}
	return strings.Compare(left.ActionID, right.ActionID)
}

func digestCanonical(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
