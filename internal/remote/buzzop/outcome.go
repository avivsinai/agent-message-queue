package buzzop

// Outcome is what one Buzz write attempt proves about its effect.
type Outcome string

const (
	// NotSent: the bytes never reached the transport. The write did not
	// take effect and may be sent again.
	NotSent Outcome = "not_sent"
	// Accepted: the transport returned the matching positive
	// acknowledgement for this write.
	Accepted Outcome = "accepted"
	// Rejected: the transport explicitly refused this attempt. It says
	// nothing about earlier attempts of the same write.
	Rejected Outcome = "rejected"
	// Unknown: the write may have taken effect; only an observation of the
	// relay's state resolves it.
	Unknown Outcome = "unknown"
)

// ReasonNoConnection is the Reason of an attempt made with no relay
// connection: nothing was sent.
const ReasonNoConnection = "no relay connection"

// Result is the outcome of one Buzz write. EventID is the event the write
// produced, when the transport named it. Reason is the transport's own words
// for an outcome other than Accepted.
type Result struct {
	Outcome Outcome
	EventID string
	Reason  string
}
