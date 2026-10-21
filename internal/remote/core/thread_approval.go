package core

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// ThreadApproval is an approval raised in a native turn that no request
// owns: the user started the turn in the harness's own terminal. It is not a
// request record and has no requests.Key. The endpoint keeps it in memory: a
// restarted process cannot answer an approval the harness sent to the process
// before it.
type ThreadApproval struct {
	// ID names the approval to the surface that shows it. It is derived
	// from the target, native session, turn and interaction id.
	ID            string
	TargetID      string
	Epoch         string
	NativeSession string
	Turn          string
	Interaction   protocol.Interaction
	// Origin is the routing of the owner share that shows the approval.
	Origin map[string]string
	// Resolution is set on the last publication: how the approval ended.
	// An expired approval is published with Interaction.RemoteAnswer false
	// and no resolution.
	Resolution *protocol.Resolution
}

// ThreadApprovalSink is where thread approvals go. Owner returns the source
// of the owner share whose approved native session is nativeSession on the
// target, or false when no share is pinned to it. Publish shows a new,
// expired or resolved approval on that share.
type ThreadApprovalSink struct {
	Owner   func(targetID, nativeSession string) (Source, bool)
	Publish func(ThreadApproval) error
}

// ThreadResponder is an Attachment that answers thread approvals: the
// approvals of turns no request owns.
type ThreadResponder interface {
	RespondThread(turnID, interactionID, option string) (protocol.Code, error)
}

// threadApprovalTTL is how long the owner can answer a thread approval from
// Buzz. After it the message says to answer in the terminal, so an approval
// whose resolution was never observed does not stay answerable.
const threadApprovalTTL = time.Hour

// threadApproval is the endpoint's record of one thread approval.
type threadApproval struct {
	ThreadApproval
	// host is the owner share's creator host. With Origin it is the only
	// source that may answer.
	host    string
	expires time.Time
	// answer is the one answer intent. A different answer is refused; the
	// same one may be sent again after a transport error.
	answer string
}

// threadApprovalID derives the approval id from what names it natively.
func threadApprovalID(targetID, nativeSession, turn, interactionID string) string {
	sum := sha256.Sum256([]byte("amq-remote/thread-approval\x00" + targetID + "\x00" + nativeSession + "\x00" + turn + "\x00" + interactionID))
	return "ta-" + hex.EncodeToString(sum[:12])
}

// SetThreadApprovals installs where thread approvals go. Until it is set,
// a thread approval is not shown and the terminal answers it.
func (e *Endpoint) SetThreadApprovals(s ThreadApprovalSink) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.threadSink = s
}

// maxThreadOwed bounds the thread approval publications kept for retry.
const maxThreadOwed = 64

// snapshot is what a publication of rec shows.
func (rec *threadApproval) snapshot() ThreadApproval {
	ta := rec.ThreadApproval
	ta.Origin = maps.Clone(ta.Origin)
	return ta
}

// publishThread shows one state of a thread approval, outside e.mu: the
// sink reads share state and syncs the ledger. A failed publication (an
// edit in the second the message was posted, a grant not yet renewed) is
// owed and retried on Tick; a newer state replaces it. The caller holds
// threadPubMu, so the states of one approval are shown in order.
func (e *Endpoint) publishThread(sink ThreadApprovalSink, ta ThreadApproval) {
	var err error
	if sink.Publish != nil {
		err = sink.Publish(ta)
	}
	e.mu.Lock()
	delete(e.threadOwed, ta.ID)
	if err != nil && len(e.threadOwed) < maxThreadOwed {
		e.threadOwed[ta.ID] = ta
	}
	e.mu.Unlock()
}

// onThreadQuestion records and shows an approval of a turn no request
// owns, when a share is pinned to the target's native session. Without one
// the terminal answers it alone.
func (e *Endpoint) onThreadQuestion(targetID string, ev NativeEvent) {
	e.threadPubMu.Lock()
	defer e.threadPubMu.Unlock()
	e.mu.Lock()
	t, sink := e.targets[targetID], e.threadSink
	e.mu.Unlock()
	if t == nil || ev.Interaction == nil || ev.Turn == "" || sink.Owner == nil {
		return
	}
	n, ok := t.att.(NativeIdentifier)
	if !ok || n.NativeSessionID() == "" {
		return
	}
	native := n.NativeSessionID()
	src, ok := sink.Owner(targetID, native)
	if !ok || src.Origin["carrier"] != "buzz" {
		return
	}
	id := threadApprovalID(targetID, native, ev.Turn, ev.Interaction.InteractionID)
	e.mu.Lock()
	if e.threadApprovals[id] != nil || e.targets[targetID] != t {
		e.mu.Unlock()
		return
	}
	rec := &threadApproval{
		ThreadApproval: ThreadApproval{ID: id, TargetID: targetID, Epoch: ev.Epoch, NativeSession: native, Turn: ev.Turn,
			Interaction: *ev.Interaction, Origin: maps.Clone(src.Origin)},
		host:    src.Host,
		expires: e.now().Add(threadApprovalTTL),
	}
	e.threadApprovals[id] = rec
	ta := rec.snapshot()
	e.mu.Unlock()
	e.publishThread(sink, ta)
}

// onThreadResolved shows how a thread approval ended and forgets it.
func (e *Endpoint) onThreadResolved(targetID string, ev NativeEvent) {
	if ev.Interaction == nil {
		return
	}
	e.threadPubMu.Lock()
	defer e.threadPubMu.Unlock()
	e.mu.Lock()
	sink := e.threadSink
	var ended []ThreadApproval
	for id, rec := range e.threadApprovals {
		if rec.TargetID != targetID || rec.Turn != ev.Turn || rec.Interaction.InteractionID != ev.Interaction.InteractionID {
			continue
		}
		r := protocol.Resolution{InteractionID: rec.Interaction.InteractionID, Outcome: protocol.ResolutionElsewhere}
		switch {
		case ev.Outcome != "":
			r.Outcome, r.Option = ev.Outcome, ev.Option
		case ev.Remote:
			r.Outcome, r.Option = protocol.ResolutionAnswered, rec.answer
		}
		rec.Resolution = &r
		delete(e.threadApprovals, id)
		ended = append(ended, rec.snapshot())
	}
	e.mu.Unlock()
	for _, ta := range ended {
		e.publishThread(sink, ta)
	}
}

// tickThreadApprovals retries owed publications, then ends the Buzz answer
// of every thread approval past its expiry: the message says to answer in
// the terminal.
func (e *Endpoint) tickThreadApprovals() {
	e.threadPubMu.Lock()
	defer e.threadPubMu.Unlock()
	e.mu.Lock()
	if e.state != stateAccepting {
		e.mu.Unlock()
		return
	}
	sink := e.threadSink
	var due []ThreadApproval
	for _, ta := range e.threadOwed {
		due = append(due, ta)
	}
	now := e.now()
	for id, rec := range e.threadApprovals {
		if now.Before(rec.expires) {
			continue
		}
		rec.Interaction.RemoteAnswer = false
		delete(e.threadApprovals, id)
		due = append(due, rec.snapshot())
	}
	e.mu.Unlock()
	// An owed state comes before the expiry of the same approval: it was
	// collected first.
	for _, ta := range due {
		e.publishThread(sink, ta)
	}
}

// RespondThreadApproval answers thread approval id with option. It is an
// in-process seam, never a wire op: only a carrier in this process reaches
// it, and only the owner share the approval was shown on may answer.
// A nil error means the answer reached the harness; a refusal
// with CodeAlreadyResolved means the approval is no longer open here.
func (e *Endpoint) RespondThreadApproval(src Source, id, option string) error {
	e.mu.Lock()
	if e.state != stateAccepting {
		e.mu.Unlock()
		return protocol.Refuse(protocol.CodeDraining, "endpoint is shutting down; answer in the terminal")
	}
	rec := e.threadApprovals[id]
	if rec == nil {
		// Resolved, expired, or raised before this process started.
		e.mu.Unlock()
		return protocol.Refuse(protocol.CodeAlreadyResolved, "the approval is no longer open here; if the terminal still asks, answer there")
	}
	if !sameShare(src, rec.host, rec.Origin) {
		e.mu.Unlock()
		return protocol.Refuse(protocol.CodeUnshared, "only the owner's Buzz share can answer this approval")
	}
	if !e.now().Before(rec.expires) {
		e.mu.Unlock()
		return protocol.Refuse(protocol.CodeExpired, "the approval expired; answer in the terminal")
	}
	t := e.targets[rec.TargetID]
	responder, ok := ThreadResponder(nil), false
	if t != nil {
		responder, ok = t.att.(ThreadResponder)
	}
	if !ok {
		e.mu.Unlock()
		return protocol.Refuse(protocol.CodeAttachmentLost, "target is not attached")
	}
	if n, isNative := t.att.(NativeIdentifier); !isNative || n.NativeSessionID() != rec.NativeSession {
		e.mu.Unlock()
		return protocol.Refuse(protocol.CodeSessionChanged, "the shared session changed")
	}
	if !offeredBy(rec.Interaction, option) {
		e.mu.Unlock()
		return protocol.Refuse(protocol.CodeInvalid, "option %q is not offered by the approval", option)
	}
	if rec.answer != "" && rec.answer != option {
		e.mu.Unlock()
		return protocol.Refuse(protocol.CodeAlreadyResolved, "an earlier answer (%s) is still being delivered", rec.answer)
	}
	rec.answer = option
	e.inFlight++
	e.mu.Unlock()
	defer e.releaseInFlight()

	code, err := responder.RespondThread(rec.Turn, rec.Interaction.InteractionID, option)
	if err != nil {
		return err // the answer may have landed: the intent stays
	}
	if code != "" {
		if code != protocol.CodeAlreadyResolved {
			e.mu.Lock()
			if cur := e.threadApprovals[id]; cur == rec && rec.answer == option {
				rec.answer = ""
			}
			e.mu.Unlock()
		}
		return protocol.Refuse(code, "%s", bareReason(code))
	}
	return nil
}

// offeredBy reports whether in offers option to a remote answer: one of
// its options that the adapter named as its one-tap approve or reject.
func offeredBy(in protocol.Interaction, option string) bool {
	if !in.RemoteAnswer || option == "" || option != in.ApproveOption && option != in.RejectOption {
		return false
	}
	for _, o := range in.Options {
		if o == option {
			return true
		}
	}
	return false
}
