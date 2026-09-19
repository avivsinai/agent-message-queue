// Package core is the amq-remote endpoint: it owns request identity and
// records, runs the admission algorithm against native attachments, applies
// native evidence, reconciles after a restart, and publishes revisions. It
// does not know how commands arrive; carriers decode with protocol and call
// Handle.
package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Source is the authenticated origin of a command. Host is the peer host from
// the AMQ carrier or "local" for the CLI; it is never a claimed handle.
type Source struct {
	Host string
	// Origin is carrier routing kept on the record so publication can find
	// its way back after a restart. It carries no authority.
	Origin map[string]string
}

// Publisher receives every new revision with the record's carrier origin. A
// returned error leaves the revision unpublished; Reconcile republishes it
// later. Republishing the same revision is safe: receivers upsert by request
// and revision.
type Publisher func(protocol.Snapshot, map[string]string) error

// CrashPoint is a test hook that may return an error at a named boundary to
// simulate a process death there. Production endpoints leave it nil.
type CrashPoint func(point string) error

// ErrCrashed is returned by Handle when the crash hook fired.
var ErrCrashed = errors.New("endpoint crashed at boundary")

// ErrDrainIncomplete is returned by Close when the bounded shutdown drain
// ended with publication obligations it could not discharge: a chained
// revision was skipped (its attempt failed or the store was closed before it
// could be adopted) or handlers were still in flight at the drain timeout.
// The durable unpublished state is preserved for Reconcile recovery on the
// next start; Close does NOT report the shutdown as clean.
var ErrDrainIncomplete = errors.New("drain incomplete")

// Crash boundary names, in the order the design lists them.
const (
	PointBeforeReceived    = "before:received_commit"
	PointAfterReceived     = "after:received_commit"
	PointBeforeDispatching = "before:dispatching_commit"
	PointAfterDispatching  = "after:dispatching_commit"
	PointBeforeNative      = "before:native_submit"
	PointAfterNative       = "after:native_submit"
	PointBeforeResult      = "before:result_commit"
	PointAfterResult       = "after:result_commit"
	PointBeforeAck         = "before:native_ack"
	PointAfterAck          = "after:native_ack"
	PointBeforePublish     = "before:publish"
	PointAfterPublish      = "after:publish"
	PointBeforePublished   = "before:published_revision_commit"
)

type target struct {
	att         Attachment
	unsubscribe func()
}

// lifecycleState is the endpoint's phase: accepting commands, draining
// in-flight handlers before Close, or closed. The state is read+written
// under e.mu; the drain wait uses e.drained (a condition variable) so
// Close blocks until the last in-flight handler exits.
type lifecycleState int

const (
	stateAccepting lifecycleState = iota
	stateDraining
	stateClosed
)

// drainTimeout is the bound on how long Close waits for in-flight handlers.
// It is generous (30s) because a single handler's worst case is one native
// RPC (20s timeout in the codex adapter) plus a commit. If a handler has
// not returned by then, it is wedged and the endpoint closes anyway — the
// store's closed flag rejects further mutations from the stale handler.
const drainTimeout = 30 * time.Second

// Endpoint is the request handler. One Endpoint owns one Store.
type Endpoint struct {
	mu             sync.Mutex
	store          *requests.Store
	targets        map[string]*target
	publish        Publisher
	crash          CrashPoint
	now            func() time.Time
	observers      []func(*requests.Record)
	changed        chan struct{}
	compactHorizon time.Duration
	lastCompact    time.Time

	// visible records revisions the publisher has already delivered whose
	// published_revision marker did not commit. Reconcile then retries the
	// MARKER, never the delivery: the message is in the caller's mailbox and
	// re-delivery after consumption is a duplicate (Pro r2 #13 / packet 4a,
	// agent-message-queue-611.22.36). In memory only: a process crash in
	// that window re-delivers, the documented exception.
	//
	// 611.22.48: visible means CONFIRMED DELIVERY awaiting its marker, NOT an
	// in-flight attempt. A separate `publishing` set owns in-flight attempts
	// so a failed publish cannot be mistaken for a delivered revision, and a
	// concurrent caller cannot clear a reservation it does not own.
	visible map[requests.Key]int64
	// publishing records keys with an in-flight publish attempt. Only one
	// publish per key runs at a time; concurrent callers for the same key see
	// it here and leave the work pending for the next Reconcile/Tick. The
	// owner clears only its own entry on completion (611.22.48).
	publishing map[requests.Key]bool
	// pubPending records accepted-but-skipped publication requests per key:
	// a publishLocked caller that hit publishing[key] coalesces its revision
	// here (highest requested revision wins). The finishing publisher of that
	// key — the single per-key owner — adopts the pending revision and
	// republishes it BEFORE releasing its drain obligation, so every accepted
	// revision is covered by exactly one obligation (its own in-flight
	// attempt or the owner's chained attempt) and Close's drain waits for the
	// whole chain (Astra B784-1, 611.22.48 follow-up round 2).
	pubPending map[requests.Key]int64
	// B13 lifecycle: state transitions accepting -> draining -> closed.
	// inFlight counts handlers between entry (registerInFlight) and exit
	// (releaseInFlight). drained is a condition variable Close waits on.
	// The entry check + increment happen in ONE critical section so a
	// transition to draining cannot slip a handler in after the gate.
	state    lifecycleState
	inFlight int
	drained  *sync.Cond
	drainTO  time.Duration
	// drainObligations records, under e.mu, the highest undischarged pending
	// publication revision for a key whose chained attempt was skipped (its
	// continuation read failed, or the endpoint closed before adoption). It is
	// per-key/per-revision, not a global latch: a later successful publication
	// of that revision (or a strictly newer one) retires the entry, so Close
	// reports an incomplete drain ONLY for obligations genuinely left
	// unpublished (codex round-4 P2-1). A failed attempt of attemptRev itself
	// is NOT an obligation: the attempt ran and was counted; the durable
	// unpublished state persists for Reconcile recovery.
	drainObligations map[requests.Key]int64
}

// Config configures New.
type Config struct {
	Store   *requests.Store
	Publish Publisher
	Crash   CrashPoint
	Now     func() time.Time
	// CompactHorizon is the age at which terminal, settled records become
	// eligible for compaction. Zero (default) disables compaction.
	CompactHorizon time.Duration
	// DrainTimeout overrides the default Close drain timeout. Tests use a
	// short value to prove the bound without sleeping 30s.
	DrainTimeout time.Duration
}

// New builds an endpoint over an open store. Attachments register through
// Register; Reconcile should run before the first command.
func New(cfg Config) *Endpoint {
	e := &Endpoint{
		store:            cfg.Store,
		targets:          map[string]*target{},
		publish:          cfg.Publish,
		crash:            cfg.Crash,
		now:              cfg.Now,
		changed:          make(chan struct{}),
		compactHorizon:   cfg.CompactHorizon,
		visible:          map[requests.Key]int64{},
		publishing:       map[requests.Key]bool{},
		pubPending:       map[requests.Key]int64{},
		drainObligations: map[requests.Key]int64{},
		state:            stateAccepting,
		drainTO:          drainTimeout,
	}
	if cfg.DrainTimeout > 0 {
		e.drainTO = cfg.DrainTimeout
	}
	e.drained = sync.NewCond(&e.mu)
	if e.now == nil {
		e.now = time.Now
	}
	if e.publish == nil {
		e.publish = func(protocol.Snapshot, map[string]string) error { return nil }
	}
	return e
}

// SetPublish replaces the publisher after construction. Serve uses this
// to break the circular dependency between the carrier and the endpoint:
// openServeStore creates the endpoint with a nil publish, then serve wires
// the carrier in once it exists.
func (e *Endpoint) SetPublish(p Publisher) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p == nil {
		p = func(protocol.Snapshot, map[string]string) error { return nil }
	}
	e.publish = p
}

// Observe registers a callback for every record write. Tests use it to
// collect state history; production uses it for the activity ring.
func (e *Endpoint) Observe(fn func(*requests.Record)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.observers = append(e.observers, fn)
}

// Register attaches a runtime under its target id and subscribes to its
// native events. Registering the same target again replaces the attachment.
func (e *Endpoint) Register(att Attachment) {
	s := att.Inspect()
	e.mu.Lock()
	defer e.mu.Unlock()
	if old, ok := e.targets[s.TargetID]; ok && old.unsubscribe != nil {
		old.unsubscribe()
	}
	t := &target{att: att}
	t.unsubscribe = att.Subscribe(func(ev NativeEvent) { e.onNative(s.TargetID, ev) })
	e.targets[s.TargetID] = t
}

// UnregisterAll unsubscribes every attachment without closing the store:
// reconcile tests use it to drop a target mid-flight (the restart shape).
// Part of B14a's small surface, matching the reviewed recut.
func (e *Endpoint) UnregisterAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, t := range e.targets {
		if t.unsubscribe != nil {
			t.unsubscribe()
		}
		delete(e.targets, id)
	}
}

// Close transitions the endpoint to draining, waits for in-flight handlers
// to finish (bounded by drainTimeout), then unsubscribes attachments and
// closes the store. A handler that never returns is abandoned: after the
// timeout, Close proceeds anyway and the store's closed flag rejects any
// further mutation the stale handler attempts. Returns an error naming how
// many handlers were still in flight if the bound was exceeded.
func (e *Endpoint) Close() error {
	e.mu.Lock()
	if e.state == stateClosed {
		e.mu.Unlock()
		return nil
	}
	e.state = stateDraining
	// Wait for in-flight handlers to drain, bounded by drainTO. The timer
	// sets a flag and broadcasts so the loop exits even if inFlight > 0.
	if e.inFlight > 0 {
		timedOut := false
		timer := time.AfterFunc(e.drainTO, func() {
			e.mu.Lock()
			timedOut = true
			e.drained.Broadcast()
			e.mu.Unlock()
		})
		defer timer.Stop()
		for e.inFlight > 0 && !timedOut {
			e.drained.Wait()
		}
	}
	stale := e.inFlight
	e.state = stateClosed
	for id, t := range e.targets {
		if t.unsubscribe != nil {
			t.unsubscribe()
		}
		delete(e.targets, id)
	}
	// Report undischarged publication obligations ONCE, then clear them:
	// Close is terminal, so the durable unpublished state is left for
	// Reconcile recovery on the next start regardless. The map is per-key so
	// a later successful publication (this Close path or a concurrent
	// Reconcile) that already retired an entry does not get re-reported
	// (codex round-4 P2-1).
	skipped := len(e.drainObligations)
	e.drainObligations = map[requests.Key]int64{}
	e.mu.Unlock()
	err := e.store.Close()
	if stale > 0 {
		return fmt.Errorf("endpoint closed with %d handler(s) still in flight after %s drain timeout", stale, e.drainTO)
	}
	if skipped > 0 {
		if err != nil {
			return fmt.Errorf("%w (store close error: %v)", ErrDrainIncomplete, err)
		}
		return fmt.Errorf("%w: accepted revision(s) left unpublished (durable state preserved for Reconcile recovery)", ErrDrainIncomplete)
	}
	return err
}

// releaseInFlight decrements the in-flight count and wakes Close if it was
// waiting for handlers to drain. Called by Handle's defer.
func (e *Endpoint) releaseInFlight() {
	e.mu.Lock()
	e.inFlight--
	if e.inFlight == 0 && e.state == stateDraining {
		e.drained.Broadcast()
	}
	e.mu.Unlock()
}

// beginReconcile gates Reconcile/Tick on the lifecycle state and registers the
// sweep as in-flight in ONE critical section, mirroring Handle's entry gate
// (611.22.47). Without this, a Tick racing Close could start new native work
// (admitDeferred -> att.Submit, reconcileCancelRetry -> att.CancelExact,
// ack replay -> att.AcknowledgeResult) after the endpoint began draining:
// Close waited only on Handle's inFlight, so shutdown began work it would not
// finish. Now Tick refuses to start once draining has begun, and when it does
// start it is counted in inFlight so Close's drain wait bounds it. The caller
// MUST defer releaseInFlight when ok is true.
func (e *Endpoint) beginReconcile() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != stateAccepting {
		return false
	}
	e.inFlight++
	return true
}

// InFlight returns the current number of in-flight handlers. Test seam for
// observing the drain state without a sleep.
func (e *Endpoint) InFlight() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inFlight
}

// IsDraining reports whether the endpoint has begun shutdown (state is
// draining or closed). Test seam for synchronizing tests on the actual
// lifecycle transition instead of an elapsed-time sleep (611.22.47, 611.22.48).
func (e *Endpoint) IsDraining() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state != stateAccepting
}

// IsDrainingState reports whether the endpoint is in the DRAINING state
// specifically (not closed). This distinguishes 'Close is waiting on
// in-flight handlers' from 'Close already set stateClosed and is past the
// drain wait', which IsDraining conflates. Test seam for the 611.22.48
// close-wait ordering regression: a test that releases a held publish after
// observing IsDraining()==true must fail if what it actually observed was
// stateClosed (Close raced past the drain without waiting).
func (e *Endpoint) IsDrainingState() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state == stateDraining
}

// Handle runs one validated command from an authenticated source and returns
// the reply document: a Snapshot for request.* and interaction.respond, a
// Session or []Session for session.*.
func (e *Endpoint) Handle(cmd *protocol.Command, src Source) (any, error) {
	if err := cmd.Validate(); err != nil {
		return nil, err
	}
	if src.Host == "" {
		return nil, protocol.Refuse(protocol.CodeInvalid, "command source host is required")
	}
	// B13 lifecycle: check state and register as in-flight in ONE critical
	// section. A command arriving during draining/closed is refused with an
	// action-required code (the caller retries) — never a failure code that
	// would be recorded as the command's outcome.
	e.mu.Lock()
	if e.state != stateAccepting {
		e.mu.Unlock()
		return nil, protocol.Refuse(protocol.CodeDraining, "endpoint is shutting down; retry the command after restart")
	}
	e.inFlight++
	e.mu.Unlock()
	defer e.releaseInFlight()

	switch cmd.Op {
	case protocol.OpRequestSubmit:
		return e.submit(cmd, src)
	case protocol.OpRequestGet:
		return e.get(cmd)
	case protocol.OpRequestCancel:
		return e.cancel(cmd, src)
	case protocol.OpSessionList:
		return e.list(), nil
	case protocol.OpSessionInspect:
		return e.inspect(cmd.TargetID)
	case protocol.OpSessionEvents:
		return e.inspect(cmd.TargetID)
	case protocol.OpInteractionRespond:
		return e.respond(cmd)
	}
	return nil, protocol.Refuse(protocol.CodeInvalid, "unknown op")
}

func (e *Endpoint) submit(cmd *protocol.Command, src Source) (protocol.Reply, error) {
	// D1 (bead 611.22.23): busy=queue and deliver=steer are disabled in v1 —
	// their ownership+evidence models do not exist yet (steer would overwrite
	// the running run's byTurn mapping; queue would admit without a NotAfter
	// check at native admission or a per-runtime reservation). The gate sits
	// BEFORE any durable write, so a disabled-mode submit consumes nothing
	// and a plain retry with reject/turn succeeds. Full model deferred to a
	// follow-up.
	if cmd.Input != nil && (cmd.Input.Busy == protocol.BusyQueue || cmd.Input.Deliver == protocol.DeliverSteer) {
		mode, value := "busy", string(cmd.Input.Busy)
		if cmd.Input.Deliver == protocol.DeliverSteer {
			mode, value = "deliver", string(cmd.Input.Deliver)
		}
		return protocol.Reply{}, protocol.Refuse(protocol.CodeUnsupported,
			"%s=%s is disabled in v1; full ownership/evidence model deferred — use busy=reject deliver=turn", mode, value)
	}
	key := requests.Key{CreatorHost: src.Host, TargetID: cmd.TargetID, RequestID: cmd.RequestID}
	digest := protocol.CommandDigest(cmd)

	e.mu.Lock()
	rec, exists, err := e.store.Get(key)
	if err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	if exists {
		// A busy-rejected tombstone from a previous attempt is not the
		// request itself: the identical resubmit must be admitted normally,
		// not answered from the tombstone (B14 minimal tombstone semantics —
		// full B2/B11 safety/reset belongs to B14d). Recognizable as a
		// tombstoned rejected record with no dispatch behind it and the same
		// digest; only a DIFFERENT digest is a conflict.
		retryable := rec.Tombstone &&
			rec.State == protocol.StateRejected &&
			rec.Code == protocol.CodeBusy &&
			rec.NativeDispatches == 0 &&
			rec.InputDigest == digest
		if !retryable {
			e.mu.Unlock()
			out := protocol.Outcome{Op: protocol.OpRequestSubmit}
			if rec.InputDigest != "" && rec.InputDigest != digest {
				out.Code = protocol.CodeRequestConflict
			}
			return protocol.Reply{Snapshot: rec.Snapshot, Outcome: out}, nil
		}
		// Fall through to the normal admission path with the existing
		// record: the deferred/Tick machinery owns it from here.
		t, code := e.admissibleLocked(cmd.TargetID, cmd.Epoch, cmd.NotAfter, cmd.Input.MinEvidence)
		if code != "" {
			e.mu.Unlock()
			return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: code}}, nil
		}
		if t == nil {
			e.mu.Unlock()
			return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit}}, nil
		}
		reserved, rerr := e.runtimeInFlightLocked(cmd.TargetID, key)
		if rerr != nil {
			e.mu.Unlock()
			return protocol.Reply{}, protocol.Refuse(protocol.CodeAttachmentLost, "%s", rerr.Error())
		}
		if reserved {
			// Still reserved: refresh the terminal busy tombstone and refuse
			// again — never a Tick-admissible placeholder.
			e.transitionLocked(rec, causeBusyTombstone, nativeEvidence{})
			if _, err := e.commitLocked(rec, e.targets[rec.TargetID]); err != nil {
				e.mu.Unlock()
				return protocol.Reply{}, err
			}
			snap := rec.Snapshot
			e.mu.Unlock()
			e.publishRevision(rec)
			return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: rec.Code}}, nil
		}
		rec.Revision++
		rec.NativeDispatches = 1
		rec.ObservedAt = protocol.FormatTime(e.now())
		e.transitionLocked(rec, causeDispatching, nativeEvidence{})
		if err := e.crashAt(PointBeforeDispatching); err != nil {
			e.mu.Unlock()
			return protocol.Reply{}, err
		}
		// Cannot use commitLocked: crash-point ordering requires Update between
		// PointBeforeDispatching and PointAfterDispatching.
		if err := e.store.Update(rec); err != nil {
			e.mu.Unlock()
			return protocol.Reply{}, err
		}
		e.notifyLocked(rec)
		if err := e.crashAt(PointAfterDispatching); err != nil {
			e.mu.Unlock()
			return protocol.Reply{}, err
		}
		input := *cmd.Input
		e.mu.Unlock()
		if err := e.crashAt(PointBeforeNative); err != nil {
			return protocol.Reply{}, err
		}
		adm, nerr := t.att.Submit(BoundRequest{Key: key, Epoch: cmd.Epoch, Input: input, NotAfter: cmd.NotAfter})
		if err := e.crashAt(PointAfterNative); err != nil {
			return protocol.Reply{}, err
		}
		e.mu.Lock()
		rec, exists, err = e.store.Get(key)
		if err != nil {
			e.mu.Unlock()
			return protocol.Reply{}, err
		}
		return e.finishAdmissionLocked(rec, exists, t, adm, nerr)
	}
	rec = &requests.Record{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   cmd.RequestID,
			CreatorHost: src.Host,
			TargetID:    cmd.TargetID,
			Epoch:       cmd.Epoch,
			Revision:    1,
			State:       protocol.StateReceived,
			InputDigest: digest,
			NotAfter:    cmd.NotAfter,
			ObservedAt:  protocol.FormatTime(e.now()),
		},
		Input:  cmd.Input,
		Origin: src.Origin,
	}
	// Decide admissibility + reservation BEFORE Create: a request we are
	// about to refuse is never written (no durable placeholder to leak on a
	// failed rejection write, and nothing for Tick to dispatch). Only Create
	// when we will dispatch or deliberately defer.
	t, code := e.admissibleLocked(cmd.TargetID, cmd.Epoch, cmd.NotAfter, cmd.Input.MinEvidence)
	if code != "" {
		// Refused (unshared/expired/stale_epoch): no durable record.
		e.mu.Unlock()
		return protocol.Reply{Snapshot: e.unpersisted(rec, protocol.StateRejected, code), Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: code}}, nil
	}
	if t == nil {
		// Target registered but offline: Create received and let Tick admit
		// or expire it later.
		if err := e.crashAt(PointBeforeReceived); err != nil {
			e.mu.Unlock()
			return protocol.Reply{}, err
		}
		if err := e.store.Create(rec); err != nil {
			e.mu.Unlock()
			var r *protocol.Refusal
			if errors.As(err, &r) && r.Code == protocol.CodeStorageFull {
				return protocol.Reply{Snapshot: e.unpersisted(rec, protocol.StateRejected, protocol.CodeStorageFull), Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: protocol.CodeStorageFull}}, nil
			}
			return protocol.Reply{}, err
		}
		e.notifyLocked(rec)
		e.mu.Unlock()
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit}}, nil
	}
	// B14 per-runtime reservation (B10): refuse a second concurrent dispatch
	// for the same target. Fail-closed — an undeterminable reservation state
	// never authorizes a dispatch.
	reserved, rerr := e.runtimeInFlightLocked(cmd.TargetID, key)
	if rerr != nil {
		// An undeterminable reservation must not leave a Tick-admissible
		// placeholder. No durable record; return a typed refusal so the caller
		// maps to the right exit, not exit 1.
		e.mu.Unlock()
		return protocol.Reply{Snapshot: e.unpersisted(rec, protocol.StateRejected, protocol.CodeAttachmentLost), Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: protocol.CodeAttachmentLost}}, protocol.Refuse(protocol.CodeAttachmentLost, "%s", rerr.Error())
	}
	if reserved {
		// Persist a TERMINAL rejected+busy tombstone directly (one write, no
		// received-then-reject window). The tombstone keeps dedup and makes an
		// identical resubmit re-admittable.
		e.transitionLocked(rec, causeBusyTombstone, nativeEvidence{})
		rec.Revision = 1
		rec.ObservedAt = protocol.FormatTime(e.now())
		if err := e.store.Create(rec); err != nil {
			e.mu.Unlock()
			return protocol.Reply{Snapshot: e.unpersisted(rec, protocol.StateRejected, protocol.CodeBusy), Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: protocol.CodeBusy}}, nil
		}
		e.notifyLocked(rec)
		busySnap := rec.Snapshot
		e.mu.Unlock()
		e.publishRevision(rec)
		return protocol.Reply{Snapshot: busySnap, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: rec.Code}}, nil
	}
	// 611.22.19 BK4: reserve room for the accepted work plus its bounded
	// result BEFORE dispatch, so storage pressure fails closed with
	// storage_full here rather than wedging mid-flight or silently evicting
	// a dedup tombstone for an active epoch. The reservation is for one
	// worst-case record (MaxRecordBytes covers a full-size result plus input
	// and overhead); the actual write re-checks under the store lock. Local
	// native harness work never reaches this store, so quota pressure cannot
	// stop it. A disabled quota (zero) reserves nothing.
	if err := e.store.Reserve(key, protocol.MaxRecordBytes); err != nil {
		var r *protocol.Refusal
		if errors.As(err, &r) && r.Code == protocol.CodeStorageFull {
			e.mu.Unlock()
			return protocol.Reply{Snapshot: e.unpersisted(rec, protocol.StateRejected, protocol.CodeStorageFull), Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: protocol.CodeStorageFull}}, nil
		}
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	// Admissible + not reserved: Create as received, then dispatch.
	if err := e.crashAt(PointBeforeReceived); err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	if err := e.store.Create(rec); err != nil {
		e.store.ReleaseReservation(key)
		e.mu.Unlock()
		var r *protocol.Refusal
		if errors.As(err, &r) && r.Code == protocol.CodeStorageFull {
			return protocol.Reply{Snapshot: e.unpersisted(rec, protocol.StateRejected, protocol.CodeStorageFull), Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: protocol.CodeStorageFull}}, nil
		}
		return protocol.Reply{}, err
	}
	e.notifyLocked(rec)
	if err := e.crashAt(PointAfterReceived); err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	rec.Revision++
	rec.NativeDispatches = 1
	rec.ObservedAt = protocol.FormatTime(e.now())
	e.transitionLocked(rec, causeDispatching, nativeEvidence{})
	if err := e.crashAt(PointBeforeDispatching); err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	// Cannot use commitLocked: crash-point ordering requires Update between
	// PointBeforeDispatching and PointAfterDispatching.
	if err := e.store.Update(rec); err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	e.notifyLocked(rec)
	if err := e.crashAt(PointAfterDispatching); err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	e.mu.Unlock()

	if err := e.crashAt(PointBeforeNative); err != nil {
		return protocol.Reply{}, err
	}
	adm, nerr := t.att.Submit(BoundRequest{Key: key, Epoch: cmd.Epoch, Input: *cmd.Input, NotAfter: cmd.NotAfter})
	if err := e.crashAt(PointAfterNative); err != nil {
		return protocol.Reply{}, err
	}

	// No defer: finishAdmissionLocked unlocks exactly once on every return.
	e.mu.Lock()
	rec, _, err = e.store.Get(key)
	if err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	return e.finishAdmissionLocked(rec, true, t, adm, nerr)
}

// admissibleLocked checks share, epoch, expiry, capability and the caller's
// minimum evidence floor. It returns the live target, or nil with an empty
// code when the target is registered but offline, or nil with a refusal code.
// The evidence floor (MinEvidence) is checked here, BEFORE any side effect: an
// attachment whose strongest submit evidence is weaker than the floor is
// refused (weaker capability is refused, not substituted — ADR invariant 5).
func (e *Endpoint) admissibleLocked(targetID, epoch, notAfter, minEvidence string) (*target, protocol.Code) {
	t, ok := e.targets[targetID]
	if !ok {
		return nil, protocol.CodeUnshared
	}
	s := t.att.Inspect()
	if s.Epoch != epoch {
		return nil, protocol.CodeStaleEpoch
	}
	deadline, err := protocol.ParseTime(notAfter)
	if err != nil || e.now().After(deadline) {
		return nil, protocol.CodeExpired
	}
	if !s.Capabilities.Submit {
		return nil, protocol.CodeUnsupported
	}
	// Evidence floor: an omitted floor preserves legacy semantics (any
	// evidence is admitted). A supplied floor is checked before dispatch so a
	// caller that needs `admitted` is refused by an adapter that can only
	// prove `submitted`, instead of being silently given the weaker guarantee.
	// A nil evidence projection is class "" (proves nothing), so it is refused
	// by any non-empty floor — fail closed, never dispatch under a floor the
	// attachment cannot meet.
	if minEvidence != "" {
		if s.Evidence == nil || !protocol.EvidenceClassMeets(s.Evidence.Submit, minEvidence) {
			return nil, protocol.CodeUnsupported
		}
	}
	if s.Attachment == "offline" {
		return nil, ""
	}
	return t, ""
}

// achievedEvidence returns the live session's submit evidence class for a
// target, for the human projection on a submit reply (the machine contract
// floor lives on SubmitInput). Empty when the target is nil or the session
// carries no evidence projection (legacy/unknown adapter). Callers must NOT
// hold e.mu when calling Inspect (it may lock the adapter).
func achievedEvidence(t *target) string {
	if t == nil {
		return ""
	}
	s := t.att.Inspect()
	if s.Evidence == nil {
		return ""
	}
	return s.Evidence.Submit
}

func (e *Endpoint) unpersisted(rec *requests.Record, state protocol.State, code protocol.Code) protocol.Snapshot {
	s := rec.Snapshot
	s.RequestRef = protocol.EncodeRef(rec.CreatorHost, rec.TargetID, rec.RequestID)
	s.State = state
	s.Code = code
	return s
}

func (e *Endpoint) get(cmd *protocol.Command) (protocol.Reply, error) {
	host, targetID, requestID, err := protocol.DecodeRef(cmd.RequestRef)
	if err != nil {
		return protocol.Reply{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, ok, err := e.store.Get(requests.Key{CreatorHost: host, TargetID: targetID, RequestID: requestID})
	if err != nil {
		return protocol.Reply{}, err
	}
	if !ok {
		return protocol.Reply{}, protocol.Refuse(protocol.CodeNotFound, "no record for request_ref")
	}
	return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestGet}}, nil
}

func (e *Endpoint) cancel(cmd *protocol.Command, src Source) (protocol.Reply, error) {
	host, targetID, requestID, err := protocol.DecodeRef(cmd.RequestRef)
	if err != nil {
		return protocol.Reply{}, err
	}
	if targetID != cmd.TargetID {
		return protocol.Reply{}, protocol.Refuse(protocol.CodeInvalid, "request_ref names a different target")
	}
	key := requests.Key{CreatorHost: host, TargetID: targetID, RequestID: requestID}
	now := protocol.FormatTime(e.now())

	e.mu.Lock()
	rec, exists, err := e.store.Get(key)
	if err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	if !exists {
		// Cancel arrived before its submit: leave a tombstone that a later
		// submit cannot execute past.
		rec = &requests.Record{
			Snapshot: protocol.Snapshot{
				Schema:      protocol.SchemaRequest,
				RequestID:   requestID,
				CreatorHost: host,
				TargetID:    targetID,
				Epoch:       cmd.Epoch,
				Revision:    1,
				State:       protocol.StateCancelled,
				Code:        protocol.CodeCancelledBeforeAdmission,
				Cancel:      &protocol.Cancel{RequestedAt: now, Disposition: protocol.CancelConfirmed},
				ObservedAt:  now,
			},
			Tombstone: true,
			Origin:    src.Origin,
		}
		err := e.store.Create(rec)
		if err != nil {
			e.mu.Unlock()
			return protocol.Reply{}, err
		}
		e.notifyLocked(rec)
		e.mu.Unlock()
		e.publishRevision(rec)
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel}}, nil
	}
	// Validate the cancel command against the original request binding rather
	// than silently substituting the record's trusted values (Pro B04). A
	// cancel naming a different epoch is for a generation that no longer
	// exists; it must not interrupt the current run.
	if cmd.Epoch != rec.Epoch {
		e.mu.Unlock()
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel, Code: protocol.CodeStaleEpoch}}, nil
	}
	if exp, perr := protocol.ParseTime(cmd.NotAfter); perr == nil && e.now().After(exp) {
		// The cancel command's own admission window has closed; do not act on
		// a stale interrupt.
		e.mu.Unlock()
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel, Code: protocol.CodeExpired}}, nil
	}
	if rec.State.Terminal() {
		e.mu.Unlock()
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel, Disposition: protocol.CancelNoopTerminal}}, nil
	}
	if rec.State == protocol.StateReceived {
		e.transitionLocked(rec, causeCancelledBeforeAdmission, nativeEvidence{})
		if _, err := e.commitLocked(rec, e.targets[targetID]); err != nil {
			e.mu.Unlock()
			return protocol.Reply{}, err
		}
		snap := rec.Snapshot
		e.mu.Unlock()
		e.publishRevision(rec)
		return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel, Disposition: protocol.CancelConfirmed}}, nil
	}
	t := e.targets[targetID]
	e.mu.Unlock()

	var ev CancelEvidence
	var nerr error
	if t == nil {
		ev = CancelEvidence{Disposition: protocol.CancelRequested, Message: "attachment offline; intent recorded"}
	} else {
		ev, nerr = t.att.CancelExact(key, rec.Epoch)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	rec, _, err = e.store.Get(key)
	if err != nil {
		return protocol.Reply{}, err
	}
	if rec.State.Terminal() {
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel, Disposition: protocol.CancelNoopTerminal}}, nil
	}
	if nerr != nil {
		ev = CancelEvidence{Disposition: protocol.CancelRequested, Message: nerr.Error()}
	}
	if ev.Disposition == protocol.CancelConfirmed {
		e.transitionLocked(rec, causeCancelledByRequest, nativeEvidence{})
	} else {
		// cancel_requested (intent recorded, run not stopped) or noop_terminal
		// (run already done, nothing to stop). Keep the disposition without
		// going terminal; reconcile re-drives if a run is bound.
		// Pro #5: guard against downgrading a confirmed cancel.
		if rec.Cancel == nil {
			rec.Cancel = &protocol.Cancel{RequestedAt: now}
		}
		if rec.Cancel.Disposition != protocol.CancelConfirmed {
			rec.Cancel.Disposition = ev.Disposition
		}
	}
	if _, err := e.commitLocked(rec, t); err != nil {
		return protocol.Reply{}, err
	}
	e.publishLocked(rec)
	return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel}}, nil
}

func (e *Endpoint) respond(cmd *protocol.Command) (protocol.Reply, error) {
	host, targetID, requestID, err := protocol.DecodeRef(cmd.RequestRef)
	if err != nil {
		return protocol.Reply{}, err
	}
	key := requests.Key{CreatorHost: host, TargetID: targetID, RequestID: requestID}
	e.mu.Lock()
	rec, exists, err := e.store.Get(key)
	t := e.targets[targetID]
	e.mu.Unlock()
	if err != nil {
		return protocol.Reply{}, err
	}
	if !exists {
		return protocol.Reply{}, protocol.Refuse(protocol.CodeNotFound, "no record for request_ref")
	}
	if t == nil {
		return protocol.Reply{}, protocol.Refuse(protocol.CodeAttachmentLost, "target is not attached")
	}
	if _, done := rec.Answered[cmd.InteractionID]; done {
		// A recorded intent is not a delivered answer. If the runtime still
		// holds the question, the earlier native call failed before the
		// answer landed and this is the replay that must re-send it; only a
		// settled answer short-circuits (gate finding on
		// agent-message-queue-611.22.36, union 42d923a4, reopening #20 at
		// the endpoint).
		settled, lerr := e.answerSettled(rec, t, key, cmd)
		if lerr != nil {
			return protocol.Reply{}, lerr
		}
		if settled {
			return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpInteractionRespond, Code: protocol.CodeAlreadyResolved}}, nil
		}
	}
	if rec.Interaction == nil || rec.Interaction.InteractionID != cmd.InteractionID {
		return protocol.Reply{}, protocol.Refuse(protocol.CodeAlreadyResolved, "no such pending interaction")
	}
	// Answer-INTENT contract (bead 611.22.12): the pending interaction is
	// revalidated under the lock that owns the persist (changed since the
	// first read), the offered option is validated against the pending
	// interaction BEFORE anything durable happens, and only an answer-INTENT
	// — not a completed answer — is persisted. A positive native refusal
	// clears the intent, so the refused answer consumes nothing and the
	// retried valid answer reaches the attachment. The intent still makes a
	// crash-then-replay idempotent: replay hits Answered and never invokes
	// the attachment a second time.
	e.mu.Lock()
	rec, exists, err = e.store.Get(key)
	if err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	if !exists {
		e.mu.Unlock()
		return protocol.Reply{}, protocol.Refuse(protocol.CodeNotFound, "no record for request_ref")
	}
	if _, done := rec.Answered[cmd.InteractionID]; done && (rec.Interaction == nil || rec.Interaction.InteractionID != cmd.InteractionID) {
		// Settled since the first read: the resolution arrived. An intent
		// whose interaction is still pending falls through and re-sends;
		// the unlocked Lookup above established the runtime still holds it.
		e.mu.Unlock()
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpInteractionRespond, Code: protocol.CodeAlreadyResolved}}, nil
	}
	if rec.Interaction == nil || rec.Interaction.InteractionID != cmd.InteractionID {
		// Second read revalidation: the pending interaction changed since the
		// first read (resolved locally, superseded, or reaped).
		e.mu.Unlock()
		return protocol.Reply{}, protocol.Refuse(protocol.CodeAlreadyResolved, "no such pending interaction")
	}
	offered := false
	for _, o := range rec.Interaction.Options {
		if o == cmd.Option {
			offered = true
		}
	}
	if !offered {
		e.mu.Unlock()
		return protocol.Reply{}, protocol.Refuse(protocol.CodeInvalid, "option %q is not offered by interaction %s", cmd.Option, cmd.InteractionID)
	}
	if rec.Answered == nil {
		rec.Answered = map[string]string{}
	}
	rec.Answered[cmd.InteractionID] = cmd.Option
	if _, err := e.commitLocked(rec, e.targets[rec.TargetID]); err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	e.mu.Unlock()

	code, rerr := t.att.Respond(key, cmd.Epoch, cmd.InteractionID, cmd.Option)
	if rerr != nil {
		// The native call itself errored without a disposition: the intent
		// stays (the answer may have landed), so the replay path — not a
		// fresh answer — decides. Surface the failure.
		return protocol.Reply{}, rerr
	}
	if code != "" {
		// A positive native refusal: clear the durable intent so the refused
		// answer consumes nothing and a later VALID answer can still be
		// delivered. Persist the clearing before surfacing the refusal.
		e.mu.Lock()
		if cur, ok, gerr := e.store.Get(key); gerr == nil && ok {
			if opt, done := cur.Answered[cmd.InteractionID]; !done || opt == cmd.Option {
				delete(cur.Answered, cmd.InteractionID)
				// Pro #4: route through commitLocked — single persist path.
				if _, uerr := e.commitLocked(cur, nil); uerr != nil {
					e.mu.Unlock()
					return protocol.Reply{}, uerr
				}
				rec = cur
			}
		}
		e.mu.Unlock()
		return protocol.Reply{}, protocol.Refuse(code, "interaction %s", cmd.InteractionID)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, _, err = e.store.Get(key)
	if err != nil {
		return protocol.Reply{}, err
	}
	return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpInteractionRespond}}, nil
}

// answerSettled reports whether a recorded answer intent for cmd.InteractionID
// is settled: the record no longer shows that interaction pending, or the
// runtime no longer holds it. A pending interaction on both sides means the
// earlier native call did not land and the caller's replay must re-send. The
// Lookup runs without e.mu; a Lookup error is surfaced so the caller retries
// instead of being told the answer was delivered.
func (e *Endpoint) answerSettled(rec *requests.Record, t *target, key requests.Key, cmd *protocol.Command) (bool, error) {
	if rec.Interaction == nil || rec.Interaction.InteractionID != cmd.InteractionID {
		return true, nil
	}
	ev, err := t.att.Lookup(key, cmd.Epoch)
	if err != nil {
		return false, err
	}
	return ev.Interaction == nil || ev.Interaction.InteractionID != cmd.InteractionID, nil
}

// Targets returns the registered target ids.
func (e *Endpoint) Targets() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.targets))
	for id := range e.targets {
		out = append(out, id)
	}
	return out
}

// Store exposes the record store for carrier-side recovery reads (Pro B09):
// the AMQ carrier reconciles claimed-but-unreceipted commands by reading the
// durable record instead of re-executing the command. Read-only use.
func (e *Endpoint) Store() *requests.Store { return e.store }

func (e *Endpoint) list() []protocol.Session {
	e.mu.Lock()
	ids := make([]string, 0, len(e.targets))
	for id := range e.targets {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	out := make([]protocol.Session, 0, len(ids))
	for _, id := range ids {
		// sessionProjection masks the steer capability the D1 gate refuses
		// so the advertised capabilities match what the endpoint actually
		// accepts (agent-message-queue-611.22.36, Pro r2 #22).
		s, err := e.sessionProjection(id)
		if err != nil {
			continue
		}
		out = append(out, s)
	}
	return out
}

// sessionProjection returns the attachment's Inspect() result with
// capabilities masked to match what the endpoint actually accepts. The D1
// gate (611.22.23) refuses deliver=steer at v1, so advertising Steer is a
// false capability signal — a client that picks operations from the
// advertised capabilities is told it can steer, then every steer is
// refused. This helper is used by BOTH list() and inspect() so the mask is
// in one place (agent-message-queue-611.22.36, Pro r2 #22).
func (e *Endpoint) sessionProjection(targetID string) (protocol.Session, error) {
	e.mu.Lock()
	t, ok := e.targets[targetID]
	e.mu.Unlock()
	if !ok {
		return protocol.Session{}, protocol.Refuse(protocol.CodeNotFound, "target %s is not registered", targetID)
	}
	s := t.att.Inspect()
	// D1 gate: mask steer while the v1 gate refuses deliver=steer.
	s.Capabilities.Steer = false
	return s, nil
}

func (e *Endpoint) inspect(targetID string) (any, error) {
	return e.sessionProjection(targetID)
}

// onNative applies one native observation to the bound record.
func (e *Endpoint) onNative(targetID string, ev NativeEvent) {
	// B14a: no defer — every early return below unlocks explicitly, and the
	// native ack (the slow, untrusted call) runs after the final unlock. The
	// synchronous publishLocked stays under the lock until B14e reworks
	// publication bounding; the ack is the unbounded-latency call and moves
	// out now.
	e.mu.Lock()
	if ev.Type == EventEpochChanged || ev.Type == EventStatus {
		e.mu.Unlock()
		return
	}
	if ev.Key.RequestID == "" {
		e.mu.Unlock()
		return
	}
	rec, ok, err := e.store.Get(ev.Key)
	if err != nil || !ok {
		e.mu.Unlock()
		return
	}
	// Native evidence is NEVER discarded. A completion/failure arriving for a
	// terminal (e.g. cancelled) record is still REAL: the run produced output
	// that must be acknowledged so the native slot releases. The result is
	// recorded via causeNone (State unchanged) and AckDigest is memoed.
	//
	// Ruling 4: guard on RunID, not on state. The ONLY event we drop is a
	// duplicate of a result we already recorded (same RunID, Result present).
	// Everything else flows — including a completion for a cancelled record.
	switch ev.Type {
	case EventRunCompleted, EventRunFailed, EventRunCancelled:
		// Pro #2: the duplicate guard must compare EVIDENCE, not just presence.
		// A late FINAL result F for the same run is NOT a duplicate of partial P
		// — the digests differ. Dropping F keeps P, memoed AckDigest(P), while
		// the runtime holds F: digests never match, slot never releases. Drop
		// only when the digest equals what we already recorded.
		if ev.RunID != "" && rec.NativeRun != nil && *rec.NativeRun == ev.RunID && rec.Result != nil &&
			protocol.EvidenceDigest(ev.Result) == protocol.EvidenceDigest(rec.Result) {
			// Same run, same evidence — a true duplicate native event.
			e.mu.Unlock()
			return
		}
		if e.crashAt(PointBeforeResult) != nil {
			e.mu.Unlock()
			return
		}
		nev := nativeEvidence{runID: ev.RunID, result: ev.Result}
		if rec.State.Terminal() {
			// A run event on an already-terminal record: record the result +
			// NativeRun without changing State (the cancel stands, the result
			// is acknowledged). runTerminal=true settles a pending cancel — the
			// run reached terminal, the cancel is resolved.
			nev.runTerminal = true
			e.transitionLocked(rec, causeNone, nev)
		} else if rec.State == protocol.StateRunning || rec.State == protocol.StateDispatching || rec.State == protocol.StateUncertain {
			switch ev.Type {
			case EventRunCompleted:
				e.transitionLocked(rec, causeCompleted, nev)
			case EventRunFailed:
				e.transitionLocked(rec, causeFailed, nev)
			default:
				e.transitionLocked(rec, causeCancelledByRequest, nev)
			}
		} else {
			// Non-terminal but not a live-run state (e.g. received): a run event
			// for a record that was never dispatched is spurious.
			e.mu.Unlock()
			return
		}
	case EventQuestion:
		if rec.State.Terminal() {
			e.mu.Unlock()
			return
		}
		// Pro #5: route through transitionLocked.
		e.transitionLocked(rec, causeNone, nativeEvidence{interaction: ev.Interaction})
	case EventQuestionResolved:
		if rec.State.Terminal() {
			e.mu.Unlock()
			return
		}
		// Pro #5: route through transitionLocked. Pro round 2 #21: the
		// resolution carries no interaction, and causeNone treats a nil
		// interaction as "unchanged", so the clear is an explicit flag.
		e.transitionLocked(rec, causeNone, nativeEvidence{clearInteraction: true})
	case EventLocalIntervention:
		if rec.State.Terminal() {
			e.mu.Unlock()
			return
		}
		// Pro #5: route through transitionLocked.
		e.transitionLocked(rec, causeNone, nativeEvidence{localIntervention: true})
	default:
		e.mu.Unlock()
		return
	}
	ackDigest, err := e.commitLocked(rec, e.targets[targetID])
	if err != nil {
		// The durable write failed on an async path with no client waiting.
		// Surface a visible storage-failure projection; Reconcile retries.
		e.notifyStorageFailureLocked(rec, err)
		e.mu.Unlock()
		return
	}
	if e.crashAt(PointAfterResult) != nil {
		e.mu.Unlock()
		return
	}
	// The native ack (slow, untrusted call) runs OUTSIDE e.mu. The durable
	// ack memo (commitLocked) has already made the intent replayable, so a
	// wedged attachment ack must not stall command handling, and a crash
	// mid-ack is recoverable via replayTerminalAck. Publication itself also
	// runs outside e.mu (611.22.48): publishLocked claims the publishing slot
	// under the lock, snapshots, releases e.mu for the carrier publish
	// (maildir open + fsync), then re-acquires for MarkPublished. visible
	// advances only after successful delivery.
	var ackAtt Attachment
	if ackDigest != "" {
		if t, ok := e.targets[targetID]; ok {
			ackAtt = t.att
		}
	}
	e.publishLocked(rec)
	ackKey, ackEpoch := ev.Key, rec.Epoch
	terminal := rec.State.Terminal()
	e.mu.Unlock()
	// B14a: use the attachment captured under the lock; never re-read
	// e.targets after unlocking (a concurrent Register would race the map).
	if terminal && ackAtt != nil && ackDigest != "" && e.crashAt(PointBeforeAck) == nil {
		ackAtt.AcknowledgeResult(ackKey, ackEpoch, ackDigest)
		// 611.22.34 crash window: the ack landed but the durable flag did
		// not. The replay path self-heals (Lookup → EvidenceNone → Mark).
		if e.crashAt(PointAfterAck) != nil {
			return
		}
		// 611.22.34: the ack DELIVERED — record it durably so the restart
		// replay converges instead of re-Lookuping this record every tick
		// (history digests never match: NativeRef suffix asymmetry). A
		// MarkAcknowledged failure is ignored, same shape as MarkPublished:
		// the record stays !Acknowledged and the next replay re-Lookups
		// (EvidenceNone after release-on-ack) then re-Marks. Self-healing.
		_ = e.store.MarkAcknowledged(ackKey)
	}
}

// notifyStorageFailureLocked surfaces a visible failure projection when a
// durable write fails on an async native-event path, so a request is never
// silently left "running". No durable write happens (the store write just
// failed); Reconcile retries the real write. Any write error surfaces, with a
// storage_full code when that is the cause and native_error otherwise. The
// caller holds e.mu.
func (e *Endpoint) notifyStorageFailureLocked(rec *requests.Record, err error) {
	if err == nil {
		return
	}
	code := protocol.CodeNativeError
	var r *protocol.Refusal
	if errors.As(err, &r) && r.Code == protocol.CodeStorageFull {
		code = protocol.CodeStorageFull
	}
	fail := *rec
	fail.State = protocol.StateUncertain
	fail.Code = code
	fail.Result = nil // never advertise a result we could not persist
	e.notifyLocked(&fail)
}

func boundResult(r *protocol.Result) *protocol.Result {
	if r == nil {
		return nil
	}
	out := *r
	if len(out.Text) > protocol.MaxResultBytes {
		out.Text = out.Text[:protocol.MaxResultBytes]
		out.Truncated = true
	}
	return &out
}

// cause is the single source of truth for why a record transitions. Every
// call site computes a cause + evidence and hands them to transitionLocked;
// no caller mutates rec.State/Code/Tombstone/Cancel/Result/NativeRun
// directly. This makes derived-field hygiene impossible to forget (the root
// cause of B14c rounds 1-3: 22 scattered sites each missed a cleanup).
type cause string

const (
	causeNone                     cause = "" // no state change (evidence-only update)
	causeDispatching              cause = "dispatching"
	causeAdmitted                 cause = "admitted" // running, non-terminal
	causeCompleted                cause = "completed"
	causeFailed                   cause = "failed"
	causeCancelledByRequest       cause = "cancelled_by_request"       // a real run was cancelled
	causeCancelledBeforeAdmission cause = "cancelled_before_admission" // admission never happened
	causeCancelRequested          cause = "cancel_requested"           // non-terminal: abort inconclusive, reconcile retries
	causeAttachmentLost           cause = "attachment_lost"            // uncertain
	causeRefused                  cause = "refused"                    // rejected: unshared/expired/stale_epoch/native_error
	causeBusyTombstone            cause = "busy_tombstone"             // rejected+busy reservation refusal (dedup + re-admit)
)

// nativeEvidence carries the evidence a transition derives fields from. Zero
// values mean "no evidence for that field" — transitionLocked preserves the
// existing value unless the cause dictates otherwise.
type nativeEvidence struct {
	runID             string           // NativeRun ("" = no run)
	result            *protocol.Result // terminal evidence
	cancel            *protocol.Cancel // cancel metadata from the command/event path
	code              protocol.Code    // explicit override for refused/attachment_lost
	interaction       *protocol.Interaction
	clearInteraction  bool // the pending interaction is resolved natively; clear it (nil interaction means unchanged)
	localIntervention bool
	runTerminal       bool // the run is definitively finished (noop_terminal) — confirm a pending cancel
}

// transitionLocked applies one state transition to rec, deriving every field
// from (cause, ev). It does NOT lock, persist, notify, or publish — the
// caller owns Revision++/ObservedAt/Update/notify/publishRevision. It NEVER
// touches fields the cause does not govern (Input, InputDigest, Key,
// CreatorHost, TargetID, RequestID, Epoch, NotAfter, Origin, Schema).
//
// Derivation rules (the ONE truth):
//  1. rec.State from cause.
//  2. rec.Code: terminal causes set it from the cause (or ev.code for
//     refused); non-terminal causes (dispatching/admitted) CLEAR Code and
//     Tombstone so a re-admitted tombstone advertises no stale busy code.
//  3. rec.Cancel: created/updated ONLY on cancel causes. cancelled_by_request
//     sets confirmed (or keeps ev.cancel.Disposition if the abort was
//     inconclusive — cancel_requested). cancelled_before_admission sets
//     confirmed. cancel_requested sets the disposition without going terminal.
//  4. rec.Result: set from ev.result when evidence carries one; a terminal
//     result arriving on an already-terminal record (onNative no longer
//     discards) updates Result WITHOUT changing State (the cancel stands, the
//     result is acknowledged).
//  5. rec.NativeRun: set from ev.runID whenever evidence carries one. Cleared
//     only on a fresh causeDispatching with no run, or causeBusyTombstone.
//  6. memoAckIntentLocked is called by the CALLER (not here) when a terminal
//     result is retained — transitionLocked only sets the fields.
func (e *Endpoint) transitionLocked(rec *requests.Record, c cause, ev nativeEvidence) {
	switch c {
	case causeNone:
		// Evidence-only update: record Result/NativeRun/Interaction without
		// changing State. Used by onNative when a completion arrives for an
		// already-terminal record (the cancel stands, the result is acked),
		// and by the noop_terminal abort path. If ev.runTerminal is set (the
		// run is definitively finished — noop_terminal), a pending cancel is
		// now confirmed (the run stopped, natively).
		if ev.result != nil {
			rec.Result = boundResult(ev.result)
		}
		if ev.runTerminal && rec.State.Terminal() && rec.Cancel != nil && rec.Cancel.Disposition != protocol.CancelConfirmed {
			rec.Cancel.Disposition = protocol.CancelConfirmed
		}
		if ev.runID != "" {
			run := ev.runID
			rec.NativeRun = &run
		}
		if ev.clearInteraction {
			rec.Interaction = nil
		} else if ev.interaction != nil {
			rec.Interaction = ev.interaction
		}
		if ev.localIntervention {
			rec.LocalIntervention = true
		}
		return
	case causeDispatching:
		rec.State = protocol.StateDispatching
		rec.Code = ""
		rec.Tombstone = false
		if ev.runID != "" {
			run := ev.runID
			rec.NativeRun = &run
		} else {
			rec.NativeRun = nil
		}
		rec.Interaction = nil
	case causeAdmitted:
		rec.State = protocol.StateRunning
		rec.Code = ""
		rec.Tombstone = false
		if ev.runID != "" {
			run := ev.runID
			rec.NativeRun = &run
		}
		if ev.interaction != nil {
			rec.Interaction = ev.interaction
		}
	case causeCompleted:
		rec.State = protocol.StateCompleted
		rec.Code = ""
		if ev.runID != "" {
			run := ev.runID
			rec.NativeRun = &run
		}
		if ev.result != nil {
			rec.Result = boundResult(ev.result)
		}
		rec.Interaction = nil
	case causeFailed:
		rec.State = protocol.StateFailed
		rec.Code = protocol.CodeNativeError
		if ev.runID != "" {
			run := ev.runID
			rec.NativeRun = &run
		}
		if ev.result != nil {
			rec.Result = boundResult(ev.result)
		}
		rec.Interaction = nil
	case causeCancelledByRequest:
		rec.State = protocol.StateCancelled
		rec.Code = protocol.CodeCancelledByRequest
		if ev.runID != "" {
			run := ev.runID
			rec.NativeRun = &run
		}
		// B4: retain partial output retained at cancellation. A cancelled run may
		// have produced a result (partial output) — dropping it here means NO
		// result and NO ack digest, so the native slot is never released.
		if ev.result != nil {
			rec.Result = boundResult(ev.result)
		}
		if rec.Cancel == nil {
			rec.Cancel = &protocol.Cancel{RequestedAt: protocol.FormatTime(e.now())}
		}
		rec.Cancel.Disposition = protocol.CancelConfirmed
		rec.Interaction = nil
	case causeCancelledBeforeAdmission:
		rec.State = protocol.StateCancelled
		rec.Code = protocol.CodeCancelledBeforeAdmission
		if rec.Cancel == nil {
			rec.Cancel = &protocol.Cancel{RequestedAt: protocol.FormatTime(e.now())}
		}
		rec.Cancel.Disposition = protocol.CancelConfirmed
		rec.Interaction = nil
	case causeCancelRequested:
		// Non-terminal: the abort was inconclusive. State stays as-is (may be
		// cancelled from a prior native event); NativeRun stays bound so
		// reconcile re-drives CancelExact. Disposition records that the stop
		// is pending, not confirmed.
		//
		// Pro #3: a confirmed disposition is a promise already kept and is not
		// rewritable. Guard it like the other two Disposition writers: if the
		// cancel was already confirmed, a transient CancelExact error must NOT
		// downgrade it to cancel_requested. A terminal, confirmed promise
		// only ever moves forward.
		if rec.Cancel == nil {
			rec.Cancel = &protocol.Cancel{RequestedAt: protocol.FormatTime(e.now())}
		}
		if rec.Cancel.Disposition != protocol.CancelConfirmed {
			rec.Cancel.Disposition = protocol.CancelRequested
		}
		if ev.runID != "" {
			run := ev.runID
			rec.NativeRun = &run
		}
	case causeAttachmentLost:
		rec.State = protocol.StateUncertain
		rec.Code = protocol.CodeAttachmentLost
		if ev.runID != "" {
			run := ev.runID
			rec.NativeRun = &run
		}
	case causeRefused:
		rec.State = protocol.StateRejected
		rec.Code = ev.code
		if rec.Code == "" {
			rec.Code = protocol.CodeNativeError
		}
		rec.Interaction = nil
	case causeBusyTombstone:
		rec.State = protocol.StateRejected
		rec.Code = protocol.CodeBusy
		rec.Tombstone = true
		rec.NativeRun = nil
		rec.Interaction = nil
	}
	// Ruling 1 Cancel rule: Cancel is written iff ev.cancel != nil or the
	// cause is a cancelled_* cause (handled in-case above). Non-cancel causes
	// that CARRY ev.cancel (e.g. causeAdmitted racing a cancel command) apply
	// it here — without overriding the in-case disposition for cancelled_*
	// causes.
	if ev.cancel != nil && c != causeCancelledByRequest && c != causeCancelledBeforeAdmission && c != causeCancelRequested {
		if rec.Cancel == nil {
			rec.Cancel = &protocol.Cancel{RequestedAt: ev.cancel.RequestedAt}
		}
		if ev.cancel.Disposition != "" {
			rec.Cancel.Disposition = ev.cancel.Disposition
		}
	}
}

// owesCancel reports whether we asked the runtime to stop this run and it
// has not confirmed. This is ONE of two SEPARATE obligations we may owe the
// runtime — it is about a CANCEL we have not confirmed, nothing else. State
// is what we promised the CALLER; a cancel we have not confirmed and a result
// we have not released are two separate things we owe the RUNTIME, and they
// never share a predicate.
//
// Used by reconcileLive's same-run early return and Reconcile's terminal
// retry selector: only a record that owes a cancel is re-driven.
// owesResult reports the THIRD obligation: the record is closed with a run
// bound but no result persisted. That is what a late result whose write
// failed looks like (onNative discards the attempted update), and neither
// owesCancel nor OwesAck describes it — replayTerminalAck returned before
// ever asking the attachment, so the retained result was never fetched or
// acknowledged (Pro r2 #15 / packet 4c, agent-message-queue-611.22.36).
// A tombstone owes nothing: compaction erased its result on purpose.
func owesResult(rec *requests.Record) bool {
	return rec.State.Terminal() && !rec.Tombstone && rec.NativeRun != nil && rec.Result == nil
}

// recoverTerminalResult asks the attachment for the result a terminal record
// is missing and applies it through the ordinary native-event path, so the
// commit, publication and acknowledgement happen exactly as they would have
// had the original write succeeded. It stops asking once the attachment
// retains nothing for the key: then there is nothing to recover.
func (e *Endpoint) recoverTerminalResult(rec *requests.Record) error {
	e.mu.Lock()
	t, ok := e.targets[rec.TargetID]
	e.mu.Unlock()
	if !ok {
		return nil
	}
	ev, err := t.att.Lookup(keyOfRecord(rec), rec.Epoch)
	if err != nil {
		return err
	}
	if !ev.Known || ev.Class == EvidenceNone || !ev.State.Terminal() || ev.Result == nil {
		return nil
	}
	typ := EventRunCompleted
	switch ev.State {
	case protocol.StateFailed:
		typ = EventRunFailed
	case protocol.StateCancelled:
		typ = EventRunCancelled
	}
	e.onNative(rec.TargetID, NativeEvent{Type: typ, Key: keyOfRecord(rec), RunID: ev.RunID, Result: ev.Result})
	return nil
}

func owesCancel(rec *requests.Record) bool {
	return rec.Cancel != nil &&
		rec.Cancel.Disposition == protocol.CancelRequested &&
		rec.NativeRun != nil
}

// owesAck reports whether the runtime is holding a result for us that we have
// not released. This is the SECOND obligation — it is about a RESULT we have
// not acknowledged, nothing else. Result != nil is what makes the digest
// non-empty (EvidenceDigest(nil) == ""), so a terminal record with no result
// owes nothing and is never re-driven — that closes the revision-churn bug
// (Pro #4) BY CONSTRUCTION.
//
// Used by replayTerminalAck's crash-gap path (when AckDigest is empty but
// Result is bound, compute the digest from Result).
func owesAck(rec *requests.Record) bool {
	return rec.OwesAck()
}

// shouldCompact rate-limits compaction to once per minute. Survives restarts
// (time-interval, not tick-count). Returns false if compaction is disabled
// (compactHorizon == 0).
func (e *Endpoint) shouldCompact() bool {
	if e.compactHorizon == 0 {
		return false
	}
	now := e.now()
	if now.Sub(e.lastCompact) < time.Minute {
		return false
	}
	e.lastCompact = now
	return true
}

// commitLocked is the persist step that pairs with transitionLocked. It
// does Revision++, ObservedAt, store.Update, notify, and memo AckDigest — so
// no caller can forget notify or the ack memo. Returns the ackDigest + the
// storage error (if any). The caller holds e.mu and owns unlocking + publish.
// Ruling 3: applyTransition+commitLocked is the ONLY persist path.
func (e *Endpoint) commitLocked(rec *requests.Record, t *target) (string, error) {
	rec.Revision++
	rec.ObservedAt = protocol.FormatTime(e.now())
	if err := e.store.Update(rec); err != nil {
		return "", err
	}
	e.notifyLocked(rec)
	ackDigest := ""
	if rec.State.Terminal() && t != nil {
		ackDigest, _ = e.memoAckIntentLocked(rec, t)
	}
	return ackDigest, nil
}

// Reconcile runs after Open and on every Tick. It re-examines every
// non-terminal record against exact native evidence, never re-submits, expires
// deferred requests whose window closed, and republishes unpublished
// revisions. It also replays native acknowledgements for terminal records:
// a crash between the terminal commit and the native ack leaves the
// attachment holding its one unacked-result slot, which would refuse every
// later submit with busy; replaying the durable ack digest releases it.
// Native attachment calls happen without the endpoint lock held; a single
// poisoned record is reported, never allowed to abort the whole pass.
func (e *Endpoint) Reconcile() error {
	// 611.22.47: gate Reconcile/Tick on lifecycle state and register the
	// sweep as in-flight so Close's drain wait bounds it. A Tick that arrives
	// once draining has begun is skipped (no new native work during shutdown).
	if !e.beginReconcile() {
		return nil
	}
	defer e.releaseInFlight()
	e.mu.Lock()
	recs, err := e.store.List()
	e.mu.Unlock()
	if err != nil {
		return err
	}
	var firstErr error
	for _, rec := range recs {
		var rerr error
		switch rec.State {
		case protocol.StateDispatching, protocol.StateRunning, protocol.StateUncertain:
			rerr = e.reconcileLive(rec)
		case protocol.StateReceived:
			if rerr = e.admitDeferred(rec); rerr == nil {
				rerr = e.replayTerminalAck(rec)
			}
		default:
			// The retry selector is owesCancel (a cancel we have not
			// confirmed). If we don't owe a cancel, replay the ack.
			// replayTerminalAck is idempotent (returns nil if the attachment
			// already released the result). A terminal record with no result
			// has an empty digest and replayTerminalAck returns immediately,
			// so there is no churn (Pro #4).
			switch {
			case owesCancel(rec):
				rerr = e.reconcileCancelRetry(rec)
			case owesResult(rec):
				// A closed record with a bound run and no result: a late result
				// whose write failed (packet 4c). Ask the attachment.
				rerr = e.recoverTerminalResult(rec)
			default:
				rerr = e.replayTerminalAck(rec)
			}
		}
		if rerr != nil && firstErr == nil {
			firstErr = rerr
		}
		e.mu.Lock()
		if cur, ok, gerr := e.store.Get(keyOfRecord(rec)); gerr == nil && ok && cur.PublishedRevision < cur.Revision {
			e.publishLocked(cur)
		}
		e.mu.Unlock()
	}
	// B14e: bounded compaction. Runs once per minute (shouldCompact rate-
	// limit), reaps terminal+settled+old records into tombstones. e.mu is
	// held per-record (CompactOne), never across the sweep (Pro B7).
	// 611.22.19 BK4: reuse the recs snapshot already fetched for the
	// reconcile pass — CompactOne re-reads each candidate under the store
	// lock and re-gates (terminal + !tombstone + old + !OwesAck + published),
	// so a stale snapshot entry is harmless and a whole second history scan
	// is avoided. This is the bounded incremental work the BK4 finding asked
	// for: one List per tick, not two.
	if e.shouldCompact() {
		cutoff := e.now().Add(-e.compactHorizon)
		compacted := 0
		for _, rec := range recs {
			// Count only successful compactions toward the limit, not every
			// record the loop looks at (Pro B1: tombstones sort ahead of
			// live records would starve forever on the i counter).
			if compacted >= 100 {
				break
			}
			if !rec.State.Terminal() || rec.Tombstone {
				continue
			}
			e.mu.Lock()
			ok, cerr := e.store.CompactOne(keyOfRecord(rec), cutoff)
			e.mu.Unlock()
			if cerr != nil {
				var rf *protocol.Refusal
				if errors.As(cerr, &rf) && rf.Code == protocol.CodeStorageFull {
					// 611.22.19 BK4 round-2 B2: a per-record storage_full must
					// NOT abort the sweep. CompactOne is a settlement write
					// (quota-exempt), so this should not fire, but a true
					// disk-full still records the error and continues so bounded
					// work per tick stays true (a refused record does not stop
					// later compactions that might free space).
					if firstErr == nil {
						firstErr = cerr
					}
					continue
				}
				// store_closed or a non-quota error: stop the sweep.
				if firstErr == nil {
					firstErr = cerr
				}
				break
			}
			if ok {
				compacted++
			}
		}
	}
	return firstErr
}

// replayTerminalAck re-releases the retained evidence of one terminal record
// whose acknowledgement the attachment may not have processed (crash after
// the durable terminal commit, before or during the native ack). The ack is
// replayed only when the record carries an ack intent for evidence the
// attachment still retains: replayTerminalAck asks Lookup first — a terminal
// record with no retained evidence at the attachment is already released, so
// re-acking it would be a stale acknowledgement for evidence that no longer
// exists. A record without an ack intent either never retained evidence
// (nothing to release) or its ack was durably confirmed; both stay silent.
// Convergence relies on the AcknowledgeResult contract: once a native ack
// lands the attachment retains nothing for the key, so Lookup reports
// EvidenceNone and a later Reconcile/Tick does not re-ack. Only the crash
// case — intent memoed, ack never landed, evidence still retained — replays.
func (e *Endpoint) replayTerminalAck(rec *requests.Record) error {
	// 611.22.34: the ack was already DELIVERED (durable confirmation) — no
	// replay, no Lookup. This is the convergence fix: without this gate the
	// restart case re-examines every terminal record every tick forever,
	// because history results carry the NativeRef suffix ("codex thread
	// turn ") the live path does not, so EvidenceDigest(ev.Result) never
	// equals the memoed AckDigest. Gate ONLY records with a memoed digest:
	// AckDigest == "" (crash between terminal commit and ack memo) still
	// needs the intent memo + ack below.
	if rec.AckDigest != "" && rec.Acknowledged {
		return nil
	}
	// owesAck (Result != nil, AckDigest == "") describes the crash-gap case
	// where the ack was never sent. But we also replay when AckDigest IS set
	// but the ack didn't land (PointBeforeAck crash). So we cannot gate on
	// owesAck alone — the Lookup below is the real gate (EvidenceNone means
	// the ack already landed). owesAck is referenced here to document why
	// computing the digest from Result is safe.
	_ = owesAck(rec)
	// The digest to acknowledge: if AckDigest was memo'd, use it. If not
	// (crash between terminal commit and ack memo, owesAck is true), compute
	// it from rec.Result — owesAck guarantees Result != nil.
	digest := rec.AckDigest
	if digest == "" {
		digest = protocol.EvidenceDigest(rec.Result)
	}
	if digest == "" {
		return nil
	}
	e.mu.Lock()
	t, ok := e.targets[rec.TargetID]
	e.mu.Unlock()
	if !ok {
		return nil
	}
	ev, err := t.att.Lookup(keyOfRecord(rec), rec.Epoch)
	if err != nil {
		return err
	}
	if !ev.Known || ev.Class == EvidenceNone {
		// Nothing retained for this key: the ack already landed before the
		// crash. Replay would acknowledge evidence the attachment discarded.
		// 611.22.34: the delivered ack is now provable — record it durably
		// so this record never re-enters the replay path (convergence).
		_ = e.store.MarkAcknowledged(keyOfRecord(rec))
		return nil
	}
	// 611.22.34 B2 (recut): HISTORY-terminated evidence. A restarted
	// attachment retains nothing in memory by construction, so history
	// evidence IS proof the run terminated — this is the pre-upgrade
	// convergence path (Acknowledged=false on every shipped record).
	// The memoed AckDigest is the FULL digest of the live result, and codex
	// sets NativeRef on the live turn/completed result too, so the memo may
	// or may not carry a NativeRef while the history result always does.
	// Compare both sides in the NativeRef-stable form, recomputed from the
	// record's own stored result; the full digest is still what the
	// attachment is asked to release, because its own compare is full-shape
	// (post-merge verification of #767, agent-message-queue-611.22.34).
	if ev.Class == EvidenceHistoryTerminated && ev.State.Terminal() && ev.Result != nil {
		if rec.Result == nil || protocol.EvidenceDigestStable(ev.Result) != protocol.EvidenceDigestStable(rec.Result) {
			// The history outcome is not the outcome this record acked; a
			// stale or foreign ack must never release different evidence.
			return nil
		}
		if rec.NativeRun != nil && ev.RunID != *rec.NativeRun {
			return nil
		}
		key := keyOfRecord(rec)
		epoch := rec.Epoch
		e.mu.Lock()
		att := t.att
		e.mu.Unlock()
		att.AcknowledgeResult(key, epoch, digest)
		// 611.22.34: delivered — record it (MarkPublished failure shape:
		// self-healing on the next replay).
		_ = e.store.MarkAcknowledged(key)
		return nil
	}
	// B2: validate retained terminal evidence against the bound run + digest,
	// NOT against the client-facing state. A cancelled record keeps its
	// promise and never flips to completed (Q2 ruling), so "endpoint cancelled
	// + native completed" is a SUPPORTED shape. The old ev.State != rec.State
	// gate rejected exactly that: digest matched, states differed, slot
	// stayed occupied, later submits refused busy forever. The digest already
	// proves it is THIS result; the run id proves it is THIS run.
	if !ev.State.Terminal() || ev.Result == nil || protocol.EvidenceDigest(ev.Result) != digest {
		// The retained evidence is not the outcome this record acked (a stale
		// or foreign ack must never release a different request's result).
		return nil
	}
	// Pro #5: identity, not content. Two runs can produce identical output
	// (same digest). Require the retained evidence's RunID to match the
	// record's bound NativeRun before acking — otherwise we release another
	// run's evidence. AcknowledgeResult's arguments carry no run id, so the
	// attachment cannot catch this either.
	if rec.NativeRun == nil || ev.RunID != *rec.NativeRun {
		return nil
	}
	// B14a: the native ack runs OUTSIDE e.mu like every other native call —
	// a wedged attachment ack on this recovery path must not hold the
	// endpoint mutex forever. The ack digest was durably memoed before it was
	// first sent, so a crash mid-ack is replayable.
	key := keyOfRecord(rec)
	epoch := rec.Epoch
	e.mu.Lock()
	att := t.att
	e.mu.Unlock()
	att.AcknowledgeResult(key, epoch, digest)
	// 611.22.34: delivered — record it. A MarkAcknowledged failure is
	// ignored (MarkPublished shape): the next replay re-Lookups, gets
	// EvidenceNone, and re-Marks. Self-healing.
	_ = e.store.MarkAcknowledged(key)
	return nil
}

// Tick retries deferred admissions and expiry; carriers call it on a timer
// and when a target comes back online.
func (e *Endpoint) Tick() error {
	return e.Reconcile()
}

func keyOfRecord(rec *requests.Record) requests.Key {
	return requests.Key{CreatorHost: rec.CreatorHost, TargetID: rec.TargetID, RequestID: rec.RequestID}
}

// runtimeInFlightLocked reports whether another request for the same target
// is in flight at the endpoint: any record in dispatching (admission
// unknown), running (native-owned), or uncertain (outcome unknown) state.
// This is the B14 per-runtime reservation — the endpoint's own knowledge of
// what it has outstanding, not the attachment's Inspect() status, which is
// advisory and racy. Scope note (B10, ratified deviation from the bead's
// literal wording): a terminal record whose ack is pending is NOT reserved —
// the attachment owns the native unacked-result slot (its Submit already
// refuses on one) and the ack-replay machinery recovers it; reserving it at
// the endpoint too double-counted and made the endpoint refuse submits the
// attachment would accept.
// B10 fail-closed: an enumeration error is PROPAGATED, never read as "none"
// — a storage error must not authorize the dispatch the reservation exists
// to prevent. A POISON record for the target itself (undecodable file whose
// identity is recovered from the path) makes the target's reservation state
// UNDETERMINABLE — also fail-closed. Poison for other targets does not
// block this target. The caller holds e.mu.
func (e *Endpoint) runtimeInFlightLocked(targetID string, exclude requests.Key) (bool, error) {
	recs, poison, err := e.store.ListWithPoison()
	if err != nil {
		return false, err
	}
	for _, p := range poison {
		if p.Key.TargetID == "" || p.Key.TargetID == targetID {
			// The target's own record is unreadable (or its identity could not
			// be recovered from the path): its reservation state cannot be
			// determined, so treat the target as possibly-busy. An empty
			// TargetID is neither "this target" nor "another" — fail closed.
			return false, fmt.Errorf("target %s has an unreadable record %s: %w", targetID, p.Path, errUndeterminableReservation)
		}
	}
	for _, r := range recs {
		if r.TargetID != targetID || keyOfRecord(r) == exclude {
			continue
		}
		switch r.State {
		case protocol.StateDispatching, protocol.StateRunning, protocol.StateUncertain:
			return true, nil
		}
	}
	return false, nil
}

// errUndeterminableReservation marks a reservation check that could not be
// completed (poison record for the target); the caller refuses to dispatch.
var errUndeterminableReservation = errors.New("reservation state undeterminable")

// reconcileCancelRetry re-drives CancelExact for a record carrying outstanding
// cancel intent (cancel_requested + NativeRun bound). Works for both terminal
// (the abort was inconclusive) and non-terminal (a cancel raced admission and
// the record is still running) records. Bounded by NotAfter.
// This is the B04 reconcile-cancel-retry mechanism, built once here.
func (e *Endpoint) reconcileCancelRetry(rec *requests.Record) error {
	key := keyOfRecord(rec)
	epoch := rec.Epoch
	targetID := rec.TargetID
	e.mu.Lock()
	t, ok := e.targets[targetID]
	e.mu.Unlock()
	if !ok || t == nil {
		return nil // attachment offline; retry next tick
	}
	ev, nerr := t.att.CancelExact(key, epoch)
	e.mu.Lock()
	rec, exists, err := e.store.Get(key)
	if err != nil || !exists {
		e.mu.Unlock()
		return nil
	}
	runID := ""
	if rec.NativeRun != nil {
		runID = *rec.NativeRun
	}
	if _, cerr := e.applyCancelOutcomeLocked(rec, t, key, epoch, runID, ev, nerr); cerr != nil {
		e.mu.Unlock()
		return cerr
	}
	e.mu.Unlock()
	return e.replayTerminalAck(rec)
}

// reconcileLive resolves one non-terminal record. It calls the attachment
// without the endpoint lock, then applies the result under the lock.
func (e *Endpoint) reconcileLive(rec *requests.Record) error {
	key := keyOfRecord(rec)
	e.mu.Lock()
	t, ok := e.targets[rec.TargetID]
	e.mu.Unlock()

	var ev Evidence
	var lookupErr error
	if ok {
		ev, lookupErr = t.att.Lookup(key, rec.Epoch)
	}

	// B14a: no defer — every early return below unlocks explicitly, and the
	// native ack (the slow, untrusted call) runs after the final unlock. A
	// wedged attachment must never hold the endpoint mutex: the whole
	// endpoint (every Handle/Reconcile/cancel) deadlocks behind it.
	e.mu.Lock()
	rec, exists, err := e.store.Get(key)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	if !exists || rec.State.Terminal() {
		e.mu.Unlock()
		return nil
	}
	switch {
	case !ok || lookupErr != nil:
		if rec.State == protocol.StateUncertain {
			e.mu.Unlock()
			return nil
		}
		e.transitionLocked(rec, causeAttachmentLost, nativeEvidence{})
	case ev.RefusalCode != "" && ev.Admitted && ev.State == protocol.StateRejected:
		// Amit-remote contract section 6/A3: a DEFINITIVE native refusal over
		// PROVEN admission (the receipt stays across a fire-time refusal).
		// Map the typed refusal code onto the durable record — rejected+code,
		// action-required — never a silent dispatch and never evidence of
		// non-admission.
		e.transitionLocked(rec, causeRefused, nativeEvidence{runID: ev.RunID, code: ev.RefusalCode})
	case ev.Class == EvidenceTentative:
		// Bound but native ownership not yet proven. Never reject a submission
		// that is still about to execute; re-check next tick.
		e.mu.Unlock()
		return nil
	case ev.Class == EvidenceUnknown:
		// Delivered but admission-unknown (transport ambiguity, or an API that
		// cannot report its own rejection). Keep the correlation, stay uncertain.
		if rec.State == protocol.StateUncertain {
			e.mu.Unlock()
			return nil
		}
		e.transitionLocked(rec, causeAttachmentLost, nativeEvidence{})
	case !ev.Known || ev.Class == EvidenceNone:
		// The adapter has a real admission primitive and retains nothing:
		// admission did not and cannot happen. Positive evidence, not a guess.
		e.transitionLocked(rec, causeRefused, nativeEvidence{})
	case ev.Admitted && ev.State.Terminal():
		nev := nativeEvidence{runID: ev.RunID, result: ev.Result, interaction: ev.Interaction}
		switch ev.State {
		case protocol.StateFailed:
			e.transitionLocked(rec, causeFailed, nev)
		case protocol.StateCancelled:
			e.transitionLocked(rec, causeCancelledByRequest, nev)
		default:
			e.transitionLocked(rec, causeCompleted, nev)
		}
		rec.LocalIntervention = rec.LocalIntervention || ev.LocalIntervention
	case ev.Admitted:
		// The same-run early return goes FIRST. A healthy running record
		// matches neither owesCancel nor owesAck and is a noop, as it was
		// before this PR. Only a record that OWES A CANCEL may route to
		// reconcileCancelRetry (Pro #1: the old needsRuntimeSettlement fired
		// on healthy work because NativeRun != nil && AckDigest == "" is
		// true for a simply-running record, and CancelExact killed every
		// prompt within one tick).
		if rec.State == protocol.StateRunning && rec.NativeRun != nil && *rec.NativeRun == ev.RunID {
			if owesCancel(rec) {
				e.mu.Unlock()
				return e.reconcileCancelRetry(rec)
			}
			e.mu.Unlock()
			return nil
		}
		e.transitionLocked(rec, causeAdmitted, nativeEvidence{runID: ev.RunID, interaction: ev.Interaction})
	default:
		e.transitionLocked(rec, causeRefused, nativeEvidence{})
	}
	ackDigest, err := e.commitLocked(rec, t)
	if err != nil {
		e.notifyStorageFailureLocked(rec, err)
		e.mu.Unlock()
		return err
	}
	// B14a: the durable ack memo was written under the lock above
	// (commitLocked), so the replay path stays correct if we crash before
	// the native call. The native ack itself runs OUTSIDE e.mu — a wedged
	// attachment ack must not hold the endpoint mutex forever.
	ackKey, ackEpoch := key, rec.Epoch
	ackAtt := Attachment(nil)
	if ackDigest != "" && ok {
		ackAtt = t.att
	}
	e.mu.Unlock()
	if ackAtt != nil && ackDigest != "" {
		ackAtt.AcknowledgeResult(ackKey, ackEpoch, ackDigest)
		// 611.22.34: delivered — record it durably (digest verified
		// non-empty above). MarkAcknowledged failure ignored (MarkPublished
		// shape): the next replay re-Lookups and re-Marks. Self-healing.
		_ = e.store.MarkAcknowledged(ackKey)
	}
	return nil
}

// admitDeferred admits a received record whose target came back inside its
// window. The native Submit happens without the endpoint lock.
func (e *Endpoint) admitDeferred(rec *requests.Record) error {
	key := keyOfRecord(rec)
	// W8 (D1 gate): busy=queue and deliver=steer are disabled in v1. The
	// gate lives in submit() for fresh commands; admitDeferred is the other
	// entrance to native Submit, so a record stored before D1 with
	// deliver=steer would be admitted here after an upgrade without this
	// check. Same gate, same code, same reason.
	if rec.Input != nil && (rec.Input.Busy == protocol.BusyQueue || rec.Input.Deliver == protocol.DeliverSteer) {
		mode, value := "busy", string(rec.Input.Busy)
		if rec.Input.Deliver == protocol.DeliverSteer {
			mode, value = "deliver", string(rec.Input.Deliver)
		}
		e.mu.Lock()
		cur, exists, err := e.store.Get(key)
		if err != nil || !exists || cur.State != protocol.StateReceived {
			e.mu.Unlock()
			return err
		}
		e.transitionLocked(cur, causeRefused, nativeEvidence{code: protocol.CodeUnsupported})
		_, err = e.commitLocked(cur, nil)
		e.mu.Unlock()
		_ = mode
		_ = value
		return err
	}
	e.mu.Lock()
	var minEvidence string
	if rec.Input != nil {
		minEvidence = rec.Input.MinEvidence
	}
	t, code := e.admissibleLocked(rec.TargetID, rec.Epoch, rec.NotAfter, minEvidence)
	if code == "" && t == nil {
		e.mu.Unlock()
		return nil // still offline, still inside the window
	}
	if code != "" {
		rec, exists, err := e.store.Get(key)
		if err != nil || !exists || rec.State != protocol.StateReceived {
			e.mu.Unlock()
			return err
		}
		e.transitionLocked(rec, causeRefused, nativeEvidence{code: code})
		_, err = e.commitLocked(rec, nil)
		e.mu.Unlock()
		return err
	}
	// Move to dispatching under the lock, then Submit unlocked. The B14
	// reservation applies here too: a deferred record whose sibling is still
	// in flight stays deferred (never dispatched) — Tick retries it after
	// the in-flight one resolves. Fail-closed on enumeration error.
	reserved, rerr := e.runtimeInFlightLocked(rec.TargetID, key)
	if rerr != nil {
		e.mu.Unlock()
		return rerr
	}
	if reserved {
		e.mu.Unlock()
		return nil // still reserved; retry on the next tick
	}
	// 611.22.19 BK4: same fail-closed reservation as the live submit path —
	// refuse storage_full before dispatch rather than wedging or evicting a
	// dedup tombstone. A deferred record already occupies space; reserving the
	// bounded result headroom keeps the quota honest under retry.
	if err := e.store.Reserve(key, protocol.MaxRecordBytes); err != nil {
		e.mu.Unlock()
		return err
	}
	rec, exists, err := e.store.Get(key)
	if err != nil || !exists || rec.State != protocol.StateReceived {
		// The record is not awaiting admission here (vanished, already
		// admitted, or already terminal). Release this attempt's
		// reservation; a later retry re-reserves idempotently.
		e.store.ReleaseReservation(key)
		e.mu.Unlock()
		return err
	}
	rec.NativeDispatches = 1
	e.transitionLocked(rec, causeDispatching, nativeEvidence{})
	if _, err := e.commitLocked(rec, e.targets[rec.TargetID]); err != nil {
		e.mu.Unlock()
		return err
	}
	input := protocol.SubmitInput{}
	if rec.Input != nil {
		input = *rec.Input
	}
	e.mu.Unlock()

	adm, nerr := t.att.Submit(BoundRequest{Key: key, Epoch: rec.Epoch, Input: input, NotAfter: rec.NotAfter})

	// No defer: finishAdmissionLocked unlocks exactly once on every return.
	e.mu.Lock()
	rec, exists, err = e.store.Get(key)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	_, ferr := e.finishAdmissionLocked(rec, exists, t, adm, nerr)
	return ferr
}

// admissionCause maps Submit's answer to the transition it justifies. It is
// the ONE reading of (adm, nerr): a transport failure proves nothing
// (attachment_lost keeps the correlation), admitted binds the run, a
// cancelled-before-admission code is a positive non-admission, and any other
// code is a positive refusal.
func admissionCause(adm Admission, nerr error) (cause, nativeEvidence) {
	nev := nativeEvidence{runID: adm.RunID}
	switch {
	case nerr != nil:
		return causeAttachmentLost, nev
	case adm.Admitted:
		return causeAdmitted, nev
	case adm.Code == protocol.CodeCancelledBeforeAdmission:
		return causeCancelledBeforeAdmission, nev
	default:
		nev.code = adm.Code
		return causeRefused, nev
	}
}

// finishAdmissionLocked applies one native Submit outcome to the durable
// record. Shared by the fresh-submit and identical-retry admission paths and
// by admitDeferred, so all three never disagree about post-admission
// semantics. The caller holds e.mu and owns unlocking: finishAdmissionLocked
// unlocks exactly once on every return.
//
// B3 (cancel-during-admission): a cancel that raced the gated admission
// emits EventRunCancelled while Submit is still blocked, so the record is
// already StateCancelled WITHOUT cancel metadata when Submit returns — the
// native cancel handler saw terminal and did not create it. The raced
// cancellation is confirmed ONLY when admission did NOT happen: a positive
// admission (adm.Admitted) means a live run was bound and the native cancel
// that moved the record is cancelled_by_request — that run must be ABORTED
// (CancelExact on the bound run), never reported as cancelled_before_admission.
// A positive admission with nerr != nil is uncertain (the nerr path).
func (e *Endpoint) finishAdmissionLocked(rec *requests.Record, exists bool, t *target, adm Admission, nerr error) (protocol.Reply, error) {
	if !exists {
		e.mu.Unlock()
		return protocol.Reply{}, protocol.Refuse(protocol.CodeNotFound, "record vanished during dispatch")
	}
	if rec.State == protocol.StateDispatching {
		// B1: if the record carries outstanding cancel intent (a concurrent
		// cancel got a transient error and persisted cancel_requested while the
		// record was still dispatching), admission of a live run must abort it,
		// not persist running. The abort condition is disposition, not state.
		if adm.Admitted && rec.Cancel != nil && rec.Cancel.Disposition == protocol.CancelRequested {
			return e.abortAdmittedRacedRun(rec, t, adm)
		}
		// The normal path: no native event moved the record while Submit was
		// in flight. Apply the admission outcome directly.
		c, nev := admissionCause(adm, nerr)
		e.transitionLocked(rec, c, nev)
		if _, err := e.commitLocked(rec, t); err != nil {
			e.mu.Unlock()
			return protocol.Reply{}, err
		}
		snap := rec.Snapshot
		e.mu.Unlock()
		ev := achievedEvidence(t)
		e.publishRevision(rec)
		return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: rec.Code, Evidence: ev}}, nil
	}
	// The record was moved by a native event while Submit was in flight
	// (the raced shape). RECONCILE from (adm, nerr, rec.State) — never branch
	// on assumptions about what happened.
	// B1: the abort condition is OUTSTANDING CANCEL INTENT, not just State ==
	// cancelled. A cancel that got a transient CancelExact error persists
	// Cancel{cancel_requested} while the record is still dispatching/running.
	// State is what we promised the caller; disposition is what we owe the
	// runtime.
	hasCancelIntent := rec.Cancel != nil && rec.Cancel.Disposition == protocol.CancelRequested
	if adm.Admitted && (rec.State == protocol.StateCancelled || hasCancelIntent) {
		// A positive admission raced a cancel (or carries cancel intent): the
		// run is live and unwanted. Bind NativeRun first (durable), then abort.
		return e.abortAdmittedRacedRun(rec, t, adm)
	}
	if adm.Admitted && (rec.State == protocol.StateCompleted || rec.State == protocol.StateFailed) {
		// The run finished before Submit returned. Record the result.
		c := causeCompleted
		if rec.State == protocol.StateFailed {
			c = causeFailed
		}
		e.transitionLocked(rec, c, nativeEvidence{runID: adm.RunID})
		if _, err := e.commitLocked(rec, t); err != nil {
			e.mu.Unlock()
			return protocol.Reply{}, err
		}
		snap := rec.Snapshot
		e.mu.Unlock()
		ev := achievedEvidence(t)
		e.publishRevision(rec)
		return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: rec.Code, Evidence: ev}}, nil
	}
	if rec.State == protocol.StateCancelled && !adm.Admitted && nerr == nil {
		// B3: cancellation raced admission and never got metadata, and the
		// native outcome positively establishes admission never happened.
		e.transitionLocked(rec, causeCancelledBeforeAdmission, nativeEvidence{})
		if _, err := e.commitLocked(rec, t); err != nil {
			e.notifyStorageFailureLocked(rec, err)
			e.mu.Unlock()
			return protocol.Reply{}, err
		}
	}
	// B4: reconcile moved the record to uncertain while Submit was in flight
	// (attachment lost, history silent). Submit's own answer is exact native
	// evidence and outranks that uncertainty: admitted binds the run and goes
	// running; a definitive code goes to its terminal state and releases the
	// reservation. A transport failure (nerr != nil), or no code at all,
	// proves nothing and leaves the record uncertain for reconcile. Without
	// this the record stays uncertain and the reservation blocks every later
	// request for that target forever.
	if rec.State == protocol.StateUncertain && nerr == nil && (adm.Admitted || adm.Code != "") {
		c, nev := admissionCause(adm, nil)
		e.transitionLocked(rec, c, nev)
		if _, err := e.commitLocked(rec, t); err != nil {
			e.notifyStorageFailureLocked(rec, err)
			e.mu.Unlock()
			return protocol.Reply{}, err
		}
		snap := rec.Snapshot
		e.mu.Unlock()
		ev := achievedEvidence(t)
		e.publishRevision(rec)
		return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: rec.Code, Evidence: ev}}, nil
	}
	// Default: return the durable snapshot. Outcome.Code is read from rec.Code
	// so snapshot.Code == outcome.Code always (Pro #4).
	snap := rec.Snapshot
	out := protocol.Outcome{Op: protocol.OpRequestSubmit, Code: rec.Code}
	if rec.Cancel != nil {
		out.Disposition = rec.Cancel.Disposition
	}
	e.mu.Unlock()
	e.publishRevision(rec)
	return protocol.Reply{Snapshot: snap, Outcome: out}, nil
}

// applyCancelOutcomeLocked handles the 3-outcome CancelExact fork. The
// caller holds e.mu on entry with rec freshly re-read from disk. It applies
// the outcome (cancel_requested / noop_terminal / confirmed), does the
// unlock-Lookup-relock dance for noop_terminal, persists via commitLocked,
// and returns the ackDigest (for the caller to send the native ack outside
// the lock). ONE function for both abortAdmittedRacedRun and
// reconcileCancelRetry — the scattered-cleanup disease, cured for cancel
// outcomes too.
func (e *Endpoint) applyCancelOutcomeLocked(rec *requests.Record, t *target, key requests.Key, epoch, runID string, ev CancelEvidence, nerr error) (string, error) {
	if nerr != nil || ev.Disposition == protocol.CancelRequested {
		// Inconclusive abort: the run was NOT confirmed stopped. Keep the
		// record non-terminal (cancel_requested, NativeRun bound) so reconcile
		// re-drives CancelExact.
		e.transitionLocked(rec, causeCancelRequested, nativeEvidence{runID: runID})
		_, err := e.commitLocked(rec, t)
		return "", err
	}
	if ev.Disposition == protocol.CancelNoopTerminal {
		// The run already finished. Lookup the retained evidence.
		e.mu.Unlock()
		var lookupEv Evidence
		var lookupErr error
		if t != nil {
			lookupEv, lookupErr = t.att.Lookup(key, epoch)
		}
		if lookupErr != nil {
			// B3: a transient Lookup failure must NOT confirm-and-commit with
			// empty evidence. Leave the disposition unconfirmed so
			// reconcileCancelRetry re-drives CancelExact on the next tick.
			e.mu.Lock()
			return "", lookupErr
		}
		e.mu.Lock()
		// Copy into the caller's *Record instead of rebinding the local param
		// — otherwise the caller reads a stale object (Pro #2/Pro #4 bug).
		fresh, ok, _ := e.store.Get(key)
		if !ok {
			return "", protocol.Refuse(protocol.CodeNotFound, "record vanished during noop-terminal lookup")
		}
		*rec = *fresh
		lookupRun := runID
		if lookupEv.RunID != "" {
			lookupRun = lookupEv.RunID
		}
		// causeNone records the result and confirms a pending cancel (folded
		// into transitionLocked) — no sprinkled force-confirmed after.
		e.transitionLocked(rec, causeNone, nativeEvidence{runID: lookupRun, result: lookupEv.Result, runTerminal: true})
		return e.commitLocked(rec, t)
	}
	// CancelExact confirmed: the run was stopped. But disk may have moved under
	// the unlock — a late completion may have committed `completed` while we
	// were unlocked. Pro #3: do NOT rewrite the promise unconditionally. If the
	// record is already terminal and NOT cancelled, settle the runtime
	// obligation only (the cancel disposition is resolved) and return.
	if rec.State.Terminal() && rec.State != protocol.StateCancelled {
		// A late native event resolved the record to a different terminal state.
		// The cancel is still resolved (the run stopped); just settle the
		// disposition without rewriting the promise.
		if rec.Cancel != nil && rec.Cancel.Disposition != protocol.CancelConfirmed {
			rec.Cancel.Disposition = protocol.CancelConfirmed
			return e.commitLocked(rec, t)
		}
		// Already settled; nothing to persist.
		return "", nil
	}
	e.transitionLocked(rec, causeCancelledByRequest, nativeEvidence{runID: runID})
	return e.commitLocked(rec, t)
}

// abortAdmittedRacedRun handles the P1 shape: Submit admitted a run, but a
// native cancel moved the record to cancelled before Submit returned. The
// run is live and unwanted — abort it via CancelExact. The abort's OUTCOME
// matters (Pro #1): confirmed -> cancelled_by_request; inconclusive ->
// non-terminal cancel_requested for reconcile retry; noop_terminal -> the
// run already finished, Lookup the evidence and record it.
// The caller holds e.mu; this helper unlocks exactly once.
func (e *Endpoint) abortAdmittedRacedRun(rec *requests.Record, t *target, adm Admission) (protocol.Reply, error) {
	key := requests.Key{CreatorHost: rec.CreatorHost, TargetID: rec.TargetID, RequestID: rec.RequestID}
	epoch := rec.Epoch
	// Bind NativeRun durably first so reconcile can find the run if the abort
	// is inconclusive.
	e.transitionLocked(rec, causeNone, nativeEvidence{runID: adm.RunID})
	if _, err := e.commitLocked(rec, t); err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	e.mu.Unlock()
	var ev CancelEvidence
	var nerr error
	if t != nil {
		ev, nerr = t.att.CancelExact(key, epoch)
	}
	e.mu.Lock()
	rec, exists, err := e.store.Get(key)
	if err != nil || !exists {
		e.mu.Unlock()
		return protocol.Reply{}, protocol.Refuse(protocol.CodeNotFound, "record vanished during abort")
	}
	ackDigest, cerr := e.applyCancelOutcomeLocked(rec, t, key, epoch, adm.RunID, ev, nerr)
	if cerr != nil {
		e.notifyStorageFailureLocked(rec, cerr)
		e.mu.Unlock()
		return protocol.Reply{}, cerr
	}
	snap := rec.Snapshot
	var ackAtt Attachment
	if ackDigest != "" && t != nil {
		ackAtt = t.att
	}
	ackKey, ackEpoch := key, epoch
	disposition := protocol.CancelDisposition("")
	if rec.Cancel != nil {
		disposition = rec.Cancel.Disposition
	}
	e.mu.Unlock()
	if ackDigest != "" {
		ackAtt.AcknowledgeResult(ackKey, ackEpoch, ackDigest)
		// 611.22.34: delivered — record it durably (digest verified
		// non-empty above). MarkAcknowledged failure ignored (MarkPublished
		// shape): the next replay re-Lookups and re-Marks. Self-healing.
		_ = e.store.MarkAcknowledged(ackKey)
	}
	e.publishRevision(rec)
	return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: rec.Code, Disposition: disposition}}, nil
}

// Wait blocks until the record for ref reaches a terminal or uncertain state
// or ctx ends. It never cancels work. Callers map ctx errors to exit 4 or 130.
func (e *Endpoint) Wait(ctx context.Context, ref string) (protocol.Snapshot, error) {
	host, targetID, requestID, err := protocol.DecodeRef(ref)
	if err != nil {
		return protocol.Snapshot{}, err
	}
	key := requests.Key{CreatorHost: host, TargetID: targetID, RequestID: requestID}
	for {
		e.mu.Lock()
		rec, ok, err := e.store.Get(key)
		ch := e.changed
		e.mu.Unlock()
		if err != nil {
			return protocol.Snapshot{}, err
		}
		if !ok {
			return protocol.Snapshot{}, protocol.Refuse(protocol.CodeNotFound, "no record for request_ref")
		}
		if rec.State.Terminal() || rec.State == protocol.StateUncertain {
			return rec.Snapshot, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return rec.Snapshot, ctx.Err()
		}
	}
}

// Snapshot returns the current record for ref without waiting.
func (e *Endpoint) Snapshot(ref string) (protocol.Snapshot, error) {
	host, targetID, requestID, err := protocol.DecodeRef(ref)
	if err != nil {
		return protocol.Snapshot{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, ok, err := e.store.Get(requests.Key{CreatorHost: host, TargetID: targetID, RequestID: requestID})
	if err != nil {
		return protocol.Snapshot{}, err
	}
	if !ok {
		return protocol.Snapshot{}, protocol.Refuse(protocol.CodeNotFound, "no record for request_ref")
	}
	return rec.Snapshot, nil
}

func (e *Endpoint) notifyLocked(rec *requests.Record) {
	for _, fn := range e.observers {
		fn(rec)
	}
	close(e.changed)
	e.changed = make(chan struct{})
}

// retireObligationLocked clears any undischarged drain obligation for key
// whose recorded revision is <= publishedRev. MarkPublished is a high-water
// mark, so confirming publishedRev covers every earlier pending revision too.
// Called on EVERY path that confirms the durable publication high-water mark:
// the fresh-delivery exit, the marker-only retry path, and the already-published
// branch (codex round-5 P2: marker-only recovery must retire the obligation,
// else Close reports ErrDrainIncomplete for a record that is fully published).
// The caller holds e.mu. Never clears an unrelated key or a strictly higher
// revision (a higher pending obligation survives a lower confirmed publish).
func (e *Endpoint) retireObligationLocked(key requests.Key, publishedRev int64) {
	if ob, ok := e.drainObligations[key]; ok && ob <= publishedRev {
		delete(e.drainObligations, key)
	}
}

// publishLocked publishes the latest revision and records it on success.
// Failures leave published_revision behind so Reconcile retries.
func (e *Endpoint) publishLocked(rec *requests.Record) {
	key := keyOfRecord(rec)
	// Fresh state under the lock: the caller's record may be stale, and a
	// concurrent reconcile may already have published and marked this
	// revision. Publishing from a stale record is the double delivery of
	// Pro r2 #13 (packet 4a, agent-message-queue-611.22.36).
	cur, ok, err := e.store.Get(key)
	if err != nil || !ok {
		return
	}
	if cur.PublishedRevision >= rec.Revision {
		rec.PublishedRevision = cur.PublishedRevision
		// codex round-5 P2: a prior successful publication already discharged
		// this revision (or a strictly newer one). Retire any obligation for
		// this key that the durable high-water mark has covered, so a later
		// Close does not report ErrDrainIncomplete for a fully-published
		// record (the obligation was recorded when an earlier chained attempt
		// skipped, then Reconcile published it via a different caller).
		e.retireObligationLocked(key, cur.PublishedRevision)
		return
	}
	// 611.22.48: visible means CONFIRMED DELIVERY awaiting its marker. If a
	// confirmed delivery for this revision is already pending its marker,
	// skip the DELIVERY (no double delivery) but still fall through to
	// MarkPublished below — the marker may need retrying (Pro r2 #13). An
	// IN-FLIGHT attempt is tracked separately in `publishing`; it does NOT
	// count as delivered.
	if e.visible[key] >= rec.Revision {
		// Marker retry path (Pro r2 #13): this revision was already delivered
		// (confirmed, awaiting its marker). Skip the DELIVERY (no double
		// delivery) and retry only the marker, honoring the crash points so
		// a lost marker stays lost within one simulated crash.
		if e.crashAt(PointAfterPublish) != nil || e.crashAt(PointBeforePublished) != nil {
			return
		}
		if err := e.store.MarkPublished(key, rec.Revision); err == nil {
			rec.PublishedRevision = rec.Revision
			if e.visible[key] == rec.Revision {
				delete(e.visible, key)
			}
			// codex round-5 P2: marker-only recovery (a prior delivery's marker
			// write failed, Reconcile retries only the marker) must also retire
			// the obligation. Without this, Close reports ErrDrainIncomplete
			// although the durable record is now fully published.
			e.retireObligationLocked(key, rec.Revision)
		}
		return
	}
	// Serialize per-key publication: if another caller is already
	// publishing this key, coalesce this caller's revision into the
	// pending obligation set (max-revision semantics, not a call count)
	// and return. This prevents two concurrent publishes for the same
	// key without holding the global endpoint mutex (611.22.48 P1-2)
	// while guaranteeing the skipped revision is still covered by
	// exactly one drain obligation (Astra B784-1): the finishing
	// publisher — the single per-key owner — adopts the pending set and
	// republishes before its drain obligation is released. Outside a
	// shutdown drain the next Tick/Reconcile would also retry it, but
	// registering the obligation uniformly keeps the invariant in every
	// state. Coalescing by highest requested revision avoids duplicate
	// same-revision deliveries (Astra round-2 P1-2).
	if e.publishing[key] {
		if rec.Revision > e.pubPending[key] {
			e.pubPending[key] = rec.Revision
		}
		return
	}
	if e.crashAt(PointBeforePublish) != nil {
		return
	}
	// Claim the in-flight slot for THIS key. Only the owner clears it (on
	// success or failure), so a concurrent caller's reservation is never
	// clobbered. visible is advanced ONLY on a successful publish.
	e.publishing[key] = true
	// 611.22.48 (2nd NO-GO): account for the accepted publication work in
	// the bounded shutdown drain. The publish runs OUTSIDE e.mu (the
	// expensive maildir open + fsync must not block other keys), but Close
	// must not observe zero handlers, close the store, and return while
	// publication is still running — a successful publish could not then
	// MarkPublished (store closed), leaving a duplicate on restart.
	// Incrementing inFlight here makes Close's drain wait for the publish
	// window, preserving the unlocked publisher.
	e.inFlight++
	snap := rec.Snapshot
	origin := rec.Origin
	attemptRev := rec.Revision
	pub := e.publish // read under e.mu (SetPublish writes under it) — round-4 fix carried over #806
	e.mu.Unlock()

	// publish ONE revision outside e.mu, then loop while skipped newer
	// revisions exist. The drain obligation (inFlight) and the per-key
	// publishing ownership are held CONTINUOUSLY across the whole chain —
	// there is no unlock gap with zero drain count in which Close could
	// observe "nothing in flight" and close the store (Astra round-2
	// P1-1). Single iterative owner, no recursive re-entry (Astra
	// round-2 P1-2).
	for {
		pubErr := pub(snap, origin)

		e.mu.Lock()
		// Success bookkeeping for THIS attempt BEFORE selecting the next
		// obligation, so the coalescing check below sees the delivery
		// state (Astra round-2 P1-2). A failed attempt does NOT advance
		// visible/PublishedRevision; the durable unpublished state is
		// preserved for Reconcile recovery (Astra round-2 answer 3).
		if pubErr == nil && e.visible[key] < attemptRev {
			e.visible[key] = attemptRev
		}
		// Select the next obligation: the highest revision coalesced by
		// concurrent callers while we owned this key. A FAILED attempt does
		// not retry the same revision (no spin loop) but still adopts a
		// strictly NEWER accepted revision under the same owner: dropping the
		// r+1 obligation because r failed would leave r+1 uncovered by any
		// drain count and let Close return nil with it unpublished (codex
		// round-3 P1). Reconcile/Tick owns retrying the failed revision
		// itself (beginReconcile still refuses during drain, so the durable
		// unpublished state simply persists for recovery after close).
		pendingRev := e.pubPending[key]
		delete(e.pubPending, key)
		var next *requests.Record
		if pendingRev > attemptRev && e.state != stateClosed {
			// Re-read the newest durable record. A store read failure leaves
			// next nil; the exit path reports the skipped obligation via
			// drainIncomplete so Close does not return a clean nil.
			e.mu.Unlock()
			if cur, ok, gerr := e.store.Get(key); gerr == nil && ok {
				next = cur
			}
			e.mu.Lock()
			// Close may have exhausted its drain timeout and set stateClosed
			// WHILE we were unlocked for the read: do not start a new chained
			// attempt against a closed store (codex round-3 P2). An already
			// running publisher is never cancelled; only a NEW attempt is
			// suppressed here.
			if e.state == stateClosed {
				next = nil
			}
		}
		if next == nil {
			// Chain complete (nothing pending beyond this attempt, the store
			// lost the record, or the endpoint closed): release the drain
			// obligation and per-key ownership, wake Close, return. If a
			// coalesced NEWER revision was NOT discharged (strictly greater
			// than attemptRev: its continuation read failed or the endpoint
			// closed before adoption), record it so Close reports an
			// incomplete drain instead of silently returning nil (codex
			// round-3 P1). A failed attempt of attemptRev ITSELF is NOT an
			// undischarged obligation: the attempt ran and was counted; the
			// durable unpublished state persists for Reconcile recovery (the
			// contract-corpus Q18 restart flow relies on Close returning nil
			// there).
			if pendingRev > attemptRev {
				// Record the highest undischarged pending revision for this
				// key. A later successful publication of pendingRev (or a
				// strictly newer revision) retires this entry in the
				// MarkPublished success path below or in a later Reconcile,
				// so Close reports incomplete ONLY if it remains genuinely
				// unpublished (codex round-4 P2-1: the previous global
				// drainIncomplete latch could be set while stateAccepting
				// and never cleared by a later successful publish).
				if e.drainObligations[key] < pendingRev {
					e.drainObligations[key] = pendingRev
				}
			}
			e.inFlight--
			if e.inFlight == 0 && e.state == stateDraining {
				e.drained.Broadcast()
			}
			delete(e.publishing, key)
			// publishLocked returns HOLDING e.mu (its callers rely on
			// that), so no unlock here.
			// Marker for the LAST attempted revision (chained revisions
			// included): MarkPublished is a high-water mark, so marking
			// attemptRev covers every earlier revision in the chain and
			// advances PublishedRevision past rec.Revision. Only on
			// SUCCESS: a failed attempt must not advance the marker (the
			// durable unpublished state stays for Reconcile recovery).
			if pubErr == nil && e.crashAt(PointAfterPublish) == nil && e.crashAt(PointBeforePublished) == nil {
				if err := e.store.MarkPublished(key, attemptRev); err == nil {
					if rec.PublishedRevision < attemptRev {
						rec.PublishedRevision = attemptRev
					}
					if e.visible[key] == attemptRev {
						delete(e.visible, key)
					}
					// Retire any undischarged obligation for this key that
					// this successful publication (or a strictly newer
					// chained attempt) has now discharged. MarkPublished is
					// a high-water mark, so attemptRev covers every earlier
					// pending revision too (codex round-4 P2-1: a later
					// successful publish must clear a previously-recorded
					// skipped obligation rather than leave a stale latch).
					e.retireObligationLocked(key, attemptRev)
				}
			}
			return
		}
		// Adopt the pending revision and publish it while STILL holding the
		// inFlight obligation and publishing[key] ownership.
		attemptRev = next.Revision
		snap = next.Snapshot
		origin = next.Origin
		e.mu.Unlock()
	}
}

func (e *Endpoint) publishRevision(rec *requests.Record) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.publishLocked(rec)
}

func (e *Endpoint) crashAt(point string) error {
	if e.crash == nil {
		return nil
	}
	if err := e.crash(point); err != nil {
		return fmt.Errorf("%w %s: %w", ErrCrashed, point, err)
	}
	return nil
}

// memoAckIntentLocked computes the evidence digest of a terminal record's
// outcome and makes the ack intent durable, returning the digest and whether
// a native ack should now be sent. Writing the memo BEFORE any native call is
// what makes acknowledgement replayable: the crash gate at PointBeforeAck
// runs after this memo, so a crash between the terminal commit and the native
// ack still leaves a record Reconcile can replay the ack from. When the
// record already carries the matching intent the ack has been sent (or left
// for replay), so fresh=false suppresses a duplicate native call. The caller
// holds e.mu.
func (e *Endpoint) memoAckIntentLocked(rec *requests.Record, t *target) (string, bool) {
	if t == nil {
		return "", false
	}
	digest := protocol.EvidenceDigest(rec.Result)
	if digest == "" || rec.AckDigest == digest {
		return digest, false
	}
	key := keyOfRecord(rec)
	cur, exists, err := e.store.Get(key)
	if err != nil || !exists {
		return "", false
	}
	if cur.AckDigest != digest {
		cur.AckDigest = digest
		if err := e.store.WriteMemo(cur); err != nil {
			return "", false
		}
	}
	rec.AckDigest = digest
	return digest, true
}
