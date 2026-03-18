package cli

// cli.go defines the top-level command dispatcher and usage text.

const (
	envRoot     = "AM_ROOT"
	envMe       = "AM_ME"
	envBaseRoot = "AM_BASE_ROOT"
)

// commandHandler maps a command name to its handler function.
// Used by Run() and routeHelp() to dispatch commands.
var commandHandlers = map[string]func([]string) error{
	"init":        runInit,
	"send":        runSend,
	"list":        runList,
	"read":        runRead,
	"ack":         runAck,
	"thread":      runThread,
	"presence":    runPresence,
	"cleanup":     runCleanup,
	"watch":       runWatch,
	"drain":       runDrain,
	"monitor":     runMonitor,
	"reply":       runReply,
	"dlq":         runDLQ,
	"wake":        runWake,
	"env":         runEnv,
	"coop":        runCoop,
	"swarm":       runSwarm,
	"who":         runWho,
	"doctor":      runDoctor,
	"shell-setup": runShellSetup,
}

func Run(args []string, version string) error {
	args, noUpdate := stripNoUpdateCheckArgs(args)
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return printUsage()
	}
	if args[0] == "help" {
		return routeHelp(args[1:])
	}
	if isVersionArg(args[0]) {
		return printVersion(version)
	}
	startUpdateNotifier(args[0], version, noUpdate)

	// upgrade needs version, handle separately.
	if args[0] == "upgrade" {
		return runUpgrade(args[1:], version)
	}

	handler, ok := commandHandlers[args[0]]
	if !ok {
		return UsageError("unknown command: %s. Run 'amq --help' for available commands", args[0])
	}
	return handler(args[1:])
}

// subcommandGroups lists commands that have subcommands.
// Used by routeHelp to validate help paths.
var subcommandGroups = map[string]bool{
	"dlq":      true,
	"coop":     true,
	"swarm":    true,
	"presence": true,
}

// routeHelp dispatches "amq help [path...]" to the appropriate command's --help.
func routeHelp(path []string) error {
	if len(path) == 0 {
		return printUsage()
	}

	// Special-case upgrade (needs version, not in commandHandlers).
	if path[0] == "upgrade" {
		if len(path) > 1 {
			return UsageError("unknown upgrade subcommand: %s. Run 'amq upgrade --help' for details", path[1])
		}
		return runUpgrade([]string{"--help"}, "")
	}

	handler, ok := commandHandlers[path[0]]
	if !ok {
		return UsageError("unknown command: %s. Run 'amq --help' for available commands", path[0])
	}

	if len(path) == 1 {
		return handler([]string{"--help"})
	}

	// Only subcommand groups accept a second path segment.
	if !subcommandGroups[path[0]] {
		return UsageError("command %q has no subcommands. Run 'amq %s --help' for details", path[0], path[0])
	}

	if len(path) > 2 {
		return UsageError("too many arguments. Run 'amq %s %s --help' for details", path[0], path[1])
	}

	// Pass "subcommand --help" to the group handler.
	return handler([]string{path[1], "--help"})
}

func printUsage() error {
	lines := []string{
		"amq - agent message queue",
		"",
		"Usage:",
		"  amq <command> [options]",
		"",
		"Commands:",
		"  init        Initialize the queue root and agent mailboxes",
		"  send        Send a message",
		"  list        List inbox messages",
		"  read        Read a message by id",
		"  ack         Acknowledge a message",
		"  thread      View a thread",
		"  presence    Set or list agent presence",
		"  cleanup     Remove stale tmp files",
		"  watch       Wait for new messages (uses fsnotify)",
		"  drain       Drain new messages (read, move to cur, ack)",
		"  monitor     Combined watch+drain for co-op mode",
		"  reply       Reply to a message (auto thread/refs)",
		"  dlq         Dead letter queue management",
		"  wake        Background waker (TIOCSTI injection, experimental)",
		"  upgrade     Upgrade amq to the latest release",
		"  env         Output shell commands to set environment variables",
		"  coop        Co-op mode setup (init, exec)",
		"  swarm       Claude Code Agent Teams integration (join, tasks, bridge)",
		"  who         Show sessions and agents in current project",
		"  doctor      Verify installation and configuration",
		"  shell-setup Output or install shell aliases (amc/amx)",
		"",
		"Global options:",
		"  --no-update-check  Disable update check",
		"",
		"Environment:",
		"  AM_ROOT              Queue root directory (set by coop exec, always a session subdirectory)",
		"  AM_ME                Default agent handle",
		"  AMQ_NO_UPDATE_CHECK  Disable update check (1/true/yes/on)",
		"",
		"Use \"amq <command> --help\" for more information about a command.",
	}
	for _, line := range lines {
		if err := writeStdoutLine(line); err != nil {
			return err
		}
	}
	return nil
}

func isVersionArg(arg string) bool {
	switch arg {
	case "--version", "-v", "version":
		return true
	default:
		return false
	}
}

func printVersion(version string) error {
	return writeStdoutLine(version)
}

// formatUnknownSubcommand returns a consistent error for unknown subcommands.
func formatUnknownSubcommand(group, sub string) error {
	return UsageError("unknown %s subcommand: %s. Run 'amq %s --help' for available subcommands", group, sub, group)
}
