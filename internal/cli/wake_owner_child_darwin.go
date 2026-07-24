//go:build darwin

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
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
	raw := strings.TrimSpace(os.Getenv(envWakePrivateStopFD))
	if raw == "" {
		return nil, func() {}, nil
	}
	if err := os.Unsetenv(envWakePrivateStopFD); err != nil {
		return nil, nil, fmt.Errorf("clear %s after ingestion: %w", envWakePrivateStopFD, err)
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return nil, nil, fmt.Errorf("%s is invalid", envWakePrivateStopFD)
	}
	file := os.NewFile(uintptr(fd), "authoritative-wake-private-stop")
	if file == nil {
		return nil, nil, fmt.Errorf("%s fd is unavailable", envWakePrivateStopFD)
	}
	if err := sealDarwinWakePrivateStopFD(file); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	stop, cleanup := watchAuthoritativeWakePrivateStop(file)
	return stop, cleanup, nil
}

func sealDarwinWakePrivateStopFD(file *os.File) error {
	if file == nil {
		return fmt.Errorf("%s fd is unavailable", envWakePrivateStopFD)
	}
	fd := file.Fd()
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
