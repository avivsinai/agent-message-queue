package amit

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// ErrNotSent marks a pre-send failure: the text never left this process, so
// a refusal is positive and retry is safe (mirrors the codex ErrNotSent
// contract).
var ErrNotSent = fmt.Errorf("amit: submit not sent")

// clientRef is the correlation token: the extension tags user-message
// entries and turn events with protocol.EncodeRef of the record key, so a
// submit and its outcome match across the event stream and the entry diff.
func clientRef(key requests.Key) string {
	return protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
}

// consume drains the extension entry tail, correlating user_message and
// agent_message entries into runs. It is called under a.mu by Inspect,
// Lookup, and CancelExact — the seam is pull-based: the entry diff resolves
// outcomes, never the send primitive (sendUserMessage swallows rejections).
func (a *Attachment) consume() {
	entries := a.source.Entries()
	for ; a.entryTail < len(entries); a.entryTail++ {
		e := entries[a.entryTail]
		switch e.Kind {
		case "status":
			// A per-request refusal from the extension is POSITIVE evidence
			// (the extension read the request and declined it) — unlike the
			// absent user_message entry, which is only uncertainty. Steer is
			// disabled in v1 (ADR); the extension answers deliver=steer with
			// {kind:"status",client_ref,status:"refused",reason:...} and the
			// endpoint must record a terminal rejection, never confirmation.
			if !strings.HasPrefix(e.Status, "refused") || e.ClientRef == "" {
				continue
			}
			for _, r := range a.runs {
				if clientRef(r.key) == e.ClientRef && !r.seenTurn && !r.state.Terminal() {
					r.state = protocol.StateRejected
					r.errText = "amit extension refused the request: " + e.Status
					a.emitLocked(core.NativeEvent{
						Type:   core.EventRunFailed,
						Key:    r.key,
						RunID:  r.runID,
						Result: r.result(),
					})
					break
				}
			}
		case "user_message":
			if e.ClientRef == "" {
				continue
			}
			for _, r := range a.runs {
				// A terminal run (e.g. refused by the extension) is final: a
				// late user_message must never resurrect it into a
				// confirmation.
				if r.state.Terminal() {
					continue
				}
				if clientRef(r.key) == e.ClientRef && !r.seenTurn {
					r.seenTurn = true
					// The user-message entry is the confirmation the send
					// primitive cannot give: the text landed in the session.
					r.state = protocol.StateRunning
				}
			}
		case "agent_message":
			// The latest agent answer completes the oldest still-running,
			// confirmed run: pi runs one turn at a time.
			for _, r := range a.runs {
				if r.seenTurn && !r.state.Terminal() {
					r.state = protocol.StateCompleted
					r.text.Reset()
					r.text.WriteString(e.Text)
					r.nativeRef = "amit session entry"
					a.emitLocked(core.NativeEvent{
						Type:   core.EventRunCompleted,
						Key:    r.key,
						RunID:  r.runID,
						Result: r.result(),
					})
					break
				}
			}
		}
	}
}

// prune bounds the retained run map: terminal+acked runs are dropped FIFO.
func (a *Attachment) prune() {
	if len(a.runs) <= maxRetained {
		return
	}
	// Deterministic drop: oldest creation order. runs has no order, so keep
	// a slice of keys in bind order.
	for len(a.runs) > maxRetained && len(a.order) > 0 {
		oldest := a.order[0]
		a.order = a.order[1:]
		if r, ok := a.runs[oldest]; ok {
			if r.state.Terminal() && r.acked {
				delete(a.runs, oldest)
			}
		}
	}
}

// emitLocked fans one event out to subscribers. Callers hold a.mu.
func (a *Attachment) emitLocked(ev core.NativeEvent) {
	for _, fn := range a.listeners {
		go fn(ev)
	}
}

// result is the ONLY place a protocol.Result is constructed in this
// package, so the bounding point (MaxResultBytes, UTF-8-rune safe) is one.
func (r *run) result() *protocol.Result {
	res := &protocol.Result{Text: r.text.String(), Error: r.errText}
	if r.nativeRef != "" {
		res.NativeRef = r.nativeRef
	}
	if len(res.Text) > protocol.MaxResultBytes {
		n := protocol.MaxResultBytes
		for n > 0 && !utf8.RuneStart(res.Text[n]) {
			n--
		}
		res.Text = res.Text[:n]
		res.Truncated = true
	}
	return res
}

// Inspect implements core.Attachment.
func (a *Attachment) Inspect() protocol.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consume()
	att, status := "live", a.source.Status()
	if a.offline || status == "offline" {
		att, status = "offline", "offline"
	}
	// No interaction surface is wired (Respond is already_resolved), so
	// PendingInteraction is always nil.
	return protocol.Session{
		Schema:             protocol.SchemaSession,
		TargetID:           a.target,
		Epoch:              a.epoch,
		Harness:            "amit",
		DisplayName:        "amit " + a.extName,
		Attachment:         att,
		Status:             status,
		PendingInteraction: nil,
		Capabilities: protocol.Capabilities{
			Inspect: true, Submit: true, CancelRequest: false, Steer: false,
			ApproveTool: false, AnswerQuestion: false, Terminal: "unavailable",
		},
		Evidence:   &protocol.Evidence{Submit: "submitted", Completion: "session_diff"},
		ObservedAt: protocol.FormatTime(a.now()),
	}
}

// Submit implements core.Attachment. Admitted means the extension confirmed
// the user-message entry landed; until then the run stays tentative. pi's
// sendUserMessage returns void and swallows rejections, so this adapter can
// never produce positive EvidenceNone.
func (a *Attachment) Submit(req core.BoundRequest) (core.Admission, error) {
	a.mu.Lock()
	if a.offline {
		a.mu.Unlock()
		return core.Admission{Code: protocol.CodeNativeError, Message: "amit extension source is closed"}, nil
	}
	if req.Epoch != a.epoch {
		a.mu.Unlock()
		return core.Admission{Code: protocol.CodeStaleEpoch}, nil
	}
	if existing, ok := a.runs[req.Key]; ok {
		a.mu.Unlock()
		return core.Admission{Admitted: true, RunID: existing.runID}, nil
	}
	a.mu.Unlock()

	if err := a.deliver(req); err != nil {
		// Deliver failed. pi's sendUserMessage cannot report WHY (void
		// return); a failure here is ambiguous, never a positive refusal:
		// the text may still have landed. Return the error so the endpoint
		// records uncertain; no correlation is bound yet because nothing
		// was sent.
		return core.Admission{}, err
	}

	a.mu.Lock()
	r := &run{key: req.Key, epoch: req.Epoch, runID: "amit:" + req.Key.RequestID, state: protocol.StateRunning, seenTurn: false}
	a.runs[req.Key] = r
	a.order = append(a.order, req.Key)
	a.mu.Unlock()

	// Re-check immediately: the entry may have landed between deliver and
	// the map insert.
	a.consume()
	a.mu.Lock()
	admitted := r.seenTurn
	rid := r.runID
	a.mu.Unlock()
	if !admitted {
		// Not yet confirmed. This is NOT a refusal: absence of the entry is
		// not proof the submit failed (the extension may be slow to write).
		// Return a non-nil error with the correlation bound so the endpoint
		// records UNCERTAIN and Lookup keeps resolving the run — mirroring
		// the codex unconfirmed path (a refusal code would commit a terminal
		// rejected record the endpoint never revisits while the text may be
		// running in the session).
		return core.Admission{RunID: rid}, fmt.Errorf("amit: user-message entry for %s not yet observed; submission uncertain", clientRef(req.Key))
	}
	return core.Admission{Admitted: true, RunID: rid}, nil
}

// Lookup implements core.Attachment.
func (a *Attachment) Lookup(key requests.Key, epoch string) (core.Evidence, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consume()
	r, ok := a.runs[key]
	if !ok {
		// Nothing retained. pi's send primitive cannot prove non-admission:
		// a lost submit and a never-submitted key look identical, so the
		// record stays uncertain (ADR: evidence class `submitted`, never
		// `admitted` for Amit).
		return core.Evidence{Known: true, Class: core.EvidenceUnknown}, nil
	}
	if r.epoch != epoch {
		return core.Evidence{Known: true, Class: core.EvidenceUnknown}, nil
	}
	ev := core.Evidence{Known: true, RunID: r.runID, State: r.state}
	switch {
	case r.acked:
		ev.Class = core.EvidenceNone
		ev.Admitted = true
	case r.seenTurn && r.state.Terminal():
		ev.Class = core.EvidenceHistoryTerminated
		ev.Admitted = true
		ev.Result = r.result()
	case r.seenTurn:
		ev.Class = core.EvidenceConfirmed
		ev.Admitted = true
	default:
		ev.Class = core.EvidenceTentative
	}
	return ev, nil
}

// CancelExact implements core.Attachment. There is no native cancel seam
// today (interrupt keystrokes are forbidden); a cancel is intent only and
// only resolved when the run is already terminal.
func (a *Attachment) CancelExact(key requests.Key, epoch string) (core.CancelEvidence, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consume()
	r, ok := a.runs[key]
	if !ok {
		return core.CancelEvidence{Disposition: protocol.CancelUnsupported, Message: "amit adapter has no native cancel seam"}, nil
	}
	if r.epoch != epoch {
		return core.CancelEvidence{Disposition: protocol.CancelUnsupported, Message: "epoch mismatch"}, nil
	}
	if r.state.Terminal() {
		return core.CancelEvidence{Disposition: protocol.CancelNoopTerminal}, nil
	}
	return core.CancelEvidence{Disposition: protocol.CancelUnsupported, Message: "amit adapter has no native cancel seam; the run resolves from the session diff"}, nil
}

// Respond implements core.Attachment: no interaction surface is wired
// (guardrail ctx.ui.select races are not bridged), so every answer is
// already-resolved.
func (a *Attachment) Respond(key requests.Key, epoch, interactionID, option string) (protocol.Code, error) {
	return protocol.CodeAlreadyResolved, nil
}

// AcknowledgeResult implements core.Attachment: releases the retained
// terminal evidence for the key once the digest matches exactly.
func (a *Attachment) AcknowledgeResult(key requests.Key, epoch, digest string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.runs[key]
	if !ok || r.epoch != epoch || !r.state.Terminal() || r.acked {
		return
	}
	if digest == "" || digest != protocol.EvidenceDigest(r.result()) {
		return
	}
	r.acked = true
	r.text.Reset()
	r.errText = ""
	a.prune()
}

// Subscribe implements core.Attachment.
func (a *Attachment) Subscribe(fn func(core.NativeEvent)) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	id := a.nextID
	a.listeners[id] = fn
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		delete(a.listeners, id)
	}
}
