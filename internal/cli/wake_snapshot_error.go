package cli

// wakeSnapshotReadChangedError marks an observation that could not bind one
// stable file identity. It is retryable only when the caller can restart the
// complete read-only decision without reusing evidence from the failed read.
type wakeSnapshotReadChangedError struct {
	err error
}

func (err *wakeSnapshotReadChangedError) Error() string {
	return err.err.Error()
}

func (err *wakeSnapshotReadChangedError) Unwrap() error {
	return err.err
}

func newWakeSnapshotReadChangedError(err error) error {
	if err == nil {
		return nil
	}
	return &wakeSnapshotReadChangedError{err: err}
}
