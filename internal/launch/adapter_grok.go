package launch

import (
	"context"
	"fmt"
	"strings"
)

// grokBuildVersion is the stable Grok Build CLI version whose static argument
// grammar is verified. Runtime capability probing still records the installed
// binary's version.
const grokBuildVersion = "1.0.5"

type GrokAdapter struct {
	executable string
	probe      commandProbe
}

func NewGrokAdapter(executable string) *GrokAdapter {
	return &GrokAdapter{executable: executable, probe: systemCommandProbe{}}
}

func (adapter *GrokAdapter) Name() string      { return GrokProvider }
func (adapter *GrokAdapter) Mode() AdapterMode { return AdapterModeMint }

func (adapter *GrokAdapter) CommittedEnvKeys() []string {
	return committedEnvKeys(grokEnvRules())
}

func (adapter *GrokAdapter) ValidateCommittedConfig(request CommittedConfigRequest) error {
	if err := validateCommittedConfig(request, grokEnvRules(), grokArgRules()); err != nil {
		return err
	}
	for _, name := range []string{"--tools", "--disallowed-tools"} {
		if err := validateSingleStaticArgument(request.Args, name); err != nil {
			return err
		}
	}
	return nil
}

func (adapter *GrokAdapter) Capabilities(ctx context.Context) AdapterCapabilities {
	result := AdapterCapabilities{Provider: GrokProvider, Mode: adapter.Mode()}
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
	// Help establishes only that the identity flags exist. The opt-in live
	// canary owns the stronger mint/exit/exact-resume semantic check.
	if err != nil || !grokHelpHasFlag(string(help), "--session-id") || !grokHelpHasFlag(string(help), "--resume") {
		result.Reason = "identity_flags_unsupported"
		return result
	}
	result.Available = true
	result.Executable = executable
	result.ProviderVersion = grokVersion(string(version))
	if result.ProviderVersion == "" {
		result.Available = false
		result.Reason = "version_probe_failed"
		return result
	}
	result.Fresh = true
	result.Resume = true
	return result
}

func (adapter *GrokAdapter) PlanFresh(request PlanRequest) (AgentPlan, error) {
	executable, err := validatePlanRequest(request, adapter.executable, GrokProvider, grokEnvRules(), grokArgRules(), grokBypassArgs())
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

func (adapter *GrokAdapter) PlanResume(request ResumeRequest) (AgentPlan, error) {
	if request.ResumePolicy != ResumeEnabled {
		return AgentPlan{}, fmt.Errorf("resume plan requires resume policy %q", ResumeEnabled)
	}
	if err := validateConversation(request.Conversation, GrokProvider); err != nil {
		return AgentPlan{}, err
	}
	executable, err := validatePlanRequest(request.PlanRequest, adapter.executable, GrokProvider, grokEnvRules(), grokArgRules(), grokBypassArgs())
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

func (adapter *GrokAdapter) CaptureIdentity(CaptureRequest) CaptureResult {
	return CaptureResult{State: CaptureUnsupported, Reason: CaptureReasonAdapterMintsIdentity}
}

func grokEnvRules() map[string]valueRule { return commonCommittedEnvRules() }

func grokArgRules() map[string]argumentRule {
	return map[string]argumentRule{
		"--tools":            {value: true, validate: validGrokToolList},
		"--disallowed-tools": {value: true, validate: validGrokToolList},
		"--effort":           {value: true, validate: oneOf("low", "medium", "high", "xhigh", "max")},
		"--model":            {value: true, validate: safeArgumentValue},
		"--no-alt-screen":    {},
		"--no-auto-update":   {},
		"--sandbox":          {value: true, validate: safeArgumentValue},
	}
}

// Grok tool policies are opaque provider names forwarded as argv values.
// Bound emptiness, length, whitespace, and C0/DEL so AMQ does not invent a
// Grok tool-name grammar or compile Claude's --allowedTools list.
func validGrokToolList(value string) bool {
	if value == "" || len(value) > 256 || strings.TrimSpace(value) != value || strings.HasPrefix(value, "-") {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func grokBypassArgs() map[string]struct{} {
	return map[string]struct{}{}
}

func grokHelpHasFlag(help, flag string) bool {
	for _, token := range strings.Fields(help) {
		token = strings.Trim(token, ",:;()[]")
		if token == flag || strings.HasPrefix(token, flag+"=") {
			return true
		}
	}
	return false
}

func grokVersion(output string) string {
	for _, field := range strings.Fields(output) {
		field = strings.Trim(field, ",:;()[]")
		if field == "" {
			continue
		}
		if field[0] >= '0' && field[0] <= '9' {
			return field
		}
	}
	return ""
}
