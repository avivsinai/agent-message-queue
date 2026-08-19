package cli

// wakeOperatorOnlyError marks an action that AMQ will not take automatically.
// Callers must treat restart_capability as operator_only: the owning terminal
// or supervisor acts; agents must not signal, guess, or retry a force path.
type wakeOperatorOnlyError struct {
	reason string
}

func (err *wakeOperatorOnlyError) Error() string {
	return err.reason
}

func (err *wakeOperatorOnlyError) RestartCapability() string {
	return wakeRestartOperatorOnly
}

func newWakeOperatorOnlyError(reason string) error {
	return &wakeOperatorOnlyError{reason: reason}
}
