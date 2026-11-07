package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
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
		bin, err := hookExecutable()
		if err == nil {
			_, err = claude.OptInStopHook(home, claude.SettingsPath(home), bin)
		}
		if err != nil {
			say(os.Stderr, "install-stop-hook: %v\n", err)
			return 1
		}
		say(stdout, "stop hook installed\n")
		return 0
	case "uninstall-stop-hook":
		if err := claude.OptOutHook(claude.SettingsPath(home), claude.HookStop); err != nil {
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
		bin, err := hookExecutable()
		if err == nil {
			_, err = claude.OptInPermissionHook(home, claude.SettingsPath(home), bin)
		}
		if err != nil {
			say(os.Stderr, "install-approval-hook: %v\n", err)
			return 1
		}
		say(stdout, "approval hook installed: the first tool dialog of each Claude Code session on this machine connects that session to its own Buzz channel (an AMQ session gets its own binding, the mailbox one /amq-remote writes; any other session a native binding, where chat needs /amq-remote --native), and every dialog appears there; the first answer, there or in the terminal, decides; /amq-remote off stops it for that session\n")
		return 0
	case "uninstall-approval-hook":
		if err := claude.OptOutHook(claude.SettingsPath(home), claude.HookApproval); err != nil {
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

// hookExecutable is the absolute amq-remote path a hook runs: the stable
// path a login service would run, so an upgrade that removes the versioned
// path keeps the hook working. A temporary build is refused.
func hookExecutable() (string, error) {
	exe, _, err := stableExecutable()
	return exe, err
}

// hookProbeEnv set to 1, with an empty stdin, makes the PermissionRequest
// receiver a probe: it prints its version and exits before it binds a
// session or posts a dialog. Doctor runs the installed hook command this way
// to prove that the command reaches this receiver. A real dialog always has
// a payload on stdin, so the variable leaked into a session's environment
// never silences one.
const hookProbeEnv = "AMQ_HOOK_PROBE"

// permissionHook runs the PermissionRequest receiver. It makes no decision
// on any error: exit 0 with no output. TERM, INT and HUP end the wait, and
// the terminal dialog decides.
func permissionHook(args []string, stdin io.Reader, stdout io.Writer) int {
	if os.Getenv(hookProbeEnv) == "1" {
		var first [1]byte
		n, _ := io.ReadFull(stdin, first[:])
		if n == 0 {
			say(stdout, "amq-remote %s", version)
			return 0
		}
		stdin = io.MultiReader(bytes.NewReader(first[:n]), stdin)
	}
	fs := flag.NewFlagSet("permission-hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	// An installed hook of an older form passes --wait; the hook has no
	// deadline, so the value is ignored.
	_ = fs.Int("wait", 0, "ignored")
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
	return claude.RunPermissionHook(stdin, stdout, done, hookBinding)
}

// hookBinding writes the binding of a Claude session that no binding serves,
// for its first approval dialog, with the code attach --self uses: the
// mailbox binding of an AMQ session (AM_ROOT and AM_ME set), else the native
// binding of --native in AM_ROOT when it is a usable root, else in the
// machine root. It registers no target with the endpoint and needs no Stop
// hook, so chat in a native session's channel waits for /amq-remote
// --native. A session whose process the owner detached (/amq-remote off)
// gets nothing.
func hookBinding(s claude.Seat) (string, error) {
	if s.Root != "" && s.Handle != "" {
		_, name, _, err := bindMailbox(s.Root, s.Handle, "", io.Discard, true, s.Transcript)
		return name, err
	}
	if s.Holder.PID <= 0 {
		return "", errors.New("no Claude process above the hook")
	}
	cand, err := claude.HookCandidate(s.Holder.PID)
	if err != nil {
		return "", err
	}
	root := s.Root
	if !usableRoot(root) {
		if root, err = binding.EnsureMachineRoot(); err != nil {
			return "", err
		}
	}
	if b, err := boundElsewhere(root, cand.Target, s.Session); err != nil {
		return "", err
	} else if b != nil {
		return "", fmt.Errorf("this session is attached through %s (binding %s)", b.Root, b.Name)
	}
	nb, _, codexes := nativeBinding(root, filepath.Join(root, stateDirName), cand, s.Session, nonEmpty(cand.Display, "claude "+cand.Target), "", false, s.Cwd)
	nb.Transcript = nonEmpty(s.Transcript, nb.Transcript)
	if err := ownerDetached(nb); err != nil {
		return "", err
	}
	inspect := inspectOutsideLock(codexes)
	name, _, err := writeBinding(nb, false, false, inspect)
	if err != nil {
		return "", err
	}
	pruneAfterAttach(inspect)
	return name, nil
}

// usableRoot reports whether root can hold a native binding as attach
// checks a mailbox root: absolute and an existing directory.
func usableRoot(root string) bool {
	if root == "" || !filepath.IsAbs(root) {
		return false
	}
	fi, err := os.Stat(root)
	return err == nil && fi.IsDir()
}

// ownerDetached refuses a session the owner turned off: a tombstone that
// detach left for b's identity and b's recorded session, in whatever process
// that session runs now (a resumed session is the same session). A tombstone
// that recorded no session turns off only its own process. Another session
// in the same place is not off. The binding stays gone until the owner
// attaches the session again.
func ownerDetached(b binding.Binding) error {
	tombs, err := binding.Tombstones()
	if err != nil {
		return err
	}
	for _, t := range tombs {
		if t.Removed != binding.RemovedDetached || t.SessionIdentity() != b.SessionIdentity() {
			continue
		}
		same := t.RecordedSession() != "" && t.RecordedSession() == b.RecordedSession()
		if t.RecordedSession() == "" {
			same = t.Holder.PID > 0 && t.Holder == b.Holder
		}
		if same {
			return fmt.Errorf("the owner turned this session off (binding %s); /amq-remote connects it again", t.Name)
		}
	}
	return nil
}
