package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

// Co-op hook receivers: amq coop install-hooks writes Claude Code hooks that
// start these only when AM_ME is set. A receiver reads the participant from
// AM_ME and AM_ROOT alone, never walks directories for .amqrc,
// and prints nothing unless it has something for the model.

// coopHookPeerActive is how recent a peer's presence must be to show it as
// active in the preamble.
const coopHookPeerActive = 10 * time.Minute

func runCoopHook(args []string) error {
	fs := flag.NewFlagSet("coop hook", flag.ContinueOnError)
	usage := usageWithFlags(fs, "amq coop hook session-start",
		"Claude Code hook receiver for the co-op hooks; amq coop install-hooks installs it.",
		"",
		"session-start prints the co-op preamble as SessionStart additionalContext:",
		"the agent's handle, session and peers, its unread count, and whether a wake",
		"rings this terminal. It reads AM_ME and AM_ROOT from the environment;",
		"without AM_ME or AM_ROOT, or when the mailbox does not exist,",
		"it prints nothing. Always exits 0 once the arguments parse.")
	if handled, err := parseFlagsAllowPositionals(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if fs.NArg() != 1 {
		return UsageError("coop hook needs exactly one of: session-start")
	}
	switch fs.Arg(0) {
	case "session-start":
		return writeCoopSessionStart(os.Stdout, time.Now())
	default:
		return UsageError("coop hook needs exactly one of: session-start")
	}
}

// coopHookParticipant returns the root and handle from the environment, or
// ok=false when this session does not take part in co-op.
func coopHookParticipant() (root, me string, ok bool) {
	me, err := normalizeHandle(os.Getenv(envMe))
	if err != nil {
		return "", "", false
	}
	root = resolveRoot(os.Getenv(envRoot))
	if root == "" {
		return "", "", false
	}
	if info, err := os.Stat(fsq.AgentBase(root, me)); err != nil || !info.IsDir() {
		return "", "", false
	}
	return root, me, true
}

func writeCoopSessionStart(w io.Writer, now time.Time) error {
	root, me, ok := coopHookParticipant()
	if !ok {
		return nil
	}
	session := inferredSessionIdentity(root)
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
	if coopHookWakeRings(root, me) {
		b.WriteString("\nA wake rings this terminal when mail arrives; do not run amq watch, amq monitor or a polling loop.")
	} else {
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

// coopHookUnread counts messages in inbox/new the way amq list --new does.
func coopHookUnread(root, me string) int {
	entries, _ := os.ReadDir(fsq.AgentInboxNew(root, me))
	n := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type().IsRegular() && !strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".md") {
			n++
		}
	}
	return n
}

// coopHookWakeRings reports whether a live wake for (root, me) writes a
// doorbell into the terminal, with amq wake check's liveness rule from one
// lock observation. A notify-only wake (mode none) does not ring the
// terminal. Any doubt reads as no wake, so the preamble tells the agent to
// drain.
func coopHookWakeRings(root, me string) bool {
	inspection := inspectWakeLock(root, me)
	live := inspection.Exists &&
		inspection.Status == wakeLockValid &&
		inspection.IdentityConfirmed &&
		inspection.Process.Running
	return live && inspection.Lock.WakeMode != wakeInjectModeNone
}
