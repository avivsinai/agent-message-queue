package launch

import (
	"context"
	"fmt"
	"strings"
)

type ClaudeAdapter struct {
	executable string
	probe      commandProbe
}

func NewClaudeAdapter(executable string) *ClaudeAdapter {
	return &ClaudeAdapter{executable: executable, probe: systemCommandProbe{}}
}

func (adapter *ClaudeAdapter) Name() string      { return ClaudeProvider }
func (adapter *ClaudeAdapter) Mode() AdapterMode { return AdapterModeMint }

func (adapter *ClaudeAdapter) CommittedEnvKeys() []string {
	return committedEnvKeys(claudeEnvRules())
}

func (adapter *ClaudeAdapter) ValidateCommittedConfig(request CommittedConfigRequest) error {
	return validateCommittedConfig(request, claudeEnvRules(), claudeArgRules())
}

func (adapter *ClaudeAdapter) Capabilities(ctx context.Context) AdapterCapabilities {
	result := AdapterCapabilities{Provider: ClaudeProvider, Mode: adapter.Mode()}
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
	help, err := adapter.probe.Output(ctx, executable, "--help")
	if err != nil || !strings.Contains(string(help), "--session-id <uuid>") || !strings.Contains(string(help), "--resume [value]") {
		result.Reason = "identity_flags_unsupported"
		return result
	}
	result.Available = true
	result.Executable = executable
	result.ProviderVersion = claudeVersion(string(version))
	if result.ProviderVersion == "" {
		result.Available = false
		result.Reason = "version_probe_failed"
		return result
	}
	result.Fresh = true
	result.Resume = true
	return result
}

func (adapter *ClaudeAdapter) PlanFresh(request PlanRequest) (AgentPlan, error) {
	executable, err := validatePlanRequest(request, adapter.executable, ClaudeProvider, claudeEnvRules(), claudeArgRules(), claudeBypassArgs())
	if err != nil {
		return AgentPlan{}, err
	}
	argv := append([]string{executable}, request.CommittedArgs...)
	argv = append(argv, request.BypassArgs...)
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

func (adapter *ClaudeAdapter) PlanResume(request ResumeRequest) (AgentPlan, error) {
	if request.ResumePolicy != ResumeEnabled {
		return AgentPlan{}, fmt.Errorf("resume plan requires resume policy %q", ResumeEnabled)
	}
	if err := validateConversation(request.Conversation, ClaudeProvider); err != nil {
		return AgentPlan{}, err
	}
	executable, err := validatePlanRequest(request.PlanRequest, adapter.executable, ClaudeProvider, claudeEnvRules(), claudeArgRules(), claudeBypassArgs())
	if err != nil {
		return AgentPlan{}, err
	}
	argv := append([]string{executable}, request.CommittedArgs...)
	argv = append(argv, request.BypassArgs...)
	argv = append(argv, "--resume", request.Conversation.ID)
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

func (adapter *ClaudeAdapter) CaptureIdentity(CaptureRequest) CaptureResult {
	return CaptureResult{State: CaptureUnsupported, Reason: CaptureReasonAdapterMintsIdentity}
}

func claudeEnvRules() map[string]valueRule { return commonCommittedEnvRules() }

func claudeArgRules() map[string]argumentRule {
	return map[string]argumentRule{
		"--allowedTools":    {value: true, validate: validClaudeAllowedTools},
		"--effort":          {value: true, validate: oneOf("low", "medium", "high", "xhigh", "max")},
		"--model":           {value: true, validate: safeArgumentValue},
		"--no-chrome":       {},
		"--permission-mode": {value: true, validate: oneOf("acceptEdits", "auto", "manual", "dontAsk", "plan")},
		"--safe-mode":       {},
		"--verbose":         {},
	}
}

func validClaudeAllowedTools(value string) bool {
	if value == "" || len(value) > 128 || strings.ContainsRune(value, 0) {
		return false
	}
	parts := strings.Split(value, ",")
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if part == "" || strings.TrimSpace(part) != part || !safeEnvironmentValue(part) {
			return false
		}
	}
	return true
}

func claudeBypassArgs() map[string]struct{} {
	return map[string]struct{}{"--dangerously-skip-permissions": {}}
}

func claudeVersion(output string) string {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func oneOf(values ...string) valueRule {
	return func(value string) bool {
		for _, candidate := range values {
			if value == candidate {
				return true
			}
		}
		return false
	}
}
