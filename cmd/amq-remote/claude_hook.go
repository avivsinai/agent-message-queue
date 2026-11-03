package main

import (
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
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
	case "install-stop-hook", "uninstall-stop-hook", "install-approval-hook", "uninstall-approval-hook":
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
		return permissionHook(args[1:], stdin, stdout)
	case "install-approval-hook":
		// The absolute path of this binary: a session the desktop app or an
		// IDE starts may not have its directory on PATH.
		bin, err := os.Executable()
		if err == nil {
			bin, err = filepath.Abs(bin)
		}
		if err == nil {
			err = claude.InstallPermissionHook(home, bin, claude.DefaultPermissionWait)
		}
		if err != nil {
			say(os.Stderr, "install-approval-hook: %v\n", err)
			return 1
		}
		say(stdout, "approval hook installed: tool dialogs of every Claude Code session on this machine appear in Buzz; the first answer, there or in the terminal, decides\n")
		return 0
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

// permissionHook runs the PermissionRequest receiver. It makes no decision
// on any error: exit 0 with no output. TERM, INT and HUP end the wait, and
// the terminal dialog decides.
func permissionHook(args []string, stdin io.Reader, stdout io.Writer) int {
	fs := flag.NewFlagSet("permission-hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	wait := fs.Int("wait", int(claude.DefaultPermissionWait/time.Second), "seconds to wait for a Buzz answer")
	if fs.Parse(args) != nil {
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
	return claude.RunPermissionHook(stdin, stdout, done, time.Duration(*wait)*time.Second)
}
