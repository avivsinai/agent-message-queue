package fsq

import "fmt"

// CommittedDurabilityError means the visible rename succeeded, but the
// affected directory metadata could not be fully synced. Retrying with a new
// identifier may duplicate an artifact that is already present at FinalPath.
type CommittedDurabilityError struct {
	FinalPath string
	Recipient string
	Err       error
}

func (e *CommittedDurabilityError) Error() string {
	if e.Recipient != "" {
		return fmt.Sprintf("delivery to %s committed at %s, but durability is indeterminate: %v; do not retry blindly", e.Recipient, e.FinalPath, e.Err)
	}
	return fmt.Sprintf("artifact committed at %s, but durability is indeterminate: %v; do not retry blindly", e.FinalPath, e.Err)
}

func (e *CommittedDurabilityError) Unwrap() error {
	return e.Err
}

// DLQTransitionError reports an incomplete DLQ transition where the envelope
// is visible but the recoverable source is still present. Completed logical
// transitions with indeterminate durability use CommittedDurabilityError.
type DLQTransitionError struct {
	EnvelopePath   string
	SourcePath     string
	SourceRetained bool
	Err            error
}

func (e *DLQTransitionError) Error() string {
	return fmt.Sprintf("DLQ envelope visible at %s; source retained at %s: %v; resolve the partial transition before retrying", e.EnvelopePath, e.SourcePath, e.Err)
}

func (e *DLQTransitionError) Unwrap() error {
	return e.Err
}

// IndeterminateQuarantineError means a permanent-claim failure could not be
// safely resolved into a completed quarantine: the exclusive ownership rename
// of the source (inbox/new → quarantine staging) failed for a reason that is
// neither a clean loss (ENOENT) nor a committed move. The message is NOT
// confirmed removed and NOT confirmed retained in a reconcilable state, so
// the carrier must NOT report the message as consumed or permanently
// classified (agent-message-queue-611.22.42, Pro B776-1). The source may still
// be in inbox/new; the next tick re-evaluates it.
type IndeterminateQuarantineError struct {
	SourcePath string
	Err        error
}

func (e *IndeterminateQuarantineError) Error() string {
	return fmt.Sprintf("quarantine of %s could not be completed safely: %v; source state is indeterminate — do not report as consumed", e.SourcePath, e.Err)
}

func (e *IndeterminateQuarantineError) Unwrap() error {
	return e.Err
}
