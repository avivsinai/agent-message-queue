package sender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

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

// drainRig drains a spool through the real endpoint and the fake runtime,
// the pair serve wires together, so the drainer classifies the replies the
// endpoint really returns.
type drainRig struct {
	t         *testing.T
	stateDir  string
	spool     *Spool
	store     *requests.Store
	ep        *core.Endpoint
	rt        *fake.Runtime
	published []map[string]string
}

func newDrainRig(t *testing.T, now time.Time) *drainRig {
	t.Helper()
	r := &drainRig{t: t, stateDir: t.TempDir()}
	spool, err := Open(r.stateDir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.spool = spool
	r.start()
	return r
}

// start opens the endpoint over the rig's store; after ep.Close it is an
// endpoint restart.
func (r *drainRig) start() {
	r.t.Helper()
	store, err := requests.Open(filepath.Join(r.stateDir, "endpoint"))
	if err != nil {
		r.t.Fatalf("open store: %v", err)
	}
	r.store = store
	r.ep = core.New(core.Config{Store: store, Publish: func(_ protocol.Snapshot, origin map[string]string) error {
		r.published = append(r.published, origin)
		return nil
	}})
	r.rt = fake.New("fake", "e_1")
	r.ep.Register(r.rt)
	ep := r.ep
	r.t.Cleanup(func() { _ = ep.Close() })
}

// enqueue persists a pending envelope the way the CLI does when the endpoint
// is down.
func (r *drainRig) enqueue(id, epoch, notAfter string) *protocol.Command {
	r.t.Helper()
	cmd := testCommand(id, "fake", epoch, notAfter)
	env := &Envelope{RequestID: id, CreatorHost: "local", TargetID: "fake", Epoch: epoch, NotAfter: notAfter, Command: cmd, Destination: "ipc:state"}
	if err := r.spool.Create(env); err != nil {
		r.t.Fatalf("Create: %v", err)
	}
	return cmd
}

func (r *drainRig) drain(now time.Time) int {
	r.t.Helper()
	n, err := NewDrainer(r.spool, r.ep, func() time.Time { return now }).Drain(context.Background())
	if err != nil {
		r.t.Fatalf("Drain: %v", err)
	}
	return n
}

func (r *drainRig) envelope(id string) *Envelope {
	r.t.Helper()
	env, ok, err := r.spool.Get("local", id)
	if err != nil || !ok {
		r.t.Fatalf("Get %s: ok=%v err=%v", id, ok, err)
	}
	return env
}

// recordPath is where the endpoint store keeps a local request's record.
func (r *drainRig) recordPath(id string) string {
	return filepath.Join(r.stateDir, "endpoint", "v1", "requests", "local", "fake__"+id+".json")
}

func (r *drainRig) hasRecord(host, id string) bool {
	r.t.Helper()
	_, ok, err := r.store.Get(requests.Key{CreatorHost: host, TargetID: "fake", RequestID: id})
	if err != nil {
		r.t.Fatalf("store get: %v", err)
	}
	return ok
}

// TestSenderDurablePersistAndDispatch is the happy path: the CLI persists an
// envelope (the endpoint is down), then the drainer replays it (the endpoint
// is up) and the envelope is settled as dispatched. This proves the durable
// sender persists identity/command/digest/target/epoch/expiry BEFORE returning
// submitted, and the drainer retries the same identity+bytes after restart.
func TestSenderDurablePersistAndDispatch(t *testing.T) {
	now := time.Now()
	r := newDrainRig(t, now)
	cmd := r.enqueue(validUUID(0), "e_1", protocol.FormatTime(now.Add(2*time.Minute)))

	// The persisted envelope has the digest and starts pending.
	got := r.envelope(cmd.RequestID)
	if got.State != StatePending {
		t.Fatalf("state=%s, want pending", got.State)
	}
	if got.InputDigest != protocol.CommandDigest(cmd) {
		t.Fatalf("digest mismatch: persisted=%s computed=%s", got.InputDigest, protocol.CommandDigest(cmd))
	}
	if got.CreatedAt == "" {
		t.Fatalf("CreatedAt not set")
	}

	// Restart: a fresh spool opens the same dir and recovers the envelope
	// with the target and epoch the caller supplied, never a substitute.
	reopened, err := Open(r.stateDir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	envs, err := reopened.List()
	if err != nil || len(envs) != 1 {
		t.Fatalf("List: err=%v len=%d", err, len(envs))
	}
	env := envs[0]
	if env.RequestID != cmd.RequestID || env.State != StatePending {
		t.Fatalf("recovered envelope mismatch: %+v", env)
	}
	if env.TargetID != "fake" || env.Epoch != "e_1" || env.Command.Epoch != "e_1" {
		t.Fatalf("recovered target=%s epoch=%s command epoch=%s, want fake/e_1/e_1", env.TargetID, env.Epoch, env.Command.Epoch)
	}

	// The drainer dispatches through the (now-up) endpoint and settles it.
	r.spool = reopened
	if n := r.drain(now); n != 1 {
		t.Fatalf("drained n=%d, want 1", n)
	}
	if got := r.envelope(cmd.RequestID); got.State != StateDispatched {
		t.Fatalf("after drain state=%s, want dispatched", got.State)
	}
	if !r.rt.HasRun(cmd.RequestID) {
		t.Fatal("the endpoint never ran the replayed command")
	}
}

// TestSenderExpireWithoutDispatch proves an envelope whose admission window
// closed while it waited is expired WITHOUT dispatch: the caller is told the
// request expired before the endpoint could admit it, never that it was
// dispatched late.
func TestSenderExpireWithoutDispatch(t *testing.T) {
	now := time.Now()
	r := newDrainRig(t, now)
	cmd := r.enqueue(validUUID(1), "e_1", protocol.FormatTime(now.Add(time.Second)))

	// The deadline passed while the envelope was waiting.
	if n := r.drain(now.Add(2 * time.Minute)); n != 1 {
		t.Fatalf("drained n=%d, want 1 (expired)", n)
	}
	if got := r.envelope(cmd.RequestID); got.State != StateExpired {
		t.Fatalf("state=%s, want expired", got.State)
	}
	if d := r.rt.Snapshot().Dispatches; d != 0 || r.hasRecord("local", cmd.RequestID) {
		t.Fatalf("expired envelope reached the endpoint: dispatches=%d", d)
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

// TestSenderB2RefusalClassification (round-3) is the core B2 regression: the
// endpoint returns a refusal as a protocol.Reply VALUE with Outcome.Code and
// a nil error. Before the fix, classifyReply asserted reply.(*protocol.Reply),
// which never matched, so every refusal became MarkDispatched. A terminal
// refusal (stale epoch, or a mode the endpoint does not support) must drain
// to failed with its code, never retry.
func TestSenderB2RefusalClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		epoch string
		busy  protocol.Busy
		want  protocol.Code
	}{
		{"stale epoch", "e_stale", protocol.BusyReject, protocol.CodeStaleEpoch},
		{"unsupported", "e_1", protocol.BusyQueue, protocol.CodeUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			r := newDrainRig(t, now)
			notAfter := protocol.FormatTime(now.Add(2 * time.Minute))
			cmd := testCommand(validUUID(0), "fake", tc.epoch, notAfter)
			cmd.Input.Busy = tc.busy
			env := &Envelope{RequestID: cmd.RequestID, CreatorHost: "local", TargetID: "fake", Epoch: tc.epoch, NotAfter: notAfter, Command: cmd, Destination: "ipc:state"}
			if err := r.spool.Create(env); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if n := r.drain(now); n != 1 {
				t.Fatalf("drained n=%d, want 1", n)
			}
			got := r.envelope(cmd.RequestID)
			if got.State != StateFailed || got.LastError != string(tc.want) {
				t.Fatalf("state=%s last_error=%s, want failed/%s", got.State, got.LastError, tc.want)
			}
			if r.hasRecord("local", cmd.RequestID) {
				t.Fatal("the refused submit left a record")
			}
		})
	}
}

// TestSenderTransientRefusalRetriesNextTick pins round-4 N2: a transient
// refusal keeps the envelope pending (MarkAttempt) and the same envelope
// dispatches on a later tick, bounded only by NotAfter. Busy IS transient: an
// offline-enqueued submit that meets one busy tick must not die as failed.
// An endpoint shutting down refuses with draining, and the envelope waits
// for the restarted endpoint.
func TestSenderTransientRefusalRetriesNextTick(t *testing.T) {
	cases := []struct {
		name       string
		code       protocol.Code
		makeBusy   func(r *drainRig, notAfter string)
		makeUnbusy func(r *drainRig)
	}{
		{
			name: "busy",
			code: protocol.CodeBusy,
			makeBusy: func(r *drainRig, notAfter string) {
				// Another local request holds the target.
				if _, err := r.ep.Handle(testCommand(validUUID(2), "fake", "e_1", notAfter), core.Source{Host: "local"}); err != nil {
					r.t.Fatalf("occupy target: %v", err)
				}
			},
			makeUnbusy: func(r *drainRig) {
				r.rt.Complete(validUUID(2), "done")
				if err := r.ep.Tick(); err != nil {
					r.t.Fatalf("tick: %v", err)
				}
			},
		},
		{
			name:       "draining",
			code:       protocol.CodeDraining,
			makeBusy:   func(r *drainRig, _ string) { _ = r.ep.Close() },
			makeUnbusy: func(r *drainRig) { r.start() },
		},
		{
			// A store read that fails without a typed refusal is the
			// endpoint being unreachable for now, not a refusal of the
			// request (Pro review of the carrier cull).
			name: "endpoint unreachable",
			code: protocol.CodeEndpointUnreachable,
			makeBusy: func(r *drainRig, _ string) {
				if err := os.MkdirAll(r.recordPath(validUUID(0)), 0o700); err != nil {
					r.t.Fatalf("obstruct record: %v", err)
				}
			},
			makeUnbusy: func(r *drainRig) {
				if err := os.Remove(r.recordPath(validUUID(0))); err != nil {
					r.t.Fatalf("clear record: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			r := newDrainRig(t, now)
			notAfter := protocol.FormatTime(now.Add(2 * time.Minute))
			cmd := r.enqueue(validUUID(0), "e_1", notAfter)
			tc.makeBusy(r, notAfter)

			// Tick 1: refused, still pending.
			if n := r.drain(now); n != 1 {
				t.Fatalf("tick 1: drained n=%d, want 1", n)
			}
			got := r.envelope(cmd.RequestID)
			if got.State != StatePending || got.Attempt != 1 || got.LastError != string(tc.code) {
				t.Fatalf("tick 1: state=%s attempt=%d last_error=%s, want pending/1/%s", got.State, got.Attempt, got.LastError, tc.code)
			}
			if r.rt.HasRun(cmd.RequestID) {
				t.Fatal("tick 1: the refused submit ran")
			}

			// Tick 2: the target is free; the same envelope dispatches.
			tc.makeUnbusy(r)
			if n := r.drain(now); n != 1 {
				t.Fatalf("tick 2: drained n=%d, want 1", n)
			}
			if got := r.envelope(cmd.RequestID); got.State != StateDispatched {
				t.Fatalf("tick 2: state=%s, want dispatched", got.State)
			}
			if !r.rt.HasRun(cmd.RequestID) {
				t.Fatal("tick 2: the endpoint never ran the command")
			}
		})
	}
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

// TestSenderDrainRefusesEditedSpoolFiles reproduces agent-message-queue-611.47:
// the drainer replayed a spool file another writer changed, so a file could
// dispatch interaction.respond, or a submit as a Buzz share, with a copied
// origin or host. Only a valid local submit is dispatched, never with an origin.
func TestSenderDrainRefusesEditedSpoolFiles(t *testing.T) {
	now := time.Now()
	r := newDrainRig(t, now)
	notAfter := protocol.FormatTime(now.Add(2 * time.Minute))
	edit := func(id string, change func(m map[string]any)) {
		t.Helper()
		cmd := r.enqueue(id, "e_1", notAfter)
		name, err := r.spool.filename(Key{CreatorHost: "local", RequestID: cmd.RequestID})
		if err != nil {
			t.Fatalf("filename: %v", err)
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		change(m)
		if raw, err = json.Marshal(m); err != nil {
			t.Fatalf("encode: %v", err)
		}
		if err := os.WriteFile(name, raw, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	origin := map[string]any{"carrier": "buzz", "body": "body-1", "channel": "dm-1"}
	edit(validUUID(0), func(m map[string]any) {
		m["command"].(map[string]any)["op"] = string(protocol.OpInteractionRespond)
		m["origin"] = origin
	})
	edit(validUUID(1), func(m map[string]any) { m["origin"] = origin })
	edit(validUUID(2), func(m map[string]any) { m["creator_host"] = "buzz-0123456789abcdef" })

	// Only the submit with a copied origin field is still a valid local
	// submit; the respond and the foreign host are never handed over.
	if n := r.drain(now); n != 1 {
		t.Fatalf("drained n=%d, want 1 (only the valid local submit)", n)
	}
	if !r.rt.HasRun(validUUID(1)) || r.rt.HasRun(validUUID(2)) || r.hasRecord("buzz-0123456789abcdef", validUUID(2)) {
		t.Fatal("dispatched set is not exactly the valid local submit")
	}
	if len(r.published) == 0 {
		t.Fatal("the dispatched submit published no revision")
	}
	for _, o := range r.published {
		if o != nil {
			t.Fatalf("dispatched submit carries origin %v, want none", o)
		}
	}
}
