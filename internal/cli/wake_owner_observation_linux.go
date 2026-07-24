//go:build linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func observeAuthoritativeWakeOwnerPlatform(owner wakeOwner) (wakeOwnerObservation, error) {
	pidfd, err := linuxPidfdOpen(owner.PID, 0)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return deadWakeOwnerObservation("owner process is not running"), nil
		}
		unsupported := wakeOwnerCapabilityUnsupported(err)
		return wakeOwnerObservation{
			State:                 wakeOwnerUnknown,
			Reason:                fmt.Sprintf("owner pidfd unavailable: %v", err),
			CapabilityUnsupported: unsupported,
		}, fmt.Errorf("pidfd_open owner process %d: %w", owner.PID, err)
	}
	exited, err := linuxPidfdPoll(pidfd, 0)
	if err != nil {
		return closeLinuxOwnerObservationPidfd(
			pidfd,
			wakeOwnerObservation{},
			fmt.Errorf("poll owner pidfd before inspection: %w", err),
		)
	}
	if exited {
		return closeLinuxOwnerObservationPidfd(
			pidfd,
			deadWakeOwnerObservation("owner process is not running"),
			nil,
		)
	}

	first := inspectWakeProcess(owner.PID)
	firstSessionID, firstSessionErr := getWakeProcessSID(owner.PID)
	exited, err = linuxPidfdPoll(pidfd, 0)
	if err != nil {
		return closeLinuxOwnerObservationPidfd(
			pidfd,
			wakeOwnerObservation{},
			fmt.Errorf("poll owner pidfd during inspection: %w", err),
		)
	}
	if exited {
		return closeLinuxOwnerObservationPidfd(
			pidfd,
			deadWakeOwnerObservation("owner process is not running"),
			nil,
		)
	}
	second := inspectWakeProcess(owner.PID)
	secondSessionID, secondSessionErr := getWakeProcessSID(owner.PID)
	exitedAfter, err := linuxPidfdPoll(pidfd, 0)
	if err != nil {
		return closeLinuxOwnerObservationPidfd(
			pidfd,
			wakeOwnerObservation{},
			fmt.Errorf("poll owner pidfd after inspection: %w", err),
		)
	}
	if exitedAfter {
		return closeLinuxOwnerObservationPidfd(
			pidfd,
			deadWakeOwnerObservation("owner process is not running"),
			nil,
		)
	}
	observation := wakeOwnerObservation{}
	observation.State, observation.Reason = classifyStableAuthoritativeWakeOwner(
		owner,
		first, firstSessionID, firstSessionErr,
		second, secondSessionID, secondSessionErr,
	)
	switch observation.State {
	case wakeOwnerSame:
		monitor, monitorErr := startLinuxWakeOwnerObservationMonitor(pidfd)
		if monitorErr != nil {
			return wakeOwnerObservation{
				State:  wakeOwnerUnknown,
				Reason: fmt.Sprintf("owner pidfd monitor unavailable: %v", monitorErr),
			}, monitorErr
		}
		observation.monitor = monitor
		return observation, nil
	case wakeOwnerDead:
		observation.done = wakeOwnerObservationAlreadyDone
	case wakeOwnerUnknown:
		return closeLinuxOwnerObservationPidfd(
			pidfd,
			observation,
			fmt.Errorf("owner process identity is unknown: %s", observation.Reason),
		)
	}
	return closeLinuxOwnerObservationPidfd(pidfd, observation, nil)
}

func closeLinuxOwnerObservationPidfd(
	pidfd int,
	observation wakeOwnerObservation,
	cause error,
) (wakeOwnerObservation, error) {
	closeErr := linuxPidfdClose(pidfd)
	if closeErr != nil {
		closeErr = fmt.Errorf("close owner pidfd: %w", closeErr)
	}
	return observation, errors.Join(cause, closeErr)
}

func startLinuxWakeOwnerObservationMonitor(pidfd int) (*wakeOwnerObservationMonitor, error) {
	cancelRead, cancelWrite, err := os.Pipe()
	if err != nil {
		_, closeErr := closeLinuxOwnerObservationPidfd(pidfd, wakeOwnerObservation{}, nil)
		return nil, errors.Join(
			fmt.Errorf("create owner pidfd monitor cancellation pipe: %w", err),
			closeErr,
		)
	}
	monitor := newWakeOwnerObservationMonitor(cancelWrite.Close)
	go func() {
		waitErr := waitLinuxWakeOwnerObservation(pidfd, int(cancelRead.Fd()))
		closeErr := errors.Join(
			linuxPidfdClose(pidfd),
			cancelRead.Close(),
		)
		monitor.finish(errors.Join(waitErr, closeErr))
	}()
	return monitor, nil
}

func waitLinuxWakeOwnerObservation(pidfd, cancelFD int) error {
	pollFDs := []unix.PollFd{
		{Fd: int32(pidfd), Events: unix.POLLIN},
		{Fd: int32(cancelFD), Events: unix.POLLIN},
	}
	for {
		pollFDs[0].Revents = 0
		pollFDs[1].Revents = 0
		_, err := unix.Poll(pollFDs, -1)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("poll retained owner pidfd: %w", err)
		}

		ownerEvents := pollFDs[0].Revents
		if ownerEvents&(unix.POLLIN|unix.POLLHUP) != 0 {
			return nil
		}
		if ownerEvents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			return fmt.Errorf("retained owner pidfd poll failed with events %#x", ownerEvents)
		}

		cancelEvents := pollFDs[1].Revents
		if cancelEvents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			return nil
		}
	}
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
