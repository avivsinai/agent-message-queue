package launch

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

const (
	CommandsBackendName = "commands"
	// InternalLaunchNonceEnv marks a command emitted by the trusted launch
	// reconciler. coop exec consumes it and never forwards it to the provider.
	InternalLaunchNonceEnv    = "AMQ_INTERNAL_LAUNCH_NONCE"
	PlanOnlyInspectEvidence   = "plan_only backend has no query surface"
	PlanOnlyCloseReason       = "plan_only backend owns no terminal resource"
	commandsProfileVersion    = 1
	commandsProfilePlatform   = "any"
	commandsProfileVersionRng = "*"
)

// Commands is the plan_only backend. It emits exact coop-exec invocations
// from a prebuilt plan and never owns a terminal resource.
type Commands struct{}

func CommandsProfile() Profile {
	return Profile{
		Backend:      CommandsBackendName,
		Platform:     commandsProfilePlatform,
		VersionRange: commandsProfileVersionRng,
		Version:      commandsProfileVersion,
		Capabilities: []Capability{CapPlanOnly},
	}
}

func (Commands) Detect() DetectResult {
	profile := CommandsProfile()
	return DetectResult{
		Available: true,
		Profile:   profile,
		Effective: slices.Clone(profile.Capabilities),
	}
}

func (Commands) Create(req CreateRequest) (CreateResult, error) {
	if req.Placement != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: placementError(PlacementUnsupportedReason)}
	}
	if req.JoinBinding != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: placementError(PlacementUnsupportedReason)}
	}
	if strings.TrimSpace(req.Session) == "" {
		return CreateResult{}, fmt.Errorf("session is required")
	}
	if req.Root == nil {
		return CreateResult{}, fmt.Errorf("pinned session root is required")
	}
	if err := req.Root.VerifyBase(); err != nil {
		return CreateResult{}, fmt.Errorf("verify pinned session root: %w", err)
	}
	if err := req.Plan.Validate(); err != nil {
		return CreateResult{}, err
	}
	amq := req.AMQPath
	if strings.TrimSpace(amq) == "" {
		amq = "amq"
	}
	planJSON, err := json.Marshal(req.Plan)
	if err != nil {
		return CreateResult{}, err
	}
	commands := make([]EmittedCommand, 0, len(req.Plan.Agents))
	for _, agent := range req.Plan.Agents {
		argv := coopExecArgv(amq, req.Root.Base(), agent.Handle, agent.Argv, agent.Execution)
		env := cloneEnv(agent.EnvOverlay)
		if env == nil {
			env = make(map[string]string, 1)
		}
		env[InternalLaunchNonceEnv] = agent.LaunchNonce
		commands = append(commands, EmittedCommand{
			Handle:      agent.Handle,
			Argv:        argv,
			Cwd:         agent.Cwd,
			Env:         env,
			LaunchNonce: agent.LaunchNonce,
			Line:        commandLine(agent.Cwd, env, argv),
		})
	}
	return CreateResult{
		Outcome:        OutcomeCommandsEmitted,
		ActionRequired: true,
		Profile:        CommandsProfile().Identity(),
		Commands:       commands,
		Plan:           planJSON,
	}, nil
}

func (Commands) Inspect(InspectRequest) (InspectResult, error) {
	return InspectResult{
		Status:         InspectUnknown,
		Evidence:       PlanOnlyInspectEvidence,
		ActionRequired: true,
	}, nil
}

func (Commands) Close(CloseRequest) (CloseResult, error) {
	return CloseResult{Outcome: OutcomeUnsupported, Reason: PlanOnlyCloseReason}, nil
}

// coopExecArgv is amq coop exec [options] <command> [-- <command-flags>].
// The command positional is required; flags after -- are agent args. A lone
// executable omits --.
func coopExecArgv(amq, sessionRoot, handle string, planArgv []string, options *PrepareExecutionOptions) []string {
	argv := []string{amq, "coop", "exec", "--root", sessionRoot, "--me", handle}
	argv = append(argv, coopExecOptionArgs(options)...)
	argv = append(argv, planArgv[0])
	if len(planArgv) > 1 {
		argv = append(argv, "--")
		argv = append(argv, planArgv[1:]...)
	}
	return argv
}

func coopExecOptionArgs(options *PrepareExecutionOptions) []string {
	if options == nil {
		return nil
	}
	args := make([]string, 0, 16)
	if options.NoGitignore {
		args = append(args, "--no-gitignore")
	}
	if options.WakeMode == "disabled" {
		args = append(args, "--no-wake", "--managed-no-wake-reason", options.AuditReason)
	} else {
		if options.RequireWake {
			args = append(args, "--require-wake")
		}
		if options.InjectorMode != "" {
			args = append(args, "--wake-inject-mode", options.InjectorMode)
		}
		if options.InjectorVia != "" {
			args = append(args, "--wake-inject-via", options.InjectorVia)
			for _, arg := range options.InjectorArgs {
				args = append(args, "--wake-inject-arg", arg)
			}
		}
	}
	for _, event := range options.SymphonyEvents {
		args = append(args, "--managed-symphony-event", event)
	}
	if options.SymphonyWorkspaceKey != "" {
		args = append(args, "--managed-symphony-workspace-key", options.SymphonyWorkspaceKey)
	}
	return args
}

func commandLine(cwd string, env map[string]string, argv []string) string {
	parts := make([]string, 0, 4+len(env)+len(argv))
	if cwd != "" {
		parts = append(parts, "cd", shellQuote(cwd), "&&")
	}
	if len(env) != 0 {
		parts = append(parts, "env")
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			parts = append(parts, k+"="+shellQuote(env[k]))
		}
	}
	for _, arg := range argv {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	if strings.IndexFunc(value, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		case strings.ContainsRune("@%_+=:,./-", r):
			return false
		default:
			return true
		}
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
