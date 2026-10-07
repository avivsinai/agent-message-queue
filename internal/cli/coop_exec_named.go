package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
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

// codexDaemonNamingTimeout bounds the daemon calls made before exec. They
// take about 2 s, and up to 30 s while the daemon starts other sessions
// (measured on codex-cli 0.160.1, agent-message-queue-611.63).
const codexDaemonNamingTimeout = 60 * time.Second

// codexDaemonNamingNotice is how long naming runs before coop exec says why
// the launch waits.
const codexDaemonNamingNotice = 3 * time.Second

// codexBackend is where a Codex TUI runs: on its own embedded app-server,
// on the shared managed daemon, or where AMQ cannot tell.
type codexBackend int

const (
	codexEmbedded codexBackend = iota
	codexOnDaemon
	codexBackendUnknown
)

// codexEmbeddedFlags and -p/--profile make a codex-cli 0.160 TUI run its own
// app-server (tui/src/daemon_startup.rs exclusion; --search becomes a -c
// override, startup_orchestration.rs:54-58), so the rollout path can name it.
var codexEmbeddedFlags = map[string]bool{
	"--no-daemon": true, "--oss": true, "--search": true, "--approve-for-me": true,
	"--not-so-yolo": true, "--strict-config": true, "--dangerously-bypass-hook-trust": true,
}

// codexValueOptions take a value (tui/src/cli.rs, utils/cli shared_options.rs
// and config_override.rs); codexFlagOptions take none. Any other option, -i
// (one or more values), and --remote (an app-server AMQ does not see) leave
// the backend unknown.
var (
	codexValueOptions = map[string]bool{
		"-m": true, "--model": true, "-a": true, "--ask-for-approval": true, "-s": true, "--sandbox": true,
		"-C": true, "--cd": true, "--add-dir": true, "-p": true, "--profile": true, "--local-provider": true,
		"-c": true, "--config": true, "--enable": true, "--disable": true,
	}
	codexFlagOptions = map[string]bool{
		"--no-alt-screen": true, "--yolo": true, "--dangerously-bypass-approvals-and-sandbox": true,
	}
	// codexGatingFeatures decide the backend or the daemon path; an override
	// of one on the command line is not in the effective-feature probe.
	codexGatingFeatures = map[string]bool{
		"daemon_auto_start": true, "bedrock_setup_wizard": true, "terminal_visualization_instructions": true,
	}
)

// codexDaemonFeatures are the features a -c, --enable or --disable may set
// with the TUI still on the daemon (codex-cli 0.160 daemon_startup.rs:84-105).
var codexDaemonFeatures = map[string]bool{
	"daemon_auto_start": true, "worktrees": true, "transcript_v2": true, "realtime_conversation": true,
	"standalone_web_search": true, "api_key_model_discovery": true, "code_mode_host": true,
	"auth_elicitation": true, "mcp_oauth_refresh_coordination": true, "remote_models": true,
	"request_rule": true, "responses_websockets_v2": true, "workspace_owner_usage_nudge": true,
	"tool_search_always_defer_mcp_tools": true, "remote_compaction_v2": true, "multi_agent_mode": true,
}

// codexConfigKeepsDaemon reports whether one configuration override keeps
// the TUI on the daemon (daemon_startup.rs config_exclusion): flag is -c,
// --config, --enable or --disable and value its argument. known is false
// for a form AMQ does not classify, such as a features table.
func codexConfigKeepsDaemon(flag, value string) (keeps, known bool) {
	if flag == "--enable" || flag == "--disable" {
		return codexDaemonFeatures[value], true
	}
	key, raw, ok := strings.Cut(value, "=")
	key = strings.TrimSpace(key)
	if !ok || key == "features" || key == "tui" {
		return false, false
	}
	feature, isFeature := strings.CutPrefix(key, "features.")
	allowed := key == "suppress_unstable_features_warning" || key == "tui.fullscreen_transcript" || isFeature && codexDaemonFeatures[feature]
	if !allowed {
		return false, true
	}
	// An allowed key keeps the daemon only with a boolean; AMQ reads only
	// the bare TOML spellings of one.
	raw = strings.TrimSpace(raw)
	return raw == "true" || raw == "false", raw == "true" || raw == "false"
}

// codexSubcommands are codex-cli 0.160's subcommands (codex --help, plus the
// hidden ones and aliases cli/src/main.rs accepts). As the
// first positional argument one is not a prompt and runs no ordinary TUI
// start, so AMQ cannot tell where it runs.
var codexSubcommands = map[string]bool{
	"agents": true, "exec": true, "e": true, "review": true, "login": true, "logout": true, "mcp": true,
	"plugin": true, "app-server": true, "remote-control": true, "app": true, "completion": true,
	"update": true, "doctor": true, "sandbox": true, "debug": true, "apply": true, "a": true,
	"resume": true, "queue": true, "archive": true, "delete": true, "migrate-rollouts": true,
	"unarchive": true, "fork": true, "cloud": true, "cloud-tasks": true, "exec-server": true, "features": true,
	"help": true, "stdio-to-uds": true, "responses-api-proxy": true, "tcp-tunnel": true, "execpolicy": true,
}

// codexLaunchBackend reports where a Codex TUI started with args in wd runs
// (codex-cli 0.160 tui/src/startup_orchestration.rs:176-540), with the
// effective features for its directory. A Codex that reports no
// daemon_auto_start feature predates the daemon and runs embedded; a
// failed feature probe establishes nothing.
func codexLaunchBackend(cmdName string, args []string, wd, codexHome string) (codexBackend, map[string]bool) {
	// The whole command line is read before deciding: --remote, an option
	// AMQ does not know, or an unclassified override outranks every reason
	// to call the TUI embedded.
	embedded, unknown, prompt := false, false, false
	for _, key := range []string{"CODEX_EXEC_SERVER_URL", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE"} {
		if _, set := os.LookupEnv(key); set {
			embedded = true
		}
	}
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break // the rest is the prompt
		}
		if !strings.HasPrefix(args[i], "-") {
			if !prompt && codexSubcommands[args[i]] {
				return codexBackendUnknown, nil
			}
			prompt = true
			continue
		}
		flag, value, inline := strings.Cut(args[i], "=")
		switch {
		case (codexFlagOptions[flag] || codexEmbeddedFlags[flag]) && inline, !codexFlagOptions[flag] && !codexEmbeddedFlags[flag] && !codexValueOptions[flag]:
			return codexBackendUnknown, nil
		case codexFlagOptions[flag]:
			continue
		case codexEmbeddedFlags[flag]:
			embedded = true
			continue
		}
		if !inline {
			if i+1 >= len(args) {
				return codexBackendUnknown, nil
			}
			i++
			value = args[i]
		}
		if flag == "-p" || flag == "--profile" {
			embedded = true
			continue
		}
		if flag != "-c" && flag != "--config" && flag != "--enable" && flag != "--disable" {
			continue
		}
		feature := strings.TrimSpace(value)
		if flag == "-c" || flag == "--config" {
			key, _, _ := strings.Cut(value, "=")
			feature = strings.TrimPrefix(strings.TrimSpace(key), "features.")
		}
		keeps, known := codexConfigKeepsDaemon(flag, value)
		switch {
		case codexGatingFeatures[feature] || !known:
			unknown = true
		case !keeps:
			embedded = true
		}
	}
	if unknown {
		return codexBackendUnknown, nil
	}
	if embedded {
		return codexEmbedded, nil
	}
	dir := codexLaunchDir(args, wd)
	features, err := codexEffectiveFeatures(cmdName, dir)
	if err != nil {
		return codexBackendUnknown, nil
	}
	if _, daemon := features["daemon_auto_start"]; !daemon {
		return codexEmbedded, features
	}
	// With bedrock_setup_wizard on, a signed-out TUI runs embedded even when
	// a daemon runs (startup_orchestration.rs:494-519, lib.rs:2305-2323);
	// AMQ does not read the sign-in state.
	if features["bedrock_setup_wizard"] {
		return codexBackendUnknown, features
	}
	if _, err := codex.ControlSocket(codexHome); err != nil && !features["daemon_auto_start"] {
		return codexEmbedded, features
	}
	return codexOnDaemon, features
}

// codexLaunchDir is the directory a Codex TUI started with args in wd runs
// in: the last -C/--cd value resolved against wd, or wd.
func codexLaunchDir(args []string, wd string) string {
	dir := wd
	for i, arg := range args {
		if arg == "--" {
			break
		}
		flag, value, inline := strings.Cut(arg, "=")
		if flag != "-C" && flag != "--cd" {
			continue
		}
		if !inline {
			if i+1 >= len(args) {
				break
			}
			value = args[i+1]
		}
		dir = value
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(wd, dir)
		}
	}
	return filepath.Clean(dir)
}

// startCodexOnNamedDaemonThread names a Codex TUI that will run on the
// managed app-server daemon (codex-cli 0.160): it creates and names a thread
// on the daemon, then returns args that resume it (4ip). The rollout path
// cannot see a daemon thread, so done is false only for an embedded Codex,
// which the caller then names that way. Otherwise naming is settled here:
// the thread is named, or AMQ says once why it is not and args are the
// user's unchanged.
func startCodexOnNamedDaemonThread(cmdName string, agentArgs []string, name string, record func(thread string) error) (args []string, done bool) {
	if launch.ProviderForExecutable(cmdName) != launch.CodexProvider {
		return nil, false
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, false
	}
	codexHome, err := codexHomeDir()
	if err != nil {
		return nil, false
	}
	backend, features := codexLaunchBackend(cmdName, agentArgs, wd, codexHome)
	report := func(reason string) ([]string, bool) {
		_ = writeStderr("%s\n", coopNamedTUIManualReminder(name, cmdName, reason))
		// The TUI may still run on the daemon, whose tool commands carry
		// no AM_* (agent-message-queue-611.61); amq there then has no
		// handle and refuses until one is given.
		_ = writeStderr("warning: amq commands inside this Codex session have no AMQ identity; pass --me %s, or export AM_ROOT and AM_ME in that command\n", strings.TrimPrefix(name, path.Dir(name)+"/"))
		return agentArgs, true
	}
	switch backend {
	case codexEmbedded:
		return nil, false
	case codexBackendUnknown:
		return report("AMQ cannot tell whether this Codex runs on its shared daemon")
	}
	cwd, ok := codexDaemonThreadDir(agentArgs, wd)
	if !ok {
		return report("these Codex options cannot carry over to a thread AMQ names")
	}
	// thread/start records trust for a directory Codex has no decision for,
	// before the TUI asks the user. Only an already trusted directory is
	// started on the daemon.
	if !codexTrustsDir(codexHome, cwd) {
		return report("Codex does not trust " + cwd + " yet")
	}
	// With terminal_visualization_instructions off the TUI's thread/resume
	// carries no developer instructions (tui/src/app_server_session.rs:
	// 2130-2132, terminal_visualization_instructions.rs:14-19), so the
	// model's instructions match a TUI start whether the daemon reloads the
	// thread or not. One setting differs, as for any `codex resume` of a
	// thread with no turns: a fresh TUI start forces model_reasoning_summary
	// to "none" unless the user set it, while a resume keeps the model
	// default.
	if features["terminal_visualization_instructions"] {
		return report("terminal_visualization_instructions is on")
	}
	sock, err := codex.ControlSocket(codexHome)
	if err != nil {
		// The TUI starts a daemon and runs on it. AMQ does not start it: `codex
		// app-server daemon start` replaces the daemon's saved feature
		// overrides (app-server-daemon/src/lib.rs:392, 405-438), which the
		// TUI's own start keeps.
		return report("no Codex daemon is running yet; this Codex starts one, so the next launch is named")
	}
	// Tool commands of a daemon thread run in the daemon's environment, not
	// the TUI's, so a thread is created only when its AMQ identity can be
	// recorded (agent-message-queue-611.61).
	if record == nil {
		return report("AMQ cannot record this session's identity for Codex tool commands")
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexDaemonNamingTimeout)
	defer cancel()
	notice := time.AfterFunc(codexDaemonNamingNotice, func() {
		_ = writeStderr("naming %s on the Codex daemon; it is busy, this can take up to a minute\n", name)
	})
	id, err := codex.StartNamedThread(ctx, sock, cwd, name)
	notice.Stop()
	if err != nil {
		return report("Codex daemon: " + err.Error())
	}
	// The resume arguments go back to coop exec, which records the
	// identity for this thread before exec and stops the launch if it
	// cannot (recordResumedCodexThread).
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

// codexDaemonThreadDir reports whether a Codex TUI on the managed daemon
// takes every option in args on `codex resume <id>` as it would on a fresh
// start, and returns the thread
// directory: the -C/--cd directory resolved against wd, or wd. Only options
// in codexDaemonOptions qualify, each once (--add-dir may repeat), with a
// value Codex accepts, and --yolo never with an approval policy, so Codex
// refuses no launch that AMQ already started a thread for. Others are
// refused by resume (--worktree) or were not checked.
func codexDaemonThreadDir(args []string, wd string) (string, bool) {
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
	record func(thread string) error,
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
		if args, done := startCodexOnNamedDaemonThread(cmdName, agentArgs, name, record); done {
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
