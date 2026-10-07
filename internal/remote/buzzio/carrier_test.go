package buzzio

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
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
	var ref string
	handle := func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "cx", Epoch: "e1"}, nil
		case protocol.OpRequestSubmit:
			ids = append(ids, cmd.RequestID+"|"+cmd.Epoch)
			if cmd.Input.MinEvidence != string(protocol.EvidenceAdmitted) || cmd.Input.Busy != protocol.BusyReject || src.Origin["carrier"] != "buzz" {
				t.Fatalf("submit command = %+v origin = %v", cmd, src.Origin)
			}
			ref = protocol.EncodeRef(src.Host, cmd.TargetID, cmd.RequestID)
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: ref, TargetID: cmd.TargetID, Revision: 1, State: protocol.StateRunning}}, nil
		}
		t.Fatalf("unexpected op %s", cmd.Op)
		return nil, nil
	}
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), fixedIdentity("thread-1"), handle)
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
	if err := c.Publish(protocol.Snapshot{RequestRef: ref, TargetID: "cx", Revision: 2, State: protocol.StateCompleted, Result: &protocol.Result{Text: "done"}}, origin); err != nil {
		t.Fatal(err)
	}

	var sent []nostr.Event
	if err := c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error {
		sent = append(sent, evt)
		return nil
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0].Kind != KindDM || sent[1].Kind != KindEdit {
		t.Fatalf("sent = %+v, want one row then one edit", sent)
	}
	if tagValue(sent[1], "e") != sent[0].ID.Hex() || sent[1].CreatedAt <= sent[0].CreatedAt {
		t.Fatalf("edit does not name its row or is not strictly later: %+v", sent[1])
	}
	// 611.52 (field, 0.86.1): the row named the opaque request ref; it
	// names the target and the state.
	if sent[0].Content != "cx: running" || sent[1].Content != "cx: completed\n\ndone" {
		t.Fatalf("row texts = %q, %q", sent[0].Content, sent[1].Content)
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
	_ = c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error { answer = append(answer, evt); return nil }, nil, nil)
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
	var ref string
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), fixedIdentity("thread-1"), func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "cx", Epoch: "e1"}, nil
		case protocol.OpRequestSubmit:
			ref = protocol.EncodeRef(src.Host, cmd.TargetID, cmd.RequestID)
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: ref, Revision: 1, State: protocol.StateRunning}}, nil
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
	rc, _, _ := ledger.ReceiptFor(ref)
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
	if cancelled != ref {
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
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindEdit), fixedIdentity("thread-1"), func(*protocol.Command, core.Source) (any, error) { // no kind 9 grant
		calls++
		return protocol.Session{TargetID: "cx", Epoch: "e1"}, nil
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
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), fixedIdentity("thread-1"), func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "cx", Epoch: "e1"}, nil
		case protocol.OpRequestSubmit:
			prompt = cmd.Input.Text
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: protocol.EncodeRef(src.Host, cmd.TargetID, cmd.RequestID), Revision: 1, State: protocol.StateRunning}}, nil
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
	_ = c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error { sent = append(sent, evt); return nil }, nil, nil)
	if len(sent) != 1 || tagValue(sent[0], "h") != "dm-1" || tagValue(sent[0], "e") != "" {
		t.Fatalf("sent = %+v, want one DM row with no reference into the mention channel", sent)
	}
}

// fixedIdentity is a native session accessor that always reports id.
func fixedIdentity(id string) func(string) string { return func(string) string { return id } }

// busyCarrier is a carrier whose target refuses every submit as busy, the
// endpoint answer behind the rows of agent-message-queue-611.58.
func busyCarrier(t *testing.T) (c *Carrier, owner [32]byte, stateDir string) {
	t.Helper()
	var body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cx", RelayHost: "relay", NativeSession: "thread-1"}
	stateDir = t.TempDir()
	ledger, err := OpenLedger(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	c = NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), fixedIdentity("thread-1"), func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "cx", Epoch: "e1"}, nil
		case protocol.OpRequestSubmit:
			ref := protocol.EncodeRef(src.Host, cmd.TargetID, cmd.RequestID)
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: ref, TargetID: cmd.TargetID, Revision: 1, State: protocol.StateRejected, Code: protocol.CodeBusy}}, nil
		}
		return nil, nil
	})
	return c, owner, stateDir
}

// agent-message-queue-611.58 (field, Buzz relay 2026-10-06): Buzz tags an
// owner's direct reply to a thread's first message with a reply e tag only.
// The result row named the owner event but no root, and the relay refused
// it: "invalid: root tag does not match thread ancestry". Pro review of #971
// added the legacy positional forms, which named no root either.
func TestRowForThreadReplyNamesTheThreadRoot(t *testing.T) {
	root, parent := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	for name, tags := range map[string]nostr.Tags{
		"reply-only to the root": {{"h", "dm-1"}, {"e", root, "", "reply"}},
		"positional, root only":  {{"h", "dm-1"}, {"e", root}},
		"positional, root first": {{"h", "dm-1"}, {"e", root}, {"e", parent}},
	} {
		t.Run(name, func(t *testing.T) {
			c, owner, _ := busyCarrier(t)
			reply := nostr.Event{CreatedAt: nostr.Now(), Kind: KindDM, Content: "yes", Tags: tags}
			if err := reply.Sign(owner); err != nil {
				t.Fatal(err)
			}
			if err := c.Ingest(reply); err != nil {
				t.Fatal(err)
			}
			var sent []nostr.Event
			if err := c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error { sent = append(sent, evt); return nil }, nil, nil); err != nil {
				t.Fatal(err)
			}
			if len(sent) != 1 || !hasTag(sent[0], "e", root, "", "root") || !hasTag(sent[0], "e", reply.ID.Hex(), "", "reply") {
				t.Fatalf("sent = %+v, want one row naming the thread root and replying to the owner event", sent)
			}
		})
	}
}

// agent-message-queue-611.58 (field, Buzz relay 2026-10-06): Flush stopped
// at the first refused row, so every later row, a new approval message
// included, never reached the owner. A refused row stays owed with the
// relay's reason: other requests' rows still go out, its own edit waits
// behind it, and both are sent in order once its backoff has passed.
func TestRefusedRowDoesNotBlockLaterRows(t *testing.T) {
	c, owner, stateDir := busyCarrier(t)
	now := time.Now()
	c.now = func() time.Time { return now }
	for i, text := range []string{"first", "second"} {
		if err := c.Ingest(ownerEvent(t, owner, "dm-1", text, now.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	const reason = "invalid: root tag does not match thread ancestry"
	var sent []nostr.Event
	refuseFirst := func(_ context.Context, evt nostr.Event) error {
		sent = append(sent, evt)
		if len(sent) == 1 {
			return &relay.RemoteError{Kind: relay.ErrRejected, Reason: reason}
		}
		return nil
	}
	if err := c.Flush(context.Background(), refuseFirst, nil, nil); err != nil || len(sent) != 2 {
		t.Fatalf("flush err = %v after %d sends, want both requests' rows tried", err, len(sent))
	}
	refused, err := RefusedOutputs(stateDir)
	if err != nil || len(refused) != 1 || refused[0].Refused.Reason != reason {
		t.Fatalf("refused outputs = %+v (err %v), want the first row owed with the relay's reason", refused, err)
	}
	// The refused request's next revision is an edit of its row: it waits
	// behind the row, which waits out its backoff.
	ref := strings.TrimSuffix(strings.TrimPrefix(refused[0].Key, "row/"), "/00000000")
	now = now.Add(2 * time.Second)
	origin := c.source(tagValue(sent[0], "e"), "").Origin
	if err := c.Publish(protocol.Snapshot{RequestRef: ref, TargetID: "cx", Revision: 2, State: protocol.StateCompleted, Result: &protocol.Result{Text: "done"}}, origin); err != nil {
		t.Fatal(err)
	}
	sent = nil
	accept := func(_ context.Context, evt nostr.Event) error { sent = append(sent, evt); return nil }
	if err := c.Flush(context.Background(), accept, nil, nil); err != nil || len(sent) != 0 {
		t.Fatalf("flush during backoff: err = %v, sent = %d, want nothing sent", err, len(sent))
	}
	now = now.Add(time.Minute)
	if err := c.Flush(context.Background(), accept, nil, nil); err != nil || len(sent) != 2 || sent[0].Kind != KindDM || sent[1].Kind != KindEdit || tagValue(sent[1], "e") != sent[0].ID.Hex() {
		t.Fatalf("flush after backoff: err = %v, sent = %+v, want the row then its edit", err, sent)
	}
	if pending, _ := c.ledger.Pending(); len(pending) != 0 {
		t.Fatalf("pending after flush = %d, want 0", len(pending))
	}
}

// Pro review of #971 round 2 (agent-message-queue-611.58): Flush ordered a
// request's outputs by key text, so a newer revision ("row/R/00000003")
// sorted before a refused approval message ("row/R/approval/ffff…") and
// went out first, also after the ledger was reopened. A request's outputs
// go out in the order they were prepared. Round 3: outputs written before
// sequence numbers (seq 0) have no provable order, so a refused one holds
// its request's other old outputs.
func TestRefusedApprovalHoldsItsRequestsNewerRows(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) { refusedApprovalHoldsNewerRows(t, legacy) })
	}
}

func refusedApprovalHoldsNewerRows(t *testing.T, legacy bool) {
	c, owner, stateDir := busyCarrier(t)
	now := time.Now()
	c.now = func() time.Time { return now }
	if err := c.Ingest(ownerEvent(t, owner, "dm-1", "go", now)); err != nil {
		t.Fatal(err)
	}
	var sent []nostr.Event
	accept := func(_ context.Context, evt nostr.Event) error { sent = append(sent, evt); return nil }
	if err := c.Flush(context.Background(), accept, nil, nil); err != nil || len(sent) != 1 {
		t.Fatalf("flush root: err = %v, sent = %d", err, len(sent))
	}
	pending, _ := c.ledger.Pending()
	ref, _, _ := c.ledger.RequestForRow(sent[0].ID.Hex())
	origin := c.source(tagValue(sent[0], "e"), "").Origin
	if len(pending) != 0 || ref == "" {
		t.Fatalf("root not accepted: pending = %d, ref = %q", len(pending), ref)
	}
	interaction := strings.Repeat("f", 32)
	now = now.Add(2 * time.Second)
	if err := c.Publish(protocol.Snapshot{RequestRef: ref, TargetID: "cx", Revision: 2, State: protocol.StateRunning,
		Interaction: &protocol.Interaction{InteractionID: interaction, Kind: "approval", Prompt: "run ls", Options: []string{"yes", "no"}, RemoteAnswer: true, ApproveOption: "yes", RejectOption: "no"}}, origin); err != nil {
		t.Fatal(err)
	}
	sent = nil
	refuseApproval := func(_ context.Context, evt nostr.Event) error {
		sent = append(sent, evt)
		if evt.Kind == KindDM {
			return &relay.RemoteError{Kind: relay.ErrRejected, Reason: "rate-limited"}
		}
		return nil
	}
	if err := c.Flush(context.Background(), refuseApproval, nil, nil); err != nil {
		t.Fatal(err)
	}
	// The terminal resolves the interaction; the newer revision is prepared
	// by a reopened ledger while the approval waits out its backoff.
	reopened, err := OpenLedger(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	c.ledger = reopened
	now = now.Add(2 * time.Second)
	if err := c.Publish(protocol.Snapshot{RequestRef: ref, TargetID: "cx", Revision: 3, State: protocol.StateCompleted, Result: &protocol.Result{Text: "done"},
		Resolved: []protocol.Resolution{{InteractionID: interaction, Outcome: protocol.ResolutionElsewhere}}}, origin); err != nil {
		t.Fatal(err)
	}
	if legacy {
		// Rewrite the owed outputs as an earlier build wrote them: no seq.
		files, _ := filepath.Glob(filepath.Join(stateDir, "buzz", "outbox", "*.json"))
		if len(files) == 0 {
			t.Fatal("no outbox files to rewrite")
		}
		for _, f := range files {
			raw, _ := os.ReadFile(f)
			var o map[string]any
			if json.Unmarshal(raw, &o) == nil {
				delete(o, "seq")
				raw, _ = json.Marshal(o)
				_ = os.WriteFile(f, raw, 0o600)
			}
		}
		if c.ledger, err = OpenLedger(stateDir); err != nil {
			t.Fatal(err)
		}
	}
	sent = nil
	if err := c.Flush(context.Background(), accept, nil, nil); err != nil || len(sent) != 0 {
		t.Fatalf("flush during the approval's backoff: err = %v, sent = %+v, want nothing sent", err, sent)
	}
	now = now.Add(time.Minute)
	// Old outputs have no provable order, so after the backoff only their
	// delivery is guaranteed, not the approval going first.
	if err := c.Flush(context.Background(), accept, nil, nil); err != nil || len(sent) == 0 || !legacy && sent[0].Kind != KindDM {
		t.Fatalf("flush after backoff: err = %v, sent = %+v, want the approval message first", err, sent)
	}
	if pending, _ := c.ledger.Pending(); len(pending) != 0 {
		t.Fatalf("pending after flush = %d, want 0", len(pending))
	}
}

// Bead agent-message-queue-611.59 (Pro review of #971 round 2): a root row
// the relay stored, but whose positive OK was lost, is refused as too old on
// every retry and held its request's later outputs forever. Finding the
// exact signed event on the relay accepts it; a failed lookup keeps it owed.
func TestStoredRowWhoseOKWasLostIsAccepted(t *testing.T) {
	for _, lookupWorks := range []bool{true, false} {
		t.Run(fmt.Sprintf("lookup=%v", lookupWorks), func(t *testing.T) {
			c, owner, _ := busyCarrier(t)
			now := time.Now()
			c.now = func() time.Time { return now }
			if err := c.Ingest(ownerEvent(t, owner, "dm-1", "go", now)); err != nil {
				t.Fatal(err)
			}
			var root nostr.Event
			tooOld := func(_ context.Context, evt nostr.Event) error {
				root = evt
				return &relay.RemoteError{Kind: relay.ErrRejected, Reason: "invalid: event too old"}
			}
			has := func(_ context.Context, id nostr.ID) (bool, error) {
				if !lookupWorks {
					return false, errors.New("the read timed out")
				}
				return id == root.ID, nil
			}
			if err := c.Flush(context.Background(), tooOld, has, nil); err != nil {
				t.Fatal(err)
			}
			pending, _ := c.ledger.Pending()
			if owed := len(pending) == 1; owed == lookupWorks {
				t.Fatalf("pending = %d after the lookup (works: %v); want the row accepted only when the relay holds it", len(pending), lookupWorks)
			}
		})
	}
}
