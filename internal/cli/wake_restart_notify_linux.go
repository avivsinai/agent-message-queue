//go:build linux

package cli

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func configureWakeRestartAdvertisementPlatform(lock *wakeLock, _, _ string) {
	if lock == nil {
		return
	}
	lock.ResumeSignal = wakeResumeSignalUSR1
	lock.ControlSocket = ""
}

func validateWakeRestartTransportPlatform(lock wakeLock, _, _ string) error {
	if lock.ResumeSignal != wakeResumeSignalUSR1 {
		return fmt.Errorf("linux wake restart requires direct SIGUSR1 delivery")
	}
	if lock.ControlSocket != "" {
		return fmt.Errorf("linux wake restart refuses a control socket")
	}
	return nil
}

func notifyWakeRestartPlatform(
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
	record wakeRestartRecord,
) error {
	if agentDir == nil {
		return fmt.Errorf("wake restart agent directory capability is missing")
	}

	pidfd := -1
	defer func() {
		if pidfd >= 0 {
			_ = linuxPidfdClose(pidfd)
		}
	}()

	return withExistingWakeMutationScopeNoWaitInDir(
		agentDir,
		func(scope *wakeMutationScope) error {
			dirfd, _, err := scope.location()
			if err != nil {
				return err
			}
			metadata := readWakeLockMetadataAt(
				dirfd,
				agentDir,
				expected.Root,
				expected.Agent,
			)
			if !sameWakeLockGeneration(expected, metadata) {
				return fmt.Errorf("wake changed before restart pidfd acquisition")
			}

			fd, err := linuxPidfdOpen(metadata.PID, 0)
			if err != nil {
				return fmt.Errorf("pidfd_open wake restart process %d: %w", metadata.PID, err)
			}
			pidfd = fd

			current := inspectWakeLockAt(
				dirfd,
				agentDir,
				expected.Root,
				expected.Agent,
			)
			if !sameWakeLockInspection(expected, current) || !current.IdentityConfirmed {
				return fmt.Errorf("wake changed before restart signal")
			}
			if err := validateWakeRestartTransportPlatform(
				current.Lock,
				expected.Root,
				expected.Agent,
			); err != nil {
				return err
			}

			observed, exists, err := readWakeRestartRecordAt(dirfd, agentDir)
			if err != nil {
				return err
			}
			if !exists || record.Status != wakeRestartPending ||
				observed.Status != wakeRestartPending ||
				observed.RequestID != record.RequestID ||
				observed.Generation != record.Generation ||
				!sameWakeRestartRecord(observed, record) {
				return fmt.Errorf("wake restart request changed before signal")
			}

			return scope.sendPidfdSignal(pidfd, unix.SIGUSR1)
		},
	)
}
