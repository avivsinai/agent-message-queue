//go:build windows

package adapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

type platformProcessSpawner struct{}

func (platformProcessSpawner) Start(_ context.Context, spec processSpec) (startedProcess, error) {
	log, err := os.OpenFile(spec.LogPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = log.Close() }()

	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = stdinReader.Close() }()

	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Stdin = stdinReader
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		_ = stdinWriter.Close()
		return nil, err
	}
	_ = stdinReader.Close()

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		_ = stdinWriter.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("create process job: %w", err)
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err == nil {
		err = windows.AssignProcessToJobObject(job, process)
		_ = windows.CloseHandle(process)
	}
	if err != nil {
		_ = stdinWriter.Close()
		_ = windows.CloseHandle(job)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("assign process job: %w", err)
	}

	// Keep the child blocked on stdin until it belongs to the job. If setup
	// fails above, no payload can have landed and the inject remains replayable.
	if _, err := io.Copy(stdinWriter, bytes.NewReader(spec.Stdin)); err != nil {
		_ = stdinWriter.Close()
		_ = windows.TerminateJobObject(job, 1)
		_ = cmd.Wait()
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("write child stdin: %w", err)
	}
	if err := stdinWriter.Close(); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		_ = cmd.Wait()
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("close child stdin: %w", err)
	}

	return &windowsStartedProcess{cmd: cmd, job: job}, nil
}

type windowsStartedProcess struct {
	cmd  *exec.Cmd
	mu   sync.Mutex
	job  windows.Handle
	once sync.Once
}

func (p *windowsStartedProcess) PID() int { return p.cmd.Process.Pid }

func (p *windowsStartedProcess) Wait() error {
	err := p.cmd.Wait()
	p.closeJob()
	return err
}

func (p *windowsStartedProcess) KillGroup() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	job := p.job
	if job == 0 {
		return nil
	}
	if err := windows.TerminateJobObject(job, 1); err != nil {
		if killErr := p.cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return errors.Join(err, killErr)
		}
	}
	return nil
}

func (p *windowsStartedProcess) closeJob() {
	p.once.Do(func() {
		p.mu.Lock()
		job := p.job
		p.job = 0
		p.mu.Unlock()
		if job != 0 {
			_ = windows.CloseHandle(job)
		}
	})
}

func pidAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return false, nil
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return true, nil
		}
		return false, fmt.Errorf("open process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	status, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false, fmt.Errorf("wait process: %w", err)
	}
	switch status {
	case uint32(windows.WAIT_TIMEOUT):
		return true, nil
	case windows.WAIT_OBJECT_0:
		return false, nil
	default:
		return false, fmt.Errorf("wait process returned status %#x", status)
	}
}

func tryFlockExclusive(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	err = windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		^uint32(0),
		^uint32(0),
		&overlapped,
	)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errInjectBusy
		}
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, ^uint32(0), ^uint32(0), &overlapped)
		_ = f.Close()
	}, nil
}
