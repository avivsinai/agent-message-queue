package buzzio

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// 611.42: a pending approval shows as its own message under the request's
// row; the owner's ✅ on that message answers exactly that interaction; the
// resolved revision edits the message with the outcome.
func TestApprovalMessageAnsweredByReaction(t *testing.T) {
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cx", RelayHost: "relay", NativeSession: "thread-1"}
	ledger, _ := OpenLedger(t.TempDir())
	var ref string
	var answered *protocol.Command
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), fixedIdentity("thread-1"), func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "cx", Epoch: "e1"}, nil
		case protocol.OpRequestSubmit:
			ref = protocol.EncodeRef(src.Host, cmd.TargetID, cmd.RequestID)
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: ref, Revision: 1, State: protocol.StateRunning}}, nil
		case protocol.OpInteractionRespond:
			answered = cmd
			return protocol.Reply{Outcome: protocol.Outcome{Op: protocol.OpInteractionRespond}}, nil
		}
		t.Fatalf("unexpected op %s", cmd.Op)
		return nil, nil
	})
	now := time.Now()
	c.now = func() time.Time { return now }
	dm := ownerEvent(t, owner, "dm-1", "run the tests", now)
	if err := c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	origin := c.source(dm.ID.Hex(), "").Origin
	pending := &protocol.Interaction{InteractionID: "item-7", Kind: "approval", Prompt: "go test ./...\nin /repo", Options: []string{"accept", "decline"}, RemoteAnswer: true, ApproveOption: "accept", RejectOption: "decline"}
	now = now.Add(time.Second)
	if err := c.Publish(protocol.Snapshot{RequestRef: ref, Epoch: "e1", Revision: 2, State: protocol.StateRunning, Interaction: pending}, origin); err != nil {
		t.Fatal(err)
	}
	var sent []nostr.Event
	flush := func() {
		sent = nil
		if err := c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error { sent = append(sent, evt); return nil }, nil); err != nil {
			t.Fatal(err)
		}
	}
	flush()
	var msg nostr.Event
	for _, evt := range sent {
		if evt.Kind == KindDM && strings.Contains(evt.Content, "Approval needed") {
			msg = evt
		}
	}
	if !strings.Contains(msg.Content, "go test ./...") || !strings.Contains(msg.Content, "React ✅") {
		t.Fatalf("approval message = %q, want the command and how to answer", msg.Content)
	}

	react := nostr.Event{CreatedAt: nostr.Timestamp(now.Unix()), Kind: KindReaction, Content: "✅", Tags: nostr.Tags{{"e", msg.ID.Hex()}}}
	if err := react.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if err := c.IngestReaction(react); err != nil {
		t.Fatal(err)
	}
	if answered == nil || answered.RequestRef != ref || answered.InteractionID != "item-7" || answered.Option != "accept" || answered.Epoch != "e1" || answered.TargetID != "cx" {
		t.Fatalf("respond command = %+v, want accept for item-7 of %s", answered, ref)
	}

	now = now.Add(time.Second)
	resolved := []protocol.Resolution{{InteractionID: "item-7", Outcome: protocol.ResolutionAnswered, Option: "accept"}}
	if err := c.Publish(protocol.Snapshot{RequestRef: ref, Epoch: "e1", Revision: 3, State: protocol.StateRunning, Resolved: resolved}, origin); err != nil {
		t.Fatal(err)
	}
	flush()
	var edit nostr.Event
	for _, evt := range sent {
		if evt.Kind == KindEdit && tagValue(evt, "e") == msg.ID.Hex() {
			edit = evt
		}
	}
	if !strings.Contains(edit.Content, "Approve was sent from Buzz") || strings.Contains(edit.Content, "React ✅") {
		t.Fatalf("outcome edit = %q, want the sent answer instead of the instructions", edit.Content)
	}
}
