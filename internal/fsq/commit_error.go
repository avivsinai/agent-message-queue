package fsq

import "fmt"

// CommittedDurabilityError means the visible rename succeeded, but the
// destination directory could not be synced. Retrying with a new identifier
// may duplicate an artifact that is already present at FinalPath.
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

// DLQTransitionError reports that a DLQ envelope is visible but its directory
// sync failed, so the claimed source was deliberately retained for recovery.
type DLQTransitionError struct {
	EnvelopePath   string
	SourcePath     string
	SourceRetained bool
	Err            error
}

func (e *DLQTransitionError) Error() string {
	return fmt.Sprintf("DLQ envelope committed at %s with indeterminate durability; source retained at %s: %v; resolve the partial transition before retrying", e.EnvelopePath, e.SourcePath, e.Err)
}

func (e *DLQTransitionError) Unwrap() error {
	return e.Err
}
