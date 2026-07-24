//go:build darwin

package cli

import "fmt"

func prepareAuthoritativeWakeStopPlatform(
	dirfd int,
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
) (authoritativeWakeStopCapability, error) {
	current := inspectWakeLockAt(dirfd, agentDir, expected.Root, expected.Agent)
	if !sameWakeLockGeneration(expected, current) {
		return authoritativeWakeStopCapability{}, fmt.Errorf("authoritative wake generation changed before cooperative stop preparation")
	}
	if classifyPersistedWakeClaim(current) != wakeClaimAuthoritative {
		return authoritativeWakeStopCapability{}, fmt.Errorf("wake claim is not authoritative during cooperative stop preparation")
	}
	if current.Status == wakeLockStale {
		// Stale is affirmative proof that the recorded generation is absent.
		// A live process here is a different occupant of the recycled PID and
		// must never prevent release or receive a signal.
		return authoritativeWakeStopCapability{Inspection: current, Absent: true}, nil
	}
	if current.Status != wakeLockValid || !current.IdentityConfirmed {
		return authoritativeWakeStopCapability{}, fmt.Errorf("authoritative wake identity is %s: %s", current.Status, current.Reason)
	}
	return authoritativeWakeStopCapability{
		Inspection: current,
		stop: func(auth wakeOwnerReleaseAuthorization) error {
			stopped, err := cooperativeStopAuthoritativeWake(current, auth)
			if err != nil {
				return err
			}
			if !stopped {
				return fmt.Errorf("authoritative wake cooperative stop did not release the generation")
			}
			return nil
		},
	}, nil
}

func inspectWakeLockForOwnerTransitionPlatform(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
) wakeLockInspection {
	return inspectWakeLockAt(dirfd, agentDir, root, me)
}
