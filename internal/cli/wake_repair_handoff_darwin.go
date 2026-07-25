//go:build darwin

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

const (
	wakeRepairChildControlStop   = "STOP"
	wakeRepairChildControlDetach = "DETACH"
)

var (
	writeWakeRepairDarwinChildControl = writeWakeRepairChildControl
	closeWakeRepairDarwinChildControl = func(file *os.File) error { return file.Close() }
	killWakeRepairDarwinChild         = func(process *os.Process) error { return process.Kill() }
)

func prepareWakeRepairChildCapabilityPlatform(cmd *exec.Cmd) (*wakeRepairChildCapability, error) {
	if cmd == nil {
		return nil, fmt.Errorf("wake repair child command is missing")
	}
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create wake repair child control pipe: %w", err)
	}
	childFD := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, readEnd)
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = setEnvVar(unsetEnvVar(cmd.Env, envWakeRepairChildControlFD),
		envWakeRepairChildControlFD, strconv.Itoa(childFD))

	var mutex sync.Mutex
	bound := false
	detached := false
	stopped := false
	writeClosed := false
	var process *os.Process
	closeWrite := func() error {
		if writeClosed {
			return nil
		}
		if err := closeWakeRepairDarwinChildControl(writeEnd); err != nil {
			return err
		}
		writeClosed = true
		return nil
	}
	return &wakeRepairChildCapability{
		bind: func(child *os.Process) error {
			mutex.Lock()
			defer mutex.Unlock()
			if child == nil || child.Pid <= 0 {
				return fmt.Errorf("wake repair child process is missing")
			}
			if bound || detached || stopped {
				return fmt.Errorf("wake repair child control capability is already bound")
			}
			bound = true
			process = child
			if err := readEnd.Close(); err != nil {
				return fmt.Errorf("close parent copy of wake repair child control fd: %w", err)
			}
			return nil
		},
		stop: func() error {
			mutex.Lock()
			defer mutex.Unlock()
			if !bound || detached || stopped || process == nil {
				return fmt.Errorf("wake repair child control capability is unavailable")
			}
			if !writeClosed {
				_ = writeWakeRepairDarwinChildControl(writeEnd, wakeRepairChildControlStop)
				_ = closeWrite()
			}
			if err := killWakeRepairDarwinChild(process); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return fmt.Errorf("kill exact wake repair child: %w", err)
			}
			stopped = true
			return nil
		},
		detach: func() error {
			// The caller is responsible for invoking this only after the exact
			// admitted acknowledgement has been received.
			mutex.Lock()
			defer mutex.Unlock()
			if !bound || detached || stopped || writeClosed {
				return fmt.Errorf("wake repair child control capability is unavailable")
			}
			if err := writeWakeRepairDarwinChildControl(writeEnd, wakeRepairChildControlDetach); err != nil {
				return err
			}
			// A complete DETACH frame is observable by the child immediately.
			// Commit detachment before closing the writer: a later close error
			// cannot restore exact stop authority after the child may already
			// have disarmed STOP/EOF.
			detached = true
			// Closing is best-effort after that commit. Close may be retried by
			// capability cleanup, but its error cannot make DETACH fail.
			_ = closeWrite()
			return nil
		},
		close: func() error {
			mutex.Lock()
			defer mutex.Unlock()
			var readErr error
			if !bound {
				readErr = readEnd.Close()
			}
			// Closing without DETACH deliberately produces EOF in the child,
			// which is fail-closed before admission.
			writeErr := closeWrite()
			return errors.Join(readErr, writeErr)
		},
	}, nil
}

func writeWakeRepairChildControl(writer io.Writer, command string) error {
	if command != wakeRepairChildControlStop && command != wakeRepairChildControlDetach {
		return fmt.Errorf("invalid wake repair child control %q", command)
	}
	data := []byte(command + "\n")
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return fmt.Errorf("write wake repair child control: %w", err)
		}
		if n <= 0 {
			return fmt.Errorf("write wake repair child control: %w", io.ErrShortWrite)
		}
		data = data[n:]
	}
	return nil
}

func wakeRepairChildStopFromEnv() (<-chan struct{}, func(), error) {
	bootstrap, err := captureAndScrubWakeRepairBootstrapEnv()
	if err != nil {
		return nil, nil, err
	}
	handoffDescriptors, err := parseWakeRepairChildHandoffDescriptors(bootstrap)
	if err != nil {
		return nil, nil, err
	}
	if handoffDescriptors.present {
		return nil, nil, fmt.Errorf(
			"wake repair handoff descriptors are not valid without the complete bootstrap transaction",
		)
	}
	ownerStopDescriptor, err := parseAuthoritativeWakePrivateStopDescriptor(bootstrap)
	if err != nil {
		return nil, nil, err
	}
	if ownerStopDescriptor.present {
		return nil, nil, fmt.Errorf(
			"%s is not valid without the complete private bootstrap transaction",
			ownerStopDescriptor.name,
		)
	}
	return wakeRepairChildStopFromBootstrap(bootstrap)
}

func wakeRepairBootstrapPlatformEnvNames() []string {
	return []string{envWakeRepairChildControlFD}
}

func parseWakeRepairChildStopDescriptor(
	bootstrap wakeRepairBootstrapEnv,
) (wakeRepairChildStopDescriptor, error) {
	raw := bootstrap.value(envWakeRepairChildControlFD)
	if raw == "" {
		return wakeRepairChildStopDescriptor{}, nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return wakeRepairChildStopDescriptor{}, fmt.Errorf(
			"%s is invalid",
			envWakeRepairChildControlFD,
		)
	}
	return wakeRepairChildStopDescriptor{
		fd:      fd,
		name:    envWakeRepairChildControlFD,
		present: true,
	}, nil
}

func prepareWakeRepairChildStopDescriptor(descriptor wakeRepairChildStopDescriptor) error {
	if !descriptor.present {
		return nil
	}
	// os/exec obtains ExtraFiles through File.Fd, which deliberately restores
	// blocking mode. Restore nonblocking mode before os.NewFile so Go registers
	// the inherited pipe with its poller; then a concurrent Close interrupts the
	// startup read and cleanup can wait for the watcher without deadlocking.
	if err := unix.SetNonblock(descriptor.fd, true); err != nil {
		return fmt.Errorf("make wake repair child control fd nonblocking: %w", err)
	}
	if err := setWakeRepairFDCloseOnExec(descriptor.fd, "child control"); err != nil {
		return err
	}
	return nil
}

func validateWakeRepairChildPlatformDescriptorTuple(
	handoff wakeRepairChildHandoffDescriptors,
	stop wakeRepairChildStopDescriptor,
) error {
	if handoff.present != stop.present {
		return fmt.Errorf("wake repair bootstrap descriptors are incomplete")
	}
	return nil
}

func adoptWakeRepairChildStopDescriptor(
	descriptor wakeRepairChildStopDescriptor,
) (<-chan struct{}, func(), error) {
	if !descriptor.present {
		return nil, func() {}, nil
	}
	file := os.NewFile(uintptr(descriptor.fd), "wake-repair-child-control")
	if file == nil {
		_ = unix.Close(descriptor.fd)
		return nil, nil, fmt.Errorf("%s fd is unavailable", envWakeRepairChildControlFD)
	}
	stop, finished := watchWakeRepairDarwinChildControl(file)
	cleanup := func() {
		_ = file.Close()
		<-finished
	}
	return stop, cleanup, nil
}

func wakeRepairChildStopFromBootstrap(
	bootstrap wakeRepairBootstrapEnv,
) (<-chan struct{}, func(), error) {
	descriptor, err := parseWakeRepairChildStopDescriptor(bootstrap)
	if err != nil {
		return nil, nil, err
	}
	if err := prepareWakeRepairChildStopDescriptor(descriptor); err != nil {
		if descriptor.present {
			_ = unix.Close(descriptor.fd)
		}
		return nil, nil, err
	}
	return adoptWakeRepairChildStopDescriptor(descriptor)
}

func watchWakeRepairDarwinChildControl(file *os.File) (<-chan struct{}, <-chan struct{}) {
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer func() { _ = file.Close() }()
		reader := bufio.NewReaderSize(file, 32)
		line, err := reader.ReadString('\n')
		switch {
		case err == nil && line == wakeRepairChildControlDetach+"\n":
			return
		case err == nil && line == wakeRepairChildControlStop+"\n":
			close(stop)
			return
		case errors.Is(err, io.EOF):
			// EOF is authoritative only during startup, before a validated
			// DETACH. Descendant-held descriptors are irrelevant after detach.
			close(stop)
			return
		default:
			// Malformed, partial, and failed control reads stop the unadmitted
			// child rather than allowing it to scan or inject.
			close(stop)
		}
	}()
	return stop, finished
}
