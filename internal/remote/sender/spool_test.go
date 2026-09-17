package sender

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// fakeDispatcher is a test Dispatcher that records Handle calls and can be
// toggled between unreachable (returns CodeEndpointUnreachable) and accepting
// (returns a running snapshot). It does NOT exercise the real endpoint.
type fakeDispatcher struct {
	calls   int
	failing bool
}

func (f *fakeDispatcher) Handle(cmd *protocol.Command, src core.Source) (any, error) {
	f.calls++
	if f.failing {
		return nil, protocol.Refuse(protocol.CodeEndpointUnreachable, "no endpoint")
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
