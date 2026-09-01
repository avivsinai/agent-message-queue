//go:build linux

package cli

import (
	"errors"
	"fmt"
	"syscall"
)

func prepareAuthoritativeWakeStopPlatform(
	scope *wakeMutationScope,
	expected wakeLockInspection,
) (capability authoritativeWakeStopCapability, retErr error) {
	defer func() { retErr = withWakeDiagnostic(retErr, expected.Root, expected.Agent) }()
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return authoritativeWakeStopCapability{}, err
	}
	metadata := readWakeLockMetadataAt(dirfd, agentDir, expected.Root, expected.Agent)
	if !sameWakeLockGeneration(expected, metadata) {
		return authoritativeWakeStopCapability{}, fmt.Errorf("authoritative wake generation changed before stable stop preparation")
	}
	if err := validateBoundWakeMutationAt(scope, metadata); err != nil {
		return authoritativeWakeStopCapability{}, err
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
	capability = authoritativeWakeStopCapability{
		Inspection: metadata,
		close:      func() error { return linuxPidfdClose(pidfd) },
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

	capability.stop = func(auth wakeOwnerReleaseAuthorization) error {
		return terminateAuthoritativeWakePidfdWithAuthorization(
			agentDir,
			metadata,
			auth,
			pidfd,
		)
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
