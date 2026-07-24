//go:build darwin || linux

package cli

type wakeOwnerReleaseAuthorization struct {
	Token    *wakeOwner
	Rollback bool
}

type authoritativeWakeStopCapability struct {
	Inspection wakeLockInspection
	Absent     bool
	stop       func(wakeOwnerReleaseAuthorization) error
	close      func() error
}

func (capability *authoritativeWakeStopCapability) Stop(auth wakeOwnerReleaseAuthorization) error {
	if capability == nil || capability.Absent || capability.stop == nil {
		return nil
	}
	return capability.stop(auth)
}

func (capability *authoritativeWakeStopCapability) Close() error {
	if capability == nil || capability.close == nil {
		return nil
	}
	closeFn := capability.close
	capability.close = nil
	return closeFn()
}

var prepareAuthoritativeWakeStop = prepareAuthoritativeWakeStopPlatform
var inspectWakeLockForOwnerTransition = inspectWakeLockForOwnerTransitionPlatform
