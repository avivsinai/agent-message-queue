//go:build darwin || linux

package cli

import "fmt"

type wakeOwnerObservation struct {
	State                 wakeOwnerIdentityState
	Reason                string
	CapabilityUnsupported bool
	close                 func() error
}

func (observation *wakeOwnerObservation) Close() error {
	if observation == nil || observation.close == nil {
		return nil
	}
	closeFn := observation.close
	observation.close = nil
	return closeFn()
}

var observeAuthoritativeWakeOwner = observeAuthoritativeWakeOwnerPlatform

func captureAuthoritativeCurrentWakeOwner() (wakeOwner, error) {
	owner, err := captureAuthoritativeCurrentWakeOwnerPlatform()
	if err != nil {
		return wakeOwner{}, fmt.Errorf("capture current wake owner: %w", err)
	}
	return owner, nil
}
