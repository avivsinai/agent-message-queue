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
	if request.Named && !ArgsHaveNameFlag(request.CommittedArgs) {
		name := request.Handle
		if request.Session != "" {
			name = request.Session + "/" + request.Handle
		}
		if !validSessionLabel(name) {
			return AgentPlan{}, fmt.Errorf("generated session name %q is invalid", name)
		}
		argv = append(argv, "--name", name)
	}
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
		"-n":                {value: true, validate: validSessionLabel},
		"--name":            {value: true, validate: validSessionLabel},
		"--no-chrome":       {},
		"--permission-mode": {value: true, validate: oneOf("acceptEdits", "auto", "manual", "dontAsk", "plan")},
		"--safe-mode":       {},
		"--verbose":         {},
	}
}

// claudeAllowedToolsMaxBytes caps the total byte length of a --allowedTools
// value. 512 accommodates a realistic scoped list such as
// `Bash(gh pr create:*),Read,Edit` and a handful of mcp__owner__tool entries
// without admitting an unbounded attacker-controlled blob. The previous cap
// of 128 rejected that real shape (issue #648 item 2), forcing consumers to
// either widen to blanket `Bash` or drop the grant.
const claudeAllowedToolsMaxBytes = 512

// validClaudeAllowedTools validates Claude's --allowedTools value against a
// single scoped-pattern grammar:
//
//	entry := name [ "(" spec ")" ]
//	list  := entry ( "," entry )*
//
// name matches ^[A-Za-z][A-Za-z0-9_]*$ (Bash, Read, mcp__x__y). The optional
// parenthesized spec is 1..N bytes with no control byte (anything < 0x20)
// and no nested parentheses; it may carry spaces, ':', '*', '-', '/', '.',
// and commas, so scoped grants like `Bash(gh pr view:*,gh pr create:*)` parse.
// Entries are split on commas at paren depth 0 only. An entry must not
// start with '-', and each entry must have no leading or trailing
// whitespace. A spec must not start with '-' either (no legitimate scoped
// pattern begins with a dash; a dash elsewhere is fine). A bare name followed by `:*` (e.g. `Bash:*`) rejects because
// the name part fails the name regex; a scoped pattern requires the
// parentheses. The grammar accepts real Claude tool-pattern syntax and
// rejects flag-looking values such as `--dangerously-skip-permissions`,
// closing the prior strictness inversion where the latter was admitted while
// the former was rejected.
func validClaudeAllowedTools(value string) bool {
	if value == "" || len(value) > claudeAllowedToolsMaxBytes || strings.ContainsRune(value, 0) || strings.ContainsAny(value, "\r\n") {
		return false
	}
	// Split into entries on commas at paren depth 0 only, so a spec may
	// itself contain commas (e.g. Bash(gh pr view:*,gh pr create:*)). Depth
	// is 0 outside any paren and 1 inside the single balanced pair; depth>1
	// or an unmatched paren rejects.
	var entries []string
	start := 0
	depth := 0
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '(':
			depth++
			if depth > 1 {
				return false
			}
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		case ',':
			if depth == 0 {
				entries = append(entries, value[start:i])
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return false
	}
	entries = append(entries, value[start:])
	for _, part := range entries {
		if part == "" || part != strings.TrimSpace(part) || strings.HasPrefix(part, "-") {
			return false
		}
		name, spec, hasSpec := strings.Cut(part, "(")
		if !validClaudeToolName(name) {
			return false
		}
		if !hasSpec {
			continue
		}
		// A scoped entry is exactly name(spec) with one balanced pair and no
		// trailing bytes after the closing paren.
		if !strings.HasSuffix(spec, ")") {
			return false
		}
		specBody := spec[:len(spec)-1]
		if specBody == "" || strings.ContainsRune(specBody, '(') || strings.ContainsRune(specBody, ')') {
			return false
		}
		// A spec must not start with '-': there is no legitimate scoped pattern
		// that begins with a dash, and refusing it keeps a flag-looking value
		// such as Bash(--dangerously-skip-permissions) from passing. A dash
		// elsewhere in the spec (e.g. Bash(git -C x:*)) is fine.
		if specBody[0] == '-' {
			return false
		}
		if containsControlByte(specBody) {
			return false
		}
		if strings.TrimSpace(specBody) != specBody {
			return false
		}
	}
	return true
}

func validClaudeToolName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i, r := range name {
		switch {
		case i == 0:
			isAlpha := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
			if !isAlpha {
				return false
			}
		case r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
		default:
			return false
		}
	}
	return true
}

// containsControlByte reports whether s contains any byte below 0x20 (NUL,
// tab, newline, CR, and other C0 control bytes). A scoped-tool spec is a
// single-line value, so any control byte rejects.
func containsControlByte(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 {
			return true
		}
	}
	return false
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
