package core

import (
	"errors"
	"maps"
	"slices"
	"sync"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// answerKey names one interaction of one request.
type answerKey struct {
	key           requests.Key
	interactionID string
}

// answerLock is one interaction's answer lock and its holders and waiters.
type answerLock struct {
	mu    sync.Mutex
	users int
}

func (e *Endpoint) respond(cmd *protocol.Command, src Source) (protocol.Reply, error) {
	host, targetID, requestID, err := protocol.DecodeRef(cmd.RequestRef)
	if err != nil {
		return protocol.Reply{}, err
	}
	if isLink(src) {
		// No link ever answers a tool prompt, and it is refused before any
		// lookup, so the answer path tells it nothing about other requests.
		return protocol.Reply{}, protocol.Refuse(protocol.CodeUnshared, "a link never answers an interaction")
	}
	key := requests.Key{CreatorHost: host, TargetID: targetID, RequestID: requestID}
	defer e.lockAnswer(key, cmd.InteractionID)()
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
	// A local request may reach the reads below, which change nothing, so
	// an answer that lost the race hears already_resolved; whether it may
	// answer is decided under the lock, before the intent is recorded.
	owner := fromOwnerShare(src, rec)
	if !owner && !localSubmitted(src, rec) {
		return protocol.Reply{}, protocol.Refuse(protocol.CodeUnshared, "only the owner's Buzz share that submitted this request can answer its interactions")
	}
	if t == nil {
		return protocol.Reply{}, protocol.Refuse(protocol.CodeAttachmentLost, "target is not attached")
	}
	if _, done := rec.Answered[cmd.InteractionID]; done {
		// A recorded intent is not a delivered answer. If the runtime still
		// holds the question, the earlier native call failed before the
		// answer landed and this is the replay that must re-send it; only a
		// settled answer short-circuits.
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
	// A recorded answer that the runtime refused never applied: another
	// answer may replace it. Asked outside the lock.
	retired := false
	if prior, done := rec.Answered[cmd.InteractionID]; done && prior != cmd.Option {
		if r, ok := t.att.(AnswerRetirer); ok {
			retired = r.AnswerRetired(key, cmd.InteractionID, prior)
		}
	}
	// Answer-INTENT contract: the pending interaction is revalidated under
	// the lock that owns the persist (changed since the
	// first read), the offered option is validated against the pending
	// interaction BEFORE anything durable happens, and only an answer-INTENT
	// — not a completed answer — is persisted. A positive native refusal
	// clears the intent, so the refused answer consumes nothing and the
	// retried valid answer reaches the attachment. After a crash, a replay
	// with the same option sends it to the attachment again unless the
	// answer is already settled, so the attachment must recognize an answer
	// it already published. While the intent stands, a different option is
	// refused (unless the attachment says the earlier answer was retired).
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
	if !owner && (e.targets[targetID] != t || !localAnswer(src, rec, t, cmd)) {
		// A local answer is rechecked on this read: the attachment or the
		// interaction may have been replaced since the first.
		e.mu.Unlock()
		return protocol.Reply{}, protocol.Refuse(protocol.CodeUnshared, "only the owner's Buzz share that submitted this request can answer its interactions")
	}
	// An exact replay of the durable answer intent goes to the attachment,
	// which recognizes an answer it already published, even when the
	// interaction no longer offers that option (it expired or its session
	// went away). Only a fresh answer must be one the interaction offers now.
	prior, done := rec.Answered[cmd.InteractionID]
	replay := done && prior == cmd.Option
	offered := replay
	for _, o := range rec.Interaction.Options {
		if o == cmd.Option {
			offered = true
		}
	}
	if !offered {
		e.mu.Unlock()
		return protocol.Reply{}, protocol.Refuse(protocol.CodeInvalid, "option %q is not offered by interaction %s", cmd.Option, cmd.InteractionID)
	}
	if done && prior != cmd.Option && !retired {
		// An earlier answer may already be with the runtime (in flight, or
		// sent before a transport error). The runtime applies the first
		// answer it gets, so a different one never replaces that intent.
		e.mu.Unlock()
		return protocol.Reply{}, protocol.Refuse(protocol.CodeAlreadyResolved, "an earlier answer (%s) is still being delivered; send it again or answer in the terminal", prior)
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

	var code protocol.Code
	var rerr error
	var reason string
	if er, ok := t.att.(EvidenceResponder); ok && len(cmd.Evidence) > 0 {
		code, rerr = er.RespondWithEvidence(key, cmd.Epoch, cmd.InteractionID, cmd.Option, cmd.Evidence)
		// A refusal is a positive refusal that names its reason.
		var refusal *protocol.Refusal
		if errors.As(rerr, &refusal) {
			code, reason, rerr = refusal.Code, refusal.Message, nil
		}
	} else if or, ok := t.att.(OriginResponder); ok {
		code, rerr = or.RespondFrom(key, cmd.Epoch, cmd.InteractionID, cmd.Option, maps.Clone(src.Origin))
	} else {
		code, rerr = t.att.Respond(key, cmd.Epoch, cmd.InteractionID, cmd.Option)
	}
	if rerr != nil {
		// The native call itself errored without a disposition: the intent
		// stays (the answer may have landed), so the replay path — not a
		// fresh answer — decides. Surface the failure.
		return protocol.Reply{}, rerr
	}
	if reason == "" {
		reason = bareReason(code)
	}
	if code != "" && replay {
		// A replay never clears the intent it found: the earlier answer may
		// have landed, as a second ✅ after an allow. Already resolved
		// then is that answer's end.
		if code != protocol.CodeAlreadyResolved {
			return protocol.Reply{}, protocol.Refuse(code, "%s", reason)
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		rec, _, err = e.store.Get(key)
		if err != nil {
			return protocol.Reply{}, err
		}
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpInteractionRespond, Code: protocol.CodeAlreadyResolved}}, nil
	}
	if code != "" {
		// A positive native refusal: clear the durable intent so the refused
		// answer consumes nothing and a later VALID answer can still be
		// delivered. Persist the clearing before surfacing the refusal.
		e.mu.Lock()
		if cur, ok, gerr := e.store.Get(key); gerr == nil && ok {
			if opt, done := cur.Answered[cmd.InteractionID]; !done || opt == cmd.Option {
				delete(cur.Answered, cmd.InteractionID)
				// Route through commitLocked — single persist path.
				if _, uerr := e.commitLocked(cur, nil); uerr != nil {
					e.mu.Unlock()
					return protocol.Reply{}, uerr
				}
				rec = cur
			}
		}
		e.mu.Unlock()
		return protocol.Reply{}, protocol.Refuse(code, "%s", reason)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, _, err = e.store.Get(key)
	if err != nil {
		return protocol.Reply{}, err
	}
	return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpInteractionRespond}}, nil
}

// bareReason says in words why the runtime refused an answer that came back
// with a code alone. The owner read "Not sent: interaction cc-…" for a late
// ✅.
func bareReason(code protocol.Code) string {
	switch code {
	case protocol.CodeExpired:
		return "the approval expired"
	case protocol.CodeInvalid:
		return "the approval does not offer that answer"
	case protocol.CodeAlreadyResolved:
		return "the approval was already answered"
	}
	return "the session refused the answer"
}

// lockAnswer takes the answer lock of one interaction and returns its
// release. Another answer to the same interaction waits for this one to
// finish, its native call and rollback included; e.mu is held only to
// find the lock.
func (e *Endpoint) lockAnswer(key requests.Key, interactionID string) func() {
	k := answerKey{key: key, interactionID: interactionID}
	e.mu.Lock()
	l := e.answering[k]
	if l == nil {
		l = &answerLock{}
		e.answering[k] = l
	}
	l.users++
	e.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		e.mu.Lock()
		if l.users--; l.users == 0 {
			delete(e.answering, k)
		}
		e.mu.Unlock()
	}
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

// oweOutcome records that interaction id's outcome is owed, once. It is
// never trimmed: dropping an id would break the obligation. A record owes at
// most one id in practice, since its pending interaction clears once, at the
// terminal transition.
func oweOutcome(rec *requests.Record, id string) {
	if owes(rec, id) || resolvedIn(rec.Resolved, id) {
		return
	}
	rec.OwedOutcomes = append(rec.OwedOutcomes, id)
}

// owes reports whether interaction id's outcome is owed.
func owes(rec *requests.Record, id string) bool {
	return slices.Contains(rec.OwedOutcomes, id)
}

// owedOutcomeRetention is how long an owed outcome stays recoverable. The
// normal case settles on the next Reconcile, because the bridge event file
// still holds interaction_resolved. The unsettleable case — a bridge that
// never wrote it, or a removed or rotated seam — would otherwise block
// compaction forever. The horizon is the request-retention horizon: once a
// terminal record is old enough to
// compact anyway, an outcome no resolver has produced is recorded as an
// explicit uncertain resolution with a visible reason, never dropped
// silently. A resolvable owed outcome still settles normally on Reconcile.
const owedOutcomeRetention = protocol.DefaultCompactHorizon

// retireOutcomeLocked replaces an owed outcome's obligation with an explicit
// uncertain resolution. It runs inside the locked update that
// retires: the obligation moves to RetiredOutcomes (so recovery keeps asking
// the resolver and a late exact resolution can still correct this), and the
// record shows delivery_unknown with a visible provenance instead of pinning
// compaction forever. It is idempotent and refuses an id that a native
// resolution already settled between the unlocked lookup and this lock.
func retireOutcomeLocked(rec *requests.Record, id string) bool {
	if resolvedIn(rec.Resolved, id) {
		return false
	}
	rec.OwedOutcomes = slices.DeleteFunc(rec.OwedOutcomes, func(owed string) bool { return owed == id })
	if !slices.Contains(rec.RetiredOutcomes, id) {
		rec.RetiredOutcomes = append(rec.RetiredOutcomes, id)
	}
	recordRetainedResolution(rec, id)
	return true
}

// recordRetainedResolution records the retention outcome for a retired id.
// Unlike recordResolution it REPLACES an earlier retention resolution for
// the same id in place (it is provisional, not final); a native resolution
// never routes through here.
func recordRetainedResolution(rec *requests.Record, id string) {
	r := protocol.Resolution{InteractionID: id, Outcome: protocol.ResolutionDeliveryUnknown}
	for i, cur := range rec.Resolved {
		if cur.InteractionID == id && cur.Outcome == protocol.ResolutionDeliveryUnknown {
			rec.Resolved[i] = r
			return
		}
	}
	rec.Resolved = append(rec.Resolved, r)
	if n := len(rec.Resolved); n > protocol.MaxResolutions {
		rec.Resolved = append([]protocol.Resolution(nil), rec.Resolved[n-protocol.MaxResolutions:]...)
	}
}

// settleOwedNative records an exact native resolution for an owed OR retired
// outcome: a retired outcome stays correctable until the record compacts, so
// a real answered/elsewhere outcome arriving after the
// horizon replaces the uncertain one. Returns false when the id was never
// owed or retired, or a native resolution already recorded it.
func settleOwedNative(rec *requests.Record, r protocol.Resolution) bool {
	if !owes(rec, r.InteractionID) && !slices.Contains(rec.RetiredOutcomes, r.InteractionID) {
		return false
	}
	for _, cur := range rec.Resolved {
		if cur.InteractionID == r.InteractionID && cur.Outcome != protocol.ResolutionDeliveryUnknown {
			// A native resolution already recorded for this id (e.g. a
			// duplicate event): nothing to correct.
			return false
		}
	}
	// Drop the provisional retention resolution, then record the exact one.
	rec.Resolved = slices.DeleteFunc(rec.Resolved, func(cur protocol.Resolution) bool {
		return cur.InteractionID == r.InteractionID && cur.Outcome == protocol.ResolutionDeliveryUnknown
	})
	rec.OwedOutcomes = slices.DeleteFunc(rec.OwedOutcomes, func(id string) bool { return id == r.InteractionID })
	rec.RetiredOutcomes = slices.DeleteFunc(rec.RetiredOutcomes, func(id string) bool { return id == r.InteractionID })
	recordResolution(rec, r.InteractionID, nativeEvidence{clearInteraction: true, outcome: r.Outcome, option: r.Option})
	return true
}

// recoverOwedOutcomes settles every outcome the record's resolver can
// produce and retires every one the retention horizon has outrun. It never
// changes the record's state.
//
// The resolver is OPTIONAL: a target that is unregistered or
// whose attachment implements no InteractionResolver can never settle an
// owed outcome, so the horizon still retires it — the exact case the horizon
// exists for is a record that survives a restart after its target
// disappeared. With a resolver, each owed id is asked outside the lock; the
// final ask for an id about to be retired runs again INSIDE the locked
// update, so an outcome the resolver gains between the two cannot be
// retired behind its own resolution.
//
// A retired outcome stays correctable: recovery keeps asking
// the resolver for retired ids until the record compacts, and a late exact
// native resolution replaces the uncertain one. Compaction does not wait
// on retired ids.
func (e *Endpoint) recoverOwedOutcomes(rec *requests.Record) error {
	key := keyOfRecord(rec)
	e.mu.Lock()
	t, ok := e.targets[rec.TargetID]
	e.mu.Unlock()
	var resolver InteractionResolver
	if ok {
		resolver, _ = t.att.(InteractionResolver)
	}

	// One unlocked ask per id (owed and retired: retired ids stay
	// correctable), then a locked pass that settles what the asks found and
	// retires what the horizon has outrun, re-checking the resolver inside
	// the lock for each id it is about to retire.
	resolutions := make(map[string]protocol.Resolution, len(rec.OwedOutcomes)+len(rec.RetiredOutcomes))
	if resolver != nil {
		for _, id := range append(slices.Clone(rec.OwedOutcomes), rec.RetiredOutcomes...) {
			if res, done := resolver.ResolvedInteraction(key, rec.Epoch, id); done && res.InteractionID == id {
				resolutions[id] = res
			}
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	cur, exists, err := e.store.Get(key)
	if err != nil || !exists {
		return err
	}
	observed, perr := protocol.ParseTime(rec.ObservedAt)
	pastHorizon := perr == nil && e.now().Sub(observed) >= owedOutcomeRetention
	changed := false
	for _, r := range resolutions {
		// settleOwedNative re-checks against the fresh record: a concurrent
		// write may have settled the obligation already.
		if settleOwedNative(cur, r) {
			changed = true
		}
	}
	for _, id := range append(slices.Clone(cur.OwedOutcomes), cur.RetiredOutcomes...) {
		if !pastHorizon || resolvedIn(cur.Resolved, id) {
			continue
		}
		// Final ask inside the locked update, against the CURRENT target:
		// a live reattach may have replaced the attachment since the
		// unlocked lookup, and the replacement may already resolve this
		// key. Never retire an outcome a resolver can settle at this moment.
		curResolver := resolver
		if t, ok := e.targets[rec.TargetID]; ok {
			if r, isResolver := t.att.(InteractionResolver); isResolver {
				curResolver = r
			}
		}
		if curResolver != nil {
			// Final ask inside the locked update: the resolver may have
			// gained the outcome between the unlocked lookup and now.
			if res, done := curResolver.ResolvedInteraction(key, rec.Epoch, id); done && res.InteractionID == id {
				if settleOwedNative(cur, res) {
					changed = true
				}
				continue
			}
		}
		if owes(cur, id) && retireOutcomeLocked(cur, id) {
			changed = true
		}
		// An already-retired id with no resolution has nothing to change.
	}
	if !changed {
		return nil
	}
	// No target: the terminal result's ack intent is not this write's
	// business; the ack replay below handles it as before.
	if _, err := e.commitLocked(cur, nil); err != nil {
		e.notifyStorageFailureLocked(cur, err)
		return err
	}
	return nil
}

// resolvedIn reports whether id has a recorded resolution.
func resolvedIn(resolved []protocol.Resolution, id string) bool {
	for _, r := range resolved {
		if r.InteractionID == id {
			return true
		}
	}
	return false
}

// reconcileInteractionLocked applies an attachment's interaction evidence to
// a live record, idempotently: the recorded outcome of the record's pending
// interaction (from InteractionResolver) closes it with that exact outcome,
// and the attachment's
// pending interaction fills a record that shows none, or refreshes the same
// interaction shown differently, unless it is already resolved. A different
// pending interaction never replaces the record's through this path; the
// native events own that order. It runs before every state change in reconcileLive, so a lost
// native event is recovered from Lookup and a terminal result never closes
// an interaction ahead of the resolution the harness reported. It reports
// whether the record changed.
func (e *Endpoint) reconcileInteractionLocked(rec *requests.Record, ev Evidence, outcome *protocol.Resolution) bool {
	changed := false
	if r := outcome; r != nil && rec.Interaction != nil && rec.Interaction.InteractionID == r.InteractionID {
		e.transitionLocked(rec, causeNone, nativeEvidence{
			clearInteraction: true, resolvedRemotely: r.Outcome == protocol.ResolutionAnswered, outcome: r.Outcome, option: r.Option,
		})
		changed = true
	}
	if in := ev.Interaction; in != nil && !resolvedIn(rec.Resolved, in.InteractionID) &&
		(rec.Interaction == nil || rec.Interaction.InteractionID == in.InteractionID && !sameInteraction(*rec.Interaction, *in)) {
		cp := *in
		e.transitionLocked(rec, causeNone, nativeEvidence{interaction: &cp})
		changed = true
	}
	return changed
}

// sameInteraction reports whether two views of an interaction are equal.
func sameInteraction(a, b protocol.Interaction) bool {
	return a.InteractionID == b.InteractionID && a.Kind == b.Kind && a.Prompt == b.Prompt &&
		slices.Equal(a.Options, b.Options) && a.RemoteAnswer == b.RemoteAnswer &&
		a.ApproveOption == b.ApproveOption && a.RejectOption == b.RejectOption
}

// recordResolution appends how the interaction id stopped being pending,
// keeping the most recent MaxResolutions entries. A clear that is neither
// an explicit native resolution nor a terminal run records nothing.
func recordResolution(rec *requests.Record, id string, ev nativeEvidence) {
	if resolvedIn(rec.Resolved, id) {
		return // recorded once; a second report of the same end changes nothing
	}
	var r protocol.Resolution
	switch {
	case ev.clearInteraction && ev.outcome != "":
		r = protocol.Resolution{InteractionID: id, Outcome: ev.outcome, Option: ev.option}
		if r.Outcome == protocol.ResolutionAnswered && r.Option == "" {
			r.Option = rec.Answered[id]
		}
	case ev.clearInteraction && ev.resolvedRemotely:
		r = protocol.Resolution{InteractionID: id, Outcome: protocol.ResolutionAnswered, Option: rec.Answered[id]}
	case ev.clearInteraction:
		r = protocol.Resolution{InteractionID: id, Outcome: protocol.ResolutionElsewhere}
	case rec.State.Terminal():
		r = protocol.Resolution{InteractionID: id, Outcome: protocol.ResolutionRunEnded}
		if opt, sent := rec.Answered[id]; sent {
			// The run ended while an answer was sent or in flight: the
			// resolution that answer caused can arrive after the run's end.
			r = protocol.Resolution{InteractionID: id, Outcome: protocol.ResolutionAnswered, Option: opt}
		}
	default:
		return
	}
	rec.Resolved = append(rec.Resolved, r)
	if n := len(rec.Resolved); n > protocol.MaxResolutions {
		rec.Resolved = append([]protocol.Resolution(nil), rec.Resolved[n-protocol.MaxResolutions:]...)
	}
}
