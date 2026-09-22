package buzzio

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// 611.16: an owner DM submits one request (admitted floor, busy=reject),
// its first revision becomes one kind 9 row and a later revision a kind
// 40003 edit of that row; a redelivered event replays the same request;
// Flush sends every owed output and leaves none.
func TestCarrierSubmitsOnceAndKeepsOneEditableRow(t *testing.T) {
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cx", RelayHost: "relay"}
	ledger, err := OpenLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	handle := func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "cx", Epoch: "e1"}, nil
		case protocol.OpRequestSubmit:
			ids = append(ids, cmd.RequestID+"|"+cmd.Epoch)
			if cmd.Input.MinEvidence != string(protocol.EvidenceAdmitted) || cmd.Input.Busy != protocol.BusyReject || src.Origin["carrier"] != "buzz" {
				t.Fatalf("submit command = %+v origin = %v", cmd, src.Origin)
			}
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: "amqr1_ref", Revision: 1, State: protocol.StateRunning}}, nil
		}
		t.Fatalf("unexpected op %s", cmd.Op)
		return nil, nil
	}
	c := NewCarrier(ledger, b, body, handle)
	now := time.Now()
	c.now = func() time.Time { return now }

	dm := ownerEvent(t, owner, "dm-1", "fix the build", now)
	if err := c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	if err := c.Ingest(dm); err != nil { // redelivery
		t.Fatal(err)
	}
	// A redelivered event replays its stored claim: the endpoint sees the
	// identical request (same id, same epoch) and its dispatch record keeps
	// it at one native submit.
	if len(ids) != 2 || ids[0] != ids[1] || ids[0] == "|" {
		t.Fatalf("submit identities = %v, want one identical request both times", ids)
	}
	now = now.Add(2 * time.Second)
	origin := c.source(dm.ID.Hex()).Origin
	if err := c.Publish(protocol.Snapshot{RequestRef: "amqr1_ref", Revision: 2, State: protocol.StateCompleted, Result: &protocol.Result{Text: "done"}}, origin); err != nil {
		t.Fatal(err)
	}

	var sent []nostr.Event
	if err := c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error {
		sent = append(sent, evt)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0].Kind != KindDM || sent[1].Kind != KindEdit {
		t.Fatalf("sent = %+v, want one row then one edit", sent)
	}
	if tagValue(sent[1], "e") != sent[0].ID.Hex() || sent[1].CreatedAt <= sent[0].CreatedAt {
		t.Fatalf("edit does not name its row or is not strictly later: %+v", sent[1])
	}
	if pending, _ := ledger.Pending(); len(pending) != 0 {
		t.Fatalf("pending after flush = %d, want 0", len(pending))
	}
}
