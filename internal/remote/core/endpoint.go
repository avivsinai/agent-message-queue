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
	key := requests.Key{CreatorHost: src.Host, TargetID: cmd.TargetID, RequestID: cmd.RequestID}
	digest := protocol.CommandDigest(cmd)

	e.mu.Lock()
	rec, exists, err := e.store.Get(key)
	if err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	if exists {
		e.mu.Unlock()
		out := protocol.Outcome{Op: protocol.OpRequestSubmit}
		if rec.InputDigest != "" && rec.InputDigest != digest {
			out.Code = protocol.CodeRequestConflict
		}
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: out}, nil
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
		rec.State = protocol.StateRejected
		rec.Code = code
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
	rec.Revision++
	rec.State = protocol.StateDispatching
	rec.NativeDispatches = 1
	rec.ObservedAt = protocol.FormatTime(e.now())
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

	e.mu.Lock()
	defer e.mu.Unlock()
	rec, _, err = e.store.Get(key)
	if err != nil {
		return protocol.Reply{}, err
	}
	if rec.State != protocol.StateDispatching {
		// A fast native event already moved the record; the evidence wins.
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit}}, nil
	}
	switch {
	case nerr != nil:
		rec.State = protocol.StateUncertain
		rec.Code = protocol.CodeAttachmentLost
	case adm.Admitted:
		rec.State = protocol.StateRunning
		run := adm.RunID
		rec.NativeRun = &run
	case adm.Code == protocol.CodeCancelledBeforeAdmission:
		rec.State = protocol.StateCancelled
		rec.Code = adm.Code
		// A cancel that raced this gated admission left the disposition
		// pending; admission never happened, so confirm it now instead of
		// persisting cancelled with cancel_requested.
		if rec.Cancel == nil {
			rec.Cancel = &protocol.Cancel{RequestedAt: protocol.FormatTime(e.now())}
		}
		rec.Cancel.Disposition = protocol.CancelConfirmed
	default:
		rec.State = protocol.StateRejected
		rec.Code = adm.Code
		if rec.Code == "" {
			rec.Code = protocol.CodeNativeError
		}
	}
	rec.Revision++
	rec.ObservedAt = protocol.FormatTime(e.now())
	if err := e.store.Update(rec); err != nil {
		return protocol.Reply{}, err
	}
	e.notifyLocked(rec)
	e.publishLocked(rec)
	return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit}}, nil
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
		rec.State = protocol.StateCancelled
		rec.Code = protocol.CodeCancelledBeforeAdmission
		rec.Cancel = &protocol.Cancel{RequestedAt: now, Disposition: protocol.CancelConfirmed}
		rec.ObservedAt = now
		err := e.store.Update(rec)
		e.notifyLocked(rec)
		e.mu.Unlock()
		if err != nil {
			return protocol.Reply{}, err
		}
		e.publishRevision(rec)
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel}}, nil
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
	rec.Cancel = &protocol.Cancel{RequestedAt: now, Disposition: ev.Disposition}
	if ev.Disposition == protocol.CancelConfirmed {
		rec.State = protocol.StateCancelled
		rec.Code = protocol.CodeCancelledByRequest
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
	// Record the answer durably before the native call so a crash-then-replay
	// cannot answer the same interaction twice.
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

	code, err := t.att.Respond(key, cmd.Epoch, cmd.InteractionID, cmd.Option)
	if err != nil {
		return protocol.Reply{}, err
	}
	if code != "" {
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
	e.mu.Lock()
	defer e.mu.Unlock()
	if ev.Type == EventEpochChanged || ev.Type == EventStatus {
		return
	}
	if ev.Key.RequestID == "" {
		return
	}
	rec, ok, err := e.store.Get(ev.Key)
	if err != nil || !ok {
		return
	}
	if rec.State.Terminal() {
		return
	}
	switch ev.Type {
	case EventRunCompleted, EventRunFailed, EventRunCancelled:
		if rec.State != protocol.StateRunning && rec.State != protocol.StateDispatching && rec.State != protocol.StateUncertain {
			return
		}
		if e.crashAt(PointBeforeResult) != nil {
			return
		}
		switch ev.Type {
		case EventRunCompleted:
			rec.State = protocol.StateCompleted
		case EventRunFailed:
			rec.State = protocol.StateFailed
			rec.Code = protocol.CodeNativeError
		default:
			rec.State = protocol.StateCancelled
			rec.Code = protocol.CodeCancelledByRequest
			if rec.Cancel != nil {
				rec.Cancel.Disposition = protocol.CancelConfirmed
			}
		}
		if ev.RunID != "" {
			run := ev.RunID
			rec.NativeRun = &run
		}
		rec.Result = boundResult(ev.Result)
		rec.Interaction = nil
	case EventQuestion:
		rec.Interaction = ev.Interaction
	case EventQuestionResolved:
		rec.Interaction = nil
	case EventLocalIntervention:
		rec.LocalIntervention = true
	default:
		return
	}
	rec.Revision++
	rec.ObservedAt = protocol.FormatTime(e.now())
	if err := e.store.Update(rec); err != nil {
		// The durable write failed on an async path with no client waiting.
		// Surface a visible storage-failure projection; Reconcile retries.
		e.notifyStorageFailureLocked(rec, err)
		return
	}
	e.notifyLocked(rec)
	if e.crashAt(PointAfterResult) != nil {
		return
	}
	if rec.State.Terminal() {
		if t, ok := e.targets[targetID]; ok {
			if e.crashAt(PointBeforeAck) == nil {
				t.att.AcknowledgeResult(ev.Key, rec.Epoch, rec.InputDigest)
			}
		}
	}
	e.publishLocked(rec)
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

// Reconcile runs after Open and on every Tick. It re-examines every
// non-terminal record against exact native evidence, never re-submits, expires
// deferred requests whose window closed, and republishes unpublished
// revisions. Native attachment calls happen without the endpoint lock held; a
// single poisoned record is reported, never allowed to abort the whole pass.
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
			rerr = e.admitDeferred(rec)
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

// Tick retries deferred admissions and expiry; carriers call it on a timer
// and when a target comes back online.
func (e *Endpoint) Tick() error {
	return e.Reconcile()
}

func keyOfRecord(rec *requests.Record) requests.Key {
	return requests.Key{CreatorHost: rec.CreatorHost, TargetID: rec.TargetID, RequestID: rec.RequestID}
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

	e.mu.Lock()
	defer e.mu.Unlock()
	rec, exists, err := e.store.Get(key)
	if err != nil {
		return err
	}
	if !exists || rec.State.Terminal() {
		return nil
	}
	switch {
	case !ok || lookupErr != nil:
		if rec.State == protocol.StateUncertain {
			return nil
		}
		rec.State = protocol.StateUncertain
		rec.Code = protocol.CodeAttachmentLost
	case ev.Class == EvidenceTentative:
		// Bound but native ownership not yet proven. Never reject a submission
		// that is still about to execute; re-check next tick.
		return nil
	case ev.Class == EvidenceUnknown:
		// Delivered but admission-unknown (transport ambiguity, or an API that
		// cannot report its own rejection). Keep the correlation, stay uncertain.
		if rec.State == protocol.StateUncertain {
			return nil
		}
		rec.State = protocol.StateUncertain
		rec.Code = protocol.CodeAttachmentLost
	case !ev.Known || ev.Class == EvidenceNone:
		// The adapter has a real admission primitive and retains nothing:
		// admission did not and cannot happen. Positive evidence, not a guess.
		rec.State = protocol.StateRejected
		rec.Code = protocol.CodeNativeError
	case ev.Admitted && ev.State.Terminal():
		rec.State = ev.State
		if ev.State == protocol.StateFailed {
			rec.Code = protocol.CodeNativeError
		}
		run := ev.RunID
		rec.NativeRun = &run
		rec.Result = boundResult(ev.Result)
		rec.LocalIntervention = rec.LocalIntervention || ev.LocalIntervention
		rec.Interaction = nil
	case ev.Admitted:
		if rec.State == protocol.StateRunning && rec.NativeRun != nil && *rec.NativeRun == ev.RunID {
			return nil
		}
		rec.State = protocol.StateRunning
		rec.Code = ""
		run := ev.RunID
		rec.NativeRun = &run
		rec.Interaction = ev.Interaction
	default:
		rec.State = protocol.StateRejected
		rec.Code = protocol.CodeNativeError
	}
	rec.Revision++
	rec.ObservedAt = protocol.FormatTime(e.now())
	if err := e.store.Update(rec); err != nil {
		e.notifyStorageFailureLocked(rec, err)
		return err
	}
	e.notifyLocked(rec)
	if rec.State.Terminal() && ok {
		t.att.AcknowledgeResult(key, rec.Epoch, rec.InputDigest)
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
		rec.State = protocol.StateRejected
		rec.Code = code
		rec.ObservedAt = protocol.FormatTime(e.now())
		err = e.store.Update(rec)
		if err == nil {
			e.notifyLocked(rec)
		}
		e.mu.Unlock()
		return err
	}
	// Move to dispatching under the lock, then Submit unlocked.
	rec, exists, err := e.store.Get(key)
	if err != nil || !exists || rec.State != protocol.StateReceived {
		e.mu.Unlock()
		return err
	}
	rec.Revision++
	rec.State = protocol.StateDispatching
	rec.NativeDispatches = 1
	rec.ObservedAt = protocol.FormatTime(e.now())
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

	e.mu.Lock()
	defer e.mu.Unlock()
	rec, exists, err = e.store.Get(key)
	if err != nil || !exists || rec.State != protocol.StateDispatching {
		return err
	}
	switch {
	case nerr != nil:
		rec.State, rec.Code = protocol.StateUncertain, protocol.CodeAttachmentLost
	case adm.Admitted:
		rec.State = protocol.StateRunning
		run := adm.RunID
		rec.NativeRun = &run
	case adm.Code == protocol.CodeCancelledBeforeAdmission:
		rec.State = protocol.StateCancelled
		rec.Code = adm.Code
		if rec.Cancel == nil {
			rec.Cancel = &protocol.Cancel{RequestedAt: protocol.FormatTime(e.now())}
		}
		rec.Cancel.Disposition = protocol.CancelConfirmed
	default:
		rec.State, rec.Code = protocol.StateRejected, adm.Code
		if rec.Code == "" {
			rec.Code = protocol.CodeNativeError
		}
	}
	rec.Revision++
	rec.ObservedAt = protocol.FormatTime(e.now())
	if err := e.store.Update(rec); err != nil {
		return err
	}
	e.notifyLocked(rec)
	return nil
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
