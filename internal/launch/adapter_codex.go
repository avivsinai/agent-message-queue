package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

const CodexNotifyV1 CaptureEvidenceSource = "codex_notify_v1"

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

func (adapter *CodexAdapter) ValidateCommittedConfig(request CommittedConfigRequest) error {
	return validateCommittedConfig(request, codexEnvRules(), codexArgRules())
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
	if helpErr != nil || resumeErr != nil ||
		!strings.Contains(string(help), "resume") ||
		!strings.Contains(string(resumeHelp), "[SESSION_ID]") {
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
	notify, err := codexNotifyOverride(request)
	if err != nil {
		return AgentPlan{}, err
	}
	argv := append([]string{executable}, request.CommittedArgs...)
	argv = append(argv, request.BypassArgs...)
	argv = append(argv, "-c", notify)
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
	notify, err := codexNotifyOverride(request.PlanRequest)
	if err != nil {
		return AgentPlan{}, err
	}
	argv := append([]string{executable, "resume"}, request.CommittedArgs...)
	argv = append(argv, request.BypassArgs...)
	argv = append(argv, "-c", notify)
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

func codexNotifyOverride(request PlanRequest) (string, error) {
	if strings.TrimSpace(request.AMQExecutable) == "" || strings.TrimSpace(request.SessionRoot) == "" {
		return "", fmt.Errorf("codex notify capture requires an AMQ executable and session root")
	}
	amqExecutable, err := filepath.EvalSymlinks(request.AMQExecutable)
	if err != nil {
		return "", fmt.Errorf("resolve codex notify AMQ executable: %w", err)
	}
	amqExecutable, err = filepath.Abs(amqExecutable)
	if err != nil {
		return "", fmt.Errorf("make codex notify AMQ executable absolute: %w", err)
	}
	sessionRoot, err := resolvedPath(request.SessionRoot)
	if err != nil {
		return "", fmt.Errorf("resolve codex notify session root: %w", err)
	}
	command := []string{
		amqExecutable, "__codex-notify", "--root", sessionRoot,
		"--handle", request.Handle, "--nonce", request.LaunchNonce,
	}
	encoded, err := json.Marshal(command)
	if err != nil {
		return "", fmt.Errorf("encode codex notify override: %w", err)
	}
	return "notify=" + string(encoded), nil
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
