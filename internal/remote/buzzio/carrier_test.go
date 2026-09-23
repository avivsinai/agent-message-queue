package buzzio

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
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
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cx", RelayHost: "relay", NativeSession: "thread-1"}
	ledger, err := OpenLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	handle := func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "cx", Epoch: "e1", NativeSessionID: "thread-1"}, nil
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
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), handle)
	now := time.Now()
	c.now = func() time.Time { return now }

	dm := ownerEvent(t, owner, "dm-1", "fix the build", now)
	if err := c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	if err := c.Ingest(dm); err != nil { // redelivery
		t.Fatal(err)
	}
	// A redelivered event is settled: the endpoint sees the request once.
	if len(ids) != 1 || ids[0] == "|" {
		t.Fatalf("submit identities = %v, want exactly one request", ids)
	}
	now = now.Add(2 * time.Second)
	origin := c.source(dm.ID.Hex(), "").Origin
	if err := c.Publish(protocol.Snapshot{RequestRef: "amqr1_ref", Revision: 2, State: protocol.StateCompleted, Result: &protocol.Result{Text: "done"}}, origin); err != nil {
		t.Fatal(err)
	}

	var sent []nostr.Event
	if err := c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error {
		sent = append(sent, evt)
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0].Kind != KindDM || sent[1].Kind != KindEdit {
		t.Fatalf("sent = %+v, want one row then one edit", sent)
	}
	if tagValue(sent[1], "e") != sent[0].ID.Hex() || sent[1].CreatedAt <= sent[0].CreatedAt {
		t.Fatalf("edit does not name its row or is not strictly later: %+v", sent[1])
	}
	// The row replies to the owner's input (h, p=owner, e input "reply"),
	// and both events carry the owner's grant for their kind.
	if tagValue(sent[0], "p") != b.Owner || !hasTag(sent[0], "e", dm.ID.Hex(), "", "reply") {
		t.Fatalf("row tags = %v, want p=owner and a reply to the input", sent[0].Tags)
	}
	// An owner reply inside a thread gets an answer naming that thread's
	// root as well as the input.
	threadRootID := sent[0].ID.Hex()
	inThread := nostr.Event{CreatedAt: nostr.Timestamp(now.Unix()), Kind: KindDM, Content: "/inspect", Tags: nostr.Tags{{"h", "dm-1"}, {"e", threadRootID, "", "root"}}}
	if err := inThread.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if err := c.Ingest(inThread); err != nil {
		t.Fatal(err)
	}
	var answer []nostr.Event
	_ = c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error { answer = append(answer, evt); return nil }, nil)
	if len(answer) != 1 || !hasTag(answer[0], "e", threadRootID, "", "root") || !hasTag(answer[0], "e", inThread.ID.Hex(), "", "reply") {
		t.Fatalf("threaded answer = %+v, want root and reply tags", answer)
	}
	for _, evt := range sent {
		if tagValue(evt, "auth") != b.Owner {
			t.Fatalf("kind %d tags = %v, want the owner's grant attached", evt.Kind, evt.Tags)
		}
	}
	if pending, _ := ledger.Pending(); len(pending) != 0 {
		t.Fatalf("pending after flush = %d, want 0", len(pending))
	}
}

// 611.16 slice 5: the owner reacting ❌ on this edge's result row cancels
// exactly that row's request.
func TestReactionOnRowCancelsItsRequest(t *testing.T) {
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cx", RelayHost: "relay", NativeSession: "thread-1"}
	ledger, _ := OpenLedger(t.TempDir())
	var cancelled string
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), func(cmd *protocol.Command, _ core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "cx", Epoch: "e1", NativeSessionID: "thread-1"}, nil
		case protocol.OpRequestSubmit:
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: "amqr1_x", Revision: 1, State: protocol.StateRunning}}, nil
		case protocol.OpRequestCancel:
			cancelled = cmd.RequestRef
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: cmd.RequestRef, Revision: 2, State: protocol.StateCancelled}}, nil
		}
		return nil, nil
	})
	now := time.Now()
	if err := c.Ingest(ownerEvent(t, owner, "dm-1", "long job", now)); err != nil {
		t.Fatal(err)
	}
	rc, _, _ := ledger.ReceiptFor("amqr1_x")
	if rc.RootEventID == "" {
		t.Fatal("setup: no result row prepared")
	}
	react := nostr.Event{CreatedAt: nostr.Timestamp(now.Unix()), Kind: KindReaction, Content: "❌", Tags: nostr.Tags{{"e", rc.RootEventID}}}
	if err := react.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if err := c.IngestReaction(react); err != nil {
		t.Fatal(err)
	}
	if cancelled != "amqr1_x" {
		t.Fatalf("cancelled = %q, want the reacted row's request", cancelled)
	}
}

// codex #866 r1 #3: a share enrolled without buzz-dm ran the owner's
// prompt and only then failed to sign the answer. Nothing is admitted
// unless every DM kind is granted.
func TestCarrierAdmitsNothingWithoutDMGrants(t *testing.T) {
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cx", RelayHost: "relay", NativeSession: "thread-1"}
	ledger, _ := OpenLedger(t.TempDir())
	calls := 0
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindEdit), func(*protocol.Command, core.Source) (any, error) { // no kind 9 grant
		calls++
		return protocol.Session{TargetID: "cx", Epoch: "e1", NativeSessionID: "thread-1"}, nil
	})
	if err := c.Ingest(ownerEvent(t, owner, "dm-1", "fix the build", time.Now())); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("ingest without a kind 9 grant: err=%v, want ErrNoGrant", err)
	}
	if pending, _ := ledger.Pending(); calls != 0 || len(pending) != 0 {
		t.Fatalf("endpoint calls = %d, owed outputs = %d; want nothing admitted or signed", calls, len(pending))
	}
}

// ownerGrant signs real owner grants for kinds, valid for an hour.
func ownerGrant(t *testing.T, owner [32]byte, bodyPub string, kinds ...uint16) Grant {
	t.Helper()
	tags := map[uint16]bodykey.AuthTag{}
	for _, k := range kinds {
		tag, err := bodykey.SignAuthTag(owner, bodyPub, bodykey.ShareConditions(k, time.Now().Add(time.Hour).Unix()))
		if err != nil {
			t.Fatal(err)
		}
		tags[k] = *tag
	}
	return func(kind uint16, at time.Time) (bodykey.AuthTag, error) {
		tag, ok := tags[kind]
		if !ok {
			return bodykey.AuthTag{}, errors.New("not granted")
		}
		return tag, tag.Satisfies(kind, at.Unix())
	}
}

func hasTag(evt nostr.Event, want ...string) bool {
	for _, tag := range evt.Tags {
		if len(tag) == len(want) {
			match := true
			for i := range want {
				match = match && tag[i] == want[i]
			}
			if match {
				return true
			}
		}
	}
	return false
}

// 611.16 slice 5: an owner message that mentions the body in an opted-in
// channel submits one request; the result row goes to the DM channel and
// carries no reference into the mentioning channel.
func TestMentionSubmitsAndAnswersInDM(t *testing.T) {
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cx", RelayHost: "relay", NativeSession: "thread-1", Mentions: map[string]bool{"team-1": true}}
	ledger, _ := OpenLedger(t.TempDir())
	var prompt string
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), func(cmd *protocol.Command, _ core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "cx", Epoch: "e1", NativeSessionID: "thread-1"}, nil
		case protocol.OpRequestSubmit:
			prompt = cmd.Input.Text
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: "amqr1_m", Revision: 1, State: protocol.StateRunning}}, nil
		}
		return nil, nil
	})
	mention := nostr.Event{CreatedAt: nostr.Now(), Kind: KindDM, Content: "nostr:npub1example fix the build", Tags: nostr.Tags{{"h", "team-1"}, {"p", b.Body}}}
	if err := mention.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if err := c.IngestMention(mention); err != nil {
		t.Fatal(err)
	}
	if prompt != "fix the build" {
		t.Fatalf("submitted prompt = %q, want the text after the mention", prompt)
	}
	var sent []nostr.Event
	_ = c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error { sent = append(sent, evt); return nil }, nil)
	if len(sent) != 1 || tagValue(sent[0], "h") != "dm-1" || tagValue(sent[0], "e") != "" {
		t.Fatalf("sent = %+v, want one DM row with no reference into the mention channel", sent)
	}
}
