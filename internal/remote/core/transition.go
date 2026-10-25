package core

import (
	"errors"
	"slices"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// onNative applies one native observation to the bound record.
func (e *Endpoint) onNative(targetID string, ev NativeEvent) {
	// No defer: every early return below unlocks explicitly, and the
	// native ack (the slow, untrusted call) runs after the final unlock.
	//
	// A thread question is not a request record's: it is shown outside
	// e.mu, ordered by threadPubMu.
	switch ev.Type {
	case EventThreadQuestion:
		e.onThreadQuestion(targetID, ev)
		return
	case EventThreadQuestionResolved:
		e.onThreadResolved(targetID, ev)
		return
	}
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
	// Guard on RunID, not on state. The ONLY event we drop is a
	// duplicate of a result we already recorded (same RunID, Result present).
	// Everything else flows — including a completion for a cancelled record.
	switch ev.Type {
	case EventRunCompleted, EventRunFailed, EventRunCancelled:
		// The duplicate guard must compare EVIDENCE, not just presence.
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
		if rec.State.Terminal() || (ev.Interaction != nil && resolvedIn(rec.Resolved, ev.Interaction.InteractionID)) {
			// A resolution recorded first (from Lookup evidence) is final:
			// a late question event never reopens it.
			e.mu.Unlock()
			return
		}
		// Route through transitionLocked.
		e.transitionLocked(rec, causeNone, nativeEvidence{interaction: ev.Interaction})
	case EventQuestionResolved:
		if ev.Interaction != nil && ev.Outcome != "" && (owes(rec, ev.Interaction.InteractionID) || slices.Contains(rec.RetiredOutcomes, ev.Interaction.InteractionID)) {
			// A late exact resolution settles an owed outcome — or corrects a
			// retention-retired one — terminal record or not;
			// the run's state does not change.
			settleOwedNative(rec, protocol.Resolution{InteractionID: ev.Interaction.InteractionID, Outcome: ev.Outcome, Option: ev.Option})
			break
		}
		if rec.State.Terminal() || (ev.Interaction != nil && (rec.Interaction == nil || rec.Interaction.InteractionID != ev.Interaction.InteractionID)) {
			// An event that names its interaction resolves only that one.
			e.mu.Unlock()
			return
		}
		// Route through transitionLocked. The resolution carries no
		// interaction, and causeNone treats a nil
		// interaction as "unchanged", so the clear is an explicit flag.
		e.transitionLocked(rec, causeNone, nativeEvidence{clearInteraction: true, resolvedRemotely: ev.Remote, outcome: ev.Outcome, option: ev.Option})
	case EventLocalIntervention:
		if rec.State.Terminal() {
			e.mu.Unlock()
			return
		}
		// Route through transitionLocked.
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
	// runs outside e.mu: publishLocked claims the publishing slot
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
	// Use the attachment captured under the lock; never re-read
	// e.targets after unlocking (a concurrent Register would race the map).
	if terminal && ackAtt != nil && ackDigest != "" && e.crashAt(PointBeforeAck) == nil {
		// A delivered ack is recorded durably so the restart replay
		// converges instead of re-Lookuping this record every tick (history
		// digests never match: NativeRef suffix asymmetry).
		_ = e.ackNative(ackAtt, ackKey, ackEpoch, ackDigest)
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
	fail.Interaction = nil
	e.failSeq++
	e.failed[keyOfRecord(rec)] = failure{snap: fail.Snapshot, seq: e.failSeq}
	e.notifyLocked(&fail)
}

// failure is a storage-failure projection and the order in which it was set.
type failure struct {
	snap protocol.Snapshot
	seq  int64
}

// failedLocked returns the storage-failure projection for the durable record
// cur. A durable terminal state is final, so it wins over the projection.
// The projection carries cur's revisions: a revision that was never stored
// must not reach a carrier that tracks revisions. The caller holds e.mu.
func (e *Endpoint) failedLocked(cur *requests.Record) (protocol.Snapshot, bool) {
	key := keyOfRecord(cur)
	f, ok := e.failed[key]
	if !ok {
		return protocol.Snapshot{}, false
	}
	if cur.State.Terminal() {
		delete(e.failed, key)
		return protocol.Snapshot{}, false
	}
	snap := f.snap
	snap.Revision = cur.Revision
	return snap, true
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
// directly. This makes derived-field hygiene impossible to forget: with scattered
// mutation sites, each site could miss a cleanup.
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
	runID            string           // NativeRun ("" = no run)
	result           *protocol.Result // terminal evidence
	cancel           *protocol.Cancel // cancel metadata from the command/event path
	code             protocol.Code    // explicit override for refused/attachment_lost
	reason           string           // adapter refusal text; causeRefused copies it onto the snapshot
	interaction      *protocol.Interaction
	clearInteraction bool // the pending interaction is resolved natively; clear it (nil interaction means unchanged)
	resolvedRemotely bool // with clearInteraction: the answer AMQ delivered resolved it
	// outcome and option, with clearInteraction, are the exact resolution
	// the harness reported. Set, they are recorded as given.
	outcome           protocol.ResolutionOutcome
	option            string
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
//     result arriving on an already-terminal record (onNative does not
//     discard it) updates Result WITHOUT changing State (the cancel stands, the
//     result is acknowledged).
//  5. rec.NativeRun: set from ev.runID whenever evidence carries one. Cleared
//     only on a fresh causeDispatching with no run, or causeBusyTombstone.
//  6. memoAckIntentLocked is called by the CALLER (not here) when a terminal
//     result is retained — transitionLocked only sets the fields.
func (e *Endpoint) transitionLocked(rec *requests.Record, c cause, ev nativeEvidence) {
	var open *protocol.Interaction
	if rec.Interaction != nil {
		cp := *rec.Interaction
		open = &cp
	}
	e.applyTransitionLocked(rec, c, ev)
	// Only a clear resolves an interaction. A replacement by another
	// interaction says nothing about the first, which may still be open.
	if open != nil && rec.Interaction == nil {
		if t := e.targets[rec.TargetID]; t != nil && !ev.clearInteraction {
			// An adapter that records how each interaction ended reports
			// the end itself; the run's end never stands in for it. The
			// outcome is owed until the adapter or a late exact
			// resolution supplies it.
			if _, isResolver := t.att.(InteractionResolver); isResolver {
				oweOutcome(rec, open.InteractionID)
				return
			}
		}
		recordResolution(rec, open.InteractionID, ev)
	}
}

func (e *Endpoint) applyTransitionLocked(rec *requests.Record, c cause, ev nativeEvidence) {
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
		rec.RefusalReason = ""
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
		rec.RefusalReason = ""
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
		rec.RefusalReason = ""
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
		rec.RefusalReason = ""
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
		rec.RefusalReason = ""
		if ev.runID != "" {
			run := ev.runID
			rec.NativeRun = &run
		}
		// Retain partial output at cancellation. A cancelled run may
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
		rec.RefusalReason = ""
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
		// A confirmed disposition is a promise already kept and is not
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
		rec.RefusalReason = ""
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
		rec.RefusalReason = protocol.BoundReason(ev.reason)
		rec.Interaction = nil
	case causeBusyTombstone:
		rec.State = protocol.StateRejected
		rec.Code = protocol.CodeBusy
		rec.RefusalReason = ""
		rec.Tombstone = true
		rec.NativeRun = nil
		rec.Interaction = nil
	}
	// Cancel rule: Cancel is written iff ev.cancel != nil or the
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

// commitLocked is the persist step that pairs with transitionLocked. It
// does Revision++, ObservedAt, store.Update, notify, and memo AckDigest — so
// no caller can forget notify or the ack memo. Returns the ackDigest + the
// storage error (if any). The caller holds e.mu and owns unlocking + publish.
// applyTransition+commitLocked is the ONLY persist path.
func (e *Endpoint) commitLocked(rec *requests.Record, t *target) (string, error) {
	rec.Revision++
	rec.ObservedAt = protocol.FormatTime(e.now())
	if err := e.store.Update(rec); err != nil {
		return "", err
	}
	delete(e.failed, keyOfRecord(rec))
	e.notifyLocked(rec)
	ackDigest := ""
	if rec.State.Terminal() && t != nil {
		ackDigest, _ = e.memoAckIntentLocked(rec, t)
	}
	return ackDigest, nil
}

func keyOfRecord(rec *requests.Record) requests.Key {
	return requests.Key{CreatorHost: rec.CreatorHost, TargetID: rec.TargetID, RequestID: rec.RequestID}
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
