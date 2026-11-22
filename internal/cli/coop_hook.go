package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// Co-op hook receivers: amq coop install-hooks writes Claude Code hooks that
// start these only when AM_ME is set. A receiver reads the participant from
// AM_ME, AM_ROOT and the session pin alone, never walks directories for
// .amqrc, and prints nothing unless it has something for the model.

// coopHookPeerActive is how recent a peer's presence must be to show it as
// active in the preamble.
const coopHookPeerActive = 10 * time.Minute

func runCoopHook(args []string) error {
	fs := flag.NewFlagSet("coop hook", flag.ContinueOnError)
	usage := usageWithFlags(fs, "amq coop hook session-start|stop|prompt|wait|codex-wait",
		"Hook receivers for the co-op hooks; amq coop install-hooks installs them.",
		"They read AM_ME, AM_ROOT and the session pin from the environment, or the",
		"identity amq coop join bound to the session, and never read .amqrc. They exit",
		"0 once the arguments parse, except that a recorded identity that cannot be",
		"used is a context mismatch (exit 5), as for every command.",
		"",
		"session-start prints the co-op preamble as SessionStart additionalContext:",
		"the agent's handle, session and peers, its unread count, and whether a wake",
		"rings this terminal. Without AM_ME or an absolute AM_ROOT, with a session pin",
		"that does not match AM_ROOT, or when the mailbox does not exist, it prints",
		"nothing.",
		"",
		"stop blocks the stop once for unread mail it has not reported yet. Under a",
		"typing wake it counts only mail created at or before the wake's startup",
		"baseline, because the wake rings for later mail. It allows the stop after",
		"5 blocks in one chain, and prints a systemMessage when the context conflicts",
		"or the mailbox cannot be read.",
		"",
		"prompt: on UserPromptSubmit, adds a line of context when unread mail waits;",
		"prints nothing for the wake's own doorbell.",
		"",
		"wait rings a supported Claude Code session when it owns the doorbell.",
		"Desktop sessions support it by default. Terminal sessions enable it with",
		"--hook-doorbell=true. The asyncRewake hook waits for unread mail due under",
		"the wake settings hold_normal, hold_low and",
		"debounce and that no hook has announced, then prints the doorbell on stderr",
		"and exits 10, which the installed command turns into the exit code 2 that",
		"wakes the session. It exits quietly when the host cannot run it or the",
		"terminal has not enabled the hook.",
		"",
		"codex-wait is the doorbell of a joined Codex chat. It runs as",
		"an async Codex hook and reads the thread id from the hook input's session_id.",
		"It rings by queueing the doorbell to the thread with codex queue (the",
		"desktop app's binary when CODEX_APP_RUNTIME_CLI_PATH names it), and always",
		"exits 0; a ring that fails prints a systemMessage and is rung again by the",
		"next waiter.")
	if handled, err := parseFlagsAllowPositionals(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if fs.NArg() != 1 {
		return UsageError("coop hook needs exactly one of: session-start, stop, prompt, wait, codex-wait")
	}
	switch fs.Arg(0) {
	case "session-start":
		return writeCoopSessionStart(os.Stdout, time.Now())
	case "stop":
		return writeCoopStop(os.Stdin, os.Stdout)
	case "prompt":
		return writeCoopPrompt(os.Stdin, os.Stdout)
	case "wait":
		return runCoopWait(context.Background())
	case "codex-wait":
		return runCoopWaitCodex(context.Background(), os.Stdin)
	default:
		return UsageError("coop hook needs exactly one of: session-start, stop, prompt, wait, codex-wait")
	}
}

// coopHookContext reads the participant from AM_ME, AM_ROOT and the session
// pin alone, with no .amqrc discovery. me is "" when AM_ME is not a valid
// handle, and root is "" when AM_ROOT is unset or relative. err reports pin
// evidence that does not match AM_ROOT, which amq env also refuses. session
// comes only from a pin that matches.
func coopHookContext() (root, me, session string, err error) {
	if me, err = normalizeHandle(os.Getenv(envMe)); err != nil {
		return "", "", "", nil
	}
	root = strings.TrimSpace(os.Getenv(envRoot))
	if !filepath.IsAbs(root) {
		return "", me, "", nil
	}
	root = absPath(filepath.Clean(root))
	pin, err := loadSessionPin()
	if err != nil {
		return root, me, "", err
	}
	if !pin.Present {
		return root, me, "", nil
	}
	mismatch, err := sessionPinMismatchWithPin(root, pin)
	if err != nil {
		return root, me, "", err
	}
	if mismatch != nil {
		return root, me, "", mismatch
	}
	return root, me, pin.Session, nil
}

func writeCoopSessionStart(w io.Writer, now time.Time) error {
	root, me, session, err := coopHookContext()
	if err != nil || root == "" || me == "" {
		return nil
	}
	if info, err := os.Stat(fsq.AgentBase(root, me)); err != nil || !info.IsDir() {
		return nil
	}
	peers := coopHookPeers(root, me, now)
	unread := coopHookUnread(root, me)

	var b strings.Builder
	b.WriteString("AMQ coop: me=" + me)
	if session != "" {
		b.WriteString(" session=" + session)
	}
	if len(peers) > 0 {
		b.WriteString(" peers=" + strings.Join(peers, ","))
	}
	b.WriteString(".")
	if unread > 0 {
		fmt.Fprintf(&b, "\nInbox: %d unread. Run amq drain --include-body.", unread)
	}
	ringer, optIn := readSeatDoorbell(root, me)
	switch {
	case ringer == doorbellNone:
		// An attended seat is never typed into, so no wake mode changes the
		// wording.
		b.WriteString("\nThis seat is attended: no doorbell is typed and no hook wakes it. Mail is announced at the person's next prompt and at a turn end; run amq drain --include-body then. Do not run amq watch, amq monitor or a polling loop.")
	case ringer == doorbellTyped:
		b.WriteString("\nA wake rings this terminal when mail arrives; do not run amq watch, amq monitor or a polling loop.")
	case optIn && coopWaitRunsInBackground(root, me):
		b.WriteString("\nA hook wakes this session when mail arrives; a wake types the doorbell only when no hook waits. Do not run amq watch, amq monitor or a polling loop.")
	case coopWaitRings(root, me):
		b.WriteString("\nA hook wakes this session when mail arrives after a turn ends; do not run amq watch, amq monitor or a polling loop.")
	default:
		b.WriteString("\nNo wake: run amq drain --include-body between tasks.")
	}

	banner := fmt.Sprintf("AMQ coop: %d peer(s)", len(peers))
	if unread > 0 {
		banner += fmt.Sprintf(", %d unread", unread)
	}
	type hookSpecific struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	}
	out, err := json.Marshal(struct {
		HookSpecificOutput hookSpecific `json:"hookSpecificOutput"`
		SystemMessage      string       `json:"systemMessage"`
	}{hookSpecific{"SessionStart", b.String()}, banner})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", out)
	return err
}

// coopHookPeers lists the other mailboxes under root, with "(active)" on
// each peer whose presence is recent.
func coopHookPeers(root, me string, now time.Time) []string {
	agents, _ := fsq.ListAgents(root)
	var peers []string
	for _, agent := range agents {
		if agent == me {
			continue
		}
		if _, err := normalizeHandle(agent); err != nil {
			continue
		}
		if p, err := presence.Read(root, agent); err == nil {
			if seen, err := time.Parse(time.RFC3339Nano, p.LastSeen); err == nil && now.Sub(seen) < coopHookPeerActive {
				agent += "(active)"
			}
		}
		peers = append(peers, agent)
	}
	return peers
}

// coopWaitRings reports whether the waiter is this session's doorbell when
// no wake rings: amq coop install-hooks installed it, and the session keeps
// it in the background.
func coopWaitRings(root, me string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	commands, err := claude.MarkedHookCommands(home, "Stop", coopHookMarker)
	if err != nil || !slices.ContainsFunc(commands, ownsCoopHook(true)) {
		return false
	}
	return coopWaitRunsInBackground(root, me)
}

// coopHookUnread counts messages in inbox/new the way amq list --new does.
func coopHookUnread(root, me string) int {
	pending, _ := coopStopPending(root, me)
	return len(pending)
}
