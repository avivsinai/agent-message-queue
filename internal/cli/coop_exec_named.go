package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
// on the daemon, then returns args that resume it (4ip). done is true when
// naming is settled: the thread is named, or the TUI will run on a daemon AMQ
// cannot name it on, which it reports; args are then the arguments to exec.
// done is false when the TUI would not join the daemon, Codex does not
// already trust the directory, or a daemon call failed; the caller then takes
// the rollout path, which names an embedded Codex.
func startCodexOnNamedDaemonThread(cmdName string, agentArgs []string, name string) (args []string, done bool) {
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
	features, err := codexEffectiveFeatures(cmdName, cwd)
	if err != nil {
		return nil, false
	}
	// With terminal_visualization_instructions off the TUI's thread/resume
	// carries no developer instructions (tui/src/app_server_session.rs:
	// 2130-2132, terminal_visualization_instructions.rs:14-19), so the
	// model's instructions match a TUI start whether the daemon reloads the
	// thread or not. One setting differs, as for any `codex resume` of a
	// thread with no turns: a fresh TUI start forces model_reasoning_summary
	// to "none" unless the user set it, while a resume keeps the model
	// default. With bedrock_setup_wizard on, a signed-out TUI runs embedded
	// even when a daemon runs (tui/src/startup_orchestration.rs:494-519,
	// lib.rs:2305-2323).
	if features["terminal_visualization_instructions"] || features["bedrock_setup_wizard"] {
		return nil, false
	}
	sock, err := codex.ControlSocket(codexHome)
	if err != nil {
		if !features["daemon_auto_start"] {
			return nil, false // the TUI runs embedded
		}
		// The TUI starts a daemon and runs on it
		// (startup_orchestration.rs:494-540), where the rollout path cannot
		// find its thread. AMQ does not start the daemon itself: `codex
		// app-server daemon start` replaces the daemon's saved feature
		// overrides (app-server-daemon/src/lib.rs:392, 405-438), which the
		// TUI's own start keeps.
		_ = writeStderr("%s\n", coopNamedTUIManualReminder(name, cmdName, "no Codex daemon is running yet; this Codex starts one, so the next launch is named"))
		return agentArgs, true
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

// codexEffectiveFeatures returns Codex's effective feature flags for dir as
// `codex features list` prints them: one "<name> <stage> <true|false>" row
// per feature, from the same layers the TUI loads (codex-cli 0.160
// cli/src/main.rs FeaturesSubcommand::List, cloud_config::load_config).
func codexEffectiveFeatures(cmdName, dir string) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), codexDaemonNamingTimeout)
	defer cancel()
	list := exec.CommandContext(ctx, cmdName, "features", "list")
	list.Dir = dir
	output, err := list.Output()
	if err != nil {
		return nil, fmt.Errorf("codex features list: %w", err)
	}
	features := make(map[string]bool)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		switch fields[len(fields)-1] {
		case "true":
			features[fields[0]] = true
		case "false":
			features[fields[0]] = false
		}
	}
	for _, required := range []string{"daemon_auto_start", "bedrock_setup_wizard"} {
		if _, ok := features[required]; !ok {
			return nil, fmt.Errorf("codex features list did not report %s", required)
		}
	}
	return features, nil
}

// codexDaemonOptions maps each option that keeps a Codex TUI on the managed
// daemon and takes effect on `codex resume <id>` as on a fresh start to its
// canonical name and the values Codex accepts (codex-cli 0.160: daemon
// exclusion in tui/src/daemon_startup.rs, resume merge in cli/src/main.rs,
// resume permissions and model overrides in tui/src/resume_permissions.rs and
// app/config_persistence.rs, value enums in utils/cli). nil values take any
// argument; a missing entry is a flag without one.
var codexDaemonOptions = map[string]struct {
	name   string
	values []string
	arg    bool
}{
	"-m": {name: "model", arg: true}, "--model": {name: "model", arg: true},
	"-a": {name: "approval", arg: true, values: []string{"on-request", "never"}}, "--ask-for-approval": {name: "approval", arg: true, values: []string{"on-request", "never"}},
	"-s": {name: "sandbox", arg: true, values: []string{"read-only", "workspace-write", "danger-full-access"}}, "--sandbox": {name: "sandbox", arg: true, values: []string{"read-only", "workspace-write", "danger-full-access"}},
	"-C": {name: "cd", arg: true}, "--cd": {name: "cd", arg: true},
	"--add-dir":       {name: "add-dir", arg: true},
	"--no-alt-screen": {name: "no-alt-screen"},
	"--yolo":          {name: "yolo"}, "--dangerously-bypass-approvals-and-sandbox": {name: "yolo"},
}

// codexDaemonThreadDir reports whether a Codex TUI started with args, in
// this environment, runs on the managed daemon and takes every option on
// `codex resume <id>` as it would on a fresh start, and returns the thread
// directory: the -C/--cd directory resolved against wd, or wd. Only options
// in codexDaemonOptions qualify, each once (--add-dir may repeat), with a
// value Codex accepts, and --yolo never with an approval policy, so Codex
// refuses no launch that AMQ already started a thread for. Others make Codex
// run its own app-server (-p, -c, --search, --oss), are refused by resume
// (--worktree), or were not checked; they keep the original path. Codex also
// runs its own app-server when CODEX_EXEC_SERVER_URL or a workload identity
// variable is set at all.
func codexDaemonThreadDir(args []string, wd string) (string, bool) {
	for _, key := range []string{"CODEX_EXEC_SERVER_URL", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE"} {
		if _, set := os.LookupEnv(key); set {
			return "", false
		}
	}
	dir := wd
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		flag, value, inline := strings.Cut(args[i], "=")
		option, ok := codexDaemonOptions[flag]
		if !ok || inline && !option.arg || seen[option.name] && option.name != "add-dir" {
			return "", false
		}
		seen[option.name] = true
		if !option.arg {
			continue
		}
		if !inline {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return "", false
			}
			i++
			value = args[i]
		}
		if value == "" || option.values != nil && !slices.Contains(option.values, value) {
			return "", false
		}
		if option.name == "cd" {
			dir = value
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(wd, dir)
			}
		}
	}
	if seen["yolo"] && seen["approval"] {
		return "", false
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", false
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
		if args, done := startCodexOnNamedDaemonThread(cmdName, agentArgs, name); done {
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
