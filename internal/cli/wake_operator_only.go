//go:build darwin

package cli

import "fmt"

// wakeOperatorOnlyError marks an action that AMQ will not take automatically.
// Callers must treat restart_capability as operator_only: the owning terminal
// or supervisor acts; agents must not signal, guess, or retry a force path.
type wakeOperatorOnlyError struct {
	reason string
	remedy wakeRemedyArgv
}

func (err *wakeOperatorOnlyError) Error() string {
	if len(err.remedy) > 0 {
		return fmt.Sprintf("%s; inspect with %s", err.reason, err.remedy.String())
	}
	return err.reason
}

func (err *wakeOperatorOnlyError) RestartCapability() string {
	return wakeRestartOperatorOnly
}

func newWakeOperatorOnlyError(reason string, remedies ...wakeRemedyArgv) error {
	var remedy wakeRemedyArgv
	if len(remedies) > 0 {
		remedy = remedies[0]
	}
	return &wakeOperatorOnlyError{reason: reason, remedy: remedy}
}
