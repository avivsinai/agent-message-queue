//go:build darwin || linux

package wakemutation

import (
	"fmt"
	"os"
)

func (lease *Lease) QueueStop(stopRequest chan<- struct{}) error {
	if stopRequest == nil {
		return fmt.Errorf("wake stop control queue is missing")
	}
	return lease.withEffect(func() error {
		select {
		case stopRequest <- struct{}{}:
			return nil
		default:
			return fmt.Errorf("wake stop control queue is full")
		}
	})
}

func (lease *Lease) QueueRestart(
	restartSignals chan<- os.Signal,
	signal os.Signal,
) error {
	if restartSignals == nil {
		return fmt.Errorf("wake restart control queue is missing")
	}
	return lease.withEffect(func() error {
		select {
		case restartSignals <- signal:
			return nil
		default:
			return fmt.Errorf("wake restart control signal queue is full")
		}
	})
}
