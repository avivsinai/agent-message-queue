//go:build linux

package cli

import (
	"errors"
	"fmt"
	"syscall"
)

func prepareAuthoritativeWakeStopPlatform(
	dirfd int,
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
) (authoritativeWakeStopCapability, error) {
	metadata := readWakeLockMetadataAt(dirfd, agentDir, expected.Root, expected.Agent)
	if !sameWakeLockGeneration(expected, metadata) {
		return authoritativeWakeStopCapability{}, fmt.Errorf("authoritative wake generation changed before stable stop preparation")
	}
	if classifyPersistedWakeClaim(metadata) != wakeClaimAuthoritative {
		return authoritativeWakeStopCapability{}, fmt.Errorf("wake claim is not authoritative during stable stop preparation")
	}

	pidfd, err := linuxPidfdOpen(metadata.PID, 0)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			metadata.Process = wakeProcessInfo{PID: metadata.PID}
			classifyWakeLock(metadata.Root, metadata.Agent, &metadata)
			return authoritativeWakeStopCapability{Inspection: metadata, Absent: true}, nil
		}
		return authoritativeWakeStopCapability{}, fmt.Errorf("pidfd_open authoritative wake process %d: %w", metadata.PID, err)
	}
	capability := authoritativeWakeStopCapability{
		Inspection: metadata,
		close:      func() error { return linuxPidfdClose(pidfd) },
	}
	exited, err := linuxPidfdPoll(pidfd, 0)
	if err != nil {
		_ = capability.Close()
		return authoritativeWakeStopCapability{}, fmt.Errorf("poll authoritative wake pidfd before inspection: %w", err)
	}
	if exited {
		capability.Absent = true
		capability.Inspection.Process = wakeProcessInfo{PID: metadata.PID}
		classifyWakeLock(metadata.Root, metadata.Agent, &capability.Inspection)
		return capability, nil
	}

	metadata.Process = inspectWakeProcess(metadata.PID)
	classifyWakeLock(metadata.Root, metadata.Agent, &metadata)
	capability.Inspection = metadata
	if !sameWakeLockGeneration(expected, metadata) {
		_ = capability.Close()
		return authoritativeWakeStopCapability{}, fmt.Errorf("authoritative wake generation changed during stable stop preparation")
	}
	switch metadata.Status {
	case wakeLockValid:
		if !metadata.IdentityConfirmed {
			_ = capability.Close()
			return authoritativeWakeStopCapability{}, fmt.Errorf("authoritative wake identity is not confirmed")
		}
	case wakeLockStale:
		// The retained pidfd names the current numeric-PID occupant. A proven
		// generation mismatch means the recorded wake itself is already absent;
		// do not signal the occupant.
		capability.Absent = true
		return capability, nil
	default:
		_ = capability.Close()
		return authoritativeWakeStopCapability{}, fmt.Errorf("authoritative wake identity is %s: %s", metadata.Status, metadata.Reason)
	}

	exited, err = linuxPidfdPoll(pidfd, 0)
	if err != nil {
		_ = capability.Close()
		return authoritativeWakeStopCapability{}, fmt.Errorf("poll authoritative wake pidfd after inspection: %w", err)
	}
	if exited {
		capability.Absent = true
		capability.Inspection.Process = wakeProcessInfo{PID: metadata.PID}
		classifyWakeLock(metadata.Root, metadata.Agent, &capability.Inspection)
		return capability, nil
	}
	capability.stop = func(wakeOwnerReleaseAuthorization) error {
		return terminateWakePidfd(pidfd)
	}
	return capability, nil
}

func inspectWakeLockForOwnerTransitionPlatform(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
) wakeLockInspection {
	return readWakeLockMetadataAt(dirfd, agentDir, root, me)
}
