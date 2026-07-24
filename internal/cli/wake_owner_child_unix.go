//go:build darwin || linux

package cli

import (
	"fmt"
	"os"
	"os/exec"
)

const envWakePrivateStopFD = "AMQ_WAKE_PRIVATE_STOP_FD"

type wakeOwnerChildCapabilityUnsupportedError struct {
	Err error
}

func (err *wakeOwnerChildCapabilityUnsupportedError) Error() string {
	return err.Err.Error()
}

func (err *wakeOwnerChildCapabilityUnsupportedError) Unwrap() error {
	return err.Err
}

type authoritativeWakeChildCapability struct {
	bind  func(*os.Process) error
	stop  func() error
	close func() error
}

func (capability *authoritativeWakeChildCapability) Bind(process *os.Process) error {
	if capability == nil || capability.bind == nil {
		return fmt.Errorf("authoritative wake child capability cannot bind")
	}
	return capability.bind(process)
}

func (capability *authoritativeWakeChildCapability) Stop() error {
	if capability == nil || capability.stop == nil {
		return fmt.Errorf("authoritative wake child capability cannot stop")
	}
	return capability.stop()
}

func (capability *authoritativeWakeChildCapability) Close() error {
	if capability == nil || capability.close == nil {
		return nil
	}
	closeFn := capability.close
	capability.close = nil
	return closeFn()
}

var prepareAuthoritativeWakeChild = prepareAuthoritativeWakeChildPlatform

func mergeWakeStopChannels(first, second <-chan struct{}) <-chan struct{} {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	merged := make(chan struct{})
	go func() {
		select {
		case <-first:
		case <-second:
		}
		close(merged)
	}()
	return merged
}

func configureAuthoritativeWakeChild(cmd *exec.Cmd) (*authoritativeWakeChildCapability, error) {
	return prepareAuthoritativeWakeChild(cmd)
}
