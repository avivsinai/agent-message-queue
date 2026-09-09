package core_test

import (
	"errors"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestCrashAfterTerminalCommitReplaysAck reproduces Pro B06: the endpoint
// crashes after the terminal result is durably committed but before (or
// during) the native AcknowledgeResult call. On restart, startup
// reconciliation must replay the native acknowledgement for the terminal
// record from its durable ack intent, releasing the attachment's one
// unacked-result slot — otherwise the next submit is refused busy ("a
// completed result awaits acknowledgement") forever. The ack digest is the
// evidence digest of the retained result, not the input digest: the fake
// runtime refuses an ack that names different evidence.
func TestCrashAfterTerminalCommitReplaysAck(t *testing.T) {
	dir := t.TempDir()
	clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clk }
	store, err := requests.Open(dir, requests.WithClock(now))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rt := fake.New("fake", "e_1")

	// Crash at before:native_ack: the terminal record (completed, result
	// bound) is durable, but the native ack never went out.
	crashArmed := true
	ep := core.New(core.Config{
		Store: store,
		Now:   func() time.Time { return clk },
		Crash: func(point string) error {
			if crashArmed && point == core.PointBeforeAck {
				crashArmed = false
				return errors.New("simulated crash before native ack")
			}
			return nil
		},
	})
	ep.Register(rt)

	id := "11111111-1111-4111-8111-1111111111c1"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("seed submit: %v", err)
	}
	rt.Complete(id, "the result")
	time.Sleep(50 * time.Millisecond)

	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, ok, err := store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || rec.State != protocol.StateCompleted {
		t.Fatalf("terminal record not durable after crash: state=%v ok=%v", recState(rec, ok), ok)
	}
	if rt.UnacknowledgedResults() != 1 {
		t.Fatal("precondition: the runtime should retain exactly one unacked result")
	}

	// Restart: a fresh endpoint over the same directory and the SAME live
	// runtime — the retained evidence survives the endpoint restart. Close
	// also closes the store (ownership release), so recovery reopens it.
	if err := ep.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	store2, err := requests.Open(dir, requests.WithClock(now))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	ep2 := core.New(core.Config{Store: store2, Now: now})
	ep2.Register(rt)
	if err := ep2.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Startup reconciliation released the retained evidence.
	if got := rt.UnacknowledgedResults(); got != 0 {
		t.Fatalf("reconcile did not replay the ack; %d unacked result(s) retained", got)
	}

	// The next submit is NOT rejected busy by the unacked result.
	replyAny, err := ep2.Handle(submitCmd("11111111-1111-4111-8111-1111111111c3"), core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("next submit errored: %v", err)
	}
	reply := replyAny.(protocol.Reply)
	if reply.Snapshot.State == protocol.StateRejected && reply.Snapshot.Code == protocol.CodeBusy {
		t.Fatalf("next submit rejected busy by the unacked result: %s/%s", reply.Snapshot.State, reply.Snapshot.Code)
	}
	if reply.Snapshot.State != protocol.StateRunning {
		t.Fatalf("next submit state = %s (%s), want running", reply.Snapshot.State, reply.Snapshot.Code)
	}

	// The durable ack digest is the evidence digest, not the input digest.
	rec, _, err = store2.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if rec.AckDigest == "" || rec.AckDigest == rec.InputDigest {
		t.Fatalf("ack digest must be the evidence digest: ack=%q input=%q", rec.AckDigest, rec.InputDigest)
	}
	if want := protocol.EvidenceDigest(rec.Result); rec.AckDigest != want {
		t.Fatalf("ack digest = %q, want evidence digest %q", rec.AckDigest, want)
	}
}

func recState(rec *requests.Record, ok bool) any {
	if !ok {
		return "absent"
	}
	return rec.State
}

// TestAckWithWrongDigestDoesNotReleaseEvidence pins the attachment half of
// the contract: an acknowledgement naming different evidence never releases
// the retained result, so a stale ack cannot free a different request's slot.
func TestAckWithWrongDigestDoesNotReleaseEvidence(t *testing.T) {
	store, _ := openStore(t)
	clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	rt := fake.New("fake", "e_1")
	// Freeze at the B06 boundary: the terminal record is durably committed
	// with its ack-intent memo, but the native ack never went out — exactly
	// the pre-replay state, with the evidence still retained at the runtime.
	crashArmed := true
	ep := core.New(core.Config{
		Store: store,
		Now:   func() time.Time { return clk },
		Crash: func(point string) error {
			if crashArmed && point == core.PointBeforeAck {
				crashArmed = false
				return errors.New("simulated crash before native ack")
			}
			return nil
		},
	})
	ep.Register(rt)

	id := "11111111-1111-4111-8111-1111111111c2"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	rt.Complete(id, "the result")
	time.Sleep(50 * time.Millisecond)
	if err := ep.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if rt.UnacknowledgedResults() == 0 {
		t.Fatal("precondition: result should be retained")
	}
	// A stale/foreign digest must not release the evidence.
	rt.AcknowledgeResult(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}, "e_1",
		protocol.EvidenceDigest(&protocol.Result{Text: "different outcome"}))
	if rt.UnacknowledgedResults() != 1 {
		t.Fatal("a wrong-digest ack released the retained evidence")
	}
	// The matching digest — the durable ack intent — does release it.
	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, _, err := store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if rec.AckDigest == "" {
		t.Fatal("precondition: crash left no durable ack intent")
	}
	rt.AcknowledgeResult(key, "e_1", rec.AckDigest)
	if rt.UnacknowledgedResults() != 0 {
		t.Fatal("the matching-digest ack did not release the retained evidence")
	}
}
