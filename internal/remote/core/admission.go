package core

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// maxLinkInFlight bounds the open (non-terminal) requests of one link. Each
// holds a store reservation of up to protocol.MaxRecordBytes, so the bound
// keeps a linked server from filling the machine's store.
const maxLinkInFlight = 4

func (e *Endpoint) submit(cmd *protocol.Command, src Source) (protocol.Reply, error) {
	// busy=queue and deliver=steer are disabled in v1 — their
	// ownership+evidence models do not exist (steer would overwrite
	// the running run's byTurn mapping; queue would admit without a NotAfter
	// check at native admission or a per-runtime reservation). The gate sits
	// BEFORE any durable write, so a disabled-mode submit consumes nothing
	// and a plain retry with reject/turn succeeds.
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
		// not answered from the tombstone (minimal tombstone semantics;
		// full safety/reset is not handled here). Recognizable as a
		// tombstoned rejected record with no dispatch behind it and the same
		// digest; only a DIFFERENT digest is a conflict. A link request is
		// never re-admitted: its busy refusal is a terminal record, which
		// compaction later turns into this same tombstone shape.
		retryable := rec.Origin["carrier"] != CarrierLink &&
			rec.Tombstone &&
			rec.State == protocol.StateRejected &&
			rec.Code == protocol.CodeBusy &&
			rec.NativeDispatches == 0 &&
			rec.InputDigest == digest
		if !retryable {
			e.mu.Unlock()
			out := protocol.Outcome{Op: protocol.OpRequestSubmit}
			if rec.InputDigest != "" && rec.InputDigest != digest {
				out.Code = protocol.CodeRequestConflict
			} else if rec.State == protocol.StateRejected {
				// Same-digest retry of a rejected submit. status/get already
				// returns the stored adapter text; the duplicate submit must too.
				out.Message = rec.RefusalReason
			}
			return protocol.Reply{Snapshot: rec.Snapshot, Outcome: out}, nil
		}
		// Fall through to the normal admission path with the existing
		// record: the deferred/Tick machinery owns it from here. A link
		// record never gets here (retryable is false for it); a Buzz one
		// does, so this third entrance to native Submit checks the native
		// session too.
		return e.resubmitTombstoneLocked(cmd, src, key, rec)
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
		Input:             cmd.Input,
		Origin:            maps.Clone(src.Origin),
		NativeSession:     src.NativeSession,
		ConsentCredential: src.Credential,
	}
	// Decide admissibility + reservation BEFORE Create: a request we are
	// about to refuse is never written (no durable placeholder to leak on a
	// failed rejection write, and nothing for Tick to dispatch). Only Create
	// when we will dispatch or deliberately defer.
	if isLink(src) && (src.NativeSession == "" || src.Credential == "") {
		e.mu.Unlock()
		return protocol.Reply{}, protocol.Refuse(protocol.CodeConsentInvalid, "a link submit runs only from verified signed bytes")
	}
	if src.Shared != nil && !slices.Contains(src.Shared, cmd.TargetID) {
		// A link reaches only the targets shared with it.
		e.mu.Unlock()
		return protocol.Reply{Snapshot: e.unpersisted(rec, protocol.StateRejected, protocol.CodeUnshared), Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: protocol.CodeUnshared}}, nil
	}
	t, code, reason := e.admissibleLocked(cmd.TargetID, cmd.Epoch, cmd.NotAfter, cmd.Input.MinEvidence)
	if code != "" {
		// Refused (unshared/expired/stale_epoch): no durable record.
		e.mu.Unlock()
		return protocol.Reply{Snapshot: e.unpersisted(rec, protocol.StateRejected, code), Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: code, Message: reason}}, nil
	}
	if isLink(src) {
		// The per-link bound refuses before any side effect, so unlike the
		// per-target busy below it leaves no record: nothing was asked yet.
		full, ferr := e.linkFullLocked(src.Host)
		if ferr != nil {
			e.mu.Unlock()
			return protocol.Reply{Snapshot: e.unpersisted(rec, protocol.StateRejected, protocol.CodeAttachmentLost), Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: protocol.CodeAttachmentLost}}, protocol.Refuse(protocol.CodeAttachmentLost, "%s", ferr.Error())
		}
		if full {
			e.mu.Unlock()
			return protocol.Reply{Snapshot: e.unpersisted(rec, protocol.StateRejected, protocol.CodeBusy), Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: protocol.CodeBusy, Message: fmt.Sprintf("this link already has %d requests open", maxLinkInFlight)}}, nil
		}
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
	// A session-bound request runs only on the native session it is bound
	// to, and a link request only with a consent key still accepted. This
	// check and the dispatching commit below share one critical section; the
	// handoff is the cutoff.
	if code := e.handoffLocked(rec, t); code != "" {
		e.mu.Unlock()
		return protocol.Reply{Snapshot: e.unpersisted(rec, protocol.StateRejected, code), Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: code}}, nil
	}
	// Per-runtime reservation: refuse a second concurrent dispatch
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
		// identical resubmit re-admittable. A link gets an ordinary refused
		// record instead: it compacts after the horizon and is never
		// re-admitted; sending again is a new request id.
		if isLink(src) {
			e.transitionLocked(rec, causeRefused, nativeEvidence{code: protocol.CodeBusy})
			// Nothing reads the input of a request that never runs, and the
			// record waits for the sink's acknowledgement before it compacts.
			rec.Input = nil
		} else {
			e.transitionLocked(rec, causeBusyTombstone, nativeEvidence{})
		}
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
	return e.dispatchReceivedLocked(cmd, key, rec, t)
}

// resubmitTombstoneLocked admits an identical resubmit of the busy-rejected
// tombstone rec by src. The caller holds e.mu; it unlocks exactly once on
// every return.
func (e *Endpoint) resubmitTombstoneLocked(cmd *protocol.Command, src Source, key requests.Key, rec *requests.Record) (protocol.Reply, error) {
	t, code, reason := e.admissibleLocked(cmd.TargetID, cmd.Epoch, cmd.NotAfter, cmd.Input.MinEvidence)
	if code != "" {
		e.mu.Unlock()
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: code, Message: reason}}, nil
	}
	if t == nil {
		e.mu.Unlock()
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit}}, nil
	}
	// The resubmit is bound to the session its source proves now, which the
	// dispatching write below stores. Only the creator host reaches this
	// record, so the source is the same carrier that stored the tombstone.
	rec.NativeSession = src.NativeSession
	if code := e.handoffLocked(rec, t); code != "" {
		e.mu.Unlock()
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: code}}, nil
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
	// Same reservation as a fresh admission: the room
	// for the result is taken before dispatch, and released on every
	// exit that does not reach the dispatching write. Storage full
	// leaves the busy tombstone as it was, still re-admittable.
	if err := e.store.Reserve(key, protocol.MaxRecordBytes); err != nil {
		e.mu.Unlock()
		var r *protocol.Refusal
		if errors.As(err, &r) && r.Code == protocol.CodeStorageFull {
			return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: protocol.CodeStorageFull}}, nil
		}
		return protocol.Reply{}, err
	}
	rec.Revision++
	rec.NativeDispatches = 1
	rec.ObservedAt = protocol.FormatTime(e.now())
	e.transitionLocked(rec, causeDispatching, nativeEvidence{})
	if err := e.crashAt(PointBeforeDispatching); err != nil {
		e.store.ReleaseReservation(key)
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	// Cannot use commitLocked: crash-point ordering requires Update between
	// PointBeforeDispatching and PointAfterDispatching.
	if err := e.store.Update(rec); err != nil {
		e.store.ReleaseReservation(key)
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	e.notifyLocked(rec)
	if err := e.crashAt(PointAfterDispatching); err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	input, origin, native := *cmd.Input, maps.Clone(rec.Origin), rec.NativeSession
	e.mu.Unlock()
	if err := e.crashAt(PointBeforeNative); err != nil {
		return protocol.Reply{}, err
	}
	adm, nerr := t.att.Submit(BoundRequest{Key: key, Epoch: cmd.Epoch, Input: input, NotAfter: cmd.NotAfter, Origin: origin, NativeSession: native})
	if err := e.crashAt(PointAfterNative); err != nil {
		return protocol.Reply{}, err
	}
	e.mu.Lock()
	rec, exists, err := e.store.Get(key)
	if err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	return e.finishAdmissionLocked(rec, exists, t, adm, nerr)
}

// dispatchReceivedLocked reserves room for the admissible new request rec,
// creates it as received, dispatches it to t and finishes the admission. The
// caller holds e.mu; it unlocks exactly once on every return.
func (e *Endpoint) dispatchReceivedLocked(cmd *protocol.Command, key requests.Key, rec *requests.Record, t *target) (protocol.Reply, error) {
	// Reserve room for the accepted work plus its bounded
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
	origin, native := maps.Clone(rec.Origin), rec.NativeSession
	e.mu.Unlock()

	if err := e.crashAt(PointBeforeNative); err != nil {
		return protocol.Reply{}, err
	}
	adm, nerr := t.att.Submit(BoundRequest{Key: key, Epoch: cmd.Epoch, Input: *cmd.Input, NotAfter: cmd.NotAfter, Origin: origin, NativeSession: native})
	if err := e.crashAt(PointAfterNative); err != nil {
		return protocol.Reply{}, err
	}

	// No defer: finishAdmissionLocked unlocks exactly once on every return.
	e.mu.Lock()
	rec, _, err := e.store.Get(key)
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
func (e *Endpoint) admissibleLocked(targetID, epoch, notAfter, minEvidence string) (*target, protocol.Code, string) {
	t, code, reason := e.admissibleCodeLocked(targetID, epoch, notAfter, minEvidence)
	return t, code, protocol.BoundReason(reason)
}

// admissibleCodeLocked is admissibleLocked's decision. reason is the
// adapter's text for a submit-false refusal, read from the same Inspect.
func (e *Endpoint) admissibleCodeLocked(targetID, epoch, notAfter, minEvidence string) (*target, protocol.Code, string) {
	t, ok := e.targets[targetID]
	if !ok {
		return nil, protocol.CodeUnshared, ""
	}
	var s protocol.Session
	blocked := ""
	if b, ok := t.att.(SubmitBlocker); ok {
		s, blocked = b.InspectSubmit()
	} else {
		s = t.att.Inspect()
	}
	if s.Epoch != epoch {
		return nil, protocol.CodeStaleEpoch, ""
	}
	deadline, err := protocol.ParseTime(notAfter)
	if err != nil || e.now().After(deadline) {
		return nil, protocol.CodeExpired, ""
	}
	if !s.Capabilities.Submit {
		return nil, protocol.CodeUnsupported, blocked
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
			return nil, protocol.CodeUnsupported, ""
		}
	}
	if s.Attachment == "offline" {
		return nil, "", ""
	}
	return t, "", ""
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

// handoffLocked decides, right before the handoff, whether a request may
// still run. A link request needs its consent key still accepted. A
// session-bound request runs only while the target is attached to the exact
// native session the record names; an unknown (empty) session on either side
// refuses, so a record stored before Buzz requests carried a session never
// runs. A request no session binds passes.
func (e *Endpoint) handoffLocked(rec *requests.Record, t *target) protocol.Code {
	if rec.Origin["carrier"] == CarrierLink && (e.consent == nil || !e.consent(rec.CreatorHost, rec.ConsentCredential)) {
		return protocol.CodeConsentInvalid
	}
	if rec.NativeSession == "" && !sessionBound(rec.Origin) {
		return ""
	}
	native := ""
	if n, ok := t.att.(NativeIdentifier); ok {
		native = n.NativeSessionID()
	}
	if native == "" || native != rec.NativeSession {
		return protocol.CodeSessionChanged
	}
	return ""
}

// sessionBound reports whether requests that arrived with origin may run in
// one native session only: a link's signed session or a Buzz share's
// approved one.
func sessionBound(origin map[string]string) bool {
	return origin["carrier"] == CarrierLink || origin["carrier"] == "buzz"
}

// linkFullLocked reports whether the link with creator host host already
// has maxLinkInFlight open requests. Every non-terminal record holds a store
// reservation (requests.Store reseeds one for each at Open), received ones
// included, so all of them count. An unreadable record of this link makes
// the count undeterminable and fails closed. The caller holds e.mu.
func (e *Endpoint) linkFullLocked(host string) (bool, error) {
	recs, poison, err := e.store.ListWithPoison()
	if err != nil {
		return false, err
	}
	for _, p := range poison {
		if p.Key.CreatorHost == "" || p.Key.CreatorHost == host {
			return false, fmt.Errorf("link %s has an unreadable record %s: %w", host, p.Path, errUndeterminableReservation)
		}
	}
	open := 0
	for _, r := range recs {
		if r.CreatorHost == host && !r.State.Terminal() {
			open++
		}
	}
	return open >= maxLinkInFlight, nil
}

// runtimeInFlightLocked reports whether another request for the same target
// is in flight at the endpoint: any record in dispatching (admission
// unknown), running (native-owned), or uncertain (outcome unknown) state.
// This is the per-runtime reservation — the endpoint's own knowledge of
// what it has outstanding, not the attachment's Inspect() status, which is
// advisory and racy. Scope note: a terminal record whose ack is pending is
// NOT reserved — the attachment owns the native unacked-result slot (its
// Submit already refuses on one) and the ack-replay machinery recovers it;
// reserving it at the endpoint too double-counted and made the endpoint
// refuse submits the attachment would accept. Fail-closed: an enumeration
// error is PROPAGATED, never read as "none" — a storage error must not
// authorize the dispatch the reservation exists
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

// admitDeferred admits a received record whose target came back inside its
// window. The native Submit happens without the endpoint lock.
func (e *Endpoint) admitDeferred(rec *requests.Record) error {
	key := keyOfRecord(rec)
	// busy=queue and deliver=steer are disabled in v1. The gate lives in
	// submit() for fresh commands; admitDeferred is the other entrance to
	// native Submit, so a record stored by an older build with deliver=steer
	// would be admitted here after an upgrade without this check. Same gate,
	// same code, same reason.
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
	t, code, reason := e.admissibleLocked(rec.TargetID, rec.Epoch, rec.NotAfter, minEvidence)
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
		e.transitionLocked(rec, causeRefused, nativeEvidence{code: code, reason: reason})
		_, err = e.commitLocked(rec, nil)
		e.mu.Unlock()
		return err
	}
	// Move to dispatching under the lock, then Submit unlocked. The
	// per-runtime reservation applies here too: a deferred record whose
	// sibling is still in flight stays deferred (never dispatched) — Tick
	// retries it after the in-flight one resolves. Fail-closed on enumeration
	// error.
	reserved, rerr := e.runtimeInFlightLocked(rec.TargetID, key)
	if rerr != nil {
		e.mu.Unlock()
		return rerr
	}
	if reserved {
		e.mu.Unlock()
		return nil // still reserved; retry on the next tick
	}
	// Same fail-closed reservation as the live submit path —
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
	// A deferred or recovered request is checked again here, in the same
	// critical section as its dispatching commit: a session or key that
	// changed while it waited, or across a restart, stops it before the
	// harness sees it.
	if rec.Origin["carrier"] == CarrierLink && e.consent == nil {
		e.store.ReleaseReservation(key)
		e.mu.Unlock()
		return nil // the consent check is not installed yet; retry on the next tick
	}
	if code := e.handoffLocked(rec, t); code != "" {
		e.store.ReleaseReservation(key)
		e.transitionLocked(rec, causeRefused, nativeEvidence{code: code})
		_, err = e.commitLocked(rec, nil)
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
	origin, native := maps.Clone(rec.Origin), rec.NativeSession
	e.mu.Unlock()

	adm, nerr := t.att.Submit(BoundRequest{Key: key, Epoch: rec.Epoch, Input: input, NotAfter: rec.NotAfter, Origin: origin, NativeSession: native})

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
		nev.reason = adm.Message
		return causeRefused, nev
	}
}

// finishAdmissionLocked applies one native Submit outcome to the durable
// record. Shared by the fresh-submit and identical-retry admission paths and
// by admitDeferred, so all three never disagree about post-admission
// semantics. The caller holds e.mu and owns unlocking: finishAdmissionLocked
// unlocks exactly once on every return.
//
// Cancel during admission: a cancel that raced the gated admission
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
		// If the record carries outstanding cancel intent (a concurrent
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
		return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: rec.Code, Evidence: ev, Message: rec.RefusalReason}}, nil
	}
	// The record was moved by a native event while Submit was in flight
	// (the raced shape). RECONCILE from (adm, nerr, rec.State) — never branch
	// on assumptions about what happened.
	// The abort condition is OUTSTANDING CANCEL INTENT, not just State ==
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
		return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: rec.Code, Evidence: ev, Message: rec.RefusalReason}}, nil
	}
	if rec.State == protocol.StateCancelled && !adm.Admitted && nerr == nil {
		// Cancellation raced admission and never got metadata, and the
		// native outcome positively establishes admission never happened.
		e.transitionLocked(rec, causeCancelledBeforeAdmission, nativeEvidence{})
		if _, err := e.commitLocked(rec, t); err != nil {
			e.notifyStorageFailureLocked(rec, err)
			e.mu.Unlock()
			return protocol.Reply{}, err
		}
	}
	// Reconcile moved the record to uncertain while Submit was in flight
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
		return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: rec.Code, Evidence: ev, Message: rec.RefusalReason}}, nil
	}
	// Default: return the durable snapshot. Outcome.Code is read from rec.Code
	// so snapshot.Code == outcome.Code always.
	snap := rec.Snapshot
	out := protocol.Outcome{Op: protocol.OpRequestSubmit, Code: rec.Code, Message: rec.RefusalReason}
	if rec.Cancel != nil {
		out.Disposition = rec.Cancel.Disposition
	}
	// A reconcile tick that saw the admitted run already moved the record to
	// running; the admission this Submit achieved still carries its evidence.
	admittedRunning := adm.Admitted && nerr == nil && rec.State == protocol.StateRunning
	e.mu.Unlock()
	if admittedRunning {
		out.Evidence = achievedEvidence(t)
	}
	e.publishRevision(rec)
	return protocol.Reply{Snapshot: snap, Outcome: out}, nil
}

// abortAdmittedRacedRun handles a raced admission: Submit admitted a run, but
// a native cancel moved the record to cancelled before Submit returned. The
// run is live and unwanted — abort it via CancelExact. The abort's OUTCOME
// matters: confirmed -> cancelled_by_request; inconclusive ->
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
		// Delivered — record it durably (digest verified non-empty above).
		// MarkAcknowledged failure ignored (MarkPublished
		// shape): the next replay re-Lookups and re-Marks. Self-healing.
		_ = e.store.MarkAcknowledged(ackKey)
	}
	e.publishRevision(rec)
	return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: rec.Code, Disposition: disposition}}, nil
}
