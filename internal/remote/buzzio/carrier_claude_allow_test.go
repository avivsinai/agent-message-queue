//go:build !windows

package buzzio

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
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
	if !strings.Contains(e.msg.Content, "React ✅ to approve") || !strings.Contains(e.msg.Content, "Interaction: "+e.iid) {
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
// file alone never allows. Each row writes an allow answer whose evidence
// fails one check; the hook prints no allow. The first row is the control:
// the same write with valid evidence allows.
func TestClaudeForgedAllowEvidenceNeverAllows(t *testing.T) {
	cases := []struct {
		name    string
		pinned  bool
		command string
		allows  bool
		forge   func(e *claudeApprovalE2E) (reaction, message nostr.Event)
		// edits, when set, replaces the evidence's complete edit set.
		edits func(e *claudeApprovalE2E) []nostr.Event
		// fetchFails runs the ✅ through the carrier with an edit read
		// that fails, instead of writing the answer file.
		fetchFails bool
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
			other := e.message(e.msg.Content + " ")
			return e.reaction(e.owner, "✅", other.ID.Hex(), time.Now()), e.msg
		}},
		{name: "message for a different command", pinned: true, forge: func(e *claudeApprovalE2E) (nostr.Event, nostr.Event) {
			m := e.message(strings.Replace(e.msg.Content, "go test ./...", "go run ./cmd/evil", 1))
			return e.reaction(e.owner, "✅", m.ID.Hex(), time.Now()), m
		}},
		{name: "hidden command", pinned: true, command: "env MY_TOKEN=1 go test ./...", forge: func(e *claudeApprovalE2E) (nostr.Event, nostr.Event) {
			req := e.request()
			prompt := req["preview"].(string) + "\n\nInteraction: " + e.iid + "\nAction: " + req["action_hash"].(string)
			m := e.message(approvalText(Approval{RequestRef: e.ref, Prompt: prompt, ApproveOption: "allow", RejectOption: "deny"}, ""))
			return e.reaction(e.owner, "✅", m.ID.Hex(), time.Now()), m
		}},
		{name: "stale reaction", pinned: true, forge: func(e *claudeApprovalE2E) (nostr.Event, nostr.Event) {
			return e.reaction(e.owner, "✅", e.msg.ID.Hex(), time.Now().Add(-10*time.Minute)), e.msg
		}},
		{name: "reaction replayed from another interaction", pinned: true, forge: func(e *claudeApprovalE2E) (nostr.Event, nostr.Event) {
			m := e.message(strings.Replace(e.msg.Content, e.iid, "cc-"+strings.Repeat("0", 32), 1))
			return e.reaction(e.owner, "✅", m.ID.Hex(), time.Now()), m
		}},
		{name: "no owner pin", pinned: false, forge: func(e *claudeApprovalE2E) (nostr.Event, nostr.Event) {
			return e.reaction(e.owner, "✅", e.msg.ID.Hex(), time.Now()), e.msg
		}},
		// 611.42.4 edit-check: an edit that shows a harmless command while
		// the original holds the real one.
		{name: "edit changes the shown command", pinned: true, forge: e2eValid, edits: func(e *claudeApprovalE2E) []nostr.Event {
			return []nostr.Event{e.edit(strings.Replace(e.msg.Content, "go test ./...", "go vet ./...", 1))}
		}},
		// 611.42.4 edit-check: an edit of the trailer only still allows.
		{name: "edit changes only the trailer", pinned: true, allows: true, forge: e2eValid, edits: func(e *claudeApprovalE2E) []nostr.Event {
			head, _, _ := strings.Cut(e.msg.Content, "\n\nReact ")
			return []nostr.Event{e.edit(head + "\n\nReact ❌ to reject, or answer in the terminal. The first answer wins.")}
		}},
		// 611.42.4 edit-check: the edits cannot be read, so the evidence is
		// incomplete.
		{name: "edit fetch fails", pinned: true, fetchFails: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			command := tc.command
			if command == "" {
				command = "go test ./..."
			}
			e := newClaudeApprovalE2EWith(t, tc.pinned, command)
			if tc.fetchFails {
				e.c.SetEditFetcher(func(context.Context, string) ([]nostr.Event, error) { return nil, errors.New("relay closed") })
				if err := e.c.IngestReaction(e.reaction(e.owner, "✅", e.msg.ID.Hex(), e.advance())); err != nil {
					t.Fatal(err)
				}
			} else {
				r, m := tc.forge(e)
				var edits []nostr.Event
				if tc.edits != nil {
					edits = tc.edits(e)
				}
				e.writeAllow(ApproveEvidence{Reaction: r, Message: m, Edits: edits, EditsComplete: true})
			}
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

// reaction is a kind 7 by key with content on target, dated at.
func (e *claudeApprovalE2E) reaction(key [32]byte, content, target string, at time.Time) nostr.Event {
	e.t.Helper()
	r := nostr.Event{CreatedAt: nostr.Timestamp(at.Unix()), Kind: KindReaction, Content: content, Tags: nostr.Tags{{"e", target}}}
	if err := r.Sign(key); err != nil {
		e.t.Fatal(err)
	}
	return r
}

// message is a kind 9 with content signed by the body key, as the carrier
// signs its approval messages.
func (e *claudeApprovalE2E) message(content string) nostr.Event {
	e.t.Helper()
	m := nostr.Event{CreatedAt: e.msg.CreatedAt, Kind: KindDM, Tags: e.msg.Tags, Content: content}
	if err := m.Sign(e.body); err != nil {
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

// edit is a kind 40003 of the approval message with content, signed by the
// body key, as the carrier signs its edits.
func (e *claudeApprovalE2E) edit(content string) nostr.Event {
	e.t.Helper()
	ed := nostr.Event{CreatedAt: e.msg.CreatedAt + 1, Kind: KindEdit, Tags: e.c.editTags(e.msg.ID.Hex()), Content: content}
	if err := ed.Sign(e.body); err != nil {
		e.t.Fatal(err)
	}
	return ed
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
