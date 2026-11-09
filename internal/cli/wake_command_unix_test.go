//go:build darwin || linux

package cli

import (
	"encoding/json"
	"fmt"
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
	if err := dialog.WriteOwner(dir, dialog.Owner{PubKey: owner.Public().Hex(), Channel: commandChannel, Target: b.SessionIdentity()}); err != nil {
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

// Review of Buzz slash commands: a copy of a typed request under a new
// queue name carries the same signed message, and the wake types each
// signed message once.
func TestWakeTypesOneSignedCommandOnce(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	owner, target := commandSeat(t, h)
	h.cfg.dialogSource = func(time.Time) (wakeDialogState, error) { return wakeDialogClosed, nil }
	id := queueCommand(t, h, owner, target, "/clear", h.now)
	q := wakecmd.Dir(fsq.AgentBase(h.root, "codex"))
	raw, err := os.ReadFile(filepath.Join(q, id+wakecmd.ReqSuffix))
	if err != nil {
		t.Fatal(err)
	}
	serveWakeCommands(h.cfg)
	if len(h.injected) != 2 {
		t.Fatalf("first delivery typed %q", h.injected)
	}
	h.injected = nil
	replay := fmt.Sprintf("%020d-%s", h.now.Add(time.Second).UnixNano(), id[21:])
	if err := os.WriteFile(filepath.Join(q, replay+wakecmd.ReqSuffix), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	serveWakeCommands(h.cfg) // the tick with no dialog after a command
	serveWakeCommands(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("the same signed message was typed again: %q", h.injected)
	}
}

// Review of Buzz slash commands: a command whose Enter was held by a
// dialog gets its Enter only while it still verifies. Expired, or no
// longer the owner's, it gets no Enter and is answered Refused.
func TestWakeHeldCommandGetsNoEnterOnceItNoLongerVerifies(t *testing.T) {
	for _, change := range []string{"expired", "owner replaced"} {
		t.Run(change, func(t *testing.T) {
			h := newWakeHoldHarness(t, wakeHoldPolicy{})
			owner, target := commandSeat(t, h)
			open := false
			h.cfg.dialogSource = func(time.Time) (wakeDialogState, error) {
				if open {
					return wakeDialogOpen, nil
				}
				return wakeDialogClosed, nil
			}
			writes := 0
			h.cfg.beforeTerminalWrite = func() error {
				writes++
				if writes == 2 {
					open = true // the dialog opens between the text and its Enter
				}
				return nil
			}
			agentDir := fsq.AgentBase(h.root, "codex")
			id := queueCommand(t, h, owner, target, "/compact", h.now)
			serveWakeCommands(h.cfg)
			if state, _, _ := wakecmd.Outcome(agentDir, id); state != wakecmd.Queued || !slices.Equal(h.injected, []string{"/compact"}) {
				t.Fatalf("held: typed %q, outcome %q; want the text alone, queued", h.injected, state)
			}
			if change == "expired" {
				h.now = h.now.Add(wakecmd.Expiry + time.Minute)
			} else {
				dir, err := dialog.DirFor("seat")
				if err != nil {
					t.Fatal(err)
				}
				if err := dialog.WriteOwner(dir, dialog.Owner{PubKey: nostr.Generate().Public().Hex(), Channel: commandChannel, Target: target}); err != nil {
					t.Fatal(err)
				}
			}
			open = false
			serveWakeCommands(h.cfg)
			serveWakeCommands(h.cfg)
			if !slices.Equal(h.injected, []string{"/compact"}) {
				t.Fatalf("typed %q; want no Enter", h.injected)
			}
			if state, _, _ := wakecmd.Outcome(agentDir, id); state != wakecmd.Refused {
				t.Fatalf("outcome = %q, want refused", state)
			}
		})
	}
}

// Review of Buzz slash commands: a dialog that opens before the first byte
// of a command keeps it queued, and it is typed once the dialog closes.
func TestWakeCommandBlockedBeforeItsFirstByteStaysQueued(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	owner, target := commandSeat(t, h)
	open := false
	h.cfg.dialogSource = func(time.Time) (wakeDialogState, error) {
		if open {
			return wakeDialogOpen, nil
		}
		return wakeDialogClosed, nil
	}
	h.cfg.beforeTerminalWrite = func() error { open = true; return nil }
	agentDir := fsq.AgentBase(h.root, "codex")
	id := queueCommand(t, h, owner, target, "/compact", h.now)
	serveWakeCommands(h.cfg)
	if state, _, _ := wakecmd.Outcome(agentDir, id); state != wakecmd.Queued || len(h.injected) != 0 {
		t.Fatalf("blocked: typed %q, outcome %q; want nothing typed, queued", h.injected, state)
	}
	open = false
	h.cfg.beforeTerminalWrite = nil
	serveWakeCommands(h.cfg)
	if want := []string{"/compact", "\r"}; !slices.Equal(h.injected, want) {
		t.Fatalf("after the dialog closed: typed %q, want %q", h.injected, want)
	}
}
