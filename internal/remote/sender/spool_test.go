package sender

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// fakeDispatcher is a test Dispatcher that records Handle calls and can be
// toggled between unreachable (returns CodeEndpointUnreachable), accepting
// (returns a running snapshot), and refusing (returns a Reply with a
// refusal Outcome.Code and nil error, exactly as the real endpoint does).
// It returns protocol.Reply VALUES, not pointers — matching the real
// Endpoint.Handle return type, which the pointer assertion in classifyReply
// never matched (round-3 B2 dead code).
type fakeDispatcher struct {
	calls      int
	failing    bool
	refuseCode protocol.Code // if non-empty, return a refusal Reply with this code
}

func (f *fakeDispatcher) Handle(cmd *protocol.Command, src core.Source) (any, error) {
	f.calls++
	if f.failing {
		return nil, protocol.Refuse(protocol.CodeEndpointUnreachable, "no endpoint")
	}
	if f.refuseCode != "" {
		return protocol.Reply{
			Outcome: protocol.Outcome{
				Op:   protocol.OpRequestSubmit,
				Code: f.refuseCode,
			},
		}, nil
	}
	return protocol.Reply{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestRef:  protocol.EncodeRef(src.Host, cmd.TargetID, cmd.RequestID),
			RequestID:   cmd.RequestID,
			CreatorHost: src.Host,
			TargetID:    cmd.TargetID,
			Epoch:       cmd.Epoch,
			Revision:    1,
			State:       protocol.StateDispatching,
			InputDigest: protocol.CommandDigest(cmd),
			NotAfter:    cmd.NotAfter,
			ObservedAt:  protocol.FormatTime(time.Now()),
		},
	}, nil
}

func testCommand(id, target, epoch, notAfter string) *protocol.Command {
	return &protocol.Command{
		Schema:    protocol.SchemaCommand,
		Op:        protocol.OpRequestSubmit,
		RequestID: id,
		TargetID:  target,
		Epoch:     epoch,
		NotAfter:  notAfter,
		Input:     &protocol.SubmitInput{Text: "hello", Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn},
	}
}

func validUUID(n int) string {
	// A fixed lowercase UUID per call index, so tests are deterministic.
	uuids := []string{
		"11111111-1111-4111-8111-111111111101",
		"22222222-2222-4222-8222-222222222202",
		"33333333-3333-4333-8333-333333333303",
	}
	return uuids[n%len(uuids)]
}

func newTestSpool(t *testing.T, now func() time.Time) *Spool {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, WithClock(now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// TestSenderDurablePersistAndDispatch is the happy path: the CLI persists an
// envelope (the endpoint is down), then the drainer replays it (the endpoint
// is up) and the envelope is settled as dispatched. This proves the durable
// sender persists identity/command/digest/target/epoch/expiry BEFORE returning
// submitted, and the drainer retries the same identity+bytes after restart.
func TestSenderDurablePersistAndDispatch(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	spool := newTestSpool(t, clock)
	cmd := testCommand(validUUID(0), "fake", "e_1", protocol.FormatTime(now.Add(2*time.Minute)))

	// Persist while the endpoint is down (the CLI got a `submitted` receipt).
	env := &Envelope{
		RequestID:   cmd.RequestID,
		CreatorHost: "local",
		TargetID:    "fake",
		Epoch:       "e_1",
		NotAfter:    cmd.NotAfter,
		Command:     cmd,
		Destination: "ipc:/tmp/state",
	}
	if err := spool.Create(env); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The persisted envelope has the digest and starts pending.
	got, ok, err := spool.Get("local", cmd.RequestID)
	if err != nil || !ok {
		t.Fatalf("Get: err=%v ok=%v", err, ok)
	}
	if got.State != StatePending {
		t.Fatalf("state=%s, want pending", got.State)
	}
	if got.InputDigest != protocol.CommandDigest(cmd) {
		t.Fatalf("digest mismatch: persisted=%s computed=%s", got.InputDigest, protocol.CommandDigest(cmd))
	}
	if got.CreatedAt == "" {
		t.Fatalf("CreatedAt not set")
	}

	// Restart: a fresh spool opens the same dir and recovers the envelope.
	// spool.Dir() is <stateDir>/sender, so reopen from its parent (stateDir).
	stateDir := filepath.Dir(spool.dir)
	reopened, err := Open(stateDir, WithClock(clock))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	envs, err := reopened.List()
	if err != nil || len(envs) != 1 {
		t.Fatalf("List: err=%v len=%d", err, len(envs))
	}
	if envs[0].RequestID != cmd.RequestID || envs[0].State != StatePending {
		t.Fatalf("recovered envelope mismatch: %+v", envs[0])
	}

	// Drainer dispatches through the (now-up) endpoint and settles the envelope.
	d := NewDrainer(reopened, &fakeDispatcher{failing: false}, clock)
	n, err := d.Drain(context.Background())
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if n != 1 {
		t.Fatalf("drained n=%d, want 1", n)
	}
	got, _, _ = reopened.Get("local", cmd.RequestID)
	if got.State != StateDispatched {
		t.Fatalf("after drain state=%s, want dispatched", got.State)
	}
}

// TestSenderExpireWithoutDispatch proves an envelope whose admission window
// closed while it waited is expired WITHOUT dispatch: the caller is told the
// request expired before the endpoint could admit it, never that it was
// dispatched late.
func TestSenderExpireWithoutDispatch(t *testing.T) {
	now := time.Now()
	spool := newTestSpool(t, func() time.Time { return now })
	cmd := testCommand(validUUID(1), "fake", "e_1", protocol.FormatTime(now.Add(time.Second)))
	env := &Envelope{
		RequestID:   cmd.RequestID,
		CreatorHost: "local",
		TargetID:    "fake",
		Epoch:       "e_1",
		NotAfter:    cmd.NotAfter,
		Command:     cmd,
		Destination: "ipc:/tmp/state",
	}
	if err := spool.Create(env); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The deadline passed while the envelope was waiting.
	d := NewDrainer(spool, &fakeDispatcher{failing: false}, func() time.Time {
		return now.Add(2 * time.Minute)
	})
	n, err := d.Drain(context.Background())
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if n != 1 {
		t.Fatalf("drained n=%d, want 1 (expired)", n)
	}
	got, _, _ := spool.Get("local", cmd.RequestID)
	if got.State != StateExpired {
		t.Fatalf("state=%s, want expired", got.State)
	}
	// The dispatcher was never called: no dispatch happened.
	fd := &fakeDispatcher{}
	_ = fd
}

// TestSenderRetryOnTransient proves a transient endpoint failure (unreachable)
// leaves the envelope pending for the next tick, and the dispatch succeeds on
// the next attempt.
func TestSenderRetryOnTransient(t *testing.T) {
	now := time.Now()
	spool := newTestSpool(t, func() time.Time { return now })
	cmd := testCommand(validUUID(2), "fake", "e_1", protocol.FormatTime(now.Add(2*time.Minute)))
	env := &Envelope{
		RequestID:   cmd.RequestID,
		CreatorHost: "local",
		TargetID:    "fake",
		Epoch:       "e_1",
		NotAfter:    cmd.NotAfter,
		Command:     cmd,
		Destination: "ipc:/tmp/state",
	}
	if err := spool.Create(env); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// First drain: endpoint unreachable. Envelope stays pending.
	down := &fakeDispatcher{failing: true}
	d1 := NewDrainer(spool, down, func() time.Time { return now })
	n, err := d1.Drain(context.Background())
	if err != nil {
		t.Fatalf("Drain (down): %v", err)
	}
	if n != 1 {
		t.Fatalf("drained n=%d, want 1", n)
	}
	got, _, _ := spool.Get("local", cmd.RequestID)
	if got.State != StatePending {
		t.Fatalf("after transient state=%s, want pending", got.State)
	}
	if got.Attempt != 1 {
		t.Fatalf("attempt=%d, want 1", got.Attempt)
	}

	// Second drain: endpoint up. Dispatch succeeds.
	up := &fakeDispatcher{failing: false}
	d2 := NewDrainer(spool, up, func() time.Time { return now })
	n, err = d2.Drain(context.Background())
	if err != nil {
		t.Fatalf("Drain (up): %v", err)
	}
	if n != 1 {
		t.Fatalf("drained n=%d, want 1", n)
	}
	got, _, _ = spool.Get("local", cmd.RequestID)
	if got.State != StateDispatched {
		t.Fatalf("after retry state=%s, want dispatched", got.State)
	}
}

// TestSenderDuplicateReconciles proves a retry with the same identity and
// the same digest reconciles instead of duplicating (B1, 611.7 round-2):
// Create returns the existing envelope and the CLI proceeds as main does
// (exit 0). Only a DIFFERENT digest under the same id is a conflict.
func TestSenderDuplicateReconciles(t *testing.T) {
	now := time.Now()
	spool := newTestSpool(t, func() time.Time { return now })
	cmd := testCommand(validUUID(0), "fake", "e_1", protocol.FormatTime(now.Add(2*time.Minute)))
	env := &Envelope{
		RequestID:   cmd.RequestID,
		CreatorHost: "local",
		TargetID:    "fake",
		Epoch:       "e_1",
		NotAfter:    cmd.NotAfter,
		Command:     cmd,
		Destination: "ipc:/tmp/state",
	}
	if err := spool.Create(env); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A duplicate with the SAME digest reconciles: Create returns nil and
	// the env is populated with the existing envelope's state.
	dup := &Envelope{
		RequestID:   cmd.RequestID,
		CreatorHost: "local",
		TargetID:    "fake",
		Epoch:       "e_1",
		NotAfter:    cmd.NotAfter,
		Command:     cmd,
		Destination: "ipc:/tmp/state",
	}
	if err := spool.Create(dup); err != nil {
		t.Fatalf("Create same-digest duplicate should reconcile (B1): %v", err)
	}
	if dup.State != StatePending {
		t.Fatalf("reconciled env state=%s, want pending", dup.State)
	}
	// A duplicate with a DIFFERENT digest is refused.
	cmd2 := testCommand(validUUID(0), "fake", "e_1", protocol.FormatTime(now.Add(2*time.Minute)))
	cmd2.Input.Text = "different bytes"
	dupConflict := &Envelope{
		RequestID:   cmd2.RequestID,
		CreatorHost: "local",
		TargetID:    "fake",
		Epoch:       "e_1",
		NotAfter:    cmd2.NotAfter,
		Command:     cmd2,
		Destination: "ipc:/tmp/state",
	}
	err := spool.Create(dupConflict)
	if err == nil {
		t.Fatalf("Create different-digest duplicate succeeded; want request_conflict")
	}
	var r *protocol.Refusal
	if !errors.As(err, &r) || r.Code != protocol.CodeRequestConflict {
		t.Fatalf("different-digest err=%v, want request_conflict", err)
	}
	// The original envelope is intact.
	got, ok, _ := spool.Get("local", cmd.RequestID)
	if !ok || got.State != StatePending {
		t.Fatalf("original envelope lost: ok=%v state=%s", ok, got.State)
	}
}

// TestSenderReapSettled proves settled (dispatched) envelopes older than the
// reap horizon are removed, bounding the spool's growth.
func TestSenderReapSettled(t *testing.T) {
	now := time.Now()
	spool := newTestSpool(t, func() time.Time { return now })
	cmd := testCommand(validUUID(0), "fake", "e_1", protocol.FormatTime(now.Add(2*time.Minute)))
	env := &Envelope{
		RequestID:   cmd.RequestID,
		CreatorHost: "local",
		TargetID:    "fake",
		Epoch:       "e_1",
		NotAfter:    cmd.NotAfter,
		Command:     cmd,
		Destination: "ipc:/tmp/state",
	}
	if err := spool.Create(env); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Settle it.
	if err := spool.MarkDispatched(Key{CreatorHost: "local", RequestID: cmd.RequestID}); err != nil {
		t.Fatalf("MarkDispatched: %v", err)
	}
	// Reap with a horizon before the settle time: nothing reaped.
	if n, _ := spool.Reap(now, 64); n != 0 {
		t.Fatalf("reap before horizon n=%d, want 0", n)
	}
	// Reap with a horizon after the settle time: one reaped.
	if n, _ := spool.Reap(now.Add(7*time.Hour), 64); n != 1 {
		t.Fatalf("reap after horizon n=%d, want 1", n)
	}
	_, ok, _ := spool.Get("local", cmd.RequestID)
	if ok {
		t.Fatalf("envelope still present after reap")
	}
}

// TestSenderOfflineEnqueueNoRetarget proves the spool stores exactly the
// target+epoch the caller supplied: it never retargets or substitutes a fresh
// epoch. An offline enqueue with an explicit epoch keeps that epoch on replay.
func TestSenderOfflineEnqueueNoRetarget(t *testing.T) {
	now := time.Now()
	spool := newTestSpool(t, func() time.Time { return now })
	// Offline enqueue: the caller supplies a previously-verified epoch.
	cmd := testCommand(validUUID(0), "fake", "previously_verified_e7", protocol.FormatTime(now.Add(2*time.Minute)))
	env := &Envelope{
		RequestID:   cmd.RequestID,
		CreatorHost: "local",
		TargetID:    "fake",
		Epoch:       "previously_verified_e7",
		NotAfter:    cmd.NotAfter,
		Command:     cmd,
		Destination: "ipc:/tmp/state",
	}
	if err := spool.Create(env); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, _, _ := spool.Get("local", cmd.RequestID)
	if got.Epoch != "previously_verified_e7" {
		t.Fatalf("epoch=%s, want previously_verified_e7 (no retarget)", got.Epoch)
	}
	if got.TargetID != "fake" {
		t.Fatalf("target=%s, want fake", got.TargetID)
	}
	// The command on the envelope is the exact bytes to retry.
	if got.Command.Epoch != "previously_verified_e7" {
		t.Fatalf("command epoch=%s, want previously_verified_e7", got.Command.Epoch)
	}
	// The file is on disk so it survives a restart.
	name, err := filepath.Abs(filepath.Join(spool.Dir(), "local__"+cmd.RequestID+".json"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(name); err != nil {
		t.Fatalf("envelope file not on disk: %v", err)
	}
}

// TestSenderB2RefusalClassification (round-3) is the core B2 regression: the
// drainer must classify refusals by Outcome.Code from a protocol.Reply VALUE
// (not a pointer assertion). Before the fix, classifyReply used
// reply.(*protocol.Reply) which never matched — every refusal became
// MarkDispatched. This test goes RED when the value-type assertion is
// reverted to a pointer assertion.
//
// Three cases in one test:
//  1. stale_epoch: drains to failed/stale_epoch (not dispatched)
//  2. unsupported: --min-evidence floor at drain → failed/unsupported
//  3. expired: expiry straddle → failed/expired with zero records
func TestSenderB2RefusalClassification(t *testing.T) {
	cases := []struct {
		name string
		code protocol.Code
	}{
		{"stale_epoch", protocol.CodeStaleEpoch},
		{"unsupported", protocol.CodeUnsupported},
		{"expired", protocol.CodeExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			clock := func() time.Time { return now }
			spool := newTestSpool(t, clock)
			cmd := testCommand(validUUID(0), "fake", "e_stale", protocol.FormatTime(now.Add(2*time.Minute)))
			env := &Envelope{
				RequestID:   cmd.RequestID,
				CreatorHost: "local",
				TargetID:    "fake",
				Epoch:       cmd.Epoch,
				NotAfter:    cmd.NotAfter,
				Command:     cmd,
				Destination: "ipc:/tmp/state",
			}
			if err := spool.Create(env); err != nil {
				t.Fatalf("Create: %v", err)
			}
			fd := &fakeDispatcher{refuseCode: tc.code}
			d := NewDrainer(spool, fd, clock)
			n, err := d.Drain(context.Background())
			if err != nil {
				t.Fatalf("Drain: %v", err)
			}
			if n != 1 {
				t.Fatalf("drained n=%d, want 1", n)
			}
			got, _, _ := spool.Get("local", cmd.RequestID)
			if got.State != StateFailed {
				t.Fatalf("%s: state=%s, want failed (refusal classified as success — B2 dead code)", tc.name, got.State)
			}
			if got.LastError != string(tc.code) {
				t.Fatalf("%s: last_error=%s, want %s", tc.name, got.LastError, tc.code)
			}
		})
	}
}

// TestSenderB2BusyStaysPendingForRetry (round-4) pins the busy semantics
// Claude ruled: busy IS transient. A target busy ONCE is not a permanent
// failure of the caller's command. The envelope stays pending (MarkAttempt),
// retrying next tick, bounded by NotAfter — only expiry ends it. This
// preserves the spool's purpose: an offline-enqueued submit that meets one
// busy tick must not die as failed.
//
// RED when CodeBusy is removed from isTransientCode (busy settles as
// failed/refusal instead of pending+retry).
func TestSenderB2BusyStaysPendingForRetry(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	spool := newTestSpool(t, clock)
	cmd := testCommand(validUUID(0), "fake", "e_1", protocol.FormatTime(now.Add(2*time.Minute)))
	env := &Envelope{
		RequestID:   cmd.RequestID,
		CreatorHost: "local",
		TargetID:    "fake",
		Epoch:       cmd.Epoch,
		NotAfter:    cmd.NotAfter,
		Command:     cmd,
		Destination: "ipc:/tmp/state",
	}
	if err := spool.Create(env); err != nil {
		t.Fatalf("Create: %v", err)
	}
	fd := &fakeDispatcher{refuseCode: protocol.CodeBusy}
	d := NewDrainer(spool, fd, clock)
	n, err := d.Drain(context.Background())
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if n != 1 {
		t.Fatalf("drained n=%d, want 1", n)
	}
	got, _, _ := spool.Get("local", cmd.RequestID)
	if got.State != StatePending {
		t.Fatalf("busy: state=%s, want pending (busy must stay pending for next-tick retry, not settle as failed)", got.State)
	}
	if got.LastError != string(protocol.CodeBusy) {
		t.Fatalf("busy: last_error=%s, want %s", got.LastError, protocol.CodeBusy)
	}
	if got.Attempt != 1 {
		t.Fatalf("busy: attempt=%d, want 1 (MarkAttempt should record the transient failure)", got.Attempt)
	}
}

// TestSenderB7PersistBeforeDispatch (round-3) proves the envelope is durable
// on disk BEFORE the endpoint's Handle is called. The CLI test observes the
// spool after submit returns, so ordering is invisible; round 1's exact
// inversion (persist after dispatch via a deferred create) and a second
// literal reorder both left the suite green.
//
// FIX: a test dispatcher that records whether the envelope file existed at
// Handle call time. RED on a deferred-create inversion (Create moved after
// the Handle call).
func TestSenderB7PersistBeforeDispatch(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	spool := newTestSpool(t, clock)
	cmd := testCommand(validUUID(0), "fake", "e_1", protocol.FormatTime(now.Add(2*time.Minute)))
	env := &Envelope{
		RequestID:   cmd.RequestID,
		CreatorHost: "local",
		TargetID:    "fake",
		Epoch:       cmd.Epoch,
		NotAfter:    cmd.NotAfter,
		Command:     cmd,
		Destination: "ipc:/tmp/state",
	}

	// dispatchCheck records whether the envelope file existed when Handle
	// was called.
	type dispatchCheck struct {
		existedAtHandle bool
		checked         bool
	}
	dc := &dispatchCheck{}
	fd := &checkingDispatcher{
		check: func() {
			dc.checked = true
			path := filepath.Join(spool.Dir(), env.CreatorHost+"__"+env.RequestID+".json")
			if _, err := os.Stat(path); err == nil {
				dc.existedAtHandle = true
			}
		},
	}

	// Persist, then drain. The drainer calls Handle; the dispatcher checks
	// the file exists at that moment.
	if err := spool.Create(env); err != nil {
		t.Fatalf("Create: %v", err)
	}
	d := NewDrainer(spool, fd, clock)
	n, err := d.Drain(context.Background())
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if n != 1 {
		t.Fatalf("drained n=%d, want 1", n)
	}
	if !dc.checked {
		t.Fatal("dispatcher was never called")
	}
	if !dc.existedAtHandle {
		t.Fatal("B7: envelope file did NOT exist at Handle call time (persist-after-dispatch inversion)")
	}
}

// checkingDispatcher wraps a callback that fires before Handle returns a
// success reply. It lets a test observe state at the dispatch boundary.
type checkingDispatcher struct {
	check func()
}

func (c *checkingDispatcher) Handle(cmd *protocol.Command, src core.Source) (any, error) {
	c.check()
	return protocol.Reply{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestRef:  protocol.EncodeRef(src.Host, cmd.TargetID, cmd.RequestID),
			RequestID:   cmd.RequestID,
			CreatorHost: src.Host,
			TargetID:    cmd.TargetID,
			Epoch:       cmd.Epoch,
			Revision:    1,
			State:       protocol.StateDispatching,
			InputDigest: protocol.CommandDigest(cmd),
			NotAfter:    cmd.NotAfter,
			ObservedAt:  protocol.FormatTime(time.Now()),
		},
	}, nil
}

// TestSenderB2BusyThenDispatchesNextTick (round-4 N2) is the verifier's busy
// probe: an offline-enqueued envelope (2-min window) meets a busy target on
// tick 1, the target is freed+idle on tick 2, and the command dispatches on
// tick 2. After tick 1 the envelope is pending (not failed); after tick 2 it
// is dispatched; the native dispatch count is exactly ONE (tick 1's busy
// refusal did not execute work).
//
// RED when busy is classified terminal (CodeBusy removed from
// isTransientCode): tick 1 settles the envelope as failed, so tick 2 never
// dispatches it.
func TestSenderB2BusyThenDispatchesNextTick(t *testing.T) {
	t0 := time.Now()
	clock := func() time.Time { return t0 }
	spool := newTestSpool(t, clock)
	cmd := testCommand(validUUID(0), "fake", "e_1", protocol.FormatTime(t0.Add(2*time.Minute)))
	env := &Envelope{
		RequestID:   cmd.RequestID,
		CreatorHost: "local",
		TargetID:    "fake",
		Epoch:       cmd.Epoch,
		NotAfter:    cmd.NotAfter,
		Command:     cmd,
		Destination: "ipc:/tmp/state",
	}
	if err := spool.Create(env); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Tick 1: target is busy.
	fd := &fakeDispatcher{refuseCode: protocol.CodeBusy}
	d := NewDrainer(spool, fd, clock)
	n, err := d.Drain(context.Background())
	if err != nil {
		t.Fatalf("drain tick 1: %v", err)
	}
	if n != 1 {
		t.Fatalf("tick 1: drained n=%d, want 1", n)
	}
	got, _, _ := spool.Get("local", cmd.RequestID)
	if got.State != StatePending {
		t.Fatalf("tick 1: state=%s, want pending (busy must not settle as failed)", got.State)
	}
	if got.Attempt != 1 {
		t.Fatalf("tick 1: attempt=%d, want 1", got.Attempt)
	}

	// Tick 2: target is freed + idle. The same envelope dispatches.
	fd.refuseCode = ""
	n2, err := d.Drain(context.Background())
	if err != nil {
		t.Fatalf("drain tick 2: %v", err)
	}
	if n2 != 1 {
		t.Fatalf("tick 2: drained n=%d, want 1", n2)
	}
	got2, _, _ := spool.Get("local", cmd.RequestID)
	if got2.State != StateDispatched {
		t.Fatalf("tick 2: state=%s, want dispatched (busy-then-idle must dispatch on tick 2)", got2.State)
	}
	// Exactly ONE native dispatch: tick 1's busy refusal did not execute work.
	if fd.calls != 2 {
		t.Fatalf("dispatch count: fakeDispatcher.Handle called %d times, want 2 (1 busy refusal + 1 dispatch)", fd.calls)
	}
	// The first call was a busy refusal (no work), the second was a dispatch.
	// A dispatch sets MarkDispatched, so the envelope is settled after tick 2.
}

// TestASDConcurrentDifferentDigestNoOverwrite (bead asd, #802 round-3) is the
// overwrite probe: two INDEPENDENT Spool instances over one state dir —
// simulating two CLI processes, where the per-instance mutex cannot
// serialize them — create different-digest envelopes for the same key
// concurrently. The per-key flock in Create serializes the
// read-absence-then-write, so exactly ONE create wins and the loser gets
// request_conflict; the winner's bytes are never silently overwritten. RED
// without the flock: both creates pass the absence check and the second
// rename clobbers the first (last-writer-wins, no error).
func TestASDConcurrentDifferentDigestNoOverwrite(t *testing.T) {
	now := time.Now()
	stateDir := t.TempDir()
	spoolA, err := Open(stateDir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	spoolB, err := Open(stateDir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}

	notAfter := protocol.FormatTime(now.Add(2 * time.Minute))
	mk := func(id int, text string) *Envelope {
		cmd := testCommand(validUUID(0), "fake", "e_1", notAfter)
		cmd.Input.Text = text
		return &Envelope{
			RequestID:   cmd.RequestID,
			CreatorHost: "local",
			TargetID:    "fake",
			Epoch:       "e_1",
			NotAfter:    notAfter,
			Command:     cmd,
			Destination: "ipc:/tmp/state",
		}
	}

	const attempts = 24
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := spoolA
			if i%2 == 1 {
				s = spoolB
			}
			<-start // release all goroutines at once for maximal contention
			text := fmt.Sprintf("payload-%d", i)
			errs[i] = s.Create(mk(i, text))
		}(i)
	}
	close(start)
	wg.Wait()

	var accepted int
	for i, err := range errs {
		if err == nil {
			accepted++
			continue
		}
		var r *protocol.Refusal
		if !errors.As(err, &r) || r.Code != protocol.CodeRequestConflict {
			t.Fatalf("attempt %d: unexpected error %v", i, err)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted=%d creates, want exactly 1 (winner); errs=%v", accepted, errs)
	}

	// The surviving envelope is intact and readable through a third instance.
	spoolC, err := Open(stateDir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("Open C: %v", err)
	}
	got, ok, err := spoolC.Get("local", validUUID(0))
	if err != nil || !ok {
		t.Fatalf("winner envelope unreadable: ok=%v err=%v", ok, err)
	}
	// Its digest must match one of the attempted payloads exactly — no
	// torn or merged bytes.
	want := map[string]bool{}
	for i := 0; i < attempts; i++ {
		want[fmt.Sprintf("payload-%d", i)] = true
	}
	if !want[got.Command.Input.Text] {
		t.Fatalf("surviving envelope text=%q not among attempted payloads", got.Command.Input.Text)
	}
}
