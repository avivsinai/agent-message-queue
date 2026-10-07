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

// Review of #972 r2: adapter refusal and error fragments reach the owner's
// DM through relay /status answers and approval-failure answers. The DM
// renderer decodes HTML entities and autolinks URLs in plain text, so every
// such fragment must sit inside an inert code span — asserted here as a
// backtick-wrapped fragment, with no parser dependency.

// The /status site: a refused request.get carries the adapter's error into
// the DM inside a code span.
func TestStatusFailureRendersTheErrorInert(t *testing.T) {
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cx", RelayHost: "relay", NativeSession: "thread-1"}
	ledger, _ := OpenLedger(t.TempDir())
	var ref string
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), fixedIdentity("thread-1"), func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "cx", Epoch: "e1"}, nil
		case protocol.OpRequestSubmit:
			ref = protocol.EncodeRef(src.Host, cmd.TargetID, cmd.RequestID)
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: ref, Revision: 1, State: protocol.StateRunning}}, nil
		case protocol.OpRequestGet:
			return nil, &protocol.Refusal{Code: protocol.CodeNativeError, Message: "refused: see &#x202E;this&#x202C; and https://evil.example"}
		}
		t.Fatalf("unexpected op %s", cmd.Op)
		return nil, nil
	})
	now := time.Now()
	c.now = func() time.Time { return now }
	if err := c.Ingest(ownerEvent(t, owner, "dm-1", "long job", now)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := c.Ingest(ownerEvent(t, owner, "dm-1", "/status "+ref, now)); err != nil {
		t.Fatal(err)
	}
	var sent []nostr.Event
	if err := c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error { sent = append(sent, evt); return nil }, nil, nil); err != nil {
		t.Fatal(err)
	}
	var answer string
	for _, evt := range sent {
		if strings.Contains(evt.Content, "Not sent") || strings.Contains(evt.Content, "failed") {
			answer = evt.Content
		}
	}
	if answer == "" {
		t.Fatalf("sent = %+v, want a refusal answer", sent)
	}
	if !strings.Contains(answer, "`native_error: refused: see &#x202E;this&#x202C; and https://evil.example`") {
		t.Fatalf("status failure did not render the adapter error as an inert code span: %q", answer)
	}
}

// The approval-failure site: a refused interaction response carries the
// refusal's message into the DM inside a code span.
func TestApprovalFailureRendersTheRefusalInert(t *testing.T) {
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cx", RelayHost: "relay", NativeSession: "thread-1"}
	ledger, _ := OpenLedger(t.TempDir())
	var ref string
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), fixedIdentity("thread-1"), func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "cx", Epoch: "e1"}, nil
		case protocol.OpRequestSubmit:
			ref = protocol.EncodeRef(src.Host, cmd.TargetID, cmd.RequestID)
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: ref, Revision: 1, State: protocol.StateRunning}}, nil
		case protocol.OpInteractionRespond:
			return nil, &protocol.Refusal{Code: protocol.CodeInvalid, Message: "declined: harness says &#x202E;no&#x202C; — see https://evil.example"}
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
	pending := &protocol.Interaction{InteractionID: "item-7", Kind: "approval", Prompt: "go test ./...", Options: []string{"accept", "decline"}, RemoteAnswer: true, ApproveOption: "accept", RejectOption: "decline"}
	now = now.Add(time.Second)
	if err := c.Publish(protocol.Snapshot{RequestRef: ref, Epoch: "e1", Revision: 2, State: protocol.StateRunning, Interaction: pending}, origin); err != nil {
		t.Fatal(err)
	}
	var sent []nostr.Event
	flush := func() {
		sent = nil
		if err := c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error { sent = append(sent, evt); return nil }, nil, nil); err != nil {
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
	react := nostr.Event{CreatedAt: nostr.Timestamp(now.Unix()), Kind: KindReaction, Content: "✅", Tags: nostr.Tags{{"e", msg.ID.Hex()}}}
	if err := react.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if err := c.IngestReaction(react); err != nil {
		t.Fatal(err)
	}
	flush()
	var answer string
	for _, evt := range sent {
		if strings.Contains(evt.Content, "Not sent") || strings.Contains(evt.Content, "declined") {
			answer = evt.Content
		}
	}
	if answer == "" {
		t.Fatalf("sent = %+v, want an approval-failure answer", sent)
	}
	if !strings.Contains(answer, "`declined: harness says &#x202E;no&#x202C; — see https://evil.example`") {
		t.Fatalf("approval failure did not render the refusal as an inert code span: %q", answer)
	}
}
