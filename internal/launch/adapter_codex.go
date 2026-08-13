package launch

import (
	"context"
	"fmt"
	"strings"
)

const CodexThreadStartedV2 CaptureEvidenceSource = "codex_app_server_thread_started_v2"

const codexCaptureVersion = "0.147.0"

type CodexAdapter struct {
	executable string
	probe      commandProbe
}

func NewCodexAdapter(executable string) *CodexAdapter {
	return &CodexAdapter{executable: executable, probe: systemCommandProbe{}}
}

func (adapter *CodexAdapter) Name() string      { return CodexProvider }
func (adapter *CodexAdapter) Mode() AdapterMode { return AdapterModeCapture }

func (adapter *CodexAdapter) CommittedEnvKeys() []string {
	return committedEnvKeys(codexEnvRules())
}

func (adapter *CodexAdapter) Capabilities(ctx context.Context) AdapterCapabilities {
	result := AdapterCapabilities{Provider: CodexProvider, Mode: adapter.Mode()}
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
	help, helpErr := adapter.probe.Output(ctx, executable, "--help")
	resumeHelp, resumeErr := adapter.probe.Output(ctx, executable, "resume", "--help")
	appServerHelp, appServerErr := adapter.probe.Output(ctx, executable, "app-server", "--help")
	if helpErr != nil || resumeErr != nil || appServerErr != nil ||
		!strings.Contains(string(help), "resume") ||
		!strings.Contains(string(resumeHelp), "[SESSION_ID]") ||
		!strings.Contains(string(appServerHelp), "generate-json-schema") {
		result.Reason = "identity_contract_unsupported"
		return result
	}
	result.Available = true
	result.Executable = executable
	result.ProviderVersion = codexVersion(string(version))
	if result.ProviderVersion == "" {
		result.Available = false
		result.Reason = "version_probe_failed"
		return result
	}
	result.Fresh = true
	result.Resume = true
	if result.ProviderVersion != codexCaptureVersion {
		result.Reason = "capture_version_unsupported"
		return result
	}
	result.Capture = true
	return result
}

func (adapter *CodexAdapter) PlanFresh(request PlanRequest) (AgentPlan, error) {
	executable, err := validatePlanRequest(request, adapter.executable, CodexProvider, codexEnvRules(), codexArgRules(), codexBypassArgs())
	if err != nil {
		return AgentPlan{}, err
	}
	argv := append([]string{executable}, request.CommittedArgs...)
	argv = append(argv, request.BypassArgs...)
	plan := AgentPlan{
		Handle: request.Handle, Argv: argv, EnvOverlay: cloneEnv(request.EnvOverlay), Cwd: request.Cwd,
		AdapterMode: AdapterModeCapture, ResumePolicy: request.ResumePolicy, LaunchNonce: request.LaunchNonce,
	}
	if err := ValidateAdapterPlan(adapter, plan); err != nil {
		return AgentPlan{}, err
	}
	return plan, nil
}

func (adapter *CodexAdapter) PlanResume(request ResumeRequest) (AgentPlan, error) {
	if request.ResumePolicy != ResumeEnabled {
		return AgentPlan{}, fmt.Errorf("resume plan requires resume policy %q", ResumeEnabled)
	}
	if err := validateConversation(request.Conversation, CodexProvider); err != nil {
		return AgentPlan{}, err
	}
	executable, err := validatePlanRequest(request.PlanRequest, adapter.executable, CodexProvider, codexEnvRules(), codexArgRules(), codexBypassArgs())
	if err != nil {
		return AgentPlan{}, err
	}
	argv := append([]string{executable, "resume"}, request.CommittedArgs...)
	argv = append(argv, request.BypassArgs...)
	argv = append(argv, request.Conversation.ID)
	plan := AgentPlan{
		Handle: request.Handle, Argv: argv, EnvOverlay: cloneEnv(request.EnvOverlay), Cwd: request.Cwd,
		AdapterMode: AdapterModeCapture, ResumePolicy: request.ResumePolicy,
		LaunchNonce: request.LaunchNonce, ConversationID: request.Conversation.ID,
		DynamicArgv: []DynamicArg{{Index: len(argv) - 1, Kind: DynamicArgConversationID}},
	}
	if err := ValidateAdapterPlan(adapter, plan); err != nil {
		return AgentPlan{}, err
	}
	return plan, nil
}

func (adapter *CodexAdapter) CaptureIdentity(request CaptureRequest) CaptureResult {
	return captureCodexIdentity(request)
}

func codexEnvRules() map[string]valueRule { return commonCommittedEnvRules() }

func codexArgRules() map[string]argumentRule {
	return map[string]argumentRule{
		"--ask-for-approval": {value: true, validate: oneOf("untrusted", "on-request")},
		"--model":            {value: true, validate: safeArgumentValue},
		"--no-alt-screen":    {},
		"--sandbox":          {value: true, validate: oneOf("read-only", "workspace-write")},
		"--search":           {},
	}
}

func codexBypassArgs() map[string]struct{} {
	return map[string]struct{}{"--dangerously-bypass-approvals-and-sandbox": {}}
}

func codexVersion(output string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(output), "codex-cli "))
}
