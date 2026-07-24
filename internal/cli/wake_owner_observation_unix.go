//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"sync"
)

var wakeOwnerObservationAlreadyDone = func() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}()

type wakeOwnerObservationMonitor struct {
	done       chan struct{}
	joined     chan struct{}
	cancel     func() error
	closeOnce  sync.Once
	closeErr   error
	monitorErr error
}

func newWakeOwnerObservationMonitor(cancel func() error) *wakeOwnerObservationMonitor {
	return &wakeOwnerObservationMonitor{
		done:   make(chan struct{}),
		joined: make(chan struct{}),
		cancel: cancel,
	}
}

func (monitor *wakeOwnerObservationMonitor) finish(err error) {
	monitor.monitorErr = err
	close(monitor.done)
	close(monitor.joined)
}

func (monitor *wakeOwnerObservationMonitor) Done() <-chan struct{} {
	if monitor == nil {
		return nil
	}
	return monitor.done
}

func (monitor *wakeOwnerObservationMonitor) Close() error {
	if monitor == nil {
		return nil
	}
	monitor.closeOnce.Do(func() {
		var cancelErr error
		if monitor.cancel != nil {
			cancelErr = monitor.cancel()
		}
		<-monitor.joined
		monitor.closeErr = errors.Join(cancelErr, monitor.monitorErr)
	})
	return monitor.closeErr
}

type wakeOwnerObservation struct {
	State                 wakeOwnerIdentityState
	Reason                string
	CapabilityUnsupported bool
	monitor               *wakeOwnerObservationMonitor
	done                  <-chan struct{}
}

func deadWakeOwnerObservation(reason string) wakeOwnerObservation {
	return wakeOwnerObservation{
		State:  wakeOwnerDead,
		Reason: reason,
		done:   wakeOwnerObservationAlreadyDone,
	}
}

func (observation *wakeOwnerObservation) Done() <-chan struct{} {
	if observation == nil {
		return nil
	}
	if observation.monitor != nil {
		return observation.monitor.Done()
	}
	return observation.done
}

func (observation *wakeOwnerObservation) Close() error {
	if observation == nil {
		return nil
	}
	return observation.monitor.Close()
}

var observeAuthoritativeWakeOwner = observeAuthoritativeWakeOwnerPlatform

func captureAuthoritativeCurrentWakeOwner() (wakeOwner, error) {
	owner, err := captureAuthoritativeCurrentWakeOwnerPlatform()
	if err != nil {
		return wakeOwner{}, fmt.Errorf("capture current wake owner: %w", err)
	}
	return owner, nil
}
