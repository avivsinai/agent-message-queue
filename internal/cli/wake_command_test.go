package cli

import (
	"slices"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/wakecmd"
)

// The owner's slash command from Buzz is typed into the Claude Code seat
// like Remote Control: exactly the command and Enter, nothing else, and only
// once no dialog is open; until then it stays queued.
func TestWakeTypesTheOwnersCommandOnceNoDialogIsOpen(t *testing.T) {
	for _, dialogOpen := range []bool{false, true} {
		h := newWakeHoldHarness(t, wakeHoldPolicy{})
		h.cfg.harness = harness.Claude
		agentDir := fsq.AgentBase(h.root, "codex")
		h.cfg.commandDir = agentDir
		open := dialogOpen
		h.cfg.dialogSource = func(time.Time) (wakeDialogState, error) {
			if open {
				return wakeDialogOpen, nil
			}
			return wakeDialogClosed, nil
		}
		id, err := wakecmd.Enqueue(agentDir, "e1", "/compact", h.now)
		if err != nil {
			t.Fatal(err)
		}

		serveWakeCommands(h.cfg)
		if dialogOpen {
			if len(h.injected) != 0 {
				t.Fatalf("typed %q while a dialog was open", h.injected)
			}
			if state, _, _ := wakecmd.Outcome(agentDir, id); state != wakecmd.Queued {
				t.Fatalf("outcome while the dialog is open = %q, want queued", state)
			}
			open = false
			serveWakeCommands(h.cfg)
		}
		if want := []string{"/compact", "\r"}; !slices.Equal(h.injected, want) {
			t.Fatalf("dialog open first %v: typed %q, want %q", dialogOpen, h.injected, want)
		}
		if state, _, _ := wakecmd.Outcome(agentDir, id); state != wakecmd.Typed {
			t.Fatalf("dialog open first %v: outcome = %q, want typed", dialogOpen, state)
		}
		h.injected = nil
		serveWakeCommands(h.cfg)
		if len(h.injected) != 0 {
			t.Fatalf("typed %q again", h.injected)
		}
	}
}
