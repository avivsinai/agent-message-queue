package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
// the TUI would not join the daemon, Codex does not already trust the
// directory, or a daemon call failed; the caller then takes the rollout path,
// which names an embedded Codex.
func startCodexOnNamedDaemonThread(cmdName string, agentArgs []string, name string) (args []string, ok bool) {
	if launch.ProviderForExecutable(cmdName) != launch.CodexProvider {
		return nil, false
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, false
	}
	cwd, ok := codexDaemonThreadDir(agentArgs, wd)
	if !ok {
		return nil, false
	}
	codexHome, err := codexHomeDir()
	if err != nil {
		return nil, false
	}
	// thread/start records trust for a directory Codex has no decision for,
	// before the TUI asks the user. Only an already trusted directory is
	// started on the daemon.
	if !codexTrustsDir(codexHome, cwd) {
		return nil, false
	}
	// With the feature off the TUI's thread/resume carries no developer
	// instructions (tui/src/app_server_session.rs:2130-2132,
	// terminal_visualization_instructions.rs:14-19), so the model's
	// instructions match a TUI start whether the daemon reloads the thread or
	// not. One setting differs, as for any `codex resume` of a thread with no
	// turns: a fresh TUI start forces model_reasoning_summary to "none" unless
	// the user set it, while a resume keeps the model default.
	if codexTerminalInstructionsEnabled(codexHome, cwd) {
		return nil, false
	}
	sock, err := codexControlSocket(cmdName, codexHome, cwd)
	if err != nil {
		if !errors.Is(err, errCodexNoDaemon) {
			_ = writeStderr("%s\n", coopNamedTUIManualReminder(name, cmdName, "Codex daemon: "+err.Error()))
		}
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

// errCodexNoDaemon means no daemon runs and AMQ does not start one; the
// rollout path then names the TUI.
var errCodexNoDaemon = errors.New("no Codex daemon to name the session on")

// codexDaemonStartTimeout bounds `codex app-server daemon start`.
const codexDaemonStartTimeout = 30 * time.Second

// codexControlSocket returns the managed daemon's control socket. A TUI that
// finds no daemon starts one and runs on it (codex-cli 0.160
// tui/src/startup_orchestration.rs:494-540, start_with_features with the
// saved feature overrides plus its own, none for the options AMQ allows).
// `codex app-server daemon start` starts it with no overrides and saves that
// (app-server-daemon/src/lib.rs:392, 405-438), so AMQ runs it only when no
// overrides are saved: then both starts are the same. Otherwise, or when
// Codex may not auto-start, it returns errCodexNoDaemon.
func codexControlSocket(cmdName, codexHome, dir string) (string, error) {
	if sock, err := codex.ControlSocket(codexHome); err == nil {
		return sock, nil
	}
	if codexDaemonAutoStartMayBeOff(codexHome, dir) || codexDaemonHasSavedFeatures(codexHome) {
		return "", errCodexNoDaemon
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexDaemonStartTimeout)
	defer cancel()
	start := exec.CommandContext(ctx, cmdName, "app-server", "daemon", "start")
	start.Dir = dir
	if output, err := start.CombinedOutput(); err != nil {
		return "", fmt.Errorf("codex app-server daemon start: %w: %s", err, bytes.TrimSpace(output))
	}
	return codex.ControlSocket(codexHome)
}

// codexDaemonHasSavedFeatures reports whether the managed daemon's settings
// (codex-cli 0.160 app-server-daemon/src/settings.rs, featureOverrides in
// $CODEX_HOME/app-server-daemon/settings.json) may hold feature overrides.
// An unreadable file counts as holding some.
func codexDaemonHasSavedFeatures(codexHome string) bool {
	raw, err := os.ReadFile(filepath.Join(codexHome, "app-server-daemon", "settings.json"))
	if os.IsNotExist(err) {
		return false
	}
	var settings struct {
		FeatureOverrides map[string]bool `json:"featureOverrides"`
	}
	return err != nil || json.Unmarshal(raw, &settings) != nil || len(settings.FeatureOverrides) > 0
}

// codexDaemonThreadDir reports whether a Codex TUI started with args, in
// this environment, runs on the managed daemon and takes every option on
// `codex resume <id>` as it would on a fresh start, and returns the thread
// directory: the -C/--cd directory resolved against wd, or wd. Only the
// options listed here qualify (codex-cli 0.160 daemon_startup.rs exclusion;
// resume merge in cli/src/main.rs; resume permissions and model overrides in
// tui/src/resume_permissions.rs and app/config_persistence.rs). Others either
// make Codex run its own app-server (-p, -c, --search, --oss), are refused by
// resume (--worktree), or were not checked; they keep the original path.
// Codex also runs its own app-server when CODEX_EXEC_SERVER_URL or a
// workload identity variable is set at all.
func codexDaemonThreadDir(args []string, wd string) (string, bool) {
	for _, key := range []string{"CODEX_EXEC_SERVER_URL", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE"} {
		if _, set := os.LookupEnv(key); set {
			return "", false
		}
	}
	dir := wd
	for i := 0; i < len(args); i++ {
		flag, value, inline := strings.Cut(args[i], "=")
		switch flag {
		case "--no-alt-screen", "--dangerously-bypass-approvals-and-sandbox", "--yolo":
			if inline {
				return "", false
			}
			continue
		case "-m", "--model", "-a", "--ask-for-approval", "-s", "--sandbox", "--add-dir", "-C", "--cd":
		default:
			return "", false
		}
		if !inline {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return "", false
			}
			i++
			value = args[i]
		}
		if value == "" {
			return "", false
		}
		if flag == "-C" || flag == "--cd" {
			dir = value
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(wd, dir)
			}
		}
	}
	return filepath.Clean(dir), true
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
