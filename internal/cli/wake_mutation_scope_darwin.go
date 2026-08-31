//go:build darwin

package cli

import (
	"os"
)

var signalWakeProcess = func(pid int, sig os.Signal) error {
	return newWakeOperatorOnlyError(
		"darwin raw numeric signaling is operator_only; stop the wake from its owning terminal or supervisor",
	)
}

func (scope *wakeMutationScope) queueStopRequest(stopRequest chan<- struct{}) error {
	if _, _, err := scope.location(); err != nil {
		return err
	}
	if _, err := scope.requireCanonicalOrDetached(); err != nil {
		return err
	}
	return scope.lease.QueueStop(stopRequest)
}

func (scope *wakeMutationScope) queueRestartSignal(
	restartSignals chan<- os.Signal,
	signal os.Signal,
) error {
	if _, _, err := scope.location(); err != nil {
		return err
	}
	if err := scope.requireCanonical(); err != nil {
		return err
	}
	return scope.lease.QueueRestart(restartSignals, signal)
}
