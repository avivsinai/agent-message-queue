package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
)

type coopNamedMode int

const (
	coopNamedModeOff coopNamedMode = iota
	coopNamedModeArgv
	coopNamedModeTUI
	coopNamedModeUnknown
)

type coopNamedResumeSyntax = launch.ResumeSyntax

const (
	coopNamedResumeFlags  = launch.ResumeSyntaxFlags
	coopNamedResumeCodex  = launch.ResumeSyntaxCodex
	coopNamedResumeCursor = launch.ResumeSyntaxCursor
)

type coopNamedHarness struct {
	mode         coopNamedMode
	resumeSyntax coopNamedResumeSyntax
}

var coopNamedHarnesses = map[string]coopNamedHarness{
	launch.ClaudeProvider: {mode: coopNamedModeArgv, resumeSyntax: coopNamedResumeFlags},
	"pi":                  {mode: coopNamedModeArgv, resumeSyntax: coopNamedResumeFlags},
	launch.CodexProvider:  {mode: coopNamedModeTUI, resumeSyntax: coopNamedResumeCodex},
	"agent":               {mode: coopNamedModeTUI, resumeSyntax: coopNamedResumeCursor},
	launch.CursorProvider: {mode: coopNamedModeTUI, resumeSyntax: coopNamedResumeCursor},
	launch.GrokProvider:   {mode: coopNamedModeUnknown, resumeSyntax: coopNamedResumeFlags},
}

func coopNamedHarnessFor(binary string) (coopNamedHarness, bool) {
	harness, ok := coopNamedHarnesses[strings.ToLower(filepath.Base(binary))]
	return harness, ok
}

func coopNamedModeFor(binary string) coopNamedMode {
	if harness, ok := coopNamedHarnessFor(binary); ok {
		return harness.mode
	}
	return coopNamedModeUnknown
}

func agentArgsHasNameFlag(args []string) bool {
	return launch.ArgsHaveNameFlag(args)
}

func agentArgsPreventAutoNameFor(cmdName string, args []string) bool {
	resumeSyntax := coopNamedResumeFlags
	if harness, ok := coopNamedHarnessFor(cmdName); ok {
		resumeSyntax = harness.resumeSyntax
	}
	return agentArgsHasNameFlag(args) || agentArgsHaveResume(args, resumeSyntax)
}

func agentArgsHaveResume(args []string, syntax coopNamedResumeSyntax) bool {
	return launch.ArgsHaveResume(args, syntax)
}

func injectCoopNamedArgv(cmdName string, agentArgs []string, me string) []string {
	if coopNamedModeFor(cmdName) != coopNamedModeArgv || agentArgsPreventAutoNameFor(cmdName, agentArgs) {
		return agentArgs
	}
	return append([]string{"--name", me}, agentArgs...)
}

func coopNamedSessionLabel(session, handle string) string {
	if session == "" {
		return handle
	}
	return session + "/" + handle
}

// coopNamedChoice is whether coop exec names the spawned CLI session, and
// whether the flag, the environment, or the launch config asked for it.
type coopNamedChoice struct {
	enabled, explicit bool
}

func resolveCoopNamedEnabled(fsFlagVisited bool, flagValue bool) (coopNamedChoice, error) {
	if fsFlagVisited {
		return coopNamedChoice{enabled: flagValue, explicit: true}, nil
	}
	if raw, ok := os.LookupEnv("AMQ_COOP_NAMED"); ok {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "0", "false", "off", "no":
			return coopNamedChoice{explicit: true}, nil
		case "1", "true", "on", "yes":
			return coopNamedChoice{enabled: true, explicit: true}, nil
		default:
			return coopNamedChoice{}, UsageError("AMQ_COOP_NAMED must be 0 or 1")
		}
	}
	config, present, err := loadProjectLaunchConfig()
	if err != nil {
		return coopNamedChoice{}, err
	}
	if present && config.Named != nil {
		return coopNamedChoice{enabled: *config.Named, explicit: true}, nil
	}
	return coopNamedChoice{enabled: true}, nil
}

func coopNamedTUICommand(binaryBase, me string) string {
	switch strings.ToLower(filepath.Base(binaryBase)) {
	case "pi":
		return "/name " + me
	default:
		return "/rename " + me
	}
}

func coopNamedTUIManualReminder(name, binaryBase, reason string) string {
	return fmt.Sprintf(
		"warning: unable to set or confirm the %s CLI display name %q (%s); this affects only the CLI display name; enter %q manually",
		filepath.Base(binaryBase), name, reason, coopNamedTUICommand(binaryBase, name),
	)
}

// codexDaemonNamingTimeout bounds the daemon calls made before exec.
const codexDaemonNamingTimeout = 10 * time.Second

// startCodexOnNamedDaemonThread names a Codex TUI that will run on the
// managed app-server daemon (codex-cli 0.160): it creates and names a thread
// on the daemon, then returns args that resume it (4ip). ok is false when
// the TUI would not join the daemon or a daemon call failed; the caller then
// takes the rollout path, which names an embedded Codex.
func startCodexOnNamedDaemonThread(cmdName string, agentArgs []string, name string) (args []string, ok bool) {
	if launch.ProviderForExecutable(cmdName) != launch.CodexProvider || !codexArgsJoinDaemon(agentArgs) ||
		os.Getenv("CODEX_EXEC_SERVER_URL") != "" {
		return nil, false
	}
	sock, err := codex.ControlSocket()
	if err != nil {
		return nil, false // no daemon: Codex runs embedded
	}
	cwd, err := os.Getwd()
	if err != nil {
		_ = writeStderr("%s\n", coopNamedTUIManualReminder(name, cmdName, err.Error()))
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexDaemonNamingTimeout)
	defer cancel()
	id, err := codex.StartNamedThread(ctx, sock, cwd, name)
	if err != nil {
		_ = writeStderr("%s\n", coopNamedTUIManualReminder(name, cmdName, "Codex daemon: "+err.Error()))
		return nil, false
	}
	_ = writeStderr("named %s\n", name)
	return append([]string{"resume", id}, agentArgs...), true
}

// codexArgsJoinDaemon reports whether a Codex TUI started with args reuses
// the managed daemon. Config overrides (-c, --enable, --disable, --search),
// --profile, --oss, --no-daemon, --strict-config,
// --dangerously-bypass-hook-trust and --remote make Codex run another
// app-server, which cannot open a thread the daemon holds (codex-cli 0.160
// tui daemon_startup). A positional argument, a prompt or a subcommand, is
// not rewritten into a resume either.
func codexArgsJoinDaemon(args []string) bool {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") || arg == "--" ||
			strings.HasPrefix(arg, "-c") || strings.HasPrefix(arg, "-p") {
			return false
		}
		flag, _, _ := strings.Cut(arg, "=")
		switch flag {
		case "--config", "--enable", "--disable", "--search", "--no-daemon", "--oss", "--profile",
			"--strict-config", "--dangerously-bypass-hook-trust", "--remote":
			return false
		}
	}
	return true
}

func coopNamedUnknownReminder(me, binary string) string {
	return fmt.Sprintf(
		"amq coop exec --named: name this CLI session %q manually (unknown binary %q)",
		me,
		filepath.Base(binary),
	)
}

func applyCoopNamedBeforeExecAt(
	named coopNamedChoice,
	cmdName string,
	agentArgs []string,
	name string,
	execStart time.Time,
) ([]string, error) {
	if !named.enabled {
		return agentArgs, nil
	}
	if agentArgsPreventAutoNameFor(cmdName, agentArgs) {
		return agentArgs, nil
	}
	switch coopNamedModeFor(cmdName) {
	case coopNamedModeArgv:
		return injectCoopNamedArgv(cmdName, agentArgs, name), nil
	case coopNamedModeTUI:
		if args, ok := startCodexOnNamedDaemonThread(cmdName, agentArgs, name); ok {
			return args, nil
		}
		if err := startCoopNamedTUIInjector(name, cmdName, execStart); err != nil {
			_ = writeStderr("warning: coop named inject: %v\n", err)
		}
		return agentArgs, nil
	case coopNamedModeUnknown:
		// Default naming is best effort: an unknown CLI starts quietly. Only
		// an explicit request is owed the manual step (7ja).
		if named.explicit {
			_ = writeStderr("%s\n", coopNamedUnknownReminder(name, cmdName))
		}
		return agentArgs, nil
	default:
		return agentArgs, nil
	}
}
