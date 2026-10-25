package core

import (
	"errors"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// maxResolveAttempts bounds how often reconcileLive asks an
// InteractionResolver again when the record's pending interaction changed
// during the unlocked call.
const maxResolveAttempts = 3

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
// acknowledged. A tombstone owes nothing: compaction erased its result on
// purpose.
func owesResult(rec *requests.Record) bool {
	return rec.OwesResult()
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
	return rec.OwesCancel()
}

// owesAck reports whether the runtime is holding a result for us that we have
// not released. This is the SECOND obligation — it is about a RESULT we have
// not acknowledged, nothing else. Result != nil is what makes the digest
// non-empty (EvidenceDigest(nil) == ""), so a terminal record with no result
// owes nothing and is never re-driven — that closes the revision-churn bug
// BY CONSTRUCTION.
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

// Reconcile runs after Open and on every Tick. It reads only the records
// the store's sweep index selects (requests.Record.NeedsSweep, the union of
// the arms below), so its cost follows open and owed work, not retention.
// It re-examines every non-terminal record against exact native evidence,
// never re-submits, expires deferred requests whose window closed, and
// republishes unpublished revisions. It also replays native
// acknowledgements for terminal records: a crash between the terminal
// commit and the native ack leaves the attachment holding its one
// unacked-result slot, which would refuse every later submit with busy;
// replaying the durable ack digest releases it.
// Native attachment calls happen without the endpoint lock held; a single
// poisoned record is reported, never allowed to abort the whole pass.
func (e *Endpoint) Reconcile() error {
	// Gate Reconcile/Tick on lifecycle state and register the
	// sweep as in-flight so Close's drain wait bounds it. A Tick that arrives
	// once draining has begun is skipped (no new native work during shutdown).
	if !e.beginReconcile() {
		return nil
	}
	defer e.releaseInFlight()
	e.mu.Lock()
	recs := e.store.ListSweep()
	e.mu.Unlock()
	var firstErr error
	for _, rec := range recs {
		e.mu.Lock()
		seen := e.failed[keyOfRecord(rec)].seq
		e.mu.Unlock()
		var rerr error
		switch rec.State {
		case protocol.StateDispatching, protocol.StateRunning, protocol.StateUncertain:
			rerr = e.reconcileLive(rec)
		case protocol.StateReceived:
			if rerr = e.admitDeferred(rec); rerr == nil {
				rerr = e.replayTerminalAck(rec)
			}
		default:
			if len(rec.OwedOutcomes) > 0 || len(rec.RetiredOutcomes) > 0 {
				// Retired outcomes stay correctable until compaction, so
				// recovery keeps asking the resolver for them too.
				if rerr = e.recoverOwedOutcomes(rec); rerr != nil {
					break
				}
			}
			// The retry selector is owesCancel (a cancel we have not
			// confirmed). If we don't owe a cancel, replay the ack.
			// replayTerminalAck is idempotent (returns nil if the attachment
			// already released the result). A terminal record with no result
			// has an empty digest and replayTerminalAck returns immediately,
			// so there is no churn.
			switch {
			case owesCancel(rec):
				rerr = e.reconcileCancelRetry(rec)
			case owesResult(rec):
				// A closed record with a bound run and no result: a late result
				// whose write failed. Ask the attachment.
				rerr = e.recoverTerminalResult(rec)
			default:
				rerr = e.replayTerminalAck(rec)
			}
		}
		if rerr == nil {
			// The pass retried the write a storage failure lost and nothing
			// failed, so the durable record is the truth again.
			e.mu.Lock()
			if f, ok := e.failed[keyOfRecord(rec)]; ok && f.seq == seen {
				delete(e.failed, keyOfRecord(rec))
			}
			e.mu.Unlock()
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
	// Bounded compaction. Runs once per minute (shouldCompact rate-
	// limit), reaps terminal+settled+old records into tombstones. e.mu is
	// held per-record (CompactOne), never across the sweep.
	// The candidates are the store's terminal records that are not
	// tombstones yet, not the sweep's records: a settled record is
	// exactly what compaction wants and the sweep skips, and a full list
	// would grow with the tombstones nothing deletes. CompactOne re-reads
	// each candidate under the store lock and re-gates (terminal +
	// !tombstone + old + !OwesAck + published), so a stale key is harmless.
	if e.shouldCompact() {
		cutoff := e.now().Add(-e.compactHorizon)
		compacted := 0
		for _, key := range e.store.CompactionCandidates() {
			// Count only successful compactions toward the limit, not every
			// record the loop looks at: records not old enough yet
			// would otherwise use it up).
			if compacted >= 100 {
				break
			}
			if ready, ferr := e.forgetBeforeCompact(key, cutoff); !ready {
				if ferr != nil && firstErr == nil {
					firstErr = ferr
				}
				continue
			}
			e.mu.Lock()
			ok, cerr := e.store.CompactOne(key, cutoff)
			e.mu.Unlock()
			if cerr != nil {
				var rf *protocol.Refusal
				if errors.As(cerr, &rf) && rf.Code == protocol.CodeStorageFull {
					// A per-record storage_full must NOT abort the sweep.
					// CompactOne is a settlement write
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

// forgetBeforeCompact lets a Forgetter attachment drop its own state for a
// record that passes the compaction gate now, and reports whether the record
// may be compacted. A passing record is never looked up again (it does not
// need the sweep and owes no result), so Forget is final. Forget runs
// outside e.mu, as every attachment call does, and only for a record the
// attachment saw (NativeDispatches > 0) whose result, if any, it has
// acknowledged: an unacknowledged result keeps the record until the
// acknowledgement replay succeeds. A failed Forget leaves the record a
// compaction candidate, so the next sweep calls Forget again. A record of an
// attachment without Forget compacts as before.
func (e *Endpoint) forgetBeforeCompact(key requests.Key, cutoff time.Time) (bool, error) {
	e.mu.Lock()
	var forgetter Forgetter
	if t, ok := e.targets[key.TargetID]; ok {
		forgetter, _ = t.att.(Forgetter)
	}
	if forgetter == nil {
		e.mu.Unlock()
		return true, nil
	}
	rec, exists, err := e.store.Get(key)
	e.mu.Unlock()
	if err != nil || !exists || !rec.Compactable(cutoff) {
		return false, err
	}
	if rec.NativeDispatches == 0 {
		return true, nil
	}
	if rec.AckDigest != "" && !rec.Acknowledged {
		return false, nil
	}
	if err := forgetter.Forget(key); err != nil {
		return false, err
	}
	return true, nil
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
	// The ack was already DELIVERED (durable confirmation) — no
	// replay, no Lookup. This gate is what makes replay converge: without it
	// the restart case re-examines every terminal record every tick forever,
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
		// The delivered ack is now provable — record it durably
		// so this record never re-enters the replay path (convergence).
		_ = e.store.MarkAcknowledged(keyOfRecord(rec))
		return nil
	}
	// HISTORY-terminated evidence. A restarted attachment retains nothing in
	// memory by construction, so history
	// evidence IS proof the run terminated — this is the pre-upgrade
	// convergence path (Acknowledged=false on every shipped record).
	// The memoed AckDigest is the FULL digest of the live result, and codex
	// sets NativeRef on the live turn/completed result too, so the memo may
	// or may not carry a NativeRef while the history result always does.
	// Compare both sides in the NativeRef-stable form, recomputed from the
	// record's own stored result; the full digest is still what the
	// attachment is asked to release, because its own compare is full-shape.
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
		return e.ackNative(att, key, epoch, digest)
	}
	// Validate retained terminal evidence against the bound run + digest,
	// NOT against the client-facing state. A cancelled record keeps its
	// promise and never flips to completed, so "endpoint cancelled
	// + native completed" is a SUPPORTED shape. Gating on ev.State !=
	// rec.State would reject exactly that: digest matches, states differ, the
	// slot stays occupied, later submits are refused busy forever. The digest
	// already proves it is THIS result; the run id proves it is THIS run.
	if !ev.State.Terminal() || ev.Result == nil || protocol.EvidenceDigest(ev.Result) != digest {
		// The retained evidence is not the outcome this record acked (a stale
		// or foreign ack must never release a different request's result).
		return nil
	}
	// Identity, not content. Two runs can produce identical output
	// (same digest). Require the retained evidence's RunID to match the
	// record's bound NativeRun before acking — otherwise we release another
	// run's evidence. AcknowledgeResult's arguments carry no run id, so the
	// attachment cannot catch this either.
	if rec.NativeRun == nil || ev.RunID != *rec.NativeRun {
		return nil
	}
	// The native ack runs OUTSIDE e.mu like every other native call —
	// a wedged attachment ack on this recovery path must not hold the
	// endpoint mutex forever. The ack digest was durably memoed before it was
	// first sent, so a crash mid-ack is replayable.
	key := keyOfRecord(rec)
	epoch := rec.Epoch
	e.mu.Lock()
	att := t.att
	e.mu.Unlock()
	return e.ackNative(att, key, epoch, digest)
}

// ackNative sends one native acknowledgement outside e.mu and records it
// durably once it is delivered: always for a plain AcknowledgeResult, and
// for a CheckedAcknowledger only when it returns nil. A refused ack is
// returned and leaves the record owing it (AckDigest set, not
// Acknowledged), so replayTerminalAck sends the same digest again. A
// MarkAcknowledged failure is ignored (MarkPublished shape): the next replay
// re-Lookups, gets EvidenceNone, and re-Marks.
func (e *Endpoint) ackNative(att Attachment, key requests.Key, epoch, digest string) error {
	if checked, ok := att.(CheckedAcknowledger); ok {
		if err := checked.AcknowledgeResultChecked(key, epoch, digest); err != nil {
			return err
		}
	} else {
		att.AcknowledgeResult(key, epoch, digest)
	}
	// Crash window: the ack landed but the durable flag did not. The replay
	// path self-heals (Lookup -> EvidenceNone -> Mark).
	if e.crashAt(PointAfterAck) != nil {
		return nil
	}
	_ = e.store.MarkAcknowledged(key)
	return nil
}

// Tick retries deferred admissions and expiry; carriers call it on a timer
// and when a target comes back online.
func (e *Endpoint) Tick() error {
	e.tickThreadApprovals()
	return e.Reconcile()
}

// reconcileCancelRetry re-drives CancelExact for a record carrying outstanding
// cancel intent (cancel_requested + NativeRun bound). Works for both terminal
// (the abort was inconclusive) and non-terminal (a cancel raced admission and
// the record is still running) records. Bounded by NotAfter.
// This is the reconcile-cancel-retry mechanism, built once here.
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
	if err != nil {
		// The cancel outcome is not saved: report it so Reconcile keeps any
		// failure projection.
		e.mu.Unlock()
		return err
	}
	if !exists {
		e.mu.Unlock()
		return nil
	}
	runID := ""
	if rec.NativeRun != nil {
		runID = *rec.NativeRun
	}
	if _, cerr := e.applyCancelOutcomeLocked(rec, t, key, epoch, runID, ev, nerr); cerr != nil {
		e.notifyStorageFailureLocked(rec, cerr)
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
	var resolver InteractionResolver
	if ok {
		resolver, _ = t.att.(InteractionResolver)
	}

	// No defer: every early return below unlocks explicitly, and the
	// native ack (the slow, untrusted call) runs after the final unlock. A
	// wedged attachment must never hold the endpoint mutex: the whole
	// endpoint (every Handle/Reconcile/cancel) deadlocks behind it.
	//
	// A resolver is asked about the pending interaction of the FRESH record,
	// outside the lock, and the record is read again after the call: when
	// its pending interaction changed meanwhile, the question is asked
	// again, a bounded number of times, and otherwise left to the next
	// pass. The outcome therefore always belongs to the interaction this
	// pass applies it to.
	var outcome *protocol.Resolution
	queried := ""
	for attempt := 0; ; attempt++ {
		e.mu.Lock()
		fresh, exists, err := e.store.Get(key)
		if err != nil {
			e.mu.Unlock()
			return err
		}
		if !exists || fresh.State.Terminal() {
			e.mu.Unlock()
			return nil
		}
		rec = fresh
		if resolver == nil || rec.Interaction == nil || rec.Interaction.InteractionID == queried {
			break // e.mu held
		}
		if attempt == maxResolveAttempts {
			e.mu.Unlock()
			return nil // the pending interaction keeps moving: next pass
		}
		id, epoch := rec.Interaction.InteractionID, rec.Epoch
		queried = id
		e.mu.Unlock()
		outcome = nil
		if res, done := resolver.ResolvedInteraction(key, epoch, id); done {
			outcome = &res
		}
	}
	if ok && lookupErr == nil && e.reconcileInteractionLocked(rec, ev, outcome) {
		if _, err := e.commitLocked(rec, t); err != nil {
			e.notifyStorageFailureLocked(rec, err)
			e.mu.Unlock()
			return err
		}
	}
	switch {
	case !ok || lookupErr != nil:
		if rec.State == protocol.StateUncertain {
			e.mu.Unlock()
			return nil
		}
		e.transitionLocked(rec, causeAttachmentLost, nativeEvidence{})
	case ev.RefusalCode != "" && ev.Admitted && ev.State == protocol.StateRejected:
		// A DEFINITIVE native refusal over
		// PROVEN admission (the receipt stays across a fire-time refusal).
		// Map the typed refusal code onto the durable record — rejected+code,
		// action-required — never a silent dispatch and never evidence of
		// non-admission.
		e.transitionLocked(rec, causeRefused, nativeEvidence{runID: ev.RunID, code: ev.RefusalCode})
	case ev.RefusalCode != "" && ev.State == protocol.StateRejected:
		// A DEFINITIVE pre-delivery refusal (no receipt: nothing ran), e.g.
		// pi's refused(generation) after a session restart. Keep the typed
		// code (stale_epoch: re-inspect and resubmit) instead of the generic
		// native_error the default refusal would record.
		e.transitionLocked(rec, causeRefused, nativeEvidence{code: ev.RefusalCode})
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
		// matches neither owesCancel nor owesAck and is a noop. Only a record
		// that OWES A CANCEL may route to reconcileCancelRetry. Routing on
		// NativeRun != nil && AckDigest == "" would be wrong: that is true
		// for a simply-running record, so CancelExact would kill every prompt
		// within one tick.
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
	// The durable ack memo was written under the lock above
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
		_ = e.ackNative(ackAtt, ackKey, ackEpoch, ackDigest)
	}
	return nil
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
			// A transient Lookup failure must NOT confirm-and-commit with
			// empty evidence. Leave the disposition unconfirmed so
			// reconcileCancelRetry re-drives CancelExact on the next tick.
			e.mu.Lock()
			return "", lookupErr
		}
		e.mu.Lock()
		// Copy into the caller's *Record instead of rebinding the local param
		// — otherwise the caller reads a stale object.
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
	// were unlocked. Do NOT rewrite the promise unconditionally. If the
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
