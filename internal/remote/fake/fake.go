// Package fake is the controllable runtime the contract corpus drives. It
// implements core.Attachment with the exact ownership semantics a real
// harness must show: serialized admission, cancellation intent honored at the
// boundary, retained evidence across endpoint restarts, and a local editor
// that remote work never touches.
package fake

import (
	"fmt"
	"sync"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

type run struct {
	id                string
	key               requests.Key
	epoch             string
	state             protocol.State
	result            *protocol.Result
	localIntervention bool
	interaction       *protocol.Interaction
	acknowledged      bool
}

// Runtime is one fake harness session.
type Runtime struct {
	mu sync.Mutex

	targetID   string
	epoch      string
	sessionID  string
	attachment string
	busy       bool
	draft      string

	// controls
	admissionGate        chan struct{}
	lookupGate           chan struct{}
	admissionFailsAfter  bool
	consumerSets         int
	interactionAnswers   []Answer
	answeredInteractions map[string]string
	runsByKey            map[requests.Key]*run
	runsByInteraction    map[string]*run
	cancelIntent         map[requests.Key]bool
	nextRun              int
	dispatches           int
	aborts               int
	ackCalls             int
	listeners            map[int]func(core.NativeEvent)
	nextListener         int
	capabilityOverride   *protocol.Capabilities
}

// Answer records who answered which interaction.
type Answer struct {
	InteractionID string
	Option        string
	Source        string
}

// New returns an idle, live fake runtime.
func New(targetID, epoch string) *Runtime {
	return &Runtime{
		targetID:             targetID,
		epoch:                epoch,
		sessionID:            "session-1",
		attachment:           "live",
		runsByKey:            map[requests.Key]*run{},
		runsByInteraction:    map[string]*run{},
		cancelIntent:         map[requests.Key]bool{},
		answeredInteractions: map[string]string{},
		listeners:            map[int]func(core.NativeEvent){},
	}
}

// Inspect implements core.Attachment.
func (r *Runtime) Inspect() protocol.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := "idle"
	switch {
	case r.attachment == "offline":
		status = "offline"
	case r.busy:
		status = "busy"
	}
	caps := protocol.Capabilities{Inspect: true, Submit: true, CancelRequest: true, AnswerQuestion: true, ApproveTool: true, Steer: true, Terminal: "unavailable"}
	if r.capabilityOverride != nil {
		caps = *r.capabilityOverride
	}
	return protocol.Session{
		Schema:       protocol.SchemaSession,
		TargetID:     r.targetID,
		Epoch:        r.epoch,
		Harness:      "fake",
		DisplayName:  "fake runtime",
		Attachment:   r.attachment,
		Status:       status,
		Capabilities: caps,
		Evidence:     &protocol.Evidence{Submit: "admitted", Completion: "run_terminal"},
		ObservedAt:   "2026-09-08T10:00:00Z",
	}
}

// Submit implements core.Attachment. It blocks while the admission gate is
// held, then atomically rechecks epoch and cancellation intent, refuses when
// busy, and binds the key to a new run.
func (r *Runtime) Submit(req core.BoundRequest) (core.Admission, error) {
	r.mu.Lock()
	gate := r.admissionGate
	r.mu.Unlock()
	if gate != nil {
		<-gate
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.dispatches++
	if r.admissionFailsAfter {
		r.admissionFailsAfter = false
		return core.Admission{Code: protocol.CodeNativeError, Message: "native admission failed after helper returned"}, nil
	}
	if req.Epoch != r.epoch {
		return core.Admission{Code: protocol.CodeStaleEpoch}, nil
	}
	if r.cancelIntent[req.Key] {
		delete(r.cancelIntent, req.Key)
		return core.Admission{Code: protocol.CodeCancelledBeforeAdmission}, nil
	}
	if existing, ok := r.runsByKey[req.Key]; ok {
		return core.Admission{Admitted: true, RunID: existing.id}, nil
	}
	if r.busy {
		if req.Input.Busy != protocol.BusyQueue {
			return core.Admission{Code: protocol.CodeBusy, Message: "a run is active"}, nil
		}
	}
	for _, rn := range r.runsByKey {
		if rn.state.Terminal() && !rn.acknowledged {
			return core.Admission{Code: protocol.CodeBusy, Message: "a completed result awaits acknowledgement"}, nil
		}
	}
	r.nextRun++
	rn := &run{id: fmt.Sprintf("run_%d", r.nextRun), key: req.Key, epoch: req.Epoch, state: protocol.StateRunning}
	r.runsByKey[req.Key] = rn
	r.busy = true
	return core.Admission{Admitted: true, RunID: rn.id}, nil
}

// Lookup implements core.Attachment.
func (r *Runtime) Lookup(key requests.Key, epoch string) (core.Evidence, error) {
	r.mu.Lock()
	gate := r.lookupGate
	r.mu.Unlock()
	if gate != nil {
		<-gate
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rn, ok := r.runsByKey[key]
	if !ok || rn.epoch != epoch {
		return core.Evidence{}, nil
	}
	if rn.acknowledged {
		// AcknowledgeResult released the retained terminal evidence. A real
		// attachment retains nothing for the key afterwards, so Lookup must
		// stop offering the result — otherwise replayTerminalAck would see it
		// on every Reconcile/Tick and re-ack forever.
		return core.Evidence{Class: core.EvidenceNone, Known: true, Admitted: true, RunID: rn.id, State: rn.state}, nil
	}
	return core.Evidence{Class: core.EvidenceConfirmed, Known: true, Admitted: true, RunID: rn.id, State: rn.state, Result: rn.result, LocalIntervention: rn.localIntervention, Interaction: rn.interaction}, nil
}

// CancelExact implements core.Attachment.
func (r *Runtime) CancelExact(key requests.Key, epoch string) (core.CancelEvidence, error) {
	r.mu.Lock()
	rn, ok := r.runsByKey[key]
	if !ok {
		r.cancelIntent[key] = true
		r.mu.Unlock()
		return core.CancelEvidence{Disposition: protocol.CancelRequested, Message: "intent recorded before admission"}, nil
	}
	if rn.epoch != epoch || rn.state.Terminal() {
		r.mu.Unlock()
		return core.CancelEvidence{Disposition: protocol.CancelNoopTerminal}, nil
	}
	r.aborts++
	rn.state = protocol.StateCancelled
	r.busy = false
	ev := core.NativeEvent{Type: core.EventRunCancelled, Key: key, RunID: rn.id}
	r.mu.Unlock()
	r.emit(ev)
	return core.CancelEvidence{Disposition: protocol.CancelConfirmed}, nil
}

// Respond implements core.Attachment.
func (r *Runtime) Respond(key requests.Key, _ string, interactionID, option string) (protocol.Code, error) {
	r.mu.Lock()
	rn, ok := r.runsByInteraction[interactionID]
	if !ok || rn.key != key {
		r.mu.Unlock()
		return protocol.CodeAlreadyResolved, nil
	}
	r.answeredInteractions[interactionID] = option
	r.interactionAnswers = append(r.interactionAnswers, Answer{InteractionID: interactionID, Option: option, Source: "remote"})
	delete(r.runsByInteraction, interactionID)
	rn.interaction = nil
	ev := core.NativeEvent{Type: core.EventQuestionResolved, Key: key, RunID: rn.id}
	r.mu.Unlock()
	r.emit(ev)
	return "", nil
}

// AcknowledgeResult implements core.Attachment. It releases the retained
// terminal evidence for the key only when the digest names exactly that
// evidence — the design contract's "stale acknowledgements cannot release a
// different request's result". An ack with a wrong or empty digest leaves
// the record retained so the busy wedge stays observable.
func (r *Runtime) AcknowledgeResult(key requests.Key, epoch, digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ackCalls++
	rn, ok := r.runsByKey[key]
	if !ok || rn.epoch != epoch || !rn.state.Terminal() {
		return
	}
	if digest == "" || digest != protocol.EvidenceDigest(rn.result) {
		return
	}
	rn.acknowledged = true
}

// Subscribe implements core.Attachment.
func (r *Runtime) Subscribe(fn func(core.NativeEvent)) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextListener++
	id := r.nextListener
	r.listeners[id] = fn
	r.consumerSets = len(r.listeners)
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.listeners, id)
		r.consumerSets = len(r.listeners)
	}
}

func (r *Runtime) emit(ev core.NativeEvent) {
	r.mu.Lock()
	fns := make([]func(core.NativeEvent), 0, len(r.listeners))
	for _, fn := range r.listeners {
		fns = append(fns, fn)
	}
	r.mu.Unlock()
	for _, fn := range fns {
		fn(ev)
	}
}

// Controls used by the corpus runner.

// HoldAdmission makes Submit block until ReleaseAdmission.
func (r *Runtime) HoldAdmission() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.admissionGate == nil {
		r.admissionGate = make(chan struct{})
	}
}

// HoldLookup makes Lookup block until ReleaseLookup.
func (r *Runtime) HoldLookup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lookupGate == nil {
		r.lookupGate = make(chan struct{})
	}
}

// ReleaseLookup unblocks held Lookups.
func (r *Runtime) ReleaseLookup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lookupGate != nil {
		close(r.lookupGate)
		r.lookupGate = nil
	}
}

// ReleaseAdmission unblocks held Submits.
func (r *Runtime) ReleaseAdmission() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.admissionGate != nil {
		close(r.admissionGate)
		r.admissionGate = nil
	}
}

// FailNextAdmissionAfterReturn makes the next Submit refuse with a native
// error after the helper call returned, the Q11 shape.
func (r *Runtime) FailNextAdmissionAfterReturn() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.admissionFailsAfter = true
}

// Complete finishes the run bound to requestID with text. It is a no-op when
// no such run is admitted, so Q19 can call it at every boundary.
func (r *Runtime) Complete(requestID, text string) bool {
	r.mu.Lock()
	var target *run
	for _, rn := range r.runsByKey {
		if rn.key.RequestID == requestID && rn.state == protocol.StateRunning {
			target = rn
		}
	}
	if target == nil {
		r.mu.Unlock()
		return false
	}
	target.state = protocol.StateCompleted
	target.result = &protocol.Result{Text: text}
	r.busy = false
	ev := core.NativeEvent{Type: core.EventRunCompleted, Key: target.key, RunID: target.id, Result: target.result}
	r.mu.Unlock()
	r.emit(ev)
	return true
}

// LocalInput records a human steering keystroke during the active run.
func (r *Runtime) LocalInput(text string) {
	r.mu.Lock()
	var target *run
	for _, rn := range r.runsByKey {
		if rn.state == protocol.StateRunning {
			target = rn
		}
	}
	if target == nil {
		r.mu.Unlock()
		return
	}
	target.localIntervention = true
	_ = text
	ev := core.NativeEvent{Type: core.EventLocalIntervention, Key: target.key, RunID: target.id}
	r.mu.Unlock()
	r.emit(ev)
}

// LocalDraft sets the unsent editor text. Remote work must leave it intact.
func (r *Runtime) LocalDraft(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.draft = text
}

// Draft returns the unsent editor text.
func (r *Runtime) Draft() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.draft
}

// SessionID returns the native conversation identity.
func (r *Runtime) SessionID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessionID
}

// Question raises a pending interaction on the run bound to requestID.
func (r *Runtime) Question(requestID, interactionID string, options []string) {
	r.mu.Lock()
	var target *run
	for _, rn := range r.runsByKey {
		if rn.key.RequestID == requestID && rn.state == protocol.StateRunning {
			target = rn
		}
	}
	if target == nil {
		r.mu.Unlock()
		return
	}
	target.interaction = &protocol.Interaction{InteractionID: interactionID, Kind: "question", Options: options, RemoteAnswer: true}
	r.runsByInteraction[interactionID] = target
	ev := core.NativeEvent{Type: core.EventQuestion, Key: target.key, RunID: target.id, Interaction: target.interaction}
	r.mu.Unlock()
	r.emit(ev)
}

// LocalAnswer answers a pending interaction from the local UI.
func (r *Runtime) LocalAnswer(interactionID, option string) {
	r.mu.Lock()
	rn, ok := r.runsByInteraction[interactionID]
	if !ok {
		r.mu.Unlock()
		return
	}
	r.answeredInteractions[interactionID] = option
	r.interactionAnswers = append(r.interactionAnswers, Answer{InteractionID: interactionID, Option: option, Source: "local"})
	delete(r.runsByInteraction, interactionID)
	rn.interaction = nil
	ev := core.NativeEvent{Type: core.EventQuestionResolved, Key: rn.key, RunID: rn.id}
	r.mu.Unlock()
	r.emit(ev)
}

// SwitchSession simulates /new, /resume, fork or reload: a new epoch and a
// new native session; old runs stay retained under their old epoch.
func (r *Runtime) SwitchSession(newEpoch string) {
	r.mu.Lock()
	r.epoch = newEpoch
	r.sessionID = "session-" + newEpoch
	r.busy = false
	r.mu.Unlock()
	r.emit(core.NativeEvent{Type: core.EventEpochChanged, Epoch: newEpoch})
}

// SetOffline toggles attachment liveness.
func (r *Runtime) SetOffline(offline bool) {
	r.mu.Lock()
	if offline {
		r.attachment = "offline"
	} else {
		r.attachment = "live"
	}
	r.mu.Unlock()
	r.emit(core.NativeEvent{Type: core.EventStatus, Attachment: r.attachment})
}

// Counters exposes what the corpus asserts about the harness side.
type Counters struct {
	Dispatches   int
	Aborts       int
	ConsumerSets int
	Answers      []Answer
	RunningKeys  []requests.Key
}

// Snapshot returns the counters.
func (r *Runtime) Snapshot() Counters {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := Counters{Dispatches: r.dispatches, Aborts: r.aborts, ConsumerSets: r.consumerSets, Answers: append([]Answer(nil), r.interactionAnswers...)}
	for k, rn := range r.runsByKey {
		if rn.state == protocol.StateRunning {
			c.RunningKeys = append(c.RunningKeys, k)
		}
	}
	return c
}

// HasRun reports whether any run is bound to requestID.
func (r *Runtime) HasRun(requestID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.runsByKey {
		if k.RequestID == requestID {
			return true
		}
	}
	return false
}

// AckCalls counts every AcknowledgeResult invocation (whether or not the
// digest matched). A converged endpoint acks a terminal result exactly once;
// a second Reconcile/Tick that re-acks the same result increments this.
func (r *Runtime) AckCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ackCalls
}

// UnacknowledgedResults counts retained terminal results the endpoint has not
// acknowledged. While this is nonzero the runtime refuses additional remote
// work — the B06 busy wedge.
func (r *Runtime) UnacknowledgedResults() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rn := range r.runsByKey {
		if rn.state.Terminal() && !rn.acknowledged {
			n++
		}
	}
	return n
}
