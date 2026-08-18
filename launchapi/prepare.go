package launchapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"

	internallaunch "github.com/avivsinai/agent-message-queue/internal/launch"
)

// Prepare validates and compiles caller intent, then inspects the exact target
// without creating or changing filesystem, trust, lease, or backend state.
func Prepare(ctx context.Context, request PrepareRequestV1) (PrepareResultV1, error) {
	if err := request.Validate(); err != nil {
		return PrepareResultV1{}, err
	}
	internalRequest, dependencies, err := prepareInputs(request)
	if err != nil {
		return PrepareResultV1{}, err
	}
	internalResult, err := internallaunch.Prepare(ctx, internalRequest, dependencies)
	if err != nil {
		return PrepareResultV1{}, err
	}
	return fromInternalPrepareResult(internalResult), nil
}

func prepareInputs(request PrepareRequestV1) (internallaunch.PrepareRequest, internallaunch.PrepareDependencies, error) {
	intentDigest, err := launchIntentDigest(request.Intent)
	if err != nil {
		return internallaunch.PrepareRequest{}, internallaunch.PrepareDependencies{}, err
	}
	stateDir, err := internallaunch.DefaultLaunchStateDir()
	if err != nil {
		return internallaunch.PrepareRequest{}, internallaunch.PrepareDependencies{}, err
	}
	trustStore, err := internallaunch.OpenTrustStore(stateDir, request.Target.ProjectRoot)
	if err != nil {
		return internallaunch.PrepareRequest{}, internallaunch.PrepareDependencies{}, fmt.Errorf("open launch trust store: %w", err)
	}
	host, err := os.Hostname()
	if err != nil {
		return internallaunch.PrepareRequest{}, internallaunch.PrepareDependencies{}, fmt.Errorf("resolve host identity: %w", err)
	}
	if host == "" {
		return internallaunch.PrepareRequest{}, internallaunch.PrepareDependencies{}, fmt.Errorf("resolve host identity: empty hostname")
	}
	amqPath, err := os.Executable()
	if err != nil {
		return internallaunch.PrepareRequest{}, internallaunch.PrepareDependencies{}, fmt.Errorf("resolve AMQ executable: %w", err)
	}
	internalRequest := internallaunch.PrepareRequest{
		Target: internallaunch.PrepareTarget{
			ProjectRoot: request.Target.ProjectRoot,
			SessionRoot: request.Target.SessionRoot,
			Session:     request.Target.Session,
		},
		Launcher: request.Launcher, IntentDigest: intentDigest, SubjectSchema: internallaunch.SubjectSchemaV2,
		Participants: make([]internallaunch.PrepareParticipant, 0, len(request.Intent.Participants)),
	}
	for _, participant := range request.Intent.Participants {
		provider := ""
		var committedArgs, bypassArgs []string
		if participant.Runnable {
			provider, err = internallaunch.ValidateStaticProviderInput(participant.Executable, participant.Args, participant.EnvOverlay)
			if err != nil {
				return internallaunch.PrepareRequest{}, internallaunch.PrepareDependencies{}, err
			}
			committedArgs, bypassArgs, err = internallaunch.PartitionStaticProviderArgs(provider, participant.Args)
			if err != nil {
				return internallaunch.PrepareRequest{}, internallaunch.PrepareDependencies{}, err
			}
		}
		internalRequest.Participants = append(internalRequest.Participants, toInternalPrepareParticipant(participant, provider, committedArgs, bypassArgs))
	}
	dependencies := internallaunch.PrepareDependencies{
		Backends:     internallaunch.DefaultBackends(),
		AdapterFor:   defaultPrepareAdapter,
		AMQPath:      amqPath,
		TrustStore:   trustStore,
		HostIdentity: host,
	}
	return internalRequest, dependencies, nil
}

func defaultPrepareAdapter(provider, executable string) internallaunch.HarnessAdapter {
	switch provider {
	case internallaunch.ClaudeProvider:
		return internallaunch.NewClaudeAdapter(executable)
	case internallaunch.CodexProvider:
		return internallaunch.NewCodexAdapter(executable)
	case internallaunch.CursorProvider:
		return internallaunch.NewCursorAdapter(executable)
	default:
		return nil
	}
}

func toInternalPrepareParticipant(participant ParticipantV1, provider string, committedArgs, bypassArgs []string) internallaunch.PrepareParticipant {
	result := internallaunch.PrepareParticipant{
		Handle: participant.Handle, Runnable: participant.Runnable, Provider: provider,
		Executable: participant.Executable, Args: slices.Clone(committedArgs), BypassArgs: slices.Clone(bypassArgs), EnvOverlay: cloneStringMap(participant.EnvOverlay),
		ResumePolicy: internallaunch.ResumePolicy(participant.ResumePolicy),
	}
	if participant.Cwd != nil {
		result.Cwd = participant.Cwd.Path
	}
	if participant.Execution != nil {
		result.Execution = toInternalExecutionOptions(*participant.Execution)
	}
	return result
}

func toInternalExecutionOptions(options ExecutionOptionsV1) internallaunch.PrepareExecutionOptions {
	result := internallaunch.PrepareExecutionOptions{
		RequireWake: options.RequireWake, NoGitignore: options.NoGitignore,
		WakeMode: string(options.Wake.Mode), AuditReason: options.Wake.AuditReason,
	}
	if options.Wake.Injector != nil {
		result.InjectorMode = string(options.Wake.Injector.Mode)
		result.InjectorVia = options.Wake.Injector.Via
		result.InjectorArgs = slices.Clone(options.Wake.Injector.Args)
	}
	if options.Integrations.Symphony != nil {
		result.SymphonyWorkspaceKey = options.Integrations.Symphony.WorkspaceKey
		for _, event := range options.Integrations.Symphony.Events {
			result.SymphonyEvents = append(result.SymphonyEvents, string(event))
		}
	}
	return result
}

func fromInternalPrepareResult(result internallaunch.PrepareResult) PrepareResultV1 {
	public := PrepareResultV1{
		ResultVersion: ResultVersionV1, SubjectSchema: result.SubjectSchema, Outcome: result.Outcome, Reason: result.Reason,
		SubjectDigest: result.SubjectDigest, PlanDigest: result.PlanDigest, TrustDigest: result.TrustDigest,
		RequiredActions: make([]RequiredActionV1, 0, len(result.RequiredActions)),
		Preview: PreviewV1{
			Target:  TargetV1{ProjectRoot: result.Target.ProjectRoot, SessionRoot: result.Target.SessionRoot, Session: result.Target.Session},
			Backend: result.Backend, Profile: result.Profile,
			Participants: make([]ParticipantPreviewV1, 0, len(result.Participants)),
			Roster: RosterDriftV1{
				Desired: slices.Clone(result.Roster.Desired), Present: slices.Clone(result.Roster.Present),
				Missing: slices.Clone(result.Roster.Missing), Extra: slices.Clone(result.Roster.Extra),
			},
			Capabilities: make([]ProviderCapabilitiesV1, 0),
		},
		Observations: make([]ParticipantObservationV1, 0, len(result.Observations)),
	}
	for _, action := range result.RequiredActions {
		allowed := make([]DecisionChoiceV1, len(action.AllowedDecisions))
		for i, choice := range action.AllowedDecisions {
			allowed[i] = DecisionChoiceV1(choice)
		}
		public.RequiredActions = append(public.RequiredActions, RequiredActionV1{
			ActionID: action.ActionID, Kind: RequiredActionKindV1(action.Kind),
			Handles: slices.Clone(action.Handles), Resources: slices.Clone(action.Resources),
			AllowedDecisions: allowed, ReasonCode: action.ReasonCode,
		})
	}
	for _, participant := range result.Participants {
		preview := ParticipantPreviewV1{
			Handle: participant.Handle, Runnable: participant.Runnable, Provider: participant.Provider,
			ResumePolicy: ResumePolicy(participant.ResumePolicy), PlannedOutcome: participant.PlannedOutcome,
		}
		if participant.Command != nil {
			preview.Command = &CommandV1{
				Argv: slices.Clone(participant.Command.Argv), Cwd: participant.Command.Cwd,
				EnvOverlay: cloneStringMap(participant.Command.EnvOverlay),
			}
		}
		if participant.Runnable {
			execution := fromInternalExecutionOptions(participant.Execution)
			preview.Execution = &execution
		}
		public.Preview.Participants = append(public.Preview.Participants, preview)
	}
	for _, observation := range result.Observations {
		public.Observations = append(public.Observations, ParticipantObservationV1{
			Handle: observation.Handle, Mailbox: observation.Mailbox, Runnable: observation.Runnable,
			Conversation: observation.Conversation, Execution: observation.Execution,
			Resource: observation.Resource, ReasonCode: observation.ReasonCode,
		})
	}
	return public
}

func fromInternalExecutionOptions(options internallaunch.PrepareExecutionOptions) ExecutionOptionsV1 {
	result := ExecutionOptionsV1{
		RequireWake: options.RequireWake, NoGitignore: options.NoGitignore,
		Wake: WakeOptionsV1{Mode: WakePolicy(options.WakeMode), AuditReason: options.AuditReason},
	}
	if options.InjectorMode != "" {
		result.Wake.Injector = &InjectorOptionsV1{
			Mode: InjectorMode(options.InjectorMode), Via: options.InjectorVia, Args: slices.Clone(options.InjectorArgs),
		}
	}
	if len(options.SymphonyEvents) > 0 || options.SymphonyWorkspaceKey != "" {
		symphony := &SymphonyOptionsV1{WorkspaceKey: options.SymphonyWorkspaceKey}
		for _, event := range options.SymphonyEvents {
			symphony.Events = append(symphony.Events, SymphonyEvent(event))
		}
		result.Integrations.Symphony = symphony
	}
	return result
}

func launchIntentDigest(intent LaunchIntentV1) (string, error) {
	data, err := json.Marshal(intent)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
