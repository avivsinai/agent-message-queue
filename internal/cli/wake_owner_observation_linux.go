//go:build linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func observeAuthoritativeWakeOwnerPlatform(owner wakeOwner) (wakeOwnerObservation, error) {
	pidfd, err := linuxPidfdOpen(owner.PID, 0)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return wakeOwnerObservation{
				State:  wakeOwnerDead,
				Reason: "owner process is not running",
			}, nil
		}
		unsupported := wakeOwnerCapabilityUnsupported(err)
		return wakeOwnerObservation{
			State:                 wakeOwnerUnknown,
			Reason:                fmt.Sprintf("owner pidfd unavailable: %v", err),
			CapabilityUnsupported: unsupported,
		}, fmt.Errorf("pidfd_open owner process %d: %w", owner.PID, err)
	}
	observation := wakeOwnerObservation{
		State: wakeOwnerUnknown,
		close: func() error { return linuxPidfdClose(pidfd) },
	}
	exited, err := linuxPidfdPoll(pidfd, 0)
	if err != nil {
		_ = observation.Close()
		return wakeOwnerObservation{}, fmt.Errorf("poll owner pidfd before inspection: %w", err)
	}
	if exited {
		observation.State = wakeOwnerDead
		observation.Reason = "owner process is not running"
		return observation, nil
	}

	first := inspectWakeProcess(owner.PID)
	firstSessionID, firstSessionErr := getWakeProcessSID(owner.PID)
	exited, err = linuxPidfdPoll(pidfd, 0)
	if err != nil {
		_ = observation.Close()
		return wakeOwnerObservation{}, fmt.Errorf("poll owner pidfd during inspection: %w", err)
	}
	if exited {
		observation.State = wakeOwnerDead
		observation.Reason = "owner process is not running"
		return observation, nil
	}
	second := inspectWakeProcess(owner.PID)
	secondSessionID, secondSessionErr := getWakeProcessSID(owner.PID)
	exitedAfter, err := linuxPidfdPoll(pidfd, 0)
	if err != nil {
		_ = observation.Close()
		return wakeOwnerObservation{}, fmt.Errorf("poll owner pidfd after inspection: %w", err)
	}
	if exitedAfter {
		observation.State = wakeOwnerDead
		observation.Reason = "owner process is not running"
		return observation, nil
	}
	observation.State, observation.Reason = classifyStableAuthoritativeWakeOwner(
		owner,
		first, firstSessionID, firstSessionErr,
		second, secondSessionID, secondSessionErr,
	)
	return observation, nil
}

func wakeOwnerCapabilityUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP)
}

func captureAuthoritativeCurrentWakeOwnerPlatform() (wakeOwner, error) {
	pid := os.Getpid()
	pidfd, err := linuxPidfdOpen(pid, 0)
	if err != nil {
		if wakeOwnerCapabilityUnsupported(err) {
			// The current process cannot be replaced while this function is
			// executing, so a strict double snapshot remains stable without a
			// pidfd. Coop will still detect the unsupported child capability
			// and deliberately launch one ownerless wake.
			return captureCurrentWakeOwnerWithoutPidfd()
		}
		return wakeOwner{}, fmt.Errorf("pidfd_open current process %d: %w", pid, err)
	}
	defer func() { _ = linuxPidfdClose(pidfd) }()
	exited, err := linuxPidfdPoll(pidfd, 0)
	if err != nil {
		return wakeOwner{}, fmt.Errorf("poll current process pidfd before inspection: %w", err)
	}
	if exited {
		return wakeOwner{}, fmt.Errorf("current process is not running")
	}
	first := inspectWakeProcess(pid)
	firstSessionID, firstSessionErr := getWakeProcessSID(pid)
	owner := wakeOwner{
		PID:          pid,
		ProcessStart: first.StartToken,
		BootID:       first.BootID,
		SessionID:    firstSessionID,
	}
	if err := validateAuthoritativeWakeOwner(owner); err != nil {
		return wakeOwner{}, err
	}
	second := inspectWakeProcess(pid)
	secondSessionID, secondSessionErr := getWakeProcessSID(pid)
	exited, err = linuxPidfdPoll(pidfd, 0)
	if err != nil {
		return wakeOwner{}, fmt.Errorf("poll current process pidfd after inspection: %w", err)
	}
	if exited {
		second = wakeProcessInfo{PID: pid}
	}
	state, reason := classifyStableAuthoritativeWakeOwner(
		owner,
		first, firstSessionID, firstSessionErr,
		second, secondSessionID, secondSessionErr,
	)
	if state != wakeOwnerSame {
		return wakeOwner{}, fmt.Errorf("current process identity is %s: %s", state, reason)
	}
	return owner, nil
}

func captureCurrentWakeOwnerWithoutPidfd() (wakeOwner, error) {
	pid := os.Getpid()
	first := inspectWakeProcess(pid)
	firstSessionID, firstSessionErr := getWakeProcessSID(pid)
	owner := wakeOwner{
		PID:          pid,
		ProcessStart: first.StartToken,
		BootID:       first.BootID,
		SessionID:    firstSessionID,
	}
	if err := validateAuthoritativeWakeOwner(owner); err != nil {
		return wakeOwner{}, err
	}
	second := inspectWakeProcess(pid)
	secondSessionID, secondSessionErr := getWakeProcessSID(pid)
	state, reason := classifyStableAuthoritativeWakeOwner(
		owner,
		first, firstSessionID, firstSessionErr,
		second, secondSessionID, secondSessionErr,
	)
	if state != wakeOwnerSame {
		return wakeOwner{}, fmt.Errorf("current process identity is %s without pidfd: %s", state, reason)
	}
	return owner, nil
}
