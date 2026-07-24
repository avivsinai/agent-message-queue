//go:build linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

var (
	linuxPidfdOpen       = unix.PidfdOpen
	linuxPidfdSendSignal = unix.PidfdSendSignal
	linuxPidfdClose      = unix.Close
	linuxPidfdPoll       = pollLinuxPidfd
)

// readWakeLockMetadata reads one exact lock generation without consulting the
// process table. Linux orphan retirement uses this to acquire a pidfd before
// the first PID-based identity inspection of the locked generation.
func readWakeLockMetadata(root, me string) wakeLockInspection {
	lockPath := filepath.Join(fsq.AgentBase(root, me), ".wake.lock")
	return readWakeLockMetadataWithReader(root, me, lockPath, func() ([]byte, os.FileInfo, error) {
		return readWakeLockFileWithInfo(lockPath)
	})
}

func terminateAndRemoveOrphanedWakeLock(inspection wakeLockInspection) (bool, error) {
	var locked wakeLockInspection
	pidfd := -1
	provenGone := false
	if err := withWakeLifecycleGuard(inspection.Root, inspection.Agent, func() error {
		locked = readWakeLockMetadata(inspection.Root, inspection.Agent)
		if !sameWakeLockGeneration(inspection, locked) {
			return nil
		}
		if err := validateWakeLockOwnerlessMutation(locked); err != nil {
			return err
		}

		// Acquire the stable process capability before any PID-based identity
		// inspection of this locked generation. From here onward, signaling and
		// exit detection use only this descriptor.
		fd, err := linuxPidfdOpen(locked.PID, 0)
		if err != nil {
			if errors.Is(err, syscall.ESRCH) {
				provenGone = true
				return removeWakeLockIfUnchangedGuarded(locked)
			}
			return fmt.Errorf("pidfd_open wake process %d: %w", locked.PID, err)
		}
		pidfd = fd

		locked.Process = inspectWakeProcess(locked.PID)
		classifyWakeLock(locked.Root, locked.Agent, &locked)
		if !sameWakeLockInspection(inspection, locked) || !locked.IdentityConfirmed {
			return nil
		}
		return nil
	}); err != nil {
		if pidfd >= 0 {
			_ = linuxPidfdClose(pidfd)
		}
		return false, err
	}
	if provenGone {
		return true, nil
	}
	if pidfd < 0 || !locked.IdentityConfirmed {
		if pidfd >= 0 {
			_ = linuxPidfdClose(pidfd)
		}
		return false, nil
	}
	defer func() { _ = linuxPidfdClose(pidfd) }()

	// Signaling and both waits happen without the lifecycle guard. The retained
	// pidfd cannot retarget a recycled numeric PID.
	if err := terminateWakePidfd(pidfd); err != nil {
		return false, err
	}

	removed := false
	err := withWakeLifecycleGuard(inspection.Root, inspection.Agent, func() error {
		current := inspectWakeLock(inspection.Root, inspection.Agent)
		if !sameWakeLockGeneration(locked, current) {
			return nil
		}
		if err := validateWakeLockStaleRemoval(current); err != nil {
			return err
		}
		if err := removeWakeLockIfUnchangedGuarded(current); err != nil {
			return err
		}
		removed = true
		return nil
	})
	return removed, err
}

func terminateWakePidfd(pidfd int) error {
	if err := linuxPidfdSendSignal(pidfd, unix.SIGTERM, nil, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("pidfd_send_signal SIGTERM: %w", err)
	}
	exited, err := linuxPidfdPoll(pidfd, wakeTerminateGrace)
	if err != nil {
		return fmt.Errorf("poll pidfd after SIGTERM: %w", err)
	}
	if exited {
		return nil
	}
	if err := linuxPidfdSendSignal(pidfd, unix.SIGKILL, nil, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("pidfd_send_signal SIGKILL: %w", err)
	}
	exited, err = linuxPidfdPoll(pidfd, wakeTerminateGrace)
	if err != nil {
		return fmt.Errorf("poll pidfd after SIGKILL: %w", err)
	}
	if !exited {
		return fmt.Errorf("wake process still alive after SIGKILL")
	}
	return nil
}

func pollLinuxPidfd(pidfd int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		timeoutMillis := int((remaining + time.Millisecond - 1) / time.Millisecond)
		fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		ready, err := unix.Poll(fds, timeoutMillis)
		if errors.Is(err, syscall.EINTR) {
			if time.Now().Before(deadline) {
				continue
			}
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if ready == 0 {
			return false, nil
		}
		revents := fds[0].Revents
		if revents&unix.POLLNVAL != 0 {
			return false, fmt.Errorf("pidfd became invalid")
		}
		if revents&(unix.POLLIN|unix.POLLHUP) != 0 {
			return true, nil
		}
		if revents&unix.POLLERR != 0 {
			return false, fmt.Errorf("pidfd poll reported an error")
		}
	}
}
