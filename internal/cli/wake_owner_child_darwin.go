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
			bound = true
			if err := readEnd.Close(); err != nil {
				return err
			}
			// Keep the owner-side stop capability across the later TUI exec,
			// but only after the wake child has spawned so the child cannot
			// inherit its own writer and defeat EOF-based owner-exit cleanup.
			flags, err := unix.FcntlInt(writeEnd.Fd(), unix.F_GETFD, 0)
			if err != nil {
				return fmt.Errorf("inspect authoritative wake owner stop fd: %w", err)
			}
			if _, err := unix.FcntlInt(writeEnd.Fd(), unix.F_SETFD, flags&^unix.FD_CLOEXEC); err != nil {
				return fmt.Errorf("retain authoritative wake owner stop fd across exec: %w", err)
			}
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

func authoritativeWakePrivateStopFromEnv() (<-chan struct{}, func(), error) {
	raw := strings.TrimSpace(os.Getenv(envWakePrivateStopFD))
	if raw == "" {
		return nil, func() {}, nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return nil, nil, fmt.Errorf("%s is invalid", envWakePrivateStopFD)
	}
	file := os.NewFile(uintptr(fd), "authoritative-wake-private-stop")
	if file == nil {
		return nil, nil, fmt.Errorf("%s fd is unavailable", envWakePrivateStopFD)
	}
	stop := make(chan struct{})
	go func() {
		var one [1]byte
		_, _ = file.Read(one[:])
		close(stop)
	}()
	return stop, func() { _ = file.Close() }, nil
}
