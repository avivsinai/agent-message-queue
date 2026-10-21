package core

import (
	"context"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Wait blocks until the record for ref reaches a terminal or uncertain state
// or ctx ends. It never cancels work. Callers map ctx errors to exit 4 or 130.
func (e *Endpoint) Wait(ctx context.Context, ref string) (protocol.Snapshot, error) {
	return e.WaitAfter(ctx, ref, nil)
}

// WaitAfter is Wait that also returns once the record's revision is past
// *after, so a follower sees each pending interaction. A nil after is Wait.
// Only the local socket waits (ipc), and it reads every record (readableBy).
func (e *Endpoint) WaitAfter(ctx context.Context, ref string, after *int64) (protocol.Snapshot, error) {
	host, targetID, requestID, err := protocol.DecodeRef(ref)
	if err != nil {
		return protocol.Snapshot{}, err
	}
	key := requests.Key{CreatorHost: host, TargetID: targetID, RequestID: requestID}
	for {
		e.mu.Lock()
		rec, ok, err := e.store.Get(key)
		var failed protocol.Snapshot
		var isFailed bool
		if err == nil && ok {
			failed, isFailed = e.failedLocked(rec)
		}
		ch := e.changed
		e.mu.Unlock()
		if err != nil {
			return protocol.Snapshot{}, err
		}
		if !ok {
			return protocol.Snapshot{}, noRecord()
		}
		if isFailed {
			return failed, nil
		}
		if rec.State.Terminal() || rec.State == protocol.StateUncertain || after != nil && rec.Revision > *after {
			return rec.Snapshot, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return rec.Snapshot, ctx.Err()
		}
	}
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
// branch (marker-only recovery must retire the obligation,
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
	// revision. Publishing from a stale record would deliver it twice.
	cur, ok, err := e.store.Get(key)
	if err != nil || !ok {
		return
	}
	if cur.PublishedRevision >= rec.Revision {
		rec.PublishedRevision = cur.PublishedRevision
		// A prior successful publication already discharged
		// this revision (or a strictly newer one). Retire any obligation for
		// this key that the durable high-water mark has covered, so a later
		// Close does not report ErrDrainIncomplete for a fully-published
		// record (the obligation was recorded when an earlier chained attempt
		// skipped, then Reconcile published it via a different caller).
		e.retireObligationLocked(key, cur.PublishedRevision)
		return
	}
	// Visible means CONFIRMED DELIVERY awaiting its marker. If a
	// confirmed delivery for this revision is already pending its marker,
	// skip the DELIVERY (no double delivery) but still fall through to
	// MarkPublished below — the marker may need retrying. An
	// IN-FLIGHT attempt is tracked separately in `publishing`; it does NOT
	// count as delivered.
	if e.visible[key] >= rec.Revision {
		e.retryMarkerLocked(key, rec)
		return
	}
	// Serialize per-key publication: if another caller is already
	// publishing this key, coalesce this caller's revision into the
	// pending obligation set (max-revision semantics, not a call count)
	// and return. This prevents two concurrent publishes for the same
	// key without holding the global endpoint mutex while guaranteeing the
	// skipped revision is still covered by
	// exactly one drain obligation: the finishing publisher — the single
	// per-key owner — adopts the pending set and
	// republishes before its drain obligation is released. Outside a
	// shutdown drain the next Tick/Reconcile would also retry it, but
	// registering the obligation uniformly keeps the invariant in every
	// state. Coalescing by highest requested revision avoids duplicate
	// same-revision deliveries.
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
	// Account for the accepted publication work in the bounded shutdown
	// drain. The publish runs OUTSIDE e.mu (the
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
	pub := e.publish // read under e.mu (SetPublish writes under it)
	e.mu.Unlock()

	// publish ONE revision outside e.mu, then loop while skipped newer
	// revisions exist. The drain obligation (inFlight) and the per-key
	// publishing ownership are held CONTINUOUSLY across the whole chain —
	// there is no unlock gap with zero drain count in which Close could
	// observe "nothing in flight" and close the store. Single iterative
	// owner, no recursive re-entry.
	for {
		pubErr := pub(snap, origin)

		e.mu.Lock()
		// Success bookkeeping for THIS attempt BEFORE selecting the next
		// obligation, so the coalescing check below sees the delivery
		// state. A failed attempt does NOT advance visible/PublishedRevision;
		// the durable unpublished state is
		// preserved for Reconcile recovery.
		if pubErr == nil && e.visible[key] < attemptRev {
			e.visible[key] = attemptRev
		}
		// Select the next obligation: the highest revision coalesced by
		// concurrent callers while we owned this key. A FAILED attempt does
		// not retry the same revision (no spin loop) but still adopts a
		// strictly NEWER accepted revision under the same owner: dropping the
		// r+1 obligation because r failed would leave r+1 uncovered by any
		// drain count and let Close return nil with it unpublished.
		// Reconcile/Tick owns retrying the failed revision
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
			// attempt against a closed store. An already running publisher is
			// never cancelled; only a NEW attempt is
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
			// incomplete drain instead of silently returning nil.
			// A failed attempt of attemptRev ITSELF is NOT an
			// undischarged obligation: the attempt ran and was counted; the
			// durable unpublished state persists for Reconcile recovery (the
			// contract-corpus restart flow relies on Close returning nil
			// there).
			if pendingRev > attemptRev {
				// Record the highest undischarged pending revision for this
				// key. A later successful publication of pendingRev (or a
				// strictly newer revision) retires this entry in the
				// MarkPublished success path below or in a later Reconcile,
				// so Close reports incomplete ONLY if it remains genuinely
				// unpublished (a global drainIncomplete latch could be set
				// while stateAccepting
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
					// pending revision too (a later successful publish must
					// clear a previously-recorded
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

// retryMarkerLocked is the marker retry path: rec's revision was already
// delivered (confirmed, awaiting its marker). It skips the DELIVERY (no
// double delivery) and retries only the marker, honoring the crash points so
// a lost marker stays lost within one simulated crash. The caller holds e.mu.
func (e *Endpoint) retryMarkerLocked(key requests.Key, rec *requests.Record) {
	if e.crashAt(PointAfterPublish) != nil || e.crashAt(PointBeforePublished) != nil {
		return
	}
	if err := e.store.MarkPublished(key, rec.Revision); err == nil {
		rec.PublishedRevision = rec.Revision
		if e.visible[key] == rec.Revision {
			delete(e.visible, key)
		}
		// Marker-only recovery (a prior delivery's marker
		// write failed, Reconcile retries only the marker) must also retire
		// the obligation. Without this, Close reports ErrDrainIncomplete
		// although the durable record is now fully published.
		e.retireObligationLocked(key, rec.Revision)
	}
}

func (e *Endpoint) publishRevision(rec *requests.Record) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.publishLocked(rec)
}
