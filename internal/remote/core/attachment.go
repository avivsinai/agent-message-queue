package core

import (
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// BoundRequest is what the endpoint hands a native attachment. The key is the
// authenticated creator host plus the request id, so two hosts cannot share
// a run map entry by reusing a UUID.
type BoundRequest struct {
	Key      requests.Key
	Epoch    string
	Input    protocol.SubmitInput
	NotAfter string
}

// Admission is the attachment's answer to Submit. Admitted means the request
// is bound to a native run; otherwise Code names the positive refusal.
type Admission struct {
	Admitted bool
	RunID    string
	Code     protocol.Code
	Message  string
}

// Evidence is what Lookup returns about a request the attachment may have
// seen. Known=false means the attachment retains nothing for the key. For an
// adapter with a real admission primitive (codex, the fake) that is positive
// evidence admission never happened. An adapter over an API that cannot report
// its own rejections (the planned Amit extension, whose sendUserMessage
// swallows errors) must NOT treat Known=false as proof of non-admission; there
// a lost submit and a never-submitted key look identical, so such an adapter
// leaves the record uncertain rather than rejecting it.
// EvidenceClass discriminates what the attachment can prove about a key, so
// the endpoint never turns "in flight" or "delivered but unknown" into a
// terminal rejection. See the remote-control ADR (evidence contract).
type EvidenceClass string

const (
	// EvidenceNone: the adapter has a real admission primitive and retains
	// nothing for the key, so admission did not and cannot happen -> reject.
	EvidenceNone EvidenceClass = "none"
	// EvidenceUnknown: the operation was delivered but admission cannot be
	// determined (transport ambiguity, an API that swallows rejections)
	// -> uncertain, keep correlation.
	EvidenceUnknown EvidenceClass = "unknown"
	// EvidenceTentative: submitted and bound, but native ownership is not yet
	// confirmed -> keep as running/dispatching, do not reject, cancel, or
	// attribute output until it confirms or times out.
	EvidenceTentative EvidenceClass = "tentative"
	// EvidenceConfirmed: native ownership is established -> real admission.
	EvidenceConfirmed EvidenceClass = "confirmed"
)

type Evidence struct {
	// Class is the authoritative discriminator; Known/Admitted are derived
	// convenience mirrors (Known = Class != None-with-nothing-retained;
	// Admitted = Class == Confirmed).
	Class             EvidenceClass
	Known             bool
	Admitted          bool
	RunID             string
	State             protocol.State
	Result            *protocol.Result
	LocalIntervention bool
	Interaction       *protocol.Interaction
}

// CancelEvidence is the attachment's answer to CancelExact.
type CancelEvidence struct {
	Disposition protocol.CancelDisposition
	Message     string
}

// NativeEventType names what a native attachment observed.
type NativeEventType string

// Native event types the endpoint consumes.
const (
	EventRunCompleted      NativeEventType = "run_completed"
	EventRunFailed         NativeEventType = "run_failed"
	EventRunCancelled      NativeEventType = "run_cancelled"
	EventQuestion          NativeEventType = "question"
	EventQuestionResolved  NativeEventType = "question_resolved"
	EventLocalIntervention NativeEventType = "local_intervention"
	EventEpochChanged      NativeEventType = "epoch_changed"
	EventStatus            NativeEventType = "status"
)

// NativeEvent is one observation from an attachment. Key is set for events
// about a bound request; Epoch and Status for runtime-level events.
type NativeEvent struct {
	Type        NativeEventType
	Key         requests.Key
	RunID       string
	Result      *protocol.Result
	Interaction *protocol.Interaction
	Epoch       string
	Status      string
	Attachment  string
}

// Attachment is the native seam contract from the design. Implementations
// serialize Submit and CancelExact against the harness's own input path and
// never touch the local editor. Every method is exact about the request and
// epoch it acts on.
type Attachment interface {
	// Inspect returns the current session projection.
	Inspect() protocol.Session
	// Submit performs the final epoch, busy and cancellation checks at the
	// owning native boundary and, if admitted, binds the key to a run.
	Submit(req BoundRequest) (Admission, error)
	// Lookup returns retained evidence for a key without dispatching.
	Lookup(key requests.Key, epoch string) (Evidence, error)
	// CancelExact cancels the bound run or records cancellation intent when
	// admission has not happened yet. It never aborts a different run.
	CancelExact(key requests.Key, epoch string) (CancelEvidence, error)
	// Respond answers one exact pending interaction with one offered option.
	Respond(key requests.Key, epoch, interactionID, option string) (protocol.Code, error)
	// AcknowledgeResult releases the retained terminal evidence for the key
	// once the endpoint has persisted it.
	AcknowledgeResult(key requests.Key, epoch, digest string)
	// Subscribe delivers native events until the returned function is called.
	Subscribe(func(NativeEvent)) func()
}
