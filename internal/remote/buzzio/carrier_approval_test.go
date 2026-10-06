package buzzio

import (
	"context"
	"crypto/rand"
	"encoding/json"
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

// 611.42.7: the owner typed ✅ as a message; it became a new prompt and was
// refused busy while the approval waited (live, 2026-10-06). A typed yes or
// no answers the target's one pending approval, as the reaction does.
func TestApprovalAnsweredByTypedReply(t *testing.T) {
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cx", RelayHost: "relay", NativeSession: "thread-1"}
	ledger, _ := OpenLedger(t.TempDir())
	var ref string
	var answered *protocol.Command
	pendingID := ""
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), fixedIdentity("thread-1"), func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			s := protocol.Session{TargetID: "cx", Epoch: "e1"}
			if pendingID != "" {
				s.ActiveRequestRef, s.PendingInteraction = &ref, &pendingID
			}
			return s, nil
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
	dm := ownerEvent(t, owner, "dm-1", "touch a file", now)
	if err := c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	pending := &protocol.Interaction{InteractionID: "item-7", Kind: "approval", Prompt: "touch a\nin /repo", Options: []string{"accept", "cancel"}, RemoteAnswer: true, ApproveOption: "accept", RejectOption: "cancel"}
	now = now.Add(time.Second)
	if err := c.Publish(protocol.Snapshot{RequestRef: ref, Epoch: "e1", Revision: 2, State: protocol.StateRunning, Interaction: pending}, c.source(dm.ID.Hex(), "").Origin); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(context.Background(), func(context.Context, nostr.Event) error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	pendingID = "item-7"
	if err := c.Ingest(ownerEvent(t, owner, "dm-1", "yes", now)); err != nil {
		t.Fatal(err)
	}
	if answered == nil || answered.RequestRef != ref || answered.InteractionID != "item-7" || answered.Option != "accept" {
		t.Fatalf("respond command = %+v, want accept for item-7 of %s", answered, ref)
	}
}

// PR #919 review: the ledger outlives a share binding, and an approval
// posted under the old binding was answered by a reaction under the new
// one. Only the binding that submitted the request answers its approvals.
func TestApprovalNotAnsweredUnderAnotherBinding(t *testing.T) {
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cx", RelayHost: "relay", NativeSession: "thread-1"}
	ledger, _ := OpenLedger(t.TempDir())
	var ref string
	answered := false
	handle := func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "cx", Epoch: "e1"}, nil
		case protocol.OpRequestSubmit:
			ref = protocol.EncodeRef(src.Host, cmd.TargetID, cmd.RequestID)
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: ref, Revision: 1, State: protocol.StateRunning}}, nil
		case protocol.OpInteractionRespond:
			answered = true
		}
		return protocol.Reply{}, nil
	}
	grant := ownerGrant(t, owner, b.Body, KindDM, KindEdit)
	old := NewCarrier(ledger, b, body, grant, fixedIdentity("thread-1"), handle)
	now := time.Now()
	old.now = func() time.Time { return now }
	dm := ownerEvent(t, owner, "dm-1", "run the tests", now)
	if err := old.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	pending := &protocol.Interaction{InteractionID: "item-7", Kind: "approval", Prompt: "ls", Options: []string{"accept", "decline"}, RemoteAnswer: true, ApproveOption: "accept", RejectOption: "decline"}
	if err := old.Publish(protocol.Snapshot{RequestRef: ref, Epoch: "e1", Revision: 2, State: protocol.StateRunning, Interaction: pending}, old.source(dm.ID.Hex(), "").Origin); err != nil {
		t.Fatal(err)
	}
	posted, ok, err := ledger.Prepared(approvalKey(ref, "item-7"))
	if err != nil || !ok {
		t.Fatalf("setup: approval not prepared: %v", err)
	}
	var msg nostr.Event
	_ = json.Unmarshal(posted.Event, &msg)

	b.NativeSession = "thread-2"
	current := NewCarrier(ledger, b, body, grant, fixedIdentity("thread-2"), handle)
	current.now = old.now
	react := nostr.Event{CreatedAt: nostr.Timestamp(now.Unix()), Kind: KindReaction, Content: "✅", Tags: nostr.Tags{{"e", msg.ID.Hex()}}}
	if err := react.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if err := current.IngestReaction(react); err != nil {
		t.Fatal(err)
	}
	if answered {
		t.Fatal("an approval from the old binding was answered under the new one")
	}
}

// Pro review of #926 r2, 2026-09-30, #2: the fence saw the approved session,
// then the session switched before the epoch was read, and the submit went
// to the unshared session. The identity is checked again with the epoch
// fixed, and nothing is dispatched.
func TestSessionSwitchBeforeEpochDispatchesNothing(t *testing.T) {
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "pi-1", RelayHost: "relay", NativeSession: "sess-1"}
	ledger, _ := OpenLedger(t.TempDir())
	session := "sess-1"
	submitted := false
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), func(string) string { return session }, func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			session = "sess-2" // /new between the fence and the epoch
			return protocol.Session{TargetID: "pi-1", Epoch: "unpinned.gen-2"}, nil
		case protocol.OpRequestSubmit:
			submitted = true
		}
		return protocol.Reply{}, nil
	})
	if err := c.Ingest(ownerEvent(t, owner, "dm-1", "run the tests", time.Now())); err != nil {
		t.Fatal(err)
	}
	if submitted {
		t.Fatal("submitted into a session that was not approved for sharing")
	}
}

// Pro review of #926 r2, 2026-09-30, #4 and #5: a posted approval kept
// "React ✅" after it stopped taking a remote answer, and a share at
// submitted evidence said a request that had not reached the session had.
func TestApprovalMessageEditedWhenNoLongerAnswerable(t *testing.T) {
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "pi-1", RelayHost: "relay",
		NativeSession: "sess-1", MinEvidence: protocol.EvidenceSubmitted}
	ledger, _ := OpenLedger(t.TempDir())
	var ref string
	answered := false
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), fixedIdentity("sess-1"), func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "pi-1", Epoch: "e1"}, nil
		case protocol.OpRequestSubmit:
			ref = protocol.EncodeRef(src.Host, cmd.TargetID, cmd.RequestID)
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: ref, Revision: 1, State: protocol.StateReceived}}, nil
		case protocol.OpInteractionRespond:
			answered = true
		}
		return protocol.Reply{}, nil
	})
	now := time.Now()
	c.now = func() time.Time { return now }
	dm := ownerEvent(t, owner, "dm-1", "run the tests", now)
	if err := c.Ingest(dm); err != nil {
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
	if len(sent) != 1 || !strings.Contains(sent[0].Content, "a request counts once the session has it") || strings.Contains(sent[0].Content, "reached the session") {
		t.Fatalf("received row = %+v, want the policy and no delivery claim", sent)
	}
	origin := c.source(dm.ID.Hex(), "").Origin
	in := protocol.Interaction{InteractionID: "tool-1", Kind: "approval", Prompt: "go test ./...", Options: []string{"Allow once", "Block"}, RemoteAnswer: true, ApproveOption: "Allow once", RejectOption: "Block"}
	now = now.Add(time.Second)
	if err := c.Publish(protocol.Snapshot{RequestRef: ref, Epoch: "e1", Revision: 2, State: protocol.StateRunning, Interaction: &in}, origin); err != nil {
		t.Fatal(err)
	}
	flush()
	var msg nostr.Event
	for _, evt := range sent {
		if strings.Contains(evt.Content, "Approval needed") {
			msg = evt
		}
	}
	stale := protocol.Interaction{InteractionID: "tool-1", Kind: "approval", Prompt: "go test ./...", Options: []string{"Block"}, RejectOption: "Block"}
	now = now.Add(time.Second)
	if err := c.Publish(protocol.Snapshot{RequestRef: ref, Epoch: "e1", Revision: 3, State: protocol.StateRunning, Interaction: &stale}, origin); err != nil {
		t.Fatal(err)
	}
	flush()
	var edit nostr.Event
	for _, evt := range sent {
		if evt.Kind == KindEdit && tagValue(evt, "e") == msg.ID.Hex() {
			edit = evt
		}
	}
	if edit.Content == "" || strings.Contains(edit.Content, "React ✅") {
		t.Fatalf("edit = %q, want the approval without the ✅ instruction", edit.Content)
	}
	now = now.Add(time.Second)
	react := nostr.Event{CreatedAt: nostr.Timestamp(now.Unix()), Kind: KindReaction, Content: "✅", Tags: nostr.Tags{{"e", msg.ID.Hex()}}}
	if err := react.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if err := c.IngestReaction(react); err != nil || answered {
		t.Fatalf("reaction = %v, answered = %v; want nothing answered", err, answered)
	}
}

// Pro review of #926 r3, 2026-09-30, #2: a crash between preparing an
// approval edit and updating its mapping left them apart for good: the
// next revision matched the old mapping, so no corrective edit followed the
// queued one. The recorded edit is now finished before anything is compared.
func TestApprovalEditCrashLeavesMessageAndMappingInAgreement(t *testing.T) {
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "pi-1", RelayHost: "relay", NativeSession: "sess-1"}
	dir := t.TempDir()
	ledger, _ := OpenLedger(dir)
	var ref string
	handle := func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "pi-1", Epoch: "e1"}, nil
		case protocol.OpRequestSubmit:
			ref = protocol.EncodeRef(src.Host, cmd.TargetID, cmd.RequestID)
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: ref, Revision: 1, State: protocol.StateRunning}}, nil
		}
		return protocol.Reply{}, nil
	}
	grant := ownerGrant(t, owner, b.Body, KindDM, KindEdit)
	c := NewCarrier(ledger, b, body, grant, fixedIdentity("sess-1"), handle)
	now := time.Now()
	c.now = func() time.Time { return now }
	dm := ownerEvent(t, owner, "dm-1", "run the tests", now)
	if err := c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	origin := c.source(dm.ID.Hex(), "").Origin
	open := protocol.Interaction{InteractionID: "tool-1", Kind: "approval", Prompt: "go test ./...", Options: []string{"Allow once", "Block"}, RemoteAnswer: true, ApproveOption: "Allow once", RejectOption: "Block"}
	closed := protocol.Interaction{InteractionID: "tool-1", Kind: "approval", Prompt: "go test ./...", Options: []string{"Block"}, RejectOption: "Block"}
	publish := func(c *Carrier, rev int64, in protocol.Interaction) error {
		now = now.Add(time.Second)
		return c.Publish(protocol.Snapshot{RequestRef: ref, Epoch: "e1", Revision: rev, State: protocol.StateRunning, Interaction: &in}, origin)
	}
	if err := publish(c, 2, open); err != nil {
		t.Fatal(err)
	}
	approvalEditPrepared = func() { panic("crash after the approval edit was prepared") }
	func() {
		defer func() { approvalEditPrepared = func() {}; _ = recover() }()
		_ = publish(c, 3, closed)
	}()
	restarted := NewCarrier(ledger, b, body, grant, fixedIdentity("sess-1"), handle)
	restarted.now = func() time.Time { return now }
	if err := publish(restarted, 4, open); err != nil {
		t.Fatal(err)
	}
	posted, _, _ := ledger.Prepared(approvalKey(ref, "tool-1"))
	var msg nostr.Event
	_ = json.Unmarshal(posted.Event, &msg)
	var last nostr.Event
	_ = restarted.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error {
		if evt.Kind == KindEdit && tagValue(evt, "e") == msg.ID.Hex() && evt.CreatedAt >= last.CreatedAt {
			last = evt
		}
		return nil
	}, nil)
	appr, _, err := ledger.ApprovalFor(msg.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if shows := strings.Contains(last.Content, "React ✅"); appr.Disabled || appr.Pending != nil || !shows {
		t.Fatalf("mapping disabled=%v pending=%v, last edit %q; want both taking the ✅ answer", appr.Disabled, appr.Pending, last.Content)
	}
}

// Pro review of #929 r4, 2026-10-01, #4: a reaction replayed after its
// answer resolved delivery_unknown is told exactly that, never "already
// answered".
func TestApprovalReplayKeepsDeliveryUnknown(t *testing.T) {
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cc", RelayHost: "relay", NativeSession: "sess-1"}
	ledger, _ := OpenLedger(t.TempDir())
	var ref string
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), fixedIdentity("sess-1"), func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "cc", Epoch: "e1"}, nil
		case protocol.OpRequestSubmit:
			ref = protocol.EncodeRef(src.Host, cmd.TargetID, cmd.RequestID)
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: ref, Revision: 1, State: protocol.StateRunning}}, nil
		case protocol.OpInteractionRespond:
			resolved := []protocol.Resolution{{InteractionID: "cc-1", Outcome: protocol.ResolutionDeliveryUnknown, Option: "deny"}}
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: ref, Revision: 3, Resolved: resolved},
				Outcome: protocol.Outcome{Op: protocol.OpInteractionRespond, Code: protocol.CodeAlreadyResolved}}, nil
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
	pending := &protocol.Interaction{InteractionID: "cc-1", Kind: "approval", Prompt: "Bash command:\ngo test ./...", Options: []string{"deny"}, RemoteAnswer: true, RejectOption: "deny"}
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
	now = now.Add(time.Second)
	react := nostr.Event{CreatedAt: nostr.Timestamp(now.Unix()), Kind: KindReaction, Content: "❌", Tags: nostr.Tags{{"e", msg.ID.Hex()}}}
	if err := react.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if err := c.IngestReaction(react); err != nil {
		t.Fatal(err)
	}
	flush()
	reply := ""
	for _, evt := range sent {
		reply += evt.Content
	}
	if strings.Contains(reply, "already answered") || !strings.Contains(reply, "may not have reached the terminal") {
		t.Fatalf("sent = %q, want the unknown delivery, not an answer", reply)
	}
}
