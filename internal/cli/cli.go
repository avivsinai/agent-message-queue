package cli

import "fmt"

const (
	envRoot = "AM_ROOT"
	envMe   = "AM_ME"
)

func Run(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printUsage()
	}

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
	if err := writeStdoutLine(""); err != nil {
		return err
	}
	if err := writeStdoutLine("Environment:"); err != nil {
		return err
	}
	if err := writeStdoutLine("  AM_ROOT   Default root directory for storage"); err != nil {
		return err
	}
	return writeStdoutLine("  AM_ME     Default agent handle")
}
