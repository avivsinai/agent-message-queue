//go:build darwin

package cli

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

type darwinWakeOwnerObservationCapability struct {
	mu            sync.Mutex
	kqueueFD      int
	cancelReadFD  int
	cancelWriteFD int
}

var writeDarwinWakeOwnerObservationCancel = unix.Write

func observeAuthoritativeWakeOwnerPlatform(owner wakeOwner) (wakeOwnerObservation, error) {
	capability, err := openDarwinWakeOwnerObservationCapability(owner.PID)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return deadWakeOwnerObservation("owner process is not running"), nil
		}
		return wakeOwnerObservation{
			State:  wakeOwnerUnknown,
			Reason: fmt.Sprintf("owner process kqueue unavailable: %v", err),
		}, fmt.Errorf("observe owner process %d with kqueue: %w", owner.PID, err)
	}

	first := inspectWakeProcess(owner.PID)
	firstSessionID, firstSessionErr := getWakeProcessSID(owner.PID)
	second := inspectWakeProcess(owner.PID)
	secondSessionID, secondSessionErr := getWakeProcessSID(owner.PID)
	state, reason := classifyStableAuthoritativeWakeOwner(
		owner,
		first, firstSessionID, firstSessionErr,
		second, secondSessionID, secondSessionErr,
	)

	exited, drainErr := drainDarwinWakeOwnerObservation(capability, owner.PID)
	if drainErr != nil {
		closeErr := capability.close()
		return wakeOwnerObservation{
				State:  wakeOwnerUnknown,
				Reason: fmt.Sprintf("owner process kqueue drain failed: %v", drainErr),
			}, errors.Join(
				fmt.Errorf("drain owner process %d kqueue before observation: %w", owner.PID, drainErr),
				closeErr,
			)
	}
	if exited || state == wakeOwnerDead {
		closeErr := capability.close()
		if closeErr != nil {
			return wakeOwnerObservation{
				State:  wakeOwnerUnknown,
				Reason: fmt.Sprintf("close dead owner process kqueue: %v", closeErr),
			}, closeErr
		}
		if exited {
			reason = "owner process is not running"
		} else if reason == "" {
			reason = "owner process is not running"
		}
		return deadWakeOwnerObservation(reason), nil
	}
	if state != wakeOwnerSame {
		closeErr := capability.close()
		unknownErr := fmt.Errorf("owner process identity is %s: %s", state, reason)
		return wakeOwnerObservation{
			State:  wakeOwnerUnknown,
			Reason: reason,
		}, errors.Join(unknownErr, closeErr)
	}

	monitor := newWakeOwnerObservationMonitor(capability.cancel)
	go func() {
		monitorErr := monitorDarwinWakeOwnerObservation(capability, owner.PID)
		monitor.finish(errors.Join(monitorErr, capability.close()))
	}()
	return wakeOwnerObservation{
		State:   wakeOwnerSame,
		Reason:  reason,
		monitor: monitor,
	}, nil
}

func openDarwinWakeOwnerObservationCapability(pid int) (*darwinWakeOwnerObservationCapability, error) {
	kqueueFD, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("create owner process kqueue: %w", err)
	}
	capability := &darwinWakeOwnerObservationCapability{
		kqueueFD:      kqueueFD,
		cancelReadFD:  -1,
		cancelWriteFD: -1,
	}
	cleanupOnError := func(cause error) (*darwinWakeOwnerObservationCapability, error) {
		return nil, errors.Join(cause, capability.close())
	}
	if err := setDarwinWakeOwnerObservationCloseOnExec(kqueueFD, "owner process kqueue"); err != nil {
		return cleanupOnError(err)
	}

	cancelFDs := make([]int, 2)
	if err := unix.Pipe(cancelFDs); err != nil {
		return cleanupOnError(fmt.Errorf("create owner observation cancellation pipe: %w", err))
	}
	capability.cancelReadFD = cancelFDs[0]
	capability.cancelWriteFD = cancelFDs[1]
	for _, descriptor := range []struct {
		fd    int
		label string
	}{
		{fd: capability.cancelReadFD, label: "owner observation cancellation read descriptor"},
		{fd: capability.cancelWriteFD, label: "owner observation cancellation write descriptor"},
	} {
		if err := setDarwinWakeOwnerObservationCloseOnExec(descriptor.fd, descriptor.label); err != nil {
			return cleanupOnError(err)
		}
	}

	changes := []unix.Kevent_t{
		{
			Ident:  uint64(pid),
			Filter: unix.EVFILT_PROC,
			Flags:  unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT,
			Fflags: unix.NOTE_EXIT,
		},
		{
			Ident:  uint64(capability.cancelReadFD),
			Filter: unix.EVFILT_READ,
			Flags:  unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT,
		},
	}
	if _, err := unix.Kevent(capability.kqueueFD, changes, nil, nil); err != nil {
		return cleanupOnError(fmt.Errorf("register owner process and cancellation kqueue filters: %w", err))
	}
	return capability, nil
}

func setDarwinWakeOwnerObservationCloseOnExec(fd int, label string) error {
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf("inspect %s flags: %w", label, err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
			return fmt.Errorf("set close-on-exec on %s: %w", label, err)
		}
		flags, err = unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil {
			return fmt.Errorf("verify %s flags: %w", label, err)
		}
	}
	if flags&unix.FD_CLOEXEC == 0 {
		return fmt.Errorf("%s is not close-on-exec", label)
	}
	return nil
}

func drainDarwinWakeOwnerObservation(
	capability *darwinWakeOwnerObservationCapability,
	pid int,
) (bool, error) {
	events := make([]unix.Kevent_t, 2)
	timeout := unix.Timespec{}
	for {
		count, err := unix.Kevent(capability.kqueueFD, nil, events, &timeout)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return false, err
		}
		exited, terminalErr, cancelled := classifyDarwinWakeOwnerObservationEvents(
			events[:count],
			pid,
			capability.cancelReadFD,
		)
		switch {
		case exited:
			return true, nil
		case terminalErr != nil:
			return false, terminalErr
		case cancelled:
			return false, fmt.Errorf("owner observation cancelled before monitor start")
		default:
			return false, nil
		}
	}
}

func monitorDarwinWakeOwnerObservation(
	capability *darwinWakeOwnerObservationCapability,
	pid int,
) error {
	events := make([]unix.Kevent_t, 2)
	for {
		count, err := unix.Kevent(capability.kqueueFD, nil, events, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("wait for owner process exit: %w", err)
		}
		exited, terminalErr, cancelled := classifyDarwinWakeOwnerObservationEvents(
			events[:count],
			pid,
			capability.cancelReadFD,
		)
		switch {
		case exited:
			return nil
		case terminalErr != nil:
			return terminalErr
		case cancelled:
			return nil
		case count == 0:
			continue
		default:
			return fmt.Errorf("owner process kqueue returned no recognized terminal event")
		}
	}
}

func classifyDarwinWakeOwnerObservationEvents(
	events []unix.Kevent_t,
	pid int,
	cancelReadFD int,
) (exited bool, terminalErr error, cancelled bool) {
	// An exact process-exit event wins over a concurrent cancellation or error.
	for _, event := range events {
		if event.Filter == unix.EVFILT_PROC &&
			event.Ident == uint64(pid) &&
			event.Fflags&unix.NOTE_EXIT != 0 {
			return true, nil, false
		}
	}
	for _, event := range events {
		if event.Flags&unix.EV_ERROR != 0 && event.Data != 0 {
			return false, fmt.Errorf(
				"owner process kqueue event failed: %w",
				syscall.Errno(event.Data),
			), false
		}
	}
	for _, event := range events {
		if event.Filter == unix.EVFILT_READ && event.Ident == uint64(cancelReadFD) {
			return false, nil, true
		}
	}
	return false, nil, false
}

func (capability *darwinWakeOwnerObservationCapability) cancel() error {
	if capability == nil {
		return nil
	}
	capability.mu.Lock()
	defer capability.mu.Unlock()
	if capability.cancelWriteFD < 0 {
		return nil
	}
	for {
		count, err := writeDarwinWakeOwnerObservationCancel(
			capability.cancelWriteFD,
			[]byte{1},
		)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.EPIPE) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("signal owner observation cancellation: %w", err)
		}
		switch count {
		case 1:
			return nil
		case 0:
			continue
		default:
			return fmt.Errorf(
				"signal owner observation cancellation wrote %d bytes, want 1",
				count,
			)
		}
	}
}

func (capability *darwinWakeOwnerObservationCapability) close() error {
	if capability == nil {
		return nil
	}
	capability.mu.Lock()
	kqueueFD := capability.kqueueFD
	cancelReadFD := capability.cancelReadFD
	cancelWriteFD := capability.cancelWriteFD
	capability.kqueueFD = -1
	capability.cancelReadFD = -1
	capability.cancelWriteFD = -1
	capability.mu.Unlock()

	var errs []error
	for _, descriptor := range []struct {
		fd    int
		label string
	}{
		{fd: cancelWriteFD, label: "owner observation cancellation write descriptor"},
		{fd: cancelReadFD, label: "owner observation cancellation read descriptor"},
		{fd: kqueueFD, label: "owner process kqueue"},
	} {
		if descriptor.fd < 0 {
			continue
		}
		if err := unix.Close(descriptor.fd); err != nil && !errors.Is(err, syscall.EBADF) {
			errs = append(errs, fmt.Errorf("close %s: %w", descriptor.label, err))
		}
	}
	return errors.Join(errs...)
}

func captureAuthoritativeCurrentWakeOwnerPlatform() (wakeOwner, error) {
	pid := os.Getpid()
	first := inspectWakeProcess(pid)
	firstSessionID, firstSessionErr := getWakeProcessSID(pid)
	if !first.Running || firstSessionErr != nil {
		return wakeOwner{}, fmt.Errorf("current process identity is unavailable")
	}
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
		return wakeOwner{}, fmt.Errorf("current process identity is %s: %s", state, reason)
	}
	return owner, nil
}
