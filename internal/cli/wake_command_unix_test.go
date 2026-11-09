//go:build darwin || linux

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/wakecmd"
)

const commandChannel = "chan"

// commandSeat makes h's seat a Buzz share whose Buzz agent recorded owner,
// and gives its wake the command queue.
func commandSeat(t *testing.T, h *wakeHoldHarness) (owner nostr.SecretKey, target string) {
	t.Helper()
	t.Setenv(binding.EnvPath, filepath.Join(secureTempDirForTest(t), "binding.json"))
	if err := binding.WriteNamed(binding.Binding{Carrier: binding.CarrierMailbox, Root: h.root, Handle: "codex", Name: "seat"}); err != nil {
		t.Fatal(err)
	}
	b, err := binding.ReadNamed("seat")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := dialog.DirFor("seat")
	if err != nil {
		t.Fatal(err)
	}
	owner = nostr.Generate()
	if err := dialog.WriteOwner(dir, dialog.Owner{PubKey: owner.Public().Hex(), Channel: commandChannel}); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(h.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	h.cfg.harness = harness.Claude
	h.cfg.commands = &wakeCommands{dir: agentDir}
	return owner, b.SessionIdentity()
}

// queueCommand queues text, signed by key at signed, as the Buzz agent does.
func queueCommand(t *testing.T, h *wakeHoldHarness, key nostr.SecretKey, target, text string, signed time.Time) string {
	t.Helper()
	evt := nostr.Event{CreatedAt: nostr.Timestamp(signed.Unix()), Kind: 9, Content: text, Tags: nostr.Tags{{"h", commandChannel}}}
	if err := evt.Sign(key); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(evt)
	if err != nil {
		t.Fatal(err)
	}
	id, err := wakecmd.Enqueue(fsq.AgentBase(h.root, "codex"), evt.ID.Hex(), wakecmd.Request{Target: target, Text: text, Event: raw}, h.now)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The owner's slash command from Buzz is typed into the Claude Code seat
// like Remote Control: exactly the command and Enter, nothing else, and only
// once no dialog is open; until then it stays queued.
func TestWakeTypesTheOwnersCommandOnceNoDialogIsOpen(t *testing.T) {
	for _, dialogOpen := range []bool{false, true} {
		h := newWakeHoldHarness(t, wakeHoldPolicy{})
		owner, target := commandSeat(t, h)
		agentDir := fsq.AgentBase(h.root, "codex")
		open := dialogOpen
		h.cfg.dialogSource = func(time.Time) (wakeDialogState, error) {
			if open {
				return wakeDialogOpen, nil
			}
			return wakeDialogClosed, nil
		}
		id := queueCommand(t, h, owner, target, "/compact", h.now)

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

// Review of Buzz slash commands: a file in the queue is not owner proof.
// The wake types only a request whose signed owner message verifies, and a
// verified command held past wakecmd.Expiry is refused, not typed.
func TestWakeTypesOnlyAVerifiedFreshOwnerCommand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		plant  func(t *testing.T, h *wakeHoldHarness, owner nostr.SecretKey, target string)
		reason string
	}{
		{name: "planted request file", plant: func(t *testing.T, h *wakeHoldHarness, _ nostr.SecretKey, _ string) {
			q := wakecmd.Dir(fsq.AgentBase(h.root, "codex"))
			if err := os.Mkdir(q, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(q, "00000000000000000001-forged.req"), []byte("/clear"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "signed by another key", plant: func(t *testing.T, h *wakeHoldHarness, _ nostr.SecretKey, target string) {
			queueCommand(t, h, nostr.Generate(), target, "/clear", h.now)
		}},
		{name: "expired", reason: "expired while a dialog was open", plant: func(t *testing.T, h *wakeHoldHarness, owner nostr.SecretKey, target string) {
			queueCommand(t, h, owner, target, "/clear", h.now.Add(-wakecmd.Expiry-time.Minute))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWakeHoldHarness(t, wakeHoldPolicy{})
			owner, target := commandSeat(t, h)
			h.cfg.dialogSource = func(time.Time) (wakeDialogState, error) { return wakeDialogClosed, nil }
			tc.plant(t, h, owner, target)

			serveWakeCommands(h.cfg)
			if len(h.injected) > 0 {
				t.Fatalf("typed %q", h.injected)
			}
			entries, err := os.ReadDir(wakecmd.Dir(fsq.AgentBase(h.root, "codex")))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				id, isOut := strings.CutSuffix(e.Name(), wakecmd.OutSuffix)
				if !isOut {
					continue
				}
				state, reason, _ := wakecmd.Outcome(fsq.AgentBase(h.root, "codex"), id)
				if state != wakecmd.Refused || (tc.reason != "" && reason != tc.reason) {
					t.Fatalf("outcome = %q: %q, want refused %q", state, reason, tc.reason)
				}
				return
			}
			t.Fatal("the request was not answered")
		})
	}
}
