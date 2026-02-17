package cli

import "fmt"

const (
	envRoot    = "AM_ROOT"
	envMe      = "AM_ME"
	envSession = "AM_SESSION"
)

func Run(args []string, version string) error {
	args, noUpdate := stripNoUpdateCheckArgs(args)
	if len(args) == 0 || isHelp(args[0]) {
		return printUsage()
	}
	if isVersionArg(args[0]) {
		return printVersion(version)
	}
	startUpdateNotifier(args[0], version, noUpdate)

	switch args[0] {
	case "init":
		return runInit(args[1:])
	case "send":
		return runSend(args[1:])
	case "list":
		return runList(args[1:])
	case "read":
		return runRead(args[1:])
	case "ack":
		return runAck(args[1:])
	case "thread":
		return runThread(args[1:])
	case "presence":
		return runPresence(args[1:])
	case "cleanup":
		return runCleanup(args[1:])
	case "watch":
		return runWatch(args[1:])
	case "drain":
		return runDrain(args[1:])
	case "monitor":
		return runMonitor(args[1:])
	case "reply":
		return runReply(args[1:])
	case "dlq":
		return runDLQ(args[1:])
	case "wake":
		return runWake(args[1:])
	case "upgrade":
		return runUpgrade(args[1:], version)
	case "env":
		return runEnv(args[1:])
	case "coop":
		return runCoop(args[1:])
	case "swarm":
		return runSwarm(args[1:])
	case "doctor":
		return runDoctor(args[1:])
	case "shell-setup":
		return runShellSetup(args[1:])
	default:
		return fmt.Errorf("unknown command: %s", args[0])
	}
}

func printUsage() error {
	if err := writeStdoutLine("amq - agent message queue"); err != nil {
		return err
	}
	if err := writeStdoutLine(""); err != nil {
		return err
	}
	if err := writeStdoutLine("Usage:"); err != nil {
		return err
	}
	if err := writeStdoutLine("  amq <command> [options]"); err != nil {
		return err
	}
	if err := writeStdoutLine(""); err != nil {
		return err
	}
	if err := writeStdoutLine("Commands:"); err != nil {
		return err
	}
	if err := writeStdoutLine("  init      Initialize the queue root and agent mailboxes"); err != nil {
		return err
	}
	if err := writeStdoutLine("  send      Send a message"); err != nil {
		return err
	}
	if err := writeStdoutLine("  list      List inbox messages"); err != nil {
		return err
	}
	if err := writeStdoutLine("  read      Read a message by id"); err != nil {
		return err
	}
	if err := writeStdoutLine("  ack       Acknowledge a message"); err != nil {
		return err
	}
	if err := writeStdoutLine("  thread    View a thread"); err != nil {
		return err
	}
	if err := writeStdoutLine("  presence  Set or list presence"); err != nil {
		return err
	}
	if err := writeStdoutLine("  cleanup   Remove stale tmp files"); err != nil {
		return err
	}
	if err := writeStdoutLine("  watch     Wait for new messages (uses fsnotify)"); err != nil {
		return err
	}
	if err := writeStdoutLine("  drain     Drain new messages (read, move to cur, ack)"); err != nil {
		return err
	}
	if err := writeStdoutLine("  monitor   Combined watch+drain for co-op mode"); err != nil {
		return err
	}
	if err := writeStdoutLine("  reply     Reply to a message (auto thread/refs)"); err != nil {
		return err
	}
	if err := writeStdoutLine("  dlq       Dead letter queue management"); err != nil {
		return err
	}
	if err := writeStdoutLine("  wake      Background waker (TIOCSTI injection, experimental)"); err != nil {
		return err
	}
	if err := writeStdoutLine("  upgrade   Upgrade amq to the latest release"); err != nil {
		return err
	}
	if err := writeStdoutLine("  env       Output shell commands to set environment variables"); err != nil {
		return err
	}
	if err := writeStdoutLine("  coop      Co-op mode setup (init, exec)"); err != nil {
		return err
	}
	if err := writeStdoutLine("  swarm     Claude Code Agent Teams integration (join, tasks, bridge)"); err != nil {
		return err
	}
	if err := writeStdoutLine("  doctor    Verify installation and configuration"); err != nil {
		return err
	}
	if err := writeStdoutLine("  shell-setup  Output or install shell aliases (amc/amx)"); err != nil {
		return err
	}
	if err := writeStdoutLine(""); err != nil {
		return err
	}
	if err := writeStdoutLine("Global options:"); err != nil {
		return err
	}
	if err := writeStdoutLine("  --no-update-check  Disable update check"); err != nil {
		return err
	}
	if err := writeStdoutLine(""); err != nil {
		return err
	}
	if err := writeStdoutLine("Environment:"); err != nil {
		return err
	}
	if err := writeStdoutLine("  AM_ROOT   Default root directory for storage"); err != nil {
		return err
	}
	if err := writeStdoutLine("  AM_ME     Default agent handle"); err != nil {
		return err
	}
	if err := writeStdoutLine("  AM_SESSION  Active isolated session name (set by coop exec --session)"); err != nil {
		return err
	}
	return writeStdoutLine("  AMQ_NO_UPDATE_CHECK  Disable update check (1/true/yes/on)")
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
