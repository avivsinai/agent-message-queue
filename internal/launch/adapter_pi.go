package launch

import (
	"context"
	"fmt"
	"strings"
)

// piMinimumVersion is the oldest pi release whose --session-id flag uses an
// exact project session ID, reopening it when present and creating it when
// missing. Older or non-release versions are refused.
const piMinimumVersion = "1.2.0"

// piVerifiedVersion is the newest pi release on which the mint and resume
// contract was verified live. It is informational only.
const piVerifiedVersion = "1.2.0"

type PiAdapter struct {
	executable string
	probe      commandProbe
}

func NewPiAdapter(executable string) *PiAdapter {
	return &PiAdapter{executable: executable, probe: systemCommandProbe{}}
}

func (adapter *PiAdapter) Name() string      { return PiProvider }
func (adapter *PiAdapter) Mode() AdapterMode { return AdapterModeMint }

func (adapter *PiAdapter) CommittedEnvKeys() []string {
	return committedEnvKeys(piEnvRules())
}

func (adapter *PiAdapter) ValidateCommittedConfig(request CommittedConfigRequest) error {
	return validateCommittedConfig(request, piEnvRules(), piArgRules())
}

func (adapter *PiAdapter) Capabilities(ctx context.Context) AdapterCapabilities {
	result := AdapterCapabilities{Provider: PiProvider, Mode: adapter.Mode()}
	executable, err := adapter.probe.LookPath(adapter.executable)
	if err != nil {
		result.Reason = "executable_not_found"
		return result
	}
	version, err := adapter.probe.Output(ctx, executable, "--version")
	if err != nil {
		result.Reason = "version_probe_failed"
		return result
	}
	result.ProviderVersion = strings.TrimSpace(string(version))
	if !piVersionSupported(result.ProviderVersion) {
		result.Reason = "provider_version_unsupported"
		return result
	}
	help, err := adapter.probe.Output(ctx, executable, "--help")
	if err != nil || !grokHelpHasFlag(string(help), "--session-id") {
		result.Reason = "identity_flags_unsupported"
		return result
	}
	result.Available = true
	result.Executable = executable
	result.Fresh = true
	result.Resume = true
	return result
}

// PlanFresh mints the session ID from the launch nonce. pi persists the
// session only after its first turn.
func (adapter *PiAdapter) PlanFresh(request PlanRequest) (AgentPlan, error) {
	executable, err := validatePlanRequest(request, adapter.executable, PiProvider, piEnvRules(), piArgRules(), piBypassArgs())
	if err != nil {
		return AgentPlan{}, err
	}
	argv := append([]string{executable}, request.CommittedArgs...)
	argv = append(argv, "--session-id", request.LaunchNonce)
	plan := AgentPlan{
		Handle: request.Handle, Argv: argv, EnvOverlay: cloneEnv(request.EnvOverlay), Cwd: request.Cwd,
		AdapterMode: AdapterModeMint, ResumePolicy: request.ResumePolicy,
		LaunchNonce: request.LaunchNonce, ConversationID: request.LaunchNonce,
		DynamicArgv: []DynamicArg{{Index: len(argv) - 1, Kind: DynamicArgLaunchNonce}},
	}
	if err := appendInitialInput(&plan, request.InitialInput); err != nil {
		return AgentPlan{}, err
	}
	if err := ValidateAdapterPlan(adapter, plan); err != nil {
		return AgentPlan{}, err
	}
	return plan, nil
}

// PlanResume reopens the exact session ID in the seat's cwd. pi's
// --session-id never matches a prefix; a seat that took no turn has no
// session file yet, and pi creates it under the same ID.
func (adapter *PiAdapter) PlanResume(request ResumeRequest) (AgentPlan, error) {
	if request.ResumePolicy != ResumeEnabled {
		return AgentPlan{}, fmt.Errorf("resume plan requires resume policy %q", ResumeEnabled)
	}
	if err := validateConversation(request.Conversation, PiProvider); err != nil {
		return AgentPlan{}, err
	}
	executable, err := validatePlanRequest(request.PlanRequest, adapter.executable, PiProvider, piEnvRules(), piArgRules(), piBypassArgs())
	if err != nil {
		return AgentPlan{}, err
	}
	argv := append([]string{executable}, request.CommittedArgs...)
	argv = append(argv, "--session-id", request.Conversation.ID)
	plan := AgentPlan{
		Handle: request.Handle, Argv: argv, EnvOverlay: cloneEnv(request.EnvOverlay), Cwd: request.Cwd,
		AdapterMode: AdapterModeMint, ResumePolicy: request.ResumePolicy,
		LaunchNonce: request.LaunchNonce, ConversationID: request.Conversation.ID,
		DynamicArgv: []DynamicArg{{Index: len(argv) - 1, Kind: DynamicArgConversationID}},
	}
	if err := ValidateAdapterPlan(adapter, plan); err != nil {
		return AgentPlan{}, err
	}
	return plan, nil
}

func (adapter *PiAdapter) CaptureIdentity(CaptureRequest) CaptureResult {
	return CaptureResult{State: CaptureUnsupported, Reason: CaptureReasonAdapterMintsIdentity}
}

func piEnvRules() map[string]valueRule { return commonCommittedEnvRules() }

// piArgRules admits no provider arguments until a form is verified.
func piArgRules() map[string]argumentRule { return map[string]argumentRule{} }

func piBypassArgs() map[string]struct{} { return map[string]struct{}{} }

func piVersionSupported(version string) bool {
	got, ok := parseReleaseVersion(version)
	if !ok {
		return false
	}
	floor, ok := parseReleaseVersion(piMinimumVersion)
	if !ok {
		return false
	}
	for i := range got {
		if got[i] != floor[i] {
			return got[i] > floor[i]
		}
	}
	return true
}
