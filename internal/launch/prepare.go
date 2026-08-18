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
	SessionRoot string `json:"session_root"`
	Session     string `json:"session"`
}

type PrepareExecutionOptions struct {
	RequireWake          bool     `json:"require_wake"`
	NoGitignore          bool     `json:"no_gitignore"`
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
}

type PrepareResult struct {
	Outcome         string
	Reason          string
	SubjectSchema   int
	SubjectDigest   string
	PlanDigest      string
	TrustDigest     string
	Target          PrepareTarget
	Backend         string
	Profile         string
	Participants    []PreparedParticipant
	Roster          PrepareRoster
	RequiredActions []PrepareRequiredAction
	Observations    []PrepareObservation
}

type prepareTargetState struct {
	target          PrepareTarget
	projectIdentity string
	sessionIdentity string
	projectRoot     *fsq.DeliveryRoot
	baseRoot        *fsq.DeliveryRoot
	sessionRoot     *fsq.DeliveryRoot
	mailboxConfig   *fsq.MailboxConfigAuthorization
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
	if state.projectRoot != nil {
		_ = state.projectRoot.Close()
	}
}

func (state *prepareTargetState) verify() error {
	if state.mailboxConfig != nil {
		if err := state.mailboxConfig.Verify(); err != nil {
			return fmt.Errorf("mailbox config changed during Prepare: %w", err)
		}
	}
	if err := state.projectRoot.VerifyBase(); err != nil {
		return fmt.Errorf("project root changed during Prepare: %w", err)
	}
	if err := state.baseRoot.VerifyBase(); err != nil {
		return fmt.Errorf("session base root changed during Prepare: %w", err)
	}
	if state.sessionRoot != nil {
		if err := state.sessionRoot.VerifyBase(); err != nil {
			return fmt.Errorf("session root changed during Prepare: %w", err)
		}
		return nil
	}
	child, err := state.baseRoot.OpenDirectChild(state.target.Session)
	if err == nil {
		_ = child.Close()
		return fmt.Errorf("session root appeared during Prepare")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
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
	Backend         string                    `json:"backend"`
	Profile         string                    `json:"profile"`
	Detection       DetectResult              `json:"detection"`
	Binding         prepareBindingObservation `json:"binding"`
	Roster          PrepareRoster             `json:"roster"`
	Actions         []PrepareRequiredAction   `json:"actions"`
	Participants    []PreparedParticipant     `json:"participants"`
	Observations    []PrepareObservation      `json:"observations"`
}

// Prepare compiles public caller intent and inspects current launch state. It
// deliberately has no write-capable dependency: no lease, Create, Close,
// Focus, trust replacement, journal writer, or mailbox repair callback is
// reachable from this function.
func Prepare(ctx context.Context, request PrepareRequest, dependencies PrepareDependencies) (PrepareResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return PrepareResult{}, err
	}
	if !validDigest(request.IntentDigest) {
		return PrepareResult{}, fmt.Errorf("invalid intent digest")
	}
	if len(request.Participants) == 0 {
		return PrepareResult{}, fmt.Errorf("prepare requires at least one participant")
	}
	subjectSchema, err := normalizeSubjectSchema(request.SubjectSchema)
	if err != nil {
		return PrepareResult{}, err
	}
	amqPath, err := resolveLaunchAMQExecutable(dependencies.AMQPath)
	if err != nil {
		return PrepareResult{}, err
	}
	dependencies.AMQPath = amqPath
	state, err := openPrepareTarget(request.Target)
	if err != nil {
		return PrepareResult{}, err
	}
	defer state.close()

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
		Participants: make([]PreparedParticipant, 0, len(request.Participants)),
		Observations: make([]PrepareObservation, 0, len(request.Participants)+len(roster.Extra)),
	}
	if detect.Profile.Backend != "" {
		result.Profile = detect.Profile.Identity()
	}

	plan := Plan{Version: PlanVersion, Agents: []AgentPlan{}}
	for _, participant := range request.Participants {
		prepared, observation, agentPlan, actions, participantErr := prepareOneParticipant(participant, state, dependencies, mailboxStates[participant.Handle])
		if participantErr != nil {
			return PrepareResult{}, participantErr
		}
		result.Participants = append(result.Participants, prepared)
		result.Observations = append(result.Observations, observation)
		result.RequiredActions = append(result.RequiredActions, actions...)
		if agentPlan != nil {
			plan.Agents = append(plan.Agents, *agentPlan)
		}
	}
	applyBindingResources(result.Observations, binding)
	if err := appendExtraObservations(&result, roster.Extra, mailboxStates, binding, state.sessionRoot); err != nil {
		return PrepareResult{}, err
	}

	result.RequiredActions = append(result.RequiredActions, prepareRebindActions(binding, bindingObservation, boundDetect, backendName, result.Profile, dependencies.HostIdentity)...)
	for i := range result.RequiredActions {
		result.RequiredActions[i].ActionID, err = prepareActionID(result.RequiredActions[i])
		if err != nil {
			return PrepareResult{}, err
		}
	}
	slices.SortFunc(result.RequiredActions, comparePrepareActions)

	result.PlanDigest, err = PreparePlanDigest(plan)
	if err != nil {
		return PrepareResult{}, err
	}
	trustPlanDigest, err := PrepareTrustPlanDigest(plan)
	if err != nil {
		return PrepareResult{}, err
	}
	result.TrustDigest, err = PrepareTrustDigest(trustPlanDigest, state.target.Session, state.target.SessionRoot, state.sessionIdentity)
	if err != nil {
		return PrepareResult{}, err
	}
	if detect.Available && len(plan.Agents) > 0 {
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
	result.SubjectDigest, err = digestCanonical(prepareSubject{
		Version: subjectSchema, IntentDigest: request.IntentDigest, PlanDigest: result.PlanDigest, TrustDigest: result.TrustDigest,
		Target: state.target, ProjectIdentity: state.projectIdentity, SessionIdentity: state.sessionIdentity,
		Backend: result.Backend, Profile: result.Profile, Detection: detect, Binding: bindingObservation, Roster: result.Roster,
		Actions: result.RequiredActions, Participants: result.Participants, Observations: result.Observations,
	})
	if err != nil {
		return PrepareResult{}, err
	}
	return result, nil
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

func prepareOneParticipant(participant PrepareParticipant, state *prepareTargetState, dependencies PrepareDependencies, mailbox string) (PreparedParticipant, PrepareObservation, *AgentPlan, []PrepareRequiredAction, error) {
	prepared := PreparedParticipant{
		Handle: participant.Handle, Runnable: participant.Runnable, Provider: participant.Provider,
		ResumePolicy: participant.ResumePolicy, Execution: participant.Execution, PlannedOutcome: "participant_only",
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
	if reason := prepareInitialInputUnsupported(participant); reason != "" {
		prepared.PlannedOutcome = "unsupported"
		observation.ReasonCode = reason
		return prepared, observation, nil, nil, nil
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

	base := PlanRequest{
		Handle: participant.Handle, ProjectRoot: state.target.ProjectRoot, SessionRoot: state.target.SessionRoot,
		AMQExecutable: dependencies.AMQPath, Cwd: cwd, AllowExternalCwd: true,
		LaunchNonce: preparePlaceholderUUID, ResumePolicy: participant.ResumePolicy,
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
	plan.Execution = clonePrepareExecutionOptions(&participant.Execution)
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
