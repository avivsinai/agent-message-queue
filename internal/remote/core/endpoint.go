// Package core is the amq-remote endpoint: it owns request identity and
// records, runs the admission algorithm against native attachments, applies
// native evidence, reconciles after a restart, and publishes revisions. It
// does not know how commands arrive; carriers decode with protocol and call
// Handle.
package core

import (
	"context"
	"encoding/json"
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

// Endpoint is the request handler. One Endpoint owns one Store.
type Endpoint struct {
	mu        sync.Mutex
	store     *requests.Store
	targets   map[string]*target
	publish   Publisher
	crash     CrashPoint
	now       func() time.Time
	observers []func(*requests.Record)
	changed   chan struct{}
}

// Config configures New.
type Config struct {
	Store   *requests.Store
	Publish Publisher
	Crash   CrashPoint
	Now     func() time.Time
}

// New builds an endpoint over an open store. Attachments register through
// Register; Reconcile should run before the first command.
func New(cfg Config) *Endpoint {
	e := &Endpoint{
		store:   cfg.Store,
		targets: map[string]*target{},
		publish: cfg.Publish,
		crash:   cfg.Crash,
		now:     cfg.Now,
		changed: make(chan struct{}),
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.publish == nil {
		e.publish = func(protocol.Snapshot, map[string]string) error { return nil }
	}
	return e
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

// Close unsubscribes from every attachment and closes the store. Records and
// the attachments' retained evidence survive for the next endpoint.
func (e *Endpoint) Close() error {
	e.mu.Lock()
	for id, t := range e.targets {
		if t.unsubscribe != nil {
			t.unsubscribe()
		}
		delete(e.targets, id)
	}
	e.mu.Unlock()
	return e.store.Close()
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

// InputDigest is the protocol digest of a submit input, computed from its
// canonical JSON so every carrier agrees on the bytes.
func InputDigest(in *protocol.SubmitInput) string {
	data, err := json.Marshal(in)
	if err != nil {
		return ""
	}
	return requests.Digest(data)
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
		t, code := e.admissibleLocked(cmd.TargetID, cmd.Epoch, cmd.NotAfter)
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
			rec.Revision++
			e.transitionLocked(rec, causeBusyTombstone, nativeEvidence{})
			rec.ObservedAt = protocol.FormatTime(e.now())
			if err := e.store.Update(rec); err != nil {
				e.mu.Unlock()
				return protocol.Reply{}, err
			}
			e.notifyLocked(rec)
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
	if err := e.crashAt(PointAfterReceived); err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	t, code := e.admissibleLocked(cmd.TargetID, cmd.Epoch, cmd.NotAfter)
	if code != "" {
		rec.Revision++
		e.transitionLocked(rec, causeRefused, nativeEvidence{code: code})
		rec.ObservedAt = protocol.FormatTime(e.now())
		err := e.store.Update(rec)
		e.notifyLocked(rec)
		e.mu.Unlock()
		if err != nil {
			return protocol.Reply{}, err
		}
		e.publishRevision(rec)
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit}}, nil
	}
	if t == nil {
		// Target registered but offline: keep received and let Tick admit
		// or expire it later.
		e.mu.Unlock()
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit}}, nil
	}
	// B14 per-runtime reservation (B10): refuse a second concurrent dispatch
	// for the same target. Fail-closed — an undeterminable reservation state
	// never authorizes a dispatch.
	reserved, rerr := e.runtimeInFlightLocked(cmd.TargetID, key)
	if rerr != nil {
		// An undeterminable reservation must not leave a Tick-admissible
		// `received` placeholder: the caller saw a failure, so the record is
		// terminal rejected (uncertain code) — reconcile re-evaluates. Return
		// a typed refusal so the caller maps to the right exit, not exit 1.
		rec.Revision++
		e.transitionLocked(rec, causeAttachmentLost, nativeEvidence{})
		rec.ObservedAt = protocol.FormatTime(e.now())
		uerr := e.store.Update(rec)
		if uerr == nil {
			e.notifyLocked(rec)
			snap := rec.Snapshot
			e.mu.Unlock()
			e.publishRevision(rec)
			return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: protocol.CodeAttachmentLost}}, protocol.Refuse(protocol.CodeAttachmentLost, "%s", rerr.Error())
		}
		e.mu.Unlock()
		return protocol.Reply{}, protocol.Refuse(protocol.CodeAttachmentLost, "%s", rerr.Error())
	}
	if reserved {
		// Persist a TERMINAL rejected+busy tombstone — never a `received`
		// placeholder a later Tick would auto-dispatch (queue-on-reject is
		// D1-disabled). The tombstone keeps dedup and makes an identical
		// resubmit re-admittable.
		rec.Revision++
		e.transitionLocked(rec, causeBusyTombstone, nativeEvidence{})
		rec.ObservedAt = protocol.FormatTime(e.now())
		if err := e.store.Update(rec); err != nil {
			e.mu.Unlock()
			return protocol.Reply{}, err
		}
		e.notifyLocked(rec)
		busySnap := rec.Snapshot
		e.mu.Unlock()
		e.publishRevision(rec)
		return protocol.Reply{Snapshot: busySnap, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: rec.Code}}, nil
	}
	rec.Revision++
	rec.NativeDispatches = 1
	rec.ObservedAt = protocol.FormatTime(e.now())
	e.transitionLocked(rec, causeDispatching, nativeEvidence{})
	if err := e.crashAt(PointBeforeDispatching); err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
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

// admissibleLocked checks share, epoch, expiry and capability. It returns the
// live target, or nil with an empty code when the target is registered but
// offline, or nil with a refusal code.
func (e *Endpoint) admissibleLocked(targetID, epoch, notAfter string) (*target, protocol.Code) {
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
	if s.Attachment == "offline" {
		return nil, ""
	}
	return t, ""
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
		e.notifyLocked(rec)
		e.mu.Unlock()
		if err != nil {
			return protocol.Reply{}, err
		}
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
		rec.Revision++
		e.transitionLocked(rec, causeCancelledBeforeAdmission, nativeEvidence{})
		rec.ObservedAt = now
		err := e.store.Update(rec)
		e.notifyLocked(rec)
		e.mu.Unlock()
		if err != nil {
			return protocol.Reply{}, err
		}
		e.publishRevision(rec)
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel, Disposition: protocol.CancelConfirmed}}, nil
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
	rec.Revision++
	if ev.Disposition == protocol.CancelConfirmed {
		e.transitionLocked(rec, causeCancelledByRequest, nativeEvidence{})
	} else {
		// cancel_requested (intent recorded, run not stopped) or noop — keep
		// the disposition without going terminal; reconcile re-drives if a
		// run is bound.
		if rec.Cancel == nil {
			rec.Cancel = &protocol.Cancel{RequestedAt: now}
		}
		rec.Cancel.Disposition = ev.Disposition
	}
	rec.ObservedAt = now
	if err := e.store.Update(rec); err != nil {
		return protocol.Reply{}, err
	}
	e.notifyLocked(rec)
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
		// A replay of an answer we already delivered; do not invoke the
		// attachment a second time.
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpInteractionRespond, Code: protocol.CodeAlreadyResolved}}, nil
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
	if _, done := rec.Answered[cmd.InteractionID]; done {
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
	rec.Revision++
	if rec.Answered == nil {
		rec.Answered = map[string]string{}
	}
	rec.Answered[cmd.InteractionID] = cmd.Option
	rec.ObservedAt = protocol.FormatTime(e.now())
	if err := e.store.Update(rec); err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	e.notifyLocked(rec)
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
				cur.Revision++
				cur.ObservedAt = protocol.FormatTime(e.now())
				if uerr := e.store.Update(cur); uerr != nil {
					// The intent could not be cleared durably. Leave the record
					// as-is and surface the failure: the replay path, which
					// revalidates the pending interaction, is the safe
					// arbiter — never a fresh answer.
					e.mu.Unlock()
					return protocol.Reply{}, uerr
				}
				rec = cur
				e.notifyLocked(rec)
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

func (e *Endpoint) list() []protocol.Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]protocol.Session, 0, len(e.targets))
	for _, t := range e.targets {
		out = append(out, t.att.Inspect())
	}
	return out
}

func (e *Endpoint) inspect(targetID string) (any, error) {
	e.mu.Lock()
	t, ok := e.targets[targetID]
	e.mu.Unlock()
	if !ok {
		return nil, protocol.Refuse(protocol.CodeNotFound, "target %s is not registered", targetID)
	}
	return t.att.Inspect(), nil
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
	if rec.State.Terminal() {
		e.mu.Unlock()
		return
	}
	switch ev.Type {
	case EventRunCompleted, EventRunFailed, EventRunCancelled:
		if rec.State != protocol.StateRunning && rec.State != protocol.StateDispatching && rec.State != protocol.StateUncertain {
			e.mu.Unlock()
			return
		}
		if e.crashAt(PointBeforeResult) != nil {
			e.mu.Unlock()
			return
		}
		nev := nativeEvidence{runID: ev.RunID, result: ev.Result}
		switch ev.Type {
		case EventRunCompleted:
			e.transitionLocked(rec, causeCompleted, nev)
		case EventRunFailed:
			e.transitionLocked(rec, causeFailed, nev)
		default:
			e.transitionLocked(rec, causeCancelledByRequest, nev)
		}
	case EventQuestion:
		rec.Interaction = ev.Interaction
	case EventQuestionResolved:
		rec.Interaction = nil
	case EventLocalIntervention:
		rec.LocalIntervention = true
	default:
		e.mu.Unlock()
		return
	}
	rec.Revision++
	rec.ObservedAt = protocol.FormatTime(e.now())
	if err := e.store.Update(rec); err != nil {
		// The durable write failed on an async path with no client waiting.
		// Surface a visible storage-failure projection; Reconcile retries.
		e.notifyStorageFailureLocked(rec, err)
		e.mu.Unlock()
		return
	}
	e.notifyLocked(rec)
	if e.crashAt(PointAfterResult) != nil {
		e.mu.Unlock()
		return
	}
	// B14a: publish stays under e.mu (it writes store metadata that Close
	// serializes against via this mutex); only the native ack — the slow,
	// untrusted call — moves OUTSIDE e.mu. The durable ack memo
	// (memoAckIntentLocked) has already made the intent replayable, so a
	// wedged attachment ack must not stall command handling, and a crash
	// mid-ack is recoverable via replayTerminalAck.
	ackDigest := ""
	var ackAtt Attachment
	if rec.State.Terminal() {
		if t, ok := e.targets[targetID]; ok {
			ackDigest, _ = e.memoAckIntentLocked(rec, t)
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
	localIntervention bool
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
		// already-terminal record (the cancel stands, the result is acked).
		if ev.result != nil {
			rec.Result = boundResult(ev.result)
		}
		if ev.runID != "" {
			run := ev.runID
			rec.NativeRun = &run
		}
		if ev.interaction != nil {
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
		if rec.Cancel == nil {
			rec.Cancel = &protocol.Cancel{RequestedAt: protocol.FormatTime(e.now())}
		}
		rec.Cancel.Disposition = protocol.CancelRequested
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
			if rec.State.Terminal() {
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
	if rec.AckDigest == "" {
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
		return nil
	}
	if ev.State != rec.State || ev.Result == nil || protocol.EvidenceDigest(ev.Result) != rec.AckDigest {
		// The retained evidence is not the outcome this record acked (a stale
		// or foreign ack must never release a different request's result).
		return nil
	}
	// B14a: the native ack runs OUTSIDE e.mu like every other native call —
	// a wedged attachment ack on this recovery path must not hold the
	// endpoint mutex forever. The ack digest was durably memoed before it was
	// first sent, so a crash mid-ack is replayable.
	key := keyOfRecord(rec)
	epoch, digest := rec.Epoch, rec.AckDigest
	e.mu.Lock()
	att := t.att
	e.mu.Unlock()
	att.AcknowledgeResult(key, epoch, digest)
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
		if ev.State == protocol.StateFailed {
			e.transitionLocked(rec, causeFailed, nev)
		} else if ev.State == protocol.StateCancelled {
			e.transitionLocked(rec, causeCancelledByRequest, nev)
		} else {
			e.transitionLocked(rec, causeCompleted, nev)
		}
		rec.LocalIntervention = rec.LocalIntervention || ev.LocalIntervention
	case ev.Admitted:
		if rec.State == protocol.StateRunning && rec.NativeRun != nil && *rec.NativeRun == ev.RunID {
			e.mu.Unlock()
			return nil
		}
		e.transitionLocked(rec, causeAdmitted, nativeEvidence{runID: ev.RunID, interaction: ev.Interaction})
	default:
		e.transitionLocked(rec, causeRefused, nativeEvidence{})
	}
	rec.Revision++
	rec.ObservedAt = protocol.FormatTime(e.now())
	if err := e.store.Update(rec); err != nil {
		e.notifyStorageFailureLocked(rec, err)
		e.mu.Unlock()
		return err
	}
	e.notifyLocked(rec)
	// B14a: the durable ack memo was written under the lock above
	// (memoAckIntentLocked), so the replay path stays correct if we
	// crash before the native call. The native ack itself runs OUTSIDE e.mu —
	// a wedged attachment ack must not hold the endpoint mutex forever.
	ackKey, ackEpoch, ackDigest, ackAtt := key, rec.Epoch, "", Attachment(nil)
	if rec.State.Terminal() && ok {
		ackDigest, _ = e.memoAckIntentLocked(rec, t)
		if ackDigest != "" {
			ackAtt = t.att
		}
	}
	e.mu.Unlock()
	if ackAtt != nil && ackDigest != "" {
		ackAtt.AcknowledgeResult(ackKey, ackEpoch, ackDigest)
	}
	return nil
}

// admitDeferred admits a received record whose target came back inside its
// window. The native Submit happens without the endpoint lock.
func (e *Endpoint) admitDeferred(rec *requests.Record) error {
	key := keyOfRecord(rec)
	e.mu.Lock()
	t, code := e.admissibleLocked(rec.TargetID, rec.Epoch, rec.NotAfter)
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
		rec.Revision++
		e.transitionLocked(rec, causeRefused, nativeEvidence{code: code})
		rec.ObservedAt = protocol.FormatTime(e.now())
		err = e.store.Update(rec)
		if err == nil {
			e.notifyLocked(rec)
		}
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
	rec, exists, err := e.store.Get(key)
	if err != nil || !exists || rec.State != protocol.StateReceived {
		e.mu.Unlock()
		return err
	}
	rec.Revision++
	rec.NativeDispatches = 1
	rec.ObservedAt = protocol.FormatTime(e.now())
	e.transitionLocked(rec, causeDispatching, nativeEvidence{})
	if err := e.store.Update(rec); err != nil {
		e.mu.Unlock()
		return err
	}
	e.notifyLocked(rec)
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
	if !exists || rec.State != protocol.StateDispatching {
		// A fast native event already moved the record; the evidence wins.
		// Return the DURABLE snapshot, never an empty success.
		if !exists {
			e.mu.Unlock()
			return protocol.Reply{}, protocol.Refuse(protocol.CodeNotFound, "record vanished during dispatch")
		}
		// A cancel that raced a POSITIVE admission leaves a live orphan run:
		// the record shows cancelled but a run is executing. Abort that run
		// before persisting — never confirm a cancel for work that is running.
		if adm.Admitted && nerr == nil && rec.State == protocol.StateCancelled {
			epoch := rec.Epoch
			key := requests.Key{CreatorHost: rec.CreatorHost, TargetID: rec.TargetID, RequestID: rec.RequestID}
			e.mu.Unlock()
			if t != nil {
				_, _ = t.att.CancelExact(key, epoch) // best-effort abort; evidence already on disk
			}
			e.mu.Lock()
			rec, exists, err := e.store.Get(key)
			if err != nil || !exists {
				e.mu.Unlock()
				return protocol.Reply{}, protocol.Refuse(protocol.CodeNotFound, "record vanished during abort")
			}
			if rec.State != protocol.StateCancelled {
				// The abort or a later native event resolved it; trust disk.
				snap := rec.Snapshot
				e.mu.Unlock()
				return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit}}, nil
			}
			// Fall through: the record is cancelled_by_request (native truth),
			// not cancelled_before_admission. Do NOT synthesize a confirmed
			// Cancel — the run existed.
			return e.finishCancelledByRequestLocked(rec)
		}
		if rec.State == protocol.StateCancelled && !adm.Admitted && nerr == nil {
			// B3: cancellation raced admission and never got metadata, and the
			// native outcome positively establishes admission never happened.
			rec.Revision++
			e.transitionLocked(rec, causeCancelledBeforeAdmission, nativeEvidence{})
			rec.ObservedAt = protocol.FormatTime(e.now())
			if err := e.store.Update(rec); err != nil {
				// The repair write failed: never publish or return a mutated
				// record that did not persist. Propagate so the caller knows
				// the durable record still holds the un-repaired state; a
				// reconcile re-drives the repair.
				e.notifyStorageFailureLocked(rec, err)
				e.mu.Unlock()
				return protocol.Reply{}, err
			}
			e.notifyLocked(rec)
		}
		snap := rec.Snapshot
		out := protocol.Outcome{Op: protocol.OpRequestSubmit}
		if rec.State == protocol.StateCancelled {
			// Emit cancelled_before_admission ONLY when the native outcome
			// positively established admission never happened. A record whose
			// own Code is cancelled_by_request (a real run was cancelled)
			// carries that code, not before-admission.
			if !adm.Admitted && nerr == nil && (adm.Code == protocol.CodeCancelledBeforeAdmission || rec.Code == "" || rec.Code == protocol.CodeCancelledBeforeAdmission) {
				out.Code = protocol.CodeCancelledBeforeAdmission
			}
			if rec.Cancel != nil {
				out.Disposition = rec.Cancel.Disposition
			}
		}
		e.mu.Unlock()
		e.publishRevision(rec)
		return protocol.Reply{Snapshot: snap, Outcome: out}, nil
	}
	switch {
	case nerr != nil:
		e.transitionLocked(rec, causeAttachmentLost, nativeEvidence{runID: adm.RunID})
	case adm.Admitted:
		e.transitionLocked(rec, causeAdmitted, nativeEvidence{runID: adm.RunID})
	case adm.Code == protocol.CodeCancelledBeforeAdmission:
		e.transitionLocked(rec, causeCancelledBeforeAdmission, nativeEvidence{})
	default:
		e.transitionLocked(rec, causeRefused, nativeEvidence{code: adm.Code})
	}
	rec.Revision++
	rec.ObservedAt = protocol.FormatTime(e.now())
	if err := e.store.Update(rec); err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	e.notifyLocked(rec)
	snap := rec.Snapshot
	e.mu.Unlock()
	e.publishRevision(rec)
	return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit}}, nil
}

// finishCancelledByRequestLocked persists a cancelled-by-request outcome for
// a record whose cancel was native truth (a real run was cancelled), carrying
// the cancelled_by_request code — never cancelled_before_admission. The caller
// holds e.mu; this helper unlocks exactly once.
func (e *Endpoint) finishCancelledByRequestLocked(rec *requests.Record) (protocol.Reply, error) {
	rec.Revision++
	e.transitionLocked(rec, causeCancelledByRequest, nativeEvidence{})
	rec.ObservedAt = protocol.FormatTime(e.now())
	if err := e.store.Update(rec); err != nil {
		e.notifyStorageFailureLocked(rec, err)
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	e.notifyLocked(rec)
	snap := rec.Snapshot
	e.mu.Unlock()
	e.publishRevision(rec)
	return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: rec.Code, Disposition: rec.Cancel.Disposition}}, nil
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

// publishLocked publishes the latest revision and records it on success.
// Failures leave published_revision behind so Reconcile retries.
func (e *Endpoint) publishLocked(rec *requests.Record) {
	if e.crashAt(PointBeforePublish) != nil {
		return
	}
	if err := e.publish(rec.Snapshot, rec.Origin); err != nil {
		return
	}
	if e.crashAt(PointAfterPublish) != nil || e.crashAt(PointBeforePublished) != nil {
		return
	}
	key := requests.Key{CreatorHost: rec.CreatorHost, TargetID: rec.TargetID, RequestID: rec.RequestID}
	if err := e.store.MarkPublished(key, rec.Revision); err == nil {
		rec.PublishedRevision = rec.Revision
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
