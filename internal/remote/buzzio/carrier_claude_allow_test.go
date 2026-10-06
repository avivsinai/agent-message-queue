//go:build !windows

package buzzio

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

// allowDecision is the one allow the PermissionRequest hook prints.
const allowDecision = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`

// Bead 611.42.4: with the owner pinned on the hook's command line, the DM
// offers ✅ for a Bash call shown whole. Through the real carrier, endpoint
// and Claude attachment, with the hook called as a function, the owner's
// signed ✅ on the approval message makes the hook print allow, and the
// approval ends answered with allow.
func TestClaudeApprovalAllowedWithOwnerSignedReaction(t *testing.T) {
	e := newClaudeApprovalE2EWith(t, true, "go test ./...")
	if !strings.Contains(e.msg.Content, "React ✅ or reply yes to approve") || !strings.Contains(e.msg.Content, "Interaction: "+e.iid) {
		t.Fatalf("approval message = %q, want ✅ offered and the interaction shown", e.msg.Content)
	}
	if err := e.c.IngestReaction(e.reaction(e.owner, "✅", e.msg.ID.Hex(), e.advance())); err != nil {
		t.Fatal(err)
	}
	e.hookExited()
	if got := strings.TrimSpace(e.out.String()); got != allowDecision {
		t.Fatalf("hook printed %q, want %s", got, allowDecision)
	}
	if res := e.resolved(); res["outcome"] != "hook_claimed" || res["option"] != "allow" {
		t.Fatalf("resolved = %v, want the hook's claim for allow", res)
	}
	e.await(approvalKey(e.ref, e.iid) + "/outcome")
	e.flush()
	if !e.edited("Approve was sent from Buzz") {
		t.Fatalf("sent = %+v, want the approval message edited with the sent approve", e.sent)
	}
}

// Bead 611.42.4: a same-user process can write any answer file, so the
// file alone never allows. Each row writes an allow answer whose proof
// fails one check, or changes the relay's history; the hook prints no
// allow. The first row is the control: the same write with valid evidence
// allows. The history rows (611.42.4 edit-check) put the edit or deletion
// on the relay, where the hook reads it itself.
func TestClaudeForgedAllowEvidenceNeverAllows(t *testing.T) {
	cases := []struct {
		name    string
		pinned  bool
		command string
		allows  bool
		forge   func(e *claudeApprovalE2E) (reaction, message nostr.Event)
		// history, when set, publishes events on the relay first.
		history func(e *claudeApprovalE2E) []nostr.Event
	}{
		{name: "valid owner reaction (control)", pinned: true, allows: true, forge: e2eValid},
		{name: "unsigned", pinned: true, forge: func(e *claudeApprovalE2E) (nostr.Event, nostr.Event) {
			r := e.reaction(e.owner, "✅", e.msg.ID.Hex(), time.Now())
			r.Sig = [64]byte{}
			return r, e.msg
		}},
		{name: "signed by another key", pinned: true, forge: func(e *claudeApprovalE2E) (nostr.Event, nostr.Event) {
			return e.reaction(randomKey(), "✅", e.msg.ID.Hex(), time.Now()), e.msg
		}},
		{name: "wrong e tag", pinned: true, forge: func(e *claudeApprovalE2E) (nostr.Event, nostr.Event) {
			other := e.message(e.body, e.msg.Tags, e.msg.Content+" ")
			return e.reaction(e.owner, "✅", other.ID.Hex(), time.Now()), e.msg
		}},
		{name: "message for a different command", pinned: true, forge: func(e *claudeApprovalE2E) (nostr.Event, nostr.Event) {
			m := e.message(e.body, e.msg.Tags, strings.Replace(e.msg.Content, "go test ./...", "go run ./cmd/evil", 1))
			return e.reaction(e.owner, "✅", m.ID.Hex(), time.Now()), m
		}},
		{name: "hidden command", pinned: true, command: "env MY_TOKEN=1 go test ./...", forge: func(e *claudeApprovalE2E) (nostr.Event, nostr.Event) {
			req := e.request()
			prompt := req["preview"].(string) + "\n\nInteraction: " + e.iid + "\nAction: " + req["action_hash"].(string)
			m := e.message(e.body, e.msg.Tags, approvalText(Approval{RequestRef: e.ref, Prompt: prompt, ApproveOption: "allow", RejectOption: "deny"}, ""))
			return e.reaction(e.owner, "✅", m.ID.Hex(), time.Now()), m
		}},
		{name: "stale reaction", pinned: true, forge: func(e *claudeApprovalE2E) (nostr.Event, nostr.Event) {
			return e.reaction(e.owner, "✅", e.msg.ID.Hex(), time.Now().Add(-10*time.Minute)), e.msg
		}},
		{name: "reaction replayed from another interaction", pinned: true, forge: func(e *claudeApprovalE2E) (nostr.Event, nostr.Event) {
			m := e.message(e.body, e.msg.Tags, strings.Replace(e.msg.Content, e.iid, "cc-"+strings.Repeat("0", 32), 1))
			return e.reaction(e.owner, "✅", m.ID.Hex(), time.Now()), m
		}},
		{name: "no owner pin", pinned: false, forge: e2eValid},
		// Pro review of #936, P1 (611.42.4): a real signed pair from another
		// share, or another channel, moved into this session's answer file.
		{name: "message from another share's body", pinned: true, forge: func(e *claudeApprovalE2E) (nostr.Event, nostr.Event) {
			m := e.message(randomKey(), e.msg.Tags, e.msg.Content)
			return e.reaction(e.owner, "✅", m.ID.Hex(), time.Now()), m
		}},
		{name: "message in another channel", pinned: true, forge: func(e *claudeApprovalE2E) (nostr.Event, nostr.Event) {
			m := e.message(e.body, nostr.Tags{{"h", "dm-2"}, {"p", nostr.GetPublicKey(e.owner).Hex()}}, e.msg.Content)
			return e.reaction(e.owner, "✅", m.ID.Hex(), time.Now()), m
		}},
		// 611.42.4 edit-check: an edit on the relay shows another command.
		{name: "edit changes the shown command", pinned: true, forge: e2eValid, history: func(e *claudeApprovalE2E) []nostr.Event {
			return []nostr.Event{e.edit(e.body, strings.Replace(e.msg.Content, "go test ./...", "go vet ./...", 1))}
		}},
		// 611.42.4 edit-check: an edit of the trailer only still allows.
		{name: "edit changes only the trailer", pinned: true, allows: true, forge: e2eValid, history: func(e *claudeApprovalE2E) []nostr.Event {
			return []nostr.Event{e.edit(e.body, e.trailerEdit())}
		}},
		// 611.42.4 edit-check: the relay hides a deleted edit, so any
		// deletion in the channel since the message refuses.
		{name: "deletion after the message", pinned: true, forge: e2eValid, history: func(e *claudeApprovalE2E) []nostr.Event {
			return []nostr.Event{e.deletion(e.body, KindDeletion, nostr.Tags{{"h", "dm-1"}, {"e", strings.Repeat("0", 64)}})}
		}},
		// Review of #936 r3, P1: the body deletes a hidden edit with a
		// created_at before the message; a time-bounded read missed it.
		{name: "backdated body deletion", pinned: true, forge: e2eValid, history: func(e *claudeApprovalE2E) []nostr.Event {
			d := nostr.Event{CreatedAt: e.msg.CreatedAt - 60, Kind: KindDeletion, Tags: nostr.Tags{{"e", strings.Repeat("1", 64)}}}
			if err := d.Sign(e.body); err != nil {
				e.t.Fatal(err)
			}
			return []nostr.Event{d}
		}},
		// Review of #936 r4, P2: the relay returns its newest page of edits,
		// so a full page may have cut an older edit that shows another call.
		{name: "a full page of edits", pinned: true, forge: e2eValid, history: func(e *claudeApprovalE2E) []nostr.Event {
			var out []nostr.Event
			for i := range maxHistoryEdits {
				ed := nostr.Event{CreatedAt: e.msg.CreatedAt + 1 + nostr.Timestamp(i), Kind: KindEdit, Tags: e.c.editTags(e.msg.ID.Hex()), Content: e.trailerEdit()}
				if err := ed.Sign(e.body); err != nil {
					e.t.Fatal(err)
				}
				out = append(out, ed)
			}
			return out
		}},
		// Pro review of #936 r2, P1: Buzz lets the owner edit the agent's
		// message.
		{name: "owner edit changes the shown command", pinned: true, forge: e2eValid, history: func(e *claudeApprovalE2E) []nostr.Event {
			return []nostr.Event{e.edit(e.owner, strings.Replace(e.msg.Content, "go test ./...", "go vet ./...", 1))}
		}},
		// Pro review of #936 r2, P1: a deletion with only an e tag, of an
		// edit, as the mobile client sends it.
		{name: "e-only deletion of an edit", pinned: true, forge: e2eValid, history: func(e *claudeApprovalE2E) []nostr.Event {
			ed := e.edit(e.body, e.trailerEdit())
			return []nostr.Event{ed, e.deletion(e.owner, KindDeletion, nostr.Tags{{"e", ed.ID.Hex()}})}
		}},
		// Pro review of #936 r2, P1: the Buzz-native delete-event hides the
		// message like kind 5.
		{name: "Buzz-native deletion of the message", pinned: true, forge: e2eValid, history: func(e *claudeApprovalE2E) []nostr.Event {
			return []nostr.Event{e.deletion(e.owner, KindGroupDeletion, nostr.Tags{{"h", "dm-1"}, {"e", e.msg.ID.Hex()}})}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			command := tc.command
			if command == "" {
				command = "go test ./..."
			}
			e := newClaudeApprovalE2EWith(t, tc.pinned, command)
			if tc.history != nil {
				for _, evt := range tc.history(e) {
					e.relay.Inject(evt)
				}
			}
			r, m := tc.forge(e)
			e.writeAllow(ApproveEvidence{Reaction: r, Message: m})
			if tc.allows {
				e.hookExited()
				if got := strings.TrimSpace(e.out.String()); got != allowDecision {
					t.Fatalf("hook printed %q, want allow", got)
				}
				return
			}
			for deadline := time.Now().Add(4 * time.Second); !strings.Contains(e.errs.String(), "ignored"); time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the hook never read the forged answer")
				}
			}
			close(e.done) // the terminal decides
			e.hookExited()
			if e.out.Len() != 0 {
				t.Fatalf("hook printed %q, want no decision", e.out.String())
			}
		})
	}
}

// Pro review of #936, P2 (611.42.4): a failed verification consumed the
// Buzz answer. Now a ✅ whose history cannot be read is refused before
// anything is answered: the DM says so, and a later ✅ allows.
func TestClaudeAllowRetriesAfterAFailedVerification(t *testing.T) {
	e := newClaudeApprovalE2EWith(t, true, "go test ./...")
	e.relayDown.Store(true)
	if err := e.c.IngestReaction(e.reaction(e.owner, "✅", e.msg.ID.Hex(), e.advance())); err != nil {
		t.Fatal(err)
	}
	e.flush()
	if !e.replied("Could not verify the approval: ") || !e.replied("React again to retry.") {
		t.Fatalf("sent = %+v, want the retry reply", e.sent)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "answers", e.iid+".json")); !os.IsNotExist(err) {
		t.Fatalf("answer file: %v, want none after a failed verification", err)
	}
	e.relayDown.Store(false)
	if err := e.c.IngestReaction(e.reaction(e.owner, "✅", e.msg.ID.Hex(), e.advance())); err != nil {
		t.Fatal(err)
	}
	e.hookExited()
	if got := strings.TrimSpace(e.out.String()); got != allowDecision {
		t.Fatalf("hook printed %q, want allow", got)
	}
}

// Pro review of #936 r2, P2: the endpoint verified an allow, then the
// hook's own verification failed, and a ❌ could no longer land. The hook
// retires exactly that proof, so the ❌ that follows denies.
func TestClaudeDenyLandsAfterTheHookRefusesAnAllow(t *testing.T) {
	e := newClaudeApprovalE2EWith(t, true, "go test ./...")
	e.hookDown.Store(true)
	if err := e.c.IngestReaction(e.reaction(e.owner, "✅", e.msg.ID.Hex(), e.advance())); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(4 * time.Second); !strings.Contains(e.errs.String(), "ignored a Buzz allow"); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the hook never refused the allow")
		}
	}
	if err := e.c.IngestReaction(e.reaction(e.owner, "❌", e.msg.ID.Hex(), e.advance())); err != nil {
		t.Fatal(err)
	}
	e.hookExited()
	if got := strings.TrimSpace(e.out.String()); !strings.Contains(got, `"behavior":"deny"`) {
		t.Fatalf("hook printed %q, want deny", got)
	}
}

// 611.42.4 edit-check: a ✅ on an altered approval message is refused with
// the altered reply, nothing is answered, and ❌ still blocks the call.
func TestClaudeAlteredApprovalRefusesAllowAndKeepsReject(t *testing.T) {
	e := newClaudeApprovalE2EWith(t, true, "go test ./...")
	e.relay.Inject(e.deletion(e.body, KindDeletion, nostr.Tags{{"h", "dm-1"}, {"e", strings.Repeat("0", 64)}}))
	if err := e.c.IngestReaction(e.reaction(e.owner, "✅", e.msg.ID.Hex(), e.advance())); err != nil {
		t.Fatal(err)
	}
	e.flush()
	if !e.replied("The approval message was altered after it was posted; check the terminal.") {
		t.Fatalf("sent = %+v, want the altered reply", e.sent)
	}
	if err := e.c.IngestReaction(e.reaction(e.owner, "❌", e.msg.ID.Hex(), e.advance())); err != nil {
		t.Fatal(err)
	}
	e.hookExited()
	if got := strings.TrimSpace(e.out.String()); !strings.Contains(got, `"behavior":"deny"`) {
		t.Fatalf("hook printed %q, want deny", got)
	}
}

// reaction is a kind 7 by key with content on target, dated at.
func (e *claudeApprovalE2E) reaction(key [32]byte, content, target string, at time.Time) nostr.Event {
	e.t.Helper()
	r := nostr.Event{CreatedAt: nostr.Timestamp(at.Unix()), Kind: KindReaction, Content: content, Tags: nostr.Tags{{"e", target}}}
	if err := r.Sign(key); err != nil {
		e.t.Fatal(err)
	}
	return r
}

// message is a kind 9 with tags and content signed by key, as the carrier
// signs its approval messages with the body key.
func (e *claudeApprovalE2E) message(key [32]byte, tags nostr.Tags, content string) nostr.Event {
	e.t.Helper()
	m := nostr.Event{CreatedAt: e.msg.CreatedAt, Kind: KindDM, Tags: tags, Content: content}
	if err := m.Sign(key); err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *claudeApprovalE2E) request() map[string]any {
	e.t.Helper()
	var req map[string]any
	if data, err := os.ReadFile(filepath.Join(e.dir, "requests", e.iid+".json")); err != nil || json.Unmarshal(data, &req) != nil {
		e.t.Fatalf("request = %v (%v)", req, err)
	}
	return req
}

// e2eValid is the owner's ✅ on the posted approval message.
func e2eValid(e *claudeApprovalE2E) (nostr.Event, nostr.Event) {
	return e.reaction(e.owner, "✅", e.msg.ID.Hex(), time.Now()), e.msg
}

// edit is a kind 40003 of the approval message with content, signed by
// key: the body key, as the carrier signs its edits, or the owner's.
func (e *claudeApprovalE2E) edit(key [32]byte, content string) nostr.Event {
	e.t.Helper()
	ed := nostr.Event{CreatedAt: e.msg.CreatedAt + 1, Kind: KindEdit, Tags: e.c.editTags(e.msg.ID.Hex()), Content: content}
	if err := ed.Sign(key); err != nil {
		e.t.Fatal(err)
	}
	return ed
}

// deletion is a deletion of kind with tags, signed by key, after the
// message.
func (e *claudeApprovalE2E) deletion(key [32]byte, kind nostr.Kind, tags nostr.Tags) nostr.Event {
	e.t.Helper()
	d := nostr.Event{CreatedAt: e.msg.CreatedAt + 1, Kind: kind, Tags: tags}
	if err := d.Sign(key); err != nil {
		e.t.Fatal(err)
	}
	return d
}

// trailerEdit is the approval message with only its trailer changed.
func (e *claudeApprovalE2E) trailerEdit() string {
	head, _, _ := strings.Cut(e.msg.Content, "\n\nReact ")
	return head + "\n\nReact ❌ or reply no to reject, or answer in the terminal. The first answer wins."
}

// replied reports a sent DM row that contains text.
func (e *claudeApprovalE2E) replied(text string) bool {
	for _, evt := range e.sent {
		if evt.Kind == KindDM && strings.Contains(evt.Content, text) {
			return true
		}
	}
	return false
}

// writeAllow writes the allow answer a same-user process could write, with
// ev as its evidence.
func (e *claudeApprovalE2E) writeAllow(ev ApproveEvidence) {
	e.t.Helper()
	evidence, _ := json.Marshal(ev)
	ans, _ := json.Marshal(map[string]any{"interaction_id": e.iid, "action_hash": e.request()["action_hash"], "option": "allow", "at": "x",
		"evidence": json.RawMessage(evidence)})
	if err := os.MkdirAll(filepath.Join(e.dir, "answers"), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "answers", e.iid+".json"), ans, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func randomKey() [32]byte {
	var k [32]byte
	_, _ = rand.Read(k[:])
	return k
}
