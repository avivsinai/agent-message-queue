//go:build darwin || linux

// Package terminalrelay preserves terminal job control across a private PTY.
package terminalrelay

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// HelperCommand is private process plumbing, dispatched before normal CLI work.
const HelperCommand = "__terminal-relay"

type event struct {
	State string
	Code  int
	Error string
}

// RunHelper owns the PTY session and is the sole reaper of its foreground
// target. Keeping its parent in the same session makes the target group
// non-orphaned, so the kernel can deliver terminal stop signals normally.
// The caller exits with the returned status.
func RunHelper(args []string) int {
	control := os.NewFile(3, "relay-control")
	status := os.NewFile(4, "relay-status")
	defer func() { _ = control.Close() }()
	defer func() { _ = status.Close() }()
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	encoder := json.NewEncoder(status)
	if len(args) < 2 {
		_ = encoder.Encode(event{Error: "incomplete terminal relay invocation"})
		return 1
	}
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	defer signal.Stop(signals)
	target := exec.Command(args[0], args[2:]...)
	target.Args[0] = args[1]
	target.Stdin, target.Stdout, target.Stderr = os.Stdin, os.Stdout, os.Stderr
	target.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Foreground: true, Ctty: 0}
	if err := target.Start(); err != nil {
		_ = encoder.Encode(event{Error: fmt.Sprintf("start relay target: %v", err)})
		return 1
	}
	defer func() { _ = target.Process.Release() }()
	pid := target.Process.Pid
	sendSignal := func(sig syscall.Signal) { _ = syscall.Kill(-pid, sig) }
	commands := make(chan syscall.Signal)
	go func() {
		defer close(commands)
		decoder := json.NewDecoder(control)
		for {
			var sig syscall.Signal
			if decoder.Decode(&sig) != nil {
				return
			}
			commands <- sig
		}
	}()
	type waitResult struct {
		status syscall.WaitStatus
		err    error
	}
	waits := make(chan waitResult)
	go func() {
		for {
			var ws syscall.WaitStatus
			_, err := syscall.Wait4(pid, &ws, syscall.WUNTRACED|syscall.WCONTINUED, nil)
			if err == syscall.EINTR {
				continue
			}
			waits <- waitResult{ws, err}
			if err != nil || ws.Exited() || ws.Signaled() {
				return
			}
		}
	}()
	var deadline <-chan time.Time
	var timer *time.Timer
	leaderDone, cleaned, targetCode := false, false, 1
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	shutdown := func() {
		if timer != nil {
			return
		}
		sendSignal(syscall.SIGTERM)
		sendSignal(syscall.SIGCONT)
		timer = time.NewTimer(300 * time.Millisecond)
		deadline = timer.C
	}
	for {
		select {
		case sig, ok := <-commands:
			if !ok {
				commands = nil
				shutdown()
			} else {
				if sig == 0 {
					if leaderDone && timer == nil {
						return targetCode
					}
				} else {
					sendSignal(sig)
				}
			}
		case sig := <-signals:
			sendSignal(sig.(syscall.Signal))
		case <-deadline:
			sendSignal(syscall.SIGKILL)
			deadline = nil
			cleaned = true
			if leaderDone {
				return targetCode
			}
		case result := <-waits:
			if result.err != nil {
				_ = encoder.Encode(event{Error: fmt.Sprintf("wait for relay target: %v", result.err)})
				waits = nil
				leaderDone = true
				shutdown()
				if cleaned {
					return targetCode
				}
				continue
			}
			ws := result.status
			update := event{}
			switch {
			case waitStopped(ws):
				update.State = "stopped"
			case waitContinued(ws):
				update.State = "continued"
			case ws.Exited():
				update.State, update.Code = "exit", ws.ExitStatus()
			case ws.Signaled():
				update.State, update.Code = "exit", 128+int(ws.Signal())
			}
			if err := encoder.Encode(update); err != nil {
				shutdown()
			}
			if update.State == "exit" {
				waits = nil
				leaderDone, targetCode = true, update.Code
				if cleaned {
					return targetCode
				}
			}
		}
	}
}
