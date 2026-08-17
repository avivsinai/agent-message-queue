package launch

import (
	"context"
	"fmt"
	"strings"
)

const cursorCaptureVersion = "2026.08.11-e8db854"

const CursorCreateChatV1 CaptureEvidenceSource = "cursor_create_chat_v1"

type CursorAdapter struct {
	executable string
	probe      commandProbe
}

func NewCursorAdapter(executable string) *CursorAdapter {
	return &CursorAdapter{executable: executable, probe: systemCommandProbe{}}
}

func (adapter *CursorAdapter) Name() string      { return CursorProvider }
func (adapter *CursorAdapter) Mode() AdapterMode { return AdapterModeCapture }

func (adapter *CursorAdapter) CommittedEnvKeys() []string {
	return committedEnvKeys(cursorEnvRules())
}

func (adapter *CursorAdapter) ValidateCommittedConfig(request CommittedConfigRequest) error {
	if err := validateCursorBypassArgs(request.Args, true); err != nil {
		return err
	}
	return validateCommittedConfig(request, cursorEnvRules(), cursorArgRules())
}

func (adapter *CursorAdapter) Capabilities(ctx context.Context) AdapterCapabilities {
	result := AdapterCapabilities{Provider: CursorProvider, Mode: adapter.Mode()}
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
	createHelp, createErr := adapter.probe.Output(ctx, executable, "create-chat", "--help")
	if helpErr != nil || createErr != nil ||
		!strings.Contains(string(help), "--resume [chatId]") ||
		!strings.Contains(string(help), "--output-format <format>") ||
		!strings.Contains(string(help), "create-chat") ||
		!strings.Contains(string(createHelp), "return its ID") {
		result.Reason = "identity_contract_unsupported"
		return result
	}
	result.Available = true
	result.Executable = executable
	result.ProviderVersion = strings.TrimSpace(string(version))
	if result.ProviderVersion == "" {
		result.Available = false
		result.Reason = "version_probe_failed"
		return result
	}
	result.Resume = true
	if result.ProviderVersion != cursorCaptureVersion {
		result.Reason = "capture_version_unsupported"
		return result
	}
	result.Fresh = true
	result.Capture = true
	result.PreSpawnAcquire = true
	return result
}

func (adapter *CursorAdapter) PlanFresh(request PlanRequest) (AgentPlan, error) {
	if err := validateCursorBypassArgs(request.BypassArgs, false); err != nil {
		return AgentPlan{}, err
	}
	executable, err := validatePlanRequest(request, adapter.executable, CursorProvider, cursorEnvRules(), cursorArgRules(), cursorBypassArgs())
	if err != nil {
		return AgentPlan{}, err
	}
	argv := append([]string{executable}, request.CommittedArgs...)
	argv = append(argv, request.BypassArgs...)
	argv = append(argv, "--resume", preSpawnConversationPlaceholder)
	plan := AgentPlan{
		Handle: request.Handle, Argv: argv, EnvOverlay: cloneEnv(request.EnvOverlay), Cwd: request.Cwd,
		AdapterMode: AdapterModeCapture, ResumePolicy: request.ResumePolicy, LaunchNonce: request.LaunchNonce,
		DynamicArgv: []DynamicArg{{Index: len(argv) - 1, Kind: DynamicArgConversationID}}, PreSpawnAcquire: true,
	}
	if err := ValidateAdapterPlan(adapter, plan); err != nil {
		return AgentPlan{}, err
	}
	return plan, nil
}

func (adapter *CursorAdapter) PlanResume(request ResumeRequest) (AgentPlan, error) {
	if request.ResumePolicy != ResumeEnabled {
		return AgentPlan{}, fmt.Errorf("resume plan requires resume policy %q", ResumeEnabled)
	}
	if err := validateConversation(request.Conversation, CursorProvider); err != nil {
		return AgentPlan{}, err
	}
	if err := validateCursorBypassArgs(request.BypassArgs, false); err != nil {
		return AgentPlan{}, err
	}
	executable, err := validatePlanRequest(request.PlanRequest, adapter.executable, CursorProvider, cursorEnvRules(), cursorArgRules(), cursorBypassArgs())
	if err != nil {
		return AgentPlan{}, err
	}
	argv := append([]string{executable}, request.CommittedArgs...)
	argv = append(argv, request.BypassArgs...)
	argv = append(argv, "--resume", request.Conversation.ID)
	plan := AgentPlan{
		Handle: request.Handle, Argv: argv, EnvOverlay: cloneEnv(request.EnvOverlay), Cwd: request.Cwd,
		AdapterMode: AdapterModeCapture, ResumePolicy: request.ResumePolicy, LaunchNonce: request.LaunchNonce,
		ConversationID: request.Conversation.ID,
		DynamicArgv:    []DynamicArg{{Index: len(argv) - 1, Kind: DynamicArgConversationID}},
	}
	if err := ValidateAdapterPlan(adapter, plan); err != nil {
		return AgentPlan{}, err
	}
	return plan, nil
}

func (adapter *CursorAdapter) CaptureIdentity(request CaptureRequest) CaptureResult {
	return captureCursorIdentity(request)
}

func cursorEnvRules() map[string]valueRule { return commonCommittedEnvRules() }

func cursorArgRules() map[string]argumentRule {
	return map[string]argumentRule{"--model": {value: true, validate: safeArgumentValue}}
}

func cursorBypassArgs() map[string]struct{} {
	return map[string]struct{}{"--force": {}, "--yolo": {}}
}

func validateCursorBypassArgs(args []string, mixed bool) error {
	count := 0
	for _, arg := range args {
		if _, ok := cursorBypassArgs()[arg]; ok {
			count++
		}
	}
	if !mixed {
		count = len(args)
	}
	if count > 1 {
		return fmt.Errorf("cursor bypass accepts at most one of --force or --yolo")
	}
	return nil
}
