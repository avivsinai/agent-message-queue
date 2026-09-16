package core_test

import (
	"errors"
	"sync"
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

// TestReconcileConvergesAfterSuccessfulAck reproduces the follow-up defect
// found reviewing PR #730: replayTerminalAck had no convergence guard, so once
// a terminal result was acknowledged normally, every later Reconcile/Tick
// re-fired the native AcknowledgeResult for it — unbounded, for the life of the
// daemon. A converged endpoint acks a terminal result exactly once: the
// attachment releases the retained evidence on that ack, so Lookup reports it
// gone and reconciliation does not re-ack.
func TestReconcileConvergesAfterSuccessfulAck(t *testing.T) {
	store, _ := openStore(t)
	clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clk }
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	id := "11111111-1111-4111-8111-1111111111c4"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Normal completion (no crash) drives onNative -> exactly one native ack.
	rt.Complete(id, "the result")
	if got := rt.UnacknowledgedResults(); got != 0 {
		t.Fatalf("completion did not ack the result: %d retained", got)
	}
	if got := rt.AckCalls(); got != 1 {
		t.Fatalf("completion acked %d times, want exactly 1", got)
	}

	// Every later Reconcile/Tick must find the evidence released and not re-ack.
	for i := 0; i < 3; i++ {
		if err := ep.Reconcile(); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if got := rt.AckCalls(); got != 1 {
		t.Fatalf("reconcile re-acked the terminal result: %d ack calls, want 1", got)
	}
}

// countingAttachment wraps an Attachment and counts Lookup + AcknowledgeResult
// calls, so the 611.22.34 regression can prove the replay path short-circuits
// already-acked records.
type countingAttachment struct {
	core.Attachment
	lookups    int
	acks       int
	lastAckKey requests.Key
	lastAckDig string
	mu         sync.Mutex
}

func (c *countingAttachment) Lookup(key requests.Key, epoch string) (core.Evidence, error) {
	c.mu.Lock()
	c.lookups++
	c.mu.Unlock()
	return c.Attachment.Lookup(key, epoch)
}

func (c *countingAttachment) AcknowledgeResult(key requests.Key, epoch, digest string) {
	c.mu.Lock()
	c.acks++
	c.lastAckKey, c.lastAckDig = key, digest
	c.mu.Unlock()
	c.Attachment.AcknowledgeResult(key, epoch, digest)
}

// TestCrashAfterAckDeliveredConvergesWithoutRelookup pins the 611.22.34 fix:
// crash at PointAfterAck — the native ack DELIVERED but the durable flag was
// not written. After restart, Reconcile must converge: the record is marked
// Acknowledged and NO further attachment Lookup or AcknowledgeResult happens
// for it on subsequent ticks. Without the fix (or without the flag), the
// record is re-examined every tick forever: history results carry the
// NativeRef suffix ("codex thread turn ") the live path does not, so
// EvidenceDigest(ev.Result) never equals the memoed AckDigest.
func TestCrashAfterAckDeliveredConvergesWithoutRelookup(t *testing.T) {
	dir := t.TempDir()
	clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clk }
	store, err := requests.Open(dir, requests.WithClock(now))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rt := fake.New("fake", "e_1")
	counting := &countingAttachment{Attachment: rt}

	// Crash at after:native_ack: the ack went out, MarkAcknowledged never ran.
	crashArmed := true
	ep := core.New(core.Config{
		Store: store,
		Now:   func() time.Time { return clk },
		Crash: func(point string) error {
			if crashArmed && point == core.PointAfterAck {
				crashArmed = false
				return errors.New("simulated crash after native ack")
			}
			return nil
		},
	})
	ep.Register(counting)

	id := "11111111-1111-4111-8111-1111111111d3"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("seed submit: %v", err)
	}
	rt.Complete(id, "the result")

	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, ok, err := store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || rec.State != protocol.StateCompleted {
		t.Fatalf("terminal record not durable after crash: state=%v ok=%v", recState(rec, ok), ok)
	}
	// PointAfterAck crash: the native ack already DELIVERED, so the runtime
	// retains nothing — but the durable flag was never written. Exactly the
	// window 611.22.34 exists for.
	if rt.UnacknowledgedResults() != 0 {
		t.Fatal("precondition: after the PointAfterAck crash the runtime should retain nothing (the ack landed)")
	}

	// Restart over the same directory: the ack must be replayed exactly once
	// (the crash window: delivered natively, not durably marked), then the
	// record converges.
	if err := ep.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	store2, err := requests.Open(dir, requests.WithClock(now))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	ep2 := core.New(core.Config{Store: store2, Now: now})
	ep2.Register(counting)
	if err := ep2.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Nothing to replay: the ack already landed pre-crash. Reconcile just
	// needs to observe the durable-flag absence via one Lookup, get
	// EvidenceNone, and mark Acknowledged — WITHOUT re-acking.
	rec, _, err = store2.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Acknowledged {
		t.Fatal("record not marked Acknowledged after the replay reconciliation (611.22.34)")
	}

	// Convergence: two more Reconciles must perform ZERO Lookups and ZERO
	// additional acks for the record. Before the fix this looped forever.
	counting.mu.Lock()
	lookupsAfterFirst := counting.lookups
	acksAfterFirst := counting.acks
	counting.mu.Unlock()
	for i := 0; i < 2; i++ {
		if err := ep2.Reconcile(); err != nil {
			t.Fatalf("reconcile %d: %v", i+2, err)
		}
	}
	counting.mu.Lock()
	defer counting.mu.Unlock()
	if counting.lookups != lookupsAfterFirst {
		t.Fatalf("already-acked record was re-Lookuped %d extra time(s) across 2 reconciles — replay does not converge (agent-message-queue-611.22.34)", counting.lookups-lookupsAfterFirst)
	}
	if counting.acks != acksAfterFirst {
		t.Fatalf("already-acked record was re-acked %d extra time(s) across 2 reconciles (agent-message-queue-611.22.34)", counting.acks-acksAfterFirst)
	}
}

// TestPointBeforeAckCrashStillReplays pins the crash-gap correctness the flag
// must NOT break: PointBeforeAck crash → AckDigest memoed, ack never sent,
// Acknowledged false → the restart replay MUST still fire (intent set +
// !Acknowledged = replayable, unchanged from pre-611.22.34 rules).
func TestPointBeforeAckCrashStillReplays(t *testing.T) {
	dir := t.TempDir()
	clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clk }
	store, err := requests.Open(dir, requests.WithClock(now))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rt := fake.New("fake", "e_1")
	counting := &countingAttachment{Attachment: rt}

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
	ep.Register(counting)

	id := "11111111-1111-4111-8111-1111111111e3"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("seed submit: %v", err)
	}
	rt.Complete(id, "the result")

	if err := ep.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	store2, err := requests.Open(dir, requests.WithClock(now))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	ep2 := core.New(core.Config{Store: store2, Now: now})
	ep2.Register(counting)
	if err := ep2.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The replay MUST have fired: exactly one ack delivered by the restart.
	counting.mu.Lock()
	acks := counting.acks
	counting.mu.Unlock()
	if acks != 1 {
		t.Fatalf("PointBeforeAck crash + restart: replay ack count = %d, want 1 (crash-gap correctness broken by the flag)", acks)
	}
	if got := rt.UnacknowledgedResults(); got != 0 {
		t.Fatalf("replay did not release the retained result: %d unacked", got)
	}
	rec, _, err := store2.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Acknowledged {
		t.Fatal("record not marked Acknowledged after the PointBeforeAck replay (611.22.34)")
	}
}

// historyShapeAttachment returns the LITERAL codex lookupHistory shape for a
// terminal history turn (attachment.go:745, post-611.22.34-B2 with Class set):
// Known, Admitted, Class=HistoryTerminated, RunID "turn:<id>", terminal state,
// result carrying the NativeRef suffix the live event did not have. This is
// what a RESTARTED codex attachment returns for a pre-upgrade record — NOT
// the fake.Runtime EvidenceNone-after-release shortcut, which outran
// production and hid this path (verifier round 1, B2).
type historyShapeAttachment struct {
	core.Attachment
	lookups int
	acks    int
	mu      sync.Mutex
}

func (h *historyShapeAttachment) Lookup(key requests.Key, epoch string) (core.Evidence, error) {
	h.mu.Lock()
	h.lookups++
	h.mu.Unlock()
	return core.Evidence{
		Known:    true,
		Admitted: true,
		Class:    core.EvidenceHistoryTerminated,
		RunID:    "turn:turnHistory1",
		State:    protocol.StateCompleted,
		Result: &protocol.Result{
			Text:      "the result",
			NativeRef: "codex thread thr_1 turn turnHistory1",
		},
	}, nil
}

func (h *historyShapeAttachment) AcknowledgeResult(key requests.Key, epoch, digest string) {
	h.mu.Lock()
	h.acks++
	h.mu.Unlock()
	h.Attachment.AcknowledgeResult(key, epoch, digest)
}

// TestPreUpgradeRecordConvergesOnHistoryShape pins the B2 recut: a terminal
// record already on disk from a SHIPPED build (Acknowledged=false, AckDigest
// memoed from the live NativeRef-free result) meets a RESTARTED attachment
// whose Lookup returns history evidence. The replay must converge: the
// NativeRef-asymmetric digest must not block the ack, the record must be
// marked Acknowledged, and subsequent reconciles must not re-Lookup it.
// Before the recut this looped forever: Class unset -> the in-memory
// EvidenceNone shortcut unreachable -> digest compare failed on the NativeRef
// suffix -> 3 reconciles, 3 lookups, never acknowledged (verifier probe).
func TestPreUpgradeRecordConvergesOnHistoryShape(t *testing.T) {
	dir := t.TempDir()
	clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clk }
	store, err := requests.Open(dir, requests.WithClock(now))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rt := fake.New("fake", "e_1")
	stub := &historyShapeAttachment{Attachment: rt}

	// Seed the durable record DIRECTLY in the pre-upgrade shape: terminal,
	// ack digest memoed from the live result (no NativeRef), Acknowledged
	// false (zero value — every shipped record looks like this). WriteMemo
	// writes raw with no state-graph validation, exactly how a shipped
	// build's bytes look after an upgrade.
	id := "11111111-1111-4111-8111-1111111111b2"
	// The live turn/completed result carries the NativeRef too: codex sets
	// r.nativeRef before result() copies it (codex/attachment.go turn/completed
	// arm). Seeding it NativeRef-free was the fake's shape, not codex's, and
	// hid the digest asymmetry (agent-message-queue-611.22.34, post-merge
	// verification of #767).
	liveResult := &protocol.Result{Text: "the result", NativeRef: "codex thread thr_1 turn turnHistory1"}
	run := "turn:turnHistory1"
	seed := &requests.Record{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   id,
			CreatorHost: "local",
			TargetID:    "fake",
			Epoch:       "e_1",
			Revision:    3,
			State:       protocol.StateCompleted,
			InputDigest: requests.Digest([]byte("hi")),
			Result:      liveResult,
			NativeRun:   &run,
			ObservedAt:  "2026-09-08T09:00:00Z",
		},
		Input:     &protocol.SubmitInput{Text: "hi"},
		AckDigest: protocol.EvidenceDigest(liveResult), // memoed from the LIVE result
	}
	if err := store.Create(&requests.Record{Snapshot: protocol.Snapshot{
		Schema:      protocol.SchemaRequest,
		RequestID:   id,
		CreatorHost: "local",
		TargetID:    "fake",
		Epoch:       "e_1",
		Revision:    1,
		State:       protocol.StateReceived,
		InputDigest: requests.Digest([]byte("hi")),
		ObservedAt:  "2026-09-08T09:00:00Z",
	}, Input: &protocol.SubmitInput{Text: "hi"}}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	if err := store.WriteMemo(seed); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2, err := requests.Open(dir, requests.WithClock(now))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	ep := core.New(core.Config{Store: store2, Now: now})
	ep.Register(stub)
	for i := 0; i < 3; i++ {
		if err := ep.Reconcile(); err != nil {
			t.Fatalf("reconcile %d: %v", i+1, err)
		}
	}
	rec, ok, err := store2.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if err != nil || !ok {
		t.Fatalf("get: %v (ok=%v)", err, ok)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !rec.Acknowledged {
		t.Fatalf("NO CONVERGENCE: pre-upgrade record still Acknowledged=false after 3 reconciles; the restarted attachment was Lookuped %d time(s) (one full thread/read each) and will be re-Lookuped every tick forever (agent-message-queue-611.22.34 B2)", stub.lookups)
	}
	if stub.lookups != 1 {
		t.Fatalf("history lookup ran %d time(s), want 1 (converge after the first proof)", stub.lookups)
	}
	if stub.acks != 1 {
		t.Fatalf("history ack ran %d time(s), want 1", stub.acks)
	}
}
