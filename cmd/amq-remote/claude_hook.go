package main

import (
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// claudeSubcommand dispatches the Stop-hook bridge subcommands. The
// receiver is fail-open: exit 0 on any error path. install/uninstall are
// the only subcommands allowed to fail (exit 1) — they run from the
// operator, never from inside Claude Code.
func claudeSubcommand(args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) == 0 {
		say(os.Stderr, "claude subcommand required (stop-hook | install-stop-hook | uninstall-stop-hook | permission-hook | install-approval-hook | uninstall-approval-hook)\n")
		return protocol.ExitUsage
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return 0 // fail-open even on home resolution failure
	}
	switch args[0] {
	case "install-stop-hook", "uninstall-stop-hook", "uninstall-approval-hook":
		// They take no options: --help or a stray argument changes nothing.
		// Only the bare command acts; a lone -- parses clean.
		if len(args) > 1 {
			fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
			fs.SetOutput(os.Stderr)
			if fs.Parse(args[1:]) == nil {
				say(os.Stderr, "%s takes no arguments\n", args[0])
			}
			return protocol.ExitUsage
		}
	}
	switch args[0] {
	case "stop-hook":
		return claude.RunStopHookReceiver(home, stdin, stdout)
	case "install-stop-hook":
		bin, err := os.Executable()
		if err != nil {
			bin = "amq-remote"
		}
		if err := claude.InstallStopHook(home, bin); err != nil {
			say(os.Stderr, "install-stop-hook: %v\n", err)
			return 1
		}
		say(stdout, "stop hook installed\n")
		return 0
	case "uninstall-stop-hook":
		if err := claude.UninstallStopHook(home); err != nil {
			say(os.Stderr, "uninstall-stop-hook: %v\n", err)
			return 1
		}
		say(stdout, "stop hook removed\n")
		return 0
	case "permission-hook":
		// No decision on any error: exit 0 with no output. TERM, INT and HUP
		// end the wait, and the terminal dialog decides.
		fs := flag.NewFlagSet("permission-hook", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		wait := fs.Int("wait", int(claude.DefaultPermissionWait/time.Second), "seconds to wait for a Buzz answer")
		var pin claude.HookPin
		fs.StringVar(&pin.Owner, "owner", "", "pinned owner pubkey (64 lowercase hex) whose signed reaction can allow a call; none means reject-only")
		fs.StringVar(&pin.Root, "root", "", "AMQ root holding the pinned share's enrolled body key")
		fs.StringVar(&pin.Session, "session", "", "pinned share session")
		fs.StringVar(&pin.Relay, "relay", "", "pinned relay URL the hook reads the approval message's history from")
		fs.StringVar(&pin.Body, "body", "", "pinned body pubkey (64 lowercase hex) of the share")
		fs.StringVar(&pin.Channel, "channel", "", "pinned DM channel id of the share")
		fs.StringVar(&pin.Target, "target", "", "pinned target id of the share")
		fs.StringVar(&pin.Surface, "surface", "", "desktop for a Buzz Desktop agent's DM pin (owner, agent pubkey as body, channel)")
		if fs.Parse(args[1:]) != nil {
			return 0
		}
		done := make(chan struct{})
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
		defer signal.Stop(sigs)
		go func() {
			<-sigs
			close(done)
		}()
		allow := allowConfig(pin)
		return claude.RunPermissionHook(home, stdin, stdout, os.Stderr, done, time.Duration(*wait)*time.Second, allow)
	case "install-approval-hook":
		return installApprovalHook(home, args[1:], stdout, os.Stderr)
	case "uninstall-approval-hook":
		if err := claude.UninstallPermissionHook(home); err != nil {
			say(os.Stderr, "uninstall-approval-hook: %v\n", err)
			return 1
		}
		say(stdout, "approval hook removed\n")
		return 0
	default:
		say(os.Stderr, "unknown claude subcommand %q\n", args[0])
		return protocol.ExitUsage
	}
}
