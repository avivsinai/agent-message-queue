//go:build darwin

package cli

import (
	"fmt"
	"os"
)

var signalWakeProcess = func(pid int, sig os.Signal) error {
	return newWakeOperatorOnlyError(
		"darwin raw numeric signaling is operator_only; stop the wake from its owning terminal or supervisor",
	)
}

func (scope *wakeMutationScope) queueStopRequest(stopRequest chan<- struct{}) error {
	if stopRequest == nil {
		return fmt.Errorf("wake stop control queue is missing")
	}
	if _, _, err := scope.location(); err != nil {
		return err
	}
	if _, err := scope.requireCanonicalOrDetached(); err != nil {
		return err
	}
	select {
	case stopRequest <- struct{}{}:
		return nil
	default:
		return fmt.Errorf("wake stop control queue is full")
	}
}

func (scope *wakeMutationScope) queueRestartSignal(
	restartSignals chan<- os.Signal,
	signal os.Signal,
) error {
	if restartSignals == nil {
		return fmt.Errorf("wake restart control queue is missing")
	}
	if _, _, err := scope.location(); err != nil {
		return err
	}
	if _, err := scope.requireCanonicalOrDetached(); err != nil {
		return err
	}
	select {
	case restartSignals <- signal:
		return nil
	default:
		return fmt.Errorf("wake restart control signal queue is full")
	}
}
