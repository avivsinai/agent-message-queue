//go:build darwin

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

func prepareAuthoritativeWakeChildPlatform(cmd *exec.Cmd) (*authoritativeWakeChildCapability, error) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create authoritative wake child stop pipe: %w", err)
	}
	childFD := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, readEnd)
	cmd.Env = setEnvVar(unsetEnvVar(cmd.Env, envWakePrivateStopFD), envWakePrivateStopFD, strconv.Itoa(childFD))

	var stopOnce sync.Once
	var stopErr error
	bound := false
	return &authoritativeWakeChildCapability{
		bind: func(process *os.Process) error {
			if process == nil || process.Pid <= 0 {
				return fmt.Errorf("authoritative wake child process is missing")
			}
			if err := validateDarwinWakeOwnerStartupRollbackFD(writeEnd); err != nil {
				return err
			}
			if err := readEnd.Close(); err != nil {
				return err
			}
			bound = true
			return nil
		},
		stop: func() error {
			stopOnce.Do(func() {
				if _, err := writeEnd.Write([]byte{1}); err != nil && !errors.Is(err, unix.EPIPE) {
					stopErr = err
				}
				if err := writeEnd.Close(); stopErr == nil {
					stopErr = err
				}
			})
			return stopErr
		},
		close: func() error {
			var first error
			if !bound {
				first = readEnd.Close()
			}
			stopOnce.Do(func() { stopErr = writeEnd.Close() })
			if first == nil {
				first = stopErr
			}
			return first
		},
	}, nil
}

func validateDarwinWakeOwnerStartupRollbackFD(writeEnd *os.File) error {
	flags, err := unix.FcntlInt(writeEnd.Fd(), unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf("inspect authoritative wake owner startup rollback fd: %w", err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		return fmt.Errorf("authoritative wake owner startup rollback fd is not close-on-exec")
	}
	return nil
}

func authoritativeWakePrivateStopFromEnv() (<-chan struct{}, func(), error) {
	bootstrap, err := captureAndScrubWakeRepairBootstrapEnv()
	if err != nil {
		return nil, nil, err
	}
	handoff, err := parseWakeRepairChildHandoffDescriptors(bootstrap)
	if err != nil {
		return nil, nil, err
	}
	repairStop, err := parseWakeRepairChildStopDescriptor(bootstrap)
	if err != nil {
		return nil, nil, err
	}
	if handoff.present || repairStop.present {
		return nil, nil, fmt.Errorf(
			"repair descriptors are not valid without the complete private bootstrap transaction",
		)
	}
	descriptor, err := parseAuthoritativeWakePrivateStopDescriptor(bootstrap)
	if err != nil {
		return nil, nil, err
	}
	if err := prepareAuthoritativeWakePrivateStopDescriptor(descriptor); err != nil {
		if descriptor.present {
			_ = unix.Close(descriptor.fd)
		}
		return nil, nil, err
	}
	return adoptAuthoritativeWakePrivateStopDescriptor(descriptor)
}

func parseAuthoritativeWakePrivateStopDescriptor(
	bootstrap wakeRepairBootstrapEnv,
) (wakePrivateStopDescriptor, error) {
	raw := bootstrap.value(envWakePrivateStopFD)
	if raw == "" {
		return wakePrivateStopDescriptor{}, nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return wakePrivateStopDescriptor{}, fmt.Errorf("%s is invalid", envWakePrivateStopFD)
	}
	return wakePrivateStopDescriptor{
		fd:      fd,
		name:    envWakePrivateStopFD,
		present: true,
	}, nil
}

func prepareAuthoritativeWakePrivateStopDescriptor(descriptor wakePrivateStopDescriptor) error {
	if !descriptor.present {
		return nil
	}
	// The inherited descriptor arrives as a raw numeric fd. Make it pollable
	// before os.NewFile adopts it so cleanup can interrupt and join the startup
	// read without relying on Darwin closing a blocking syscall from another
	// goroutine.
	if err := unix.SetNonblock(descriptor.fd, true); err != nil {
		return fmt.Errorf("make %s fd nonblocking: %w", envWakePrivateStopFD, err)
	}
	return sealDarwinWakePrivateStopFDNumber(uintptr(descriptor.fd))
}

func adoptAuthoritativeWakePrivateStopDescriptor(
	descriptor wakePrivateStopDescriptor,
) (<-chan struct{}, func(), error) {
	if !descriptor.present {
		return nil, func() {}, nil
	}
	file := os.NewFile(uintptr(descriptor.fd), "authoritative-wake-private-stop")
	if file == nil {
		_ = unix.Close(descriptor.fd)
		return nil, nil, fmt.Errorf("%s fd is unavailable", envWakePrivateStopFD)
	}
	stop, cleanup := watchAuthoritativeWakePrivateStop(file)
	return stop, cleanup, nil
}

func sealDarwinWakePrivateStopFDNumber(fd uintptr) error {
	flags, err := unix.FcntlInt(fd, unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf("inspect %s fd flags: %w", envWakePrivateStopFD, err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		if _, err := unix.FcntlInt(fd, unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
			return fmt.Errorf("seal %s fd across exec: %w", envWakePrivateStopFD, err)
		}
	}
	flags, err = unix.FcntlInt(fd, unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf("verify %s fd flags: %w", envWakePrivateStopFD, err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		return fmt.Errorf("%s fd is not close-on-exec", envWakePrivateStopFD)
	}
	return nil
}

func watchAuthoritativeWakePrivateStop(file *os.File) (<-chan struct{}, func()) {
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer func() { _ = file.Close() }()
		var one [1]byte
		if count, _ := file.Read(one[:]); count == 1 {
			close(stop)
		}
	}()
	var cleanupOnce sync.Once
	return stop, func() {
		cleanupOnce.Do(func() {
			_ = file.Close()
			<-finished
		})
	}
}
