package core

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// PromptReport is a session hook's report of one prompt: the target and
// attachment instance its locator named, what the hook's input held, the id
// of a granted stub when the prompt is one, and how long the hook still
// waits for an answer.
type PromptReport struct {
	TargetID, Instance string
	Prompt             Prompt
	StubID             string
	Wait               time.Duration
}

// untakenReason ends a request dispatched as a stub whose text no hook took
// by its deadline.
const untakenReason = "the session did not take the task before its deadline"

// unattributedReason ends a request dispatched as a stub whose answer the
// session's transcript does not tie to the text the endpoint gave.
const unattributedReason = "the answer cannot be attributed to this task; the task may have run"

// GrantedPrompt answers a session hook's report of one prompt and returns
// the text of the granted request whose stub raised that prompt, or "".
//
// It is the one place where the text of a request dispatched as a stub
// leaves the endpoint, under the endpoint lock, after every check passed and
// after the record durably holds grant_text_given, so a request gives its
// text at most once, also across a restart. The checks use the values of this
// call (its session and its mode), never a stored report. The authorization
// moment is the read of the grant state inside handoffLocked in this call: a
// revoke, a key change or an expiry that the machine applied before that read
// refuses the text; one applied after it does not recall the task.
//
// A report from a stale locator (another target, another kind of
// attachment, another instance) changes nothing. The hook waits for the
// answer until now + req.Wait, taken at entry: a report whose request is
// still being dispatched waits for the record's next change until then, and
// once that deadline is reached the call returns "" with nothing written, so
// no text is marked given after the hook stopped waiting.
func (e *Endpoint) GrantedPrompt(req PromptReport) string {
	deadline := e.now().Add(req.Wait)
	if !e.beginReconcile() {
		return ""
	}
	defer e.releaseInFlight()
	e.mu.Lock()
	t, ok := e.targets[req.TargetID]
	var h PromptHandover
	if ok {
		h, ok = t.att.(PromptHandover)
	}
	if !ok || req.Instance == "" || h.PromptInstance() != req.Instance {
		e.mu.Unlock()
		return ""
	}
	e.mu.Unlock()
	h.ObservePrompt(req.Prompt, e.now())
	if req.StubID == "" {
		return ""
	}
	for {
		e.mu.Lock()
		if e.targets[req.TargetID] != t {
			e.mu.Unlock()
			return ""
		}
		rec := e.stubRecordLocked(req.TargetID, h, req.StubID)
		if rec == nil || rec.GrantTextGiven || !e.now().Before(deadline) {
			e.mu.Unlock()
			return ""
		}
		if rec.State == protocol.StateDispatching {
			// The native Submit has not returned to the endpoint yet. Wait
			// for the next record change, as Wait does, with no lock held.
			ch := e.changed
			e.mu.Unlock()
			timer := time.NewTimer(deadline.Sub(e.now()))
			select {
			case <-ch:
				timer.Stop()
			case <-timer.C:
				return ""
			}
			continue
		}
		return e.handGrantedTextLocked(rec, t, h, req.Prompt, deadline)
	}
}

// stubRecordLocked returns the record of target targetID, dispatching or
// running, that was dispatched as a stub with id stubID, or nil. The durable
// record is the source, so a request is found from the moment its dispatch
// was recorded. The caller holds e.mu.
func (e *Endpoint) stubRecordLocked(targetID string, h PromptHandover, stubID string) *requests.Record {
	recs, _, err := e.store.ListReserving(func(k requests.Key) bool { return k.TargetID == targetID })
	if err != nil {
		return nil
	}
	for _, r := range recs {
		if r.TargetID != targetID || !r.GrantAtPrompt {
			continue
		}
		if r.State != protocol.StateDispatching && r.State != protocol.StateRunning {
			continue
		}
		if h.GrantedStubID(keyOfRecord(r)) == stubID {
			return r
		}
	}
	return nil
}

// handGrantedTextLocked runs the hand-over checks of a running record
// dispatched as a stub, in order, against the values of the prompt p. A
// cancel that was asked for ends the record cancelled; a refusal ends it
// rejected; a pass marks it durably and returns its text. A record with no
// stored text gets no mark. One clock read after the reads decides: at or
// after deadline nothing is written, and at or after the request's
// not_after no text is given. The caller holds e.mu; it unlocks exactly
// once.
func (e *Endpoint) handGrantedTextLocked(rec *requests.Record, t *target, h PromptHandover, p Prompt, deadline time.Time) string {
	var code protocol.Code
	reason := ""
	notAfter, err := protocol.ParseTime(rec.NotAfter)
	cancelled := rec.Cancel != nil
	switch {
	case cancelled:
	case rec.Input == nil:
		// No text to give: nothing is marked and nothing is refused.
		e.mu.Unlock()
		return ""
	case p.NativeSession != rec.NativeSession:
		code = protocol.CodeSessionChanged
	case err != nil || !e.now().Before(notAfter):
		code = protocol.CodeExpired
	case rec.Epoch != t.att.Inspect().Epoch:
		code = protocol.CodeStaleEpoch
	default:
		// handoffLocked reads several live snapshots (the consent key, the
		// grant, the attached session), each as the machine holds it at
		// this moment.
		code, reason = e.handoffLocked(rec, t)
		if code == "" {
			if msg := h.PromptRefusal(p); msg != "" {
				code, reason = protocol.CodeConsentInvalid, msg
			}
		}
	}
	// The decision point: the reads above can outlast the hook's wait, and
	// past the deadline nothing is written. A write that starts in time
	// may still finish after the hook disconnected.
	now := e.now()
	if !now.Before(deadline) {
		e.mu.Unlock()
		return ""
	}
	if code == "" && !cancelled && !now.Before(notAfter) {
		// The request's own deadline passed during the reads.
		code, reason = protocol.CodeExpired, ""
	}
	if cancelled {
		// The requester asked to cancel before the text was given: the
		// task does not start.
		e.transitionLocked(rec, causeCancelledByRequest, nativeEvidence{})
		if _, err := e.commitLocked(rec, t); err == nil {
			e.publishLocked(rec)
		}
		e.mu.Unlock()
		return ""
	}
	if code != "" {
		e.transitionLocked(rec, causeRefused, nativeEvidence{code: code, reason: reason})
		if _, err := e.commitLocked(rec, t); err != nil {
			// The record stays running; the deadline rule of Reconcile ends it.
			e.mu.Unlock()
			return ""
		}
		e.publishLocked(rec)
		e.mu.Unlock()
		return ""
	}
	rec.GrantTextGiven = true
	if _, err := e.commitLocked(rec, t); err != nil {
		rec.GrantTextGiven = false
		e.mu.Unlock()
		return ""
	}
	text := rec.Input.Text
	e.publishLocked(rec)
	e.mu.Unlock()
	return text
}

// markGrantAtPrompt marks a granted record about to be dispatched to t as
// dispatched as a stub when t's attachment gives granted text at a prompt.
// The mark is part of the write that makes the record dispatching.
func markGrantAtPrompt(rec *requests.Record, t *target) {
	if rec.GrantDigest == "" || t == nil {
		return
	}
	if _, ok := t.att.(PromptHandover); ok {
		rec.GrantAtPrompt = true
	}
}

// textDigest is the sha256 (hex) of text, the form of GrantedTextDigest.
func textDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// grantedAttributed reports whether the evidence digest ties a run's answer
// to rec, a record dispatched as a stub: the endpoint gave the text, the
// transcript showed exactly that text as the run's only prompt, and the
// machine has not already ended rec failed with its answer withheld.
func grantedAttributed(rec *requests.Record, digest string) bool {
	if !rec.GrantTextGiven || rec.Input == nil || digest == "" || digest != textDigest(rec.Input.Text) {
		return false
	}
	return rec.State != protocol.StateFailed || rec.RefusalReason == ""
}

// grantedResult is the result that a write may store on rec from ev. Every
// write of a result goes through it. For a record dispatched as a stub the
// text is kept only when ev ties it to the record (grantedAttributed); in
// every other case the stored result keeps the native reference only, which
// still settles what the record owes. Any other record stores ev's result
// as given.
func grantedResult(rec *requests.Record, ev nativeEvidence) *protocol.Result {
	if ev.result == nil || !rec.GrantAtPrompt || grantedAttributed(rec, ev.grantedDigest) {
		return ev.result
	}
	return &protocol.Result{NativeRef: ev.result.NativeRef}
}

// storedForm is the result as a write would store it on rec, for comparing
// native evidence with rec.Result. A record not dispatched as a stub
// compares the evidence as given.
func storedForm(rec *requests.Record, ev nativeEvidence) *protocol.Result {
	if !rec.GrantAtPrompt {
		return ev.result
	}
	return boundResult(grantedResult(rec, ev))
}

// grantedOutcome maps the cause that native evidence of a run's end
// justifies for rec. A record dispatched as a stub completes only when the
// evidence ties the answer to it; evidence of a completed or failed run that
// does not ends it failed with the answer withheld and the reason why, and
// forget reports that the stored prompt report is no longer evidence. Every
// other record and cause is returned as given.
func grantedOutcome(rec *requests.Record, c cause, ev nativeEvidence) (cause, nativeEvidence, bool) {
	if !rec.GrantAtPrompt || (c != causeCompleted && c != causeFailed) || grantedAttributed(rec, ev.grantedDigest) {
		return c, ev, false
	}
	ev.reason = unattributedReason
	return causeFailed, ev, true
}

// untaken reports whether rec was dispatched as a stub, its text was not
// given, it has not ended, and its deadline passed at now.
func untaken(rec *requests.Record, now time.Time) bool {
	if !rec.GrantAtPrompt || rec.GrantTextGiven {
		return false
	}
	switch rec.State {
	case protocol.StateDispatching, protocol.StateRunning, protocol.StateUncertain:
	default:
		return false
	}
	notAfter, err := protocol.ParseTime(rec.NotAfter)
	return err != nil || !now.Before(notAfter)
}

// settleUntaken ends a record whose stub the session did not take by its
// deadline: rejected expired, durably, with no evidence and no attachment
// needed. It decides on the record read again under the lock, so a text
// given meanwhile keeps the record. handled is false when the rule does not
// apply. A failed write returns its error and leaves the record for the next
// pass.
func (e *Endpoint) settleUntaken(key requests.Key) (handled bool, err error) {
	e.mu.Lock()
	rec, exists, err := e.store.Get(key)
	if err != nil {
		e.mu.Unlock()
		return false, err
	}
	at := e.now()
	if !exists || !untaken(rec, at) {
		e.mu.Unlock()
		return false, nil
	}
	e.transitionLocked(rec, causeRefused, nativeEvidence{code: protocol.CodeExpired, reason: untakenReason})
	if _, err := e.commitLocked(rec, e.targets[rec.TargetID]); err != nil {
		e.notifyStorageFailureLocked(rec, err)
		e.mu.Unlock()
		return true, err
	}
	h := e.promptHandoverLocked(rec.TargetID)
	e.mu.Unlock()
	if h != nil {
		h.ForgetPrompt(at)
	}
	return true, nil
}

// promptHandoverLocked is the target's attachment as a PromptHandover, or
// nil. The caller holds e.mu.
func (e *Endpoint) promptHandoverLocked(targetID string) PromptHandover {
	t, ok := e.targets[targetID]
	if !ok {
		return nil
	}
	h, _ := t.att.(PromptHandover)
	return h
}
