//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
	"github.com/fsnotify/fsnotify"
)

type wakeScriptedInboxReader struct {
	readDir    func() ([]os.DirEntry, error)
	readHeader func(string) (format.Header, error)
}

func (reader wakeScriptedInboxReader) ReadDir() ([]os.DirEntry, error) {
	return reader.readDir()
}

func (reader wakeScriptedInboxReader) ReadHeader(name string) (format.Header, error) {
	if reader.readHeader == nil {
		return format.Header{}, os.ErrNotExist
	}
	return reader.readHeader(name)
}

func awaitWakeScan(t *testing.T, scans <-chan time.Time, done <-chan error) time.Time {
	t.Helper()
	select {
	case at := <-scans:
		return at
	case err := <-done:
		t.Fatalf("wake loop exited before inbox scan: %v", err)
	case <-time.After(time.Second):
		t.Fatal("wake loop did not scan inbox")
	}
	return time.Time{}
}

func deliverPartialWakeMessageForTest(t *testing.T, root, me, id string) {
	t.Helper()
	ensureCoopWakeMailboxForTest(t, root, me)
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      id,
			From:    "peer",
			To:      []string{me},
			Thread:  "p2p/peer__" + me,
			Subject: id,
			Created: "2026-07-30T08:00:00Z",
		},
		Body: "body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(t, root, me, id+".md", data); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyNewMessagesForegroundPGRPResumesAtFirstMissingChunk(t *testing.T) {
	tests := []struct {
		name      string
		me        string
		mode      string
		failAt    int
		firstWant []string
		retryWant []string
	}{
		{
			name:      "paste payload",
			me:        "grok",
			mode:      wakeInjectModePaste,
			failAt:    1,
			firstWant: nil,
			retryWant: []string{coopWakeDoorbell, "\r"},
		},
		{
			name:      "paste submit",
			me:        "grok",
			mode:      wakeInjectModePaste,
			failAt:    2,
			firstWant: []string{coopWakeDoorbell},
			retryWant: []string{"\r"},
		},
		{
			name:      "raw codex payload",
			me:        "codex",
			mode:      wakeInjectModeRaw,
			failAt:    1,
			firstWant: nil,
			retryWant: []string{coopWakeDoorbell, "\n", "\r", "\r"},
		},
		{
			name:      "raw codex prelude",
			me:        "codex",
			mode:      wakeInjectModeRaw,
			failAt:    2,
			firstWant: []string{coopWakeDoorbell},
			retryWant: []string{"\n", "\r", "\r"},
		},
		{
			name:      "raw codex first submit",
			me:        "codex",
			mode:      wakeInjectModeRaw,
			failAt:    3,
			firstWant: []string{coopWakeDoorbell, "\n"},
			retryWant: []string{"\r", "\r"},
		},
		{
			name:      "raw codex rescue submit",
			me:        "codex",
			mode:      wakeInjectModeRaw,
			failAt:    4,
			firstWant: []string{coopWakeDoorbell, "\n", "\r"},
			retryWant: []string{"\r"},
		},
		{
			name:      "raw claude first submit",
			me:        "claude",
			mode:      wakeInjectModeRaw,
			failAt:    2,
			firstWant: []string{coopWakeDoorbell},
			retryWant: []string{"\r", "\r"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			deliverPartialWakeMessageForTest(t, root, tc.me, strings.ReplaceAll(tc.name, " ", "-"))
			stubRawInputDrained(t, func(time.Duration, time.Duration) (time.Duration, bool, error) {
				return 0, true, nil
			})
			stubRawInjectSleep(t)

			var writes []string
			guardCalls := 0
			failed := false
			cfg := &wakeConfig{
				root:           root,
				me:             tc.me,
				session:        "session1",
				wakeOwner:      &wakeOwner{},
				injectMode:     tc.mode,
				attentionIsTTY: func() bool { return false },
				beforeTerminalWrite: func() error {
					guardCalls++
					if !failed && guardCalls == tc.failAt {
						failed = true
						return newWakeTerminalForegroundPGRPChangedLoss(101, 202)
					}
					return nil
				},
				terminalWrite: func(text string) error {
					writes = append(writes, text)
					return nil
				},
			}

			err := notifyNewMessages(cfg)
			if !isWakeTerminalForegroundPGRPChanged(err) {
				t.Fatalf("first notify error = %T %v, want foreground-PGRP change", err, err)
			}
			if got := strings.Join(writes, "|"); got != strings.Join(tc.firstWant, "|") {
				t.Fatalf("first writes = %q, want %q", got, strings.Join(tc.firstWant, "|"))
			}
			if cfg.doorbell.attempts != 0 {
				t.Fatalf("attempts after foreground refusal = %d, want 0", cfg.doorbell.attempts)
			}

			writes = nil
			if err := notifyNewMessages(cfg); err != nil {
				t.Fatalf("retry notify: %v", err)
			}
			if got := strings.Join(writes, "|"); got != strings.Join(tc.retryWant, "|") {
				t.Fatalf("retry writes = %q, want %q", got, strings.Join(tc.retryWant, "|"))
			}
			if cfg.doorbell.token != coopWakeDoorbellTokenForTests {
				t.Fatalf("retry token = %q, want preserved token", cfg.doorbell.token)
			}
			if cfg.doorbell.attempts != 1 {
				t.Fatalf("attempts after completed retry = %d, want 1", cfg.doorbell.attempts)
			}
		})
	}
}

func TestNotifyNewMessagesTerminalWritePGRPDoesNotAdvanceResumePhase(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		failAt    int
		firstWant []string
		retryWant []string
	}{
		{
			name:      "paste submit",
			mode:      wakeInjectModePaste,
			failAt:    2,
			firstWant: []string{coopWakeDoorbell},
			retryWant: []string{"\r"},
		},
		{
			name:      "raw prelude",
			mode:      wakeInjectModeRaw,
			failAt:    2,
			firstWant: []string{coopWakeDoorbell},
			retryWant: []string{"\n", "\r", "\r"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			deliverPartialWakeMessageForTest(t, root, "codex", "terminal-write-"+strings.ReplaceAll(tc.name, " ", "-"))
			stubRawInputDrained(t, func(time.Duration, time.Duration) (time.Duration, bool, error) {
				return 0, true, nil
			})
			stubRawInjectSleep(t)

			var writes []string
			writeCalls := 0
			failed := false
			cfg := &wakeConfig{
				root:           root,
				me:             "codex",
				session:        "session1",
				wakeOwner:      &wakeOwner{},
				injectMode:     tc.mode,
				attentionIsTTY: func() bool { return false },
				terminalWrite: func(text string) error {
					writeCalls++
					if !failed && writeCalls == tc.failAt {
						failed = true
						return newWakeTerminalForegroundPGRPChangedLoss(101, 202)
					}
					writes = append(writes, text)
					return nil
				},
			}

			err := notifyNewMessages(cfg)
			if !isWakeTerminalForegroundPGRPChanged(err) {
				t.Fatalf("first notify error = %T %v, want foreground-PGRP change", err, err)
			}
			if got := strings.Join(writes, "|"); got != strings.Join(tc.firstWant, "|") {
				t.Fatalf("first writes = %q, want %q", got, strings.Join(tc.firstWant, "|"))
			}

			writes = nil
			if err := notifyNewMessages(cfg); err != nil {
				t.Fatalf("retry notify: %v", err)
			}
			if got := strings.Join(writes, "|"); got != strings.Join(tc.retryWant, "|") {
				t.Fatalf("retry writes = %q, want %q", got, strings.Join(tc.retryWant, "|"))
			}
		})
	}
}

func TestNotifyNewMessagesRawRescuePGRPAfterDrainInspectionErrorResumesRescueOnly(t *testing.T) {
	root := secureTempDirForTest(t)
	deliverPartialWakeMessageForTest(t, root, "codex", "rescue-after-drain-error")
	drainCalls := 0
	stubRawInputDrained(t, func(time.Duration, time.Duration) (time.Duration, bool, error) {
		drainCalls++
		if drainCalls == 2 {
			return 0, false, errors.New("queue inspection unavailable")
		}
		return 0, true, nil
	})
	stubRawInjectSleep(t)

	var writes []string
	guardCalls := 0
	failed := false
	cfg := &wakeConfig{
		root:           root,
		me:             "codex",
		session:        "session1",
		wakeOwner:      &wakeOwner{},
		injectMode:     wakeInjectModeRaw,
		attentionIsTTY: func() bool { return false },
		beforeTerminalWrite: func() error {
			guardCalls++
			if !failed && guardCalls == 4 {
				failed = true
				return newWakeTerminalForegroundPGRPChangedLoss(101, 202)
			}
			return nil
		},
		terminalWrite: func(text string) error {
			writes = append(writes, text)
			return nil
		},
	}

	err := notifyNewMessages(cfg)
	if !isWakeTerminalForegroundPGRPChanged(err) {
		t.Fatalf("first notify error = %T %v, want foreground-PGRP change", err, err)
	}
	if got := strings.Join(writes, "|"); got != coopWakeDoorbell+"|\n|\r" {
		t.Fatalf("first writes = %q, want payload, prelude, and first submit", got)
	}

	writes = nil
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("retry notify: %v", err)
	}
	if got := strings.Join(writes, "|"); got != "\r" {
		t.Fatalf("retry writes = %q, want only rescue submit", got)
	}
}

func TestNotifyNewMessagesReconcilesPartialDeliveryAfterInboxDrain(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		failAt        int
		reconcileWant []string
	}{
		{
			name:          "paste payload",
			mode:          wakeInjectModePaste,
			failAt:        1,
			reconcileWant: nil,
		},
		{
			name:          "paste submit",
			mode:          wakeInjectModePaste,
			failAt:        2,
			reconcileWant: []string{"\r"},
		},
		{
			name:          "raw prelude",
			mode:          wakeInjectModeRaw,
			failAt:        2,
			reconcileWant: []string{"\n", "\r", "\r"},
		},
		{
			name:          "raw first submit",
			mode:          wakeInjectModeRaw,
			failAt:        3,
			reconcileWant: []string{"\r", "\r"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			deliverPartialWakeMessageForTest(t, root, "codex", "reconcile-"+strings.ReplaceAll(tc.name, " ", "-"))
			stubRawInputDrained(t, func(time.Duration, time.Duration) (time.Duration, bool, error) {
				return 0, true, nil
			})
			stubRawInjectSleep(t)

			var writes []string
			guardCalls := 0
			failed := false
			cfg := &wakeConfig{
				root:           root,
				me:             "codex",
				session:        "session1",
				wakeOwner:      &wakeOwner{},
				injectMode:     tc.mode,
				attentionIsTTY: func() bool { return false },
				beforeTerminalWrite: func() error {
					guardCalls++
					if !failed && guardCalls == tc.failAt {
						failed = true
						return newWakeTerminalForegroundPGRPChangedLoss(101, 202)
					}
					return nil
				},
				terminalWrite: func(text string) error {
					writes = append(writes, text)
					return nil
				},
			}

			err := notifyNewMessages(cfg)
			if !isWakeTerminalForegroundPGRPChanged(err) {
				t.Fatalf("first notify error = %T %v, want foreground-PGRP change", err, err)
			}
			if drained := runDrainJSON(t, root, "codex", 0, false); drained.Count != 1 {
				t.Fatalf("drained count = %d, want 1", drained.Count)
			}

			writes = nil
			if err := notifyNewMessages(cfg); err != nil {
				t.Fatalf("empty-inbox reconciliation: %v", err)
			}
			if got := strings.Join(writes, "|"); got != strings.Join(tc.reconcileWant, "|") {
				t.Fatalf("reconciliation writes = %q, want %q", got, strings.Join(tc.reconcileWant, "|"))
			}
		})
	}
}

func TestRunWakeLoopAuthorityRetryResumesMissingPasteSubmitWithoutPayloadReplay(t *testing.T) {
	root := secureTempDirForTest(t)
	deliverPartialWakeMessageForTest(t, root, "codex", "run-loop-partial-paste")

	originalAuthorityRetryDelay := wakeTerminalAuthorityRetryDelay
	wakeTerminalAuthorityRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() {
		wakeTerminalAuthorityRetryDelay = originalAuthorityRetryDelay
	})

	var writes []string
	failed := false
	completed := make(chan struct{}, 1)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			session:     "session1",
			wakeOwner:   &wakeOwner{},
			injectMode:  wakeInjectModePaste,
			controlStop: stop,
			preconditionCheck: func(*wakeConfig) error {
				return nil
			},
			terminalWrite: func(text string) error {
				if text == "\r" && !failed {
					failed = true
					return newWakeTerminalForegroundPGRPChangedLoss(101, 202)
				}
				writes = append(writes, text)
				if text == "\r" {
					select {
					case completed <- struct{}{}:
					default:
					}
				}
				return nil
			},
			attentionIsTTY: func() bool { return false },
		})
	}()

	select {
	case <-completed:
	case err := <-done:
		t.Fatalf("wake loop exited before authority retry completed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not complete missing paste submit")
	}
	close(stop)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wake loop stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not stop")
	}

	if got := strings.Join(writes, "|"); got != coopWakeDoorbell+"|\r" {
		t.Fatalf("terminal writes = %q, want one payload and resumed submit", got)
	}
}

func stubSignalWakeProcess(t *testing.T, fn func(pid int, sig os.Signal) error) {
	t.Helper()
	oldSignal := signalWakeProcess
	oldGrace := wakeTerminateGrace
	signalWakeProcess = fn
	wakeTerminateGrace = 0
	t.Cleanup(func() {
		signalWakeProcess = oldSignal
		wakeTerminateGrace = oldGrace
	})
}

func TestWakeHelpDocumentsMaxHoldDemotion(t *testing.T) {
	output, err := captureEnvStdout(t, func() error {
		return runWakeWithLoop([]string{"--help"}, runWakeLoop)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "input remains active through --input-max-hold") ||
		!strings.Contains(output, "skips synthetic input") ||
		!strings.Contains(output, "heuristic is advisory") {
		t.Fatalf("wake help omits max-hold demotion:\n%s", output)
	}
}

func stubWakeCurrentTTY(t *testing.T, fn func() string) {
	t.Helper()
	old := getWakeCurrentTTY
	getWakeCurrentTTY = fn
	t.Cleanup(func() {
		getWakeCurrentTTY = old
	})
}

func stubWakeProcessSID(t *testing.T, fn func(pid int) (int, error)) {
	t.Helper()
	old := getWakeProcessSID
	getWakeProcessSID = fn
	t.Cleanup(func() {
		getWakeProcessSID = old
	})
}

func writeWakePreparedForTest(t *testing.T, root, me string) {
	t.Helper()
	inspection := inspectWakeLock(root, me)
	if err := writeWakePreparedFile(root, me, inspection); err != nil {
		t.Fatalf("writeWakePreparedFile: %v", err)
	}
}

func stubWakeTTYSupport(t *testing.T) {
	t.Helper()
	oldAvailable := wakeTIOCSTIAvailable
	oldIsTTY := wakeInputIsTTY
	oldRead := readTIOCSTILegacySysctl
	wakeTIOCSTIAvailable = func() bool { return true }
	wakeInputIsTTY = func() bool { return true }
	readTIOCSTILegacySysctl = func() ([]byte, error) { return nil, os.ErrNotExist }
	t.Cleanup(func() {
		wakeTIOCSTIAvailable = oldAvailable
		wakeInputIsTTY = oldIsTTY
		readTIOCSTILegacySysctl = oldRead
	})
}

func liveWakeOwnerObservationForTest() wakeOwnerObservation {
	var monitor *wakeOwnerObservationMonitor
	monitor = newWakeOwnerObservationMonitor(func() error {
		monitor.finish(nil)
		return nil
	})
	return wakeOwnerObservation{
		State:   wakeOwnerSame,
		monitor: monitor,
	}
}

func writeExecutableForTest(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(secureTempDirForTest(t), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	return path
}

func assertWakeLockOwnedByCurrentProcess(t *testing.T, lockPath string) {
	t.Helper()
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read replacement lock: %v", err)
	}
	var got wakeLock
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal replacement lock: %v", err)
	}
	if got.PID != os.Getpid() {
		t.Fatalf("replacement pid = %d, want %d", got.PID, os.Getpid())
	}
}

func TestRunWakeWithLoopInjectViaSkipsTTYStartupRequirement(t *testing.T) {
	oldRead := readTIOCSTILegacySysctl
	sysctlReads := 0
	readTIOCSTILegacySysctl = func() ([]byte, error) {
		sysctlReads++
		return []byte("0\n"), nil
	}
	t.Cleanup(func() {
		readTIOCSTILegacySysctl = oldRead
	})

	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	var got wakeConfig
	errDone := errors.New("done")
	injector := writeExecutableForTest(t, "inject tool")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--inject-arg", "exec",
		"--inject-arg", "Team Alpha",
		"--inject-timeout", "250ms",
	}, func(cfg wakeConfig) error {
		got = cfg
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
	if got.injectVia != injector {
		t.Fatalf("expected inject executable with spaces, got %q", got.injectVia)
	}
	if strings.Join(got.injectArgs, "|") != "exec|Team Alpha" {
		t.Fatalf("expected fixed inject args, got %#v", got.injectArgs)
	}
	if got.injectTimeout != 250*time.Millisecond {
		t.Fatalf("expected inject timeout 250ms, got %s", got.injectTimeout)
	}
	if sysctlReads != 0 {
		t.Fatalf("--inject-via read TIOCSTI sysctl %d times, want 0", sysctlReads)
	}
}

func TestRunWakeWithLoopReadableDisabledTIOCSTIDegradesToNonInput(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	oldRead := readTIOCSTILegacySysctl
	oldTTY := wakeInputIsTTY
	readTIOCSTILegacySysctl = func() ([]byte, error) {
		return []byte("0\n"), nil
	}
	ttyChecks := 0
	wakeInputIsTTY = func() bool {
		ttyChecks++
		return false
	}
	t.Cleanup(func() {
		readTIOCSTILegacySysctl = oldRead
		wakeInputIsTTY = oldTTY
	})

	errDone := errors.New("done")
	stderr := captureWakeStderr(t, func() {
		err := runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-mode", "raw",
		}, func(cfg wakeConfig) error {
			if cfg.injectMode != wakeInjectModeNone {
				t.Fatalf("inject mode = %q, want non-input", cfg.injectMode)
			}
			p, err := presence.Read(root, "orchestrator")
			if err != nil {
				t.Fatalf("read durable notifier status: %v", err)
			}
			if p.NotifierStatus != wakeInjectorUnsupportedStatus ||
				p.NotifierMode != wakeInjectModeRaw ||
				!strings.Contains(p.NotifierReason, tiocstiLegacySysctlPath) {
				t.Fatalf("durable notifier status = %#v", p)
			}
			return errDone
		})
		if !errors.Is(err, errDone) {
			t.Fatalf("runWakeWithLoop error = %v, want sentinel", err)
		}
	})
	if ttyChecks != 0 {
		t.Fatalf("TTY checks = %d, want 0 after advisory downgrade", ttyChecks)
	}
	if count := strings.Count(stderr, "warning:"); count != 1 {
		t.Fatalf("warning count = %d, want 1:\n%s", count, stderr)
	}
	for _, want := range []string{
		tiocstiLegacySysctlPath,
		"--inject-via",
		"non-input",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("warning missing %q:\n%s", want, stderr)
		}
	}
}

func TestRunWakeWithLoopUnknownTIOCSTIHintDoesNotChangeMode(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		err  error
	}{
		{name: "absent", err: os.ErrNotExist},
		{name: "unreadable", err: os.ErrPermission},
		{name: "enabled", data: []byte("1\n")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatalf("EnsureRootDirs: %v", err)
			}
			if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
				t.Fatalf("EnsureAgentDirs: %v", err)
			}

			oldRead := readTIOCSTILegacySysctl
			oldTTY := wakeInputIsTTY
			readTIOCSTILegacySysctl = func() ([]byte, error) {
				return test.data, test.err
			}
			wakeInputIsTTY = func() bool { return true }
			t.Cleanup(func() {
				readTIOCSTILegacySysctl = oldRead
				wakeInputIsTTY = oldTTY
			})

			errDone := errors.New("done")
			err := runWakeWithLoop([]string{
				"--root", root,
				"--me", "orchestrator",
				"--inject-mode", "raw",
			}, func(cfg wakeConfig) error {
				if cfg.injectMode != wakeInjectModeRaw {
					t.Fatalf("inject mode = %q, want raw", cfg.injectMode)
				}
				return errDone
			})
			if !errors.Is(err, errDone) {
				t.Fatalf("runWakeWithLoop error = %v, want sentinel", err)
			}
		})
	}
}

func TestRunWakeWithLoopInterruptCommandDefaultsOffAndRemainsOptIn(t *testing.T) {
	tests := []struct {
		name             string
		interruptCmdArgs []string
		wantPrefix       string
	}{
		{
			name:       "default emits notice without ctrl-c",
			wantPrefix: "",
		},
		{
			name:             "explicit ctrl-c injects before notice",
			interruptCmdArgs: []string{"--interrupt-cmd", "ctrl-c"},
			wantPrefix:       "\x03\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatalf("EnsureRootDirs: %v", err)
			}
			if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
				t.Fatalf("EnsureAgentDirs: %v", err)
			}

			msg := format.Message{
				Header: format.Header{
					Schema:   1,
					ID:       "msg-urgent",
					From:     "codex",
					To:       []string{"alice"},
					Thread:   "p2p/alice__codex",
					Subject:  "help needed",
					Created:  "2026-07-26T12:00:00Z",
					Priority: "urgent",
					Labels:   []string{"interrupt"},
				},
				Body: "urgent body",
			}
			data, err := msg.Marshal()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if _, err := deliverToInboxForTest(t, root, "alice", "msg-urgent.md", data); err != nil {
				t.Fatalf("deliver: %v", err)
			}

			logPath := filepath.Join(root, "inject.log")
			injector := filepath.Join(root, "inject.sh")
			if err := os.WriteFile(
				injector,
				[]byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> "+logPath+"\n"),
				0o755,
			); err != nil {
				t.Fatalf("write injector: %v", err)
			}

			errDone := errors.New("done")
			args := []string{
				"--root", root,
				"--me", "alice",
				"--inject-via", injector,
			}
			args = append(args, tt.interruptCmdArgs...)
			err = runWakeWithLoop(args, func(cfg wakeConfig) error {
				if err := notifyNewMessages(&cfg); err != nil {
					t.Fatalf("notifyNewMessages: %v", err)
				}
				return errDone
			})
			if !errors.Is(err, errDone) {
				t.Fatalf("runWakeWithLoop error = %v, want sentinel", err)
			}

			got, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read injector log: %v", err)
			}
			want := tt.wantPrefix + coopWakeDoorbell + "\n"
			if string(got) != want {
				t.Fatalf("injector log = %q, want %q", string(got), want)
			}
		})
	}
}

func TestRunWakeWithLoopRejectsBlankInterruptCommand(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "equals empty", args: []string{"--interrupt-cmd="}},
		{name: "separate empty", args: []string{"--interrupt-cmd", ""}},
		{name: "whitespace", args: []string{"--interrupt-cmd", " \t "}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatalf("EnsureRootDirs: %v", err)
			}
			if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
				t.Fatalf("EnsureAgentDirs: %v", err)
			}

			logPath := filepath.Join(root, "inject.log")
			injector := filepath.Join(root, "inject.sh")
			if err := os.WriteFile(
				injector,
				[]byte("#!/bin/sh\nprintf 'invoked\\n' >> "+logPath+"\n"),
				0o755,
			); err != nil {
				t.Fatalf("write injector: %v", err)
			}

			args := []string{
				"--root", root,
				"--me", "alice",
				"--inject-via", injector,
			}
			args = append(args, tt.args...)
			loopCalled := false
			err := runWakeWithLoop(args, func(wakeConfig) error {
				loopCalled = true
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "invalid interrupt-cmd") {
				t.Fatalf("runWakeWithLoop error = %v, want invalid interrupt-cmd", err)
			}
			if loopCalled {
				t.Fatal("wake loop ran for blank interrupt command")
			}
			if _, err := os.Stat(logPath); !os.IsNotExist(err) {
				t.Fatalf("injector log exists after rejected command: %v", err)
			}
		})
	}
}

func TestRunWakeWithLoopWritesReadyFileAfterLock(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--ready-file", readyPath,
	}, func(cfg wakeConfig) error {
		if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
			t.Fatalf("ready file published before wake preparation: %v", statErr)
		}
		if err := cfg.onPrepared(nil); err != nil {
			t.Fatalf("publish readiness: %v", err)
		}
		if _, statErr := os.Stat(readyPath); statErr != nil {
			t.Fatalf("expected ready file after wake preparation: %v", statErr)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopBaselinesBeforeReadiness(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	inboxNew := fsq.AgentInboxNew(root, "orchestrator")
	if err := os.WriteFile(filepath.Join(inboxNew, "stale.md"), []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale message: %v", err)
	}

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--baseline-existing",
		"--ready-file", readyPath,
	}, func(cfg wakeConfig) error {
		if !cfg.baselineRequested {
			t.Fatal("baseline request was not carried into the owned wake loop")
		}
		if cfg.baselineExisting != nil {
			t.Fatal("baseline was captured before watcher setup and wake ownership")
		}
		watcher, watcherErr := fsnotify.NewWatcher()
		if watcherErr != nil {
			t.Fatalf("NewWatcher: %v", watcherErr)
		}
		defer func() { _ = watcher.Close() }()
		if watcherErr := watcher.Add(inboxNew); watcherErr != nil {
			t.Fatalf("watch inbox: %v", watcherErr)
		}
		if prepErr := prepareWakeBaseline(&cfg, watcher, inboxNew); prepErr != nil {
			t.Fatalf("prepareWakeBaseline: %v", prepErr)
		}
		if _, ok := cfg.baselineExisting["stale.md"]; !ok {
			t.Fatal("stale message missing from watcher-armed startup baseline")
		}
		if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
			t.Fatalf("ready file published before baseline preparation: %v", statErr)
		}
		if err := cfg.onPrepared(nil); err != nil {
			t.Fatalf("publish readiness: %v", err)
		}
		if _, statErr := os.Stat(readyPath); statErr != nil {
			t.Fatalf("expected ready file after baseline snapshot: %v", statErr)
		}
		if _, exists, preparedErr := readWakeGenerationFile(wakePreparedPath(root, "orchestrator"), "wake prepared marker"); preparedErr != nil || !exists {
			t.Fatalf("generation-bound prepared marker missing: exists=%v err=%v", exists, preparedErr)
		}
		if err := os.WriteFile(filepath.Join(inboxNew, "fresh.md"), []byte("fresh"), 0o600); err != nil {
			t.Fatalf("write fresh message: %v", err)
		}
		if _, ok := cfg.baselineExisting["fresh.md"]; ok {
			t.Fatal("message arriving after readiness was incorrectly baselined")
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopRejectsCanonicalAgentReplacementAfterAcquisition(t *testing.T) {
	for _, ownerBound := range []bool{false, true} {
		name := "ownerless"
		if ownerBound {
			name = "owner-bound"
		}
		t.Run(name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
				t.Fatal(err)
			}
			if ownerBound {
				owner := wakeOwner{
					PID:          4242,
					ProcessStart: "12345",
					BootID:       "11111111-1111-1111-1111-111111111111",
					SessionID:    99,
				}
				encoded, err := encodeWakeOwnerEnv(owner)
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv(envWakeOwner, encoded)
				oldObserve := observeAuthoritativeWakeOwner
				observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
					if got != owner {
						t.Fatalf("owner = %#v, want %#v", got, owner)
					}
					return liveWakeOwnerObservationForTest(), nil
				}
				t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })
			} else {
				t.Setenv(envWakeOwner, "")
			}

			injector := writeExecutableForTest(t, "injector")
			readyPath := filepath.Join(t.TempDir(), "wake.ready")
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.Mkdir(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			agentPath := fsq.AgentBase(root, "codex")
			detachedPath := agentPath + ".detached"
			err := runWakeWithLoop([]string{
				"--root", root,
				"--me", "codex",
				"--inject-via", injector,
				"--ready-file", readyPath,
			}, func(cfg wakeConfig) error {
				if cfg.retainedAgent == nil {
					t.Fatal("acquisition capability was not threaded into wake config")
				}
				if err := os.Rename(agentPath, detachedPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(agentPath, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(agentPath, "inbox")); err != nil {
					t.Fatal(err)
				}
				return runWakeLoop(cfg)
			})
			if err == nil ||
				!strings.Contains(err.Error(), "agent") ||
				!strings.Contains(err.Error(), "retained authority") {
				t.Fatalf("canonical replacement error = %v", err)
			}
			entries, readErr := os.ReadDir(outside)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("replacement symlink target mutated: %#v", entries)
			}
			if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
				t.Fatalf("ready file published after authority replacement: %v", statErr)
			}
			if _, statErr := os.Stat(filepath.Join(agentPath, wakePreparedFileName)); !os.IsNotExist(statErr) {
				t.Fatalf("prepared marker published in replacement: %v", statErr)
			}
		})
	}
}

func TestRunWakeWithLoopAcceptExistingRejectsCanonicalAgentReplacement(t *testing.T) {
	for _, ownerBound := range []bool{false, true} {
		name := "ownerless"
		if ownerBound {
			name = "owner-bound"
		}
		t.Run(name, func(t *testing.T) {
			wakePID := os.Getpid()
			root := secureTempDirForTest(t)
			injector := writeExecutableForTest(t, "injector")
			owner := wakeOwner{
				PID:          4343,
				ProcessStart: "12345",
				BootID:       "11111111-1111-1111-1111-111111111111",
				SessionID:    99,
			}
			target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
			if ownerBound {
				target.Owner = &owner
				encoded, err := encodeWakeOwnerEnv(owner)
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv(envWakeOwner, encoded)
				oldObserve := observeAuthoritativeWakeOwner
				observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
					if got != owner {
						t.Fatalf("owner = %#v, want %#v", got, owner)
					}
					return liveWakeOwnerObservationForTest(), nil
				}
				t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })
			} else {
				t.Setenv(envWakeOwner, "")
			}
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				if pid == wakePID {
					return wakeProcessInfo{
						PID:        pid,
						Running:    true,
						StartToken: "67890",
						BootID:     owner.BootID,
						Executable: "/opt/homebrew/bin/amq",
						Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "codex", "--inject-via", injector},
					}
				}
				return wakeProcessInfo{PID: pid}
			})
			lock := bindWakeLockToTarget(wakeLock{
				PID:          wakePID,
				TTY:          "unknown",
				ProcessStart: "67890",
				BootID:       owner.BootID,
				Executable:   "/opt/homebrew/bin/amq",
				Generation:   "generation-1",
			}, target)
			if ownerBound {
				lock.Owner = &owner
				lock.OwnerSchema = wakeOwnerLockSchema
				lock.WakeMode = wakeOwnerWakeMode
			}
			lockPath := writeWakeLockForTest(t, root, "codex", lock)
			if ownerBound {
				if err := os.Chmod(lockPath, wakeOwnerLockFileMode); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeWakeTarget(root, "codex", target); err != nil {
				t.Fatal(err)
			}

			retryEntered := make(chan struct{}, 1)
			originalRetry := waitForWakePreparedRetry
			waitForWakePreparedRetry = func(time.Time) bool {
				select {
				case retryEntered <- struct{}{}:
				default:
				}
				return true
			}
			t.Cleanup(func() { waitForWakePreparedRetry = originalRetry })

			agentPath := fsq.AgentBase(root, "codex")
			detachedPath := agentPath + ".detached"
			readyPath := filepath.Join(t.TempDir(), "wake.ready")
			done := make(chan error, 1)
			go func() {
				done <- runWakeWithLoop([]string{
					"--root", root,
					"--me", "codex",
					"--inject-via", injector,
					"--ready-file", readyPath,
					"--accept-existing-wake",
				}, func(cfg wakeConfig) error {
					t.Errorf("loop should not run with an existing wake: %#v", cfg)
					return nil
				})
			}()
			select {
			case <-retryEntered:
			case err := <-done:
				t.Fatalf("existing wake returned before replacement: %v", err)
			case <-time.After(time.Second):
				t.Fatal("existing wake did not poll for preparation")
			}
			if err := os.Rename(agentPath, detachedPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(agentPath, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := wakeReady{
				Schema:       wakeReadySchema,
				Generation:   lock.Generation,
				TargetDigest: lock.TargetDigest,
			}
			if err := writeWakeGenerationFile(
				filepath.Join(detachedPath, wakePreparedFileName),
				"wake prepared marker",
				marker,
			); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil ||
					!strings.Contains(err.Error(), "canonical wake agent directory") ||
					!strings.Contains(err.Error(), "retained authority") {
					t.Fatalf("canonical replacement error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("existing wake did not reject replacement agent directory")
			}
			if _, err := os.Stat(readyPath); !os.IsNotExist(err) {
				t.Fatalf("ready file published for detached wake: %v", err)
			}
		})
	}
}

func TestWakeReadyCleanupPreservesReplacement(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	original := wakeReady{
		Schema:       wakeReadySchema,
		Generation:   "original",
		TargetDigest: "original-target",
	}
	publication, err := publishWakeReadyFile(readyPath, original)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = publication.Close() }()

	replacement := wakeReady{
		Schema:       wakeReadySchema,
		Generation:   "replacement",
		TargetDigest: "replacement-target",
	}
	originalHook := beforeWakeReadyCleanupUnlink
	beforeWakeReadyCleanupUnlink = func() {
		beforeWakeReadyCleanupUnlink = func() {}
		if err := writeWakeGenerationFile(readyPath, "replacement wake ready file", replacement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeWakeReadyCleanupUnlink = originalHook })

	err = publication.removeIfUnchanged()
	if err == nil || !strings.Contains(err.Error(), "changed before removal; preserving it") {
		t.Fatalf("replacement cleanup error = %v, want preservation", err)
	}
	current, exists, err := readWakeReadyFile(readyPath)
	if err != nil || !exists || current != replacement {
		t.Fatalf("replacement wake ready file = %#v, exists=%v, err=%v", current, exists, err)
	}
}

func TestWakeReadyPublicationCleansInstalledMarkerAfterSyncFailure(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	syncErr := errors.New("sync unavailable")
	originalSync := syncWakeOwnerDirFD
	syncWakeOwnerDirFD = func(int) error { return syncErr }
	t.Cleanup(func() { syncWakeOwnerDirFD = originalSync })

	publication, err := publishWakeReadyFile(readyPath, wakeReady{
		Schema:     wakeReadySchema,
		Generation: "sync-failure",
	})
	if publication != nil {
		t.Fatalf("sync failure returned publication capability: %#v", publication)
	}
	if !errors.Is(err, syncErr) {
		t.Fatalf("publication error = %v, want sync failure", err)
	}
	if _, err := os.Stat(readyPath); !os.IsNotExist(err) {
		t.Fatalf("ready marker survived sync failure: %v", err)
	}
}

func TestWakeReadyPublicationPreservesReplacementAfterValidationFailure(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	replacement := wakeReady{
		Schema:       wakeReadySchema,
		Generation:   "replacement",
		TargetDigest: "replacement-target",
	}
	originalHook := afterWakeReadyPublicationWrite
	afterWakeReadyPublicationWrite = func() {
		afterWakeReadyPublicationWrite = func() {}
		if err := writeWakeGenerationFile(readyPath, "replacement wake ready file", replacement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { afterWakeReadyPublicationWrite = originalHook })

	publication, err := publishWakeReadyFile(readyPath, wakeReady{
		Schema:     wakeReadySchema,
		Generation: "original",
	})
	if publication != nil {
		t.Fatalf("validation failure returned publication capability: %#v", publication)
	}
	if err == nil || !strings.Contains(err.Error(), "changed during publication; preserving it") {
		t.Fatalf("publication error = %v, want replacement validation failure", err)
	}
	current, exists, err := readWakeReadyFile(readyPath)
	if err != nil || !exists || current != replacement {
		t.Fatalf("replacement wake ready file = %#v, exists=%v, err=%v", current, exists, err)
	}
}

func TestRunWakeWithLoopAcceptExistingRemovesReadyAfterOwnerLoss(t *testing.T) {
	wakePID := os.Getpid()
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	owner := wakeOwner{
		PID:          4343,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
	target.Owner = &owner
	encoded, err := encodeWakeOwnerEnv(owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envWakeOwner, encoded)

	ownerAlive := true
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		if got != owner {
			t.Fatalf("owner = %#v, want %#v", got, owner)
		}
		if !ownerAlive {
			return wakeOwnerObservation{
				State:  wakeOwnerDead,
				Reason: "test owner exited after readiness publication",
			}, nil
		}
		return liveWakeOwnerObservationForTest(), nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "67890",
				BootID:     owner.BootID,
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "codex", "--inject-via", injector},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	lock := bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "67890",
		BootID:       owner.BootID,
		Executable:   "/opt/homebrew/bin/amq",
		Generation:   "generation-1",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &owner,
	}, target)
	lock.WakeMode = wakeOwnerWakeMode
	lockPath := writeWakeLockForTest(t, root, "codex", lock)
	if err := os.Chmod(lockPath, wakeOwnerLockFileMode); err != nil {
		t.Fatal(err)
	}
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	writeWakePreparedForTest(t, root, "codex")

	originalHook := afterExistingWakeReadyPublication
	afterExistingWakeReadyPublication = func() { ownerAlive = false }
	t.Cleanup(func() { afterExistingWakeReadyPublication = originalHook })
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	err = runWakeWithLoop([]string{
		"--root", root,
		"--me", "codex",
		"--inject-via", injector,
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an existing wake: %#v", cfg)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "owner is dead") {
		t.Fatalf("owner loss error = %v", err)
	}
	if _, err := os.Stat(readyPath); !os.IsNotExist(err) {
		t.Fatalf("ready file survived owner loss: %v", err)
	}
}

func TestRunWakeWithLoopDoesNotRecreateMissingInboxAfterAcquisition(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "injector")
	inboxPath := fsq.AgentInboxNew(root, "codex")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "codex",
		"--inject-via", injector,
	}, func(cfg wakeConfig) error {
		if err := os.Remove(inboxPath); err != nil {
			t.Fatal(err)
		}
		return runWakeLoop(cfg)
	})
	if err == nil {
		t.Fatal("missing acquired inbox started a wake loop")
	}
	if _, statErr := os.Stat(inboxPath); !os.IsNotExist(statErr) {
		t.Fatalf("missing acquired inbox was recreated: %v", statErr)
	}
}

func TestBaselineFreshMailboxStillStarts(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}

	baseline, err := snapshotWakeExistingMessages(root, "fresh-agent")
	if err != nil {
		t.Fatalf("missing inbox/new should be treated as an empty baseline: %v", err)
	}
	if len(baseline) != 0 {
		t.Fatalf("baseline = %#v, want empty", baseline)
	}
}

func TestPrepareWakeBaselineInvalidatesSameNameReplacementDuringSnapshot(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	inboxNew := fsq.AgentInboxNew(root, "alice")
	samePath := filepath.Join(inboxNew, "same.md")
	if err := os.WriteFile(samePath, []byte("old"), 0o600); err != nil {
		t.Fatalf("write old message: %v", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer func() { _ = watcher.Close() }()
	if err := watcher.Add(inboxNew); err != nil {
		t.Fatalf("watch inbox: %v", err)
	}

	originalInfo := snapshotWakeDirEntryInfo
	replaced := false
	snapshotWakeDirEntryInfo = func(entry os.DirEntry) (os.FileInfo, error) {
		if entry.Name() == "same.md" && !replaced {
			replaced = true
			if err := os.Rename(samePath, filepath.Join(inboxNew, "old.md")); err != nil {
				return nil, err
			}
			if err := os.WriteFile(samePath, []byte("replacement"), 0o600); err != nil {
				return nil, err
			}
		}
		return entry.Info()
	}
	t.Cleanup(func() { snapshotWakeDirEntryInfo = originalInfo })

	cfg := wakeConfig{root: root, me: "alice", baselineRequested: true}
	if err := prepareWakeBaseline(&cfg, watcher, inboxNew); err != nil {
		t.Fatalf("prepareWakeBaseline: %v", err)
	}
	if !replaced {
		t.Fatal("same-name replacement hook did not run")
	}
	if _, ignored := cfg.baselineExisting["same.md"]; ignored {
		t.Fatal("same-name replacement created during snapshot remained baselined")
	}
}

func TestFailWakeOnWatcherErrorClearsBaselineAndExits(t *testing.T) {
	cfg := wakeConfig{baselineExisting: map[string]wakeFileIdentity{"stale.md": {}}}
	cause := errors.New("overflow")
	err := failWakeOnWatcherError(&cfg, "watcher error", cause)
	if err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("watcher failure = %v", err)
	}
	if cfg.baselineExisting != nil {
		t.Fatalf("watcher error retained baseline tombstones: %#v", cfg.baselineExisting)
	}
}

func TestRunWakeLoopStopsBeforePublishingPreparedMarker(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	prepared := false
	err := runWakeLoop(wakeConfig{
		root:        secureTempDirForTest(t),
		me:          "orchestrator",
		controlStop: stop,
		onPrepared: func(wakeAdmissionWatcher) error {
			prepared = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("runWakeLoop: %v", err)
	}
	if prepared {
		t.Fatal("prepared marker was published after cooperative stop was already pending")
	}
}

func TestRunWakeLoopOwnerlessStartupScanAndQueuedEventEmitOnce(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	cleanup, err := acquireWakeLock(root, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	lock := inspectWakeLock(root, "codex")
	stop := make(chan struct{})
	done := make(chan error, 1)
	first := make(chan struct{}, 1)
	var notices atomic.Int64
	stubTIOCSTIInject(t, func(text string) error {
		if len(text) > 1 {
			if notices.Add(1) == 1 {
				first <- struct{}{}
			}
		}
		return nil
	})
	stubRawInputDrained(t, func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	})
	stubRawInjectSleep(t)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:               root,
			me:                 "codex",
			session:            "session1",
			debounce:           5 * time.Millisecond,
			injectMode:         wakeInjectModeRaw,
			controlStop:        stop,
			terminalGeneration: lock.Lock.Generation,
			terminalTTY:        lock.Lock.TTY,
			onPrepared: func(wakeAdmissionWatcher) error {
				deliverWakeWatcherMessageForTest(t, root, "codex", "startup", "startup")
				return nil
			},
			preconditionCheck: func(*wakeConfig) error { return nil },
		})
	}()
	select {
	case <-first:
	case err := <-done:
		t.Fatalf("wake loop exited before startup notice: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not emit startup notice")
	}
	select {
	case err := <-done:
		t.Fatalf("wake loop exited while checking queued event dedup: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := notices.Load(); got != 1 {
		t.Fatalf("startup scan plus queued event emitted %d notices, want 1", got)
	}
}

func TestRunWakeLoopRearmsOrdinaryInboxWatcher(t *testing.T) {
	originalScanRetryBase := wakeInboxScanRetryBase
	originalScanRetryMax := wakeInboxScanRetryMax
	wakeInboxScanRetryBase = 20 * time.Millisecond
	wakeInboxScanRetryMax = 100 * time.Millisecond
	t.Cleanup(func() {
		wakeInboxScanRetryBase = originalScanRetryBase
		wakeInboxScanRetryMax = originalScanRetryMax
	})

	for _, test := range []struct {
		name    string
		replace func(t *testing.T, inboxPath string)
	}{
		{
			name: "remove and recreate",
			replace: func(t *testing.T, inboxPath string) {
				t.Helper()
				if err := os.RemoveAll(inboxPath); err != nil {
					t.Fatalf("remove inbox/new: %v", err)
				}
				if err := os.Mkdir(inboxPath, 0o700); err != nil {
					t.Fatalf("recreate inbox/new: %v", err)
				}
			},
		},
		{
			name: "rename and recreate",
			replace: func(t *testing.T, inboxPath string) {
				t.Helper()
				if err := os.Rename(inboxPath, inboxPath+".detached"); err != nil {
					t.Fatalf("rename inbox/new: %v", err)
				}
				if err := os.Mkdir(inboxPath, 0o700); err != nil {
					t.Fatalf("recreate inbox/new: %v", err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			ensureCoopWakeMailboxForTest(t, root, "codex")
			inboxPath := fsq.AgentInboxNew(root, "codex")
			ready := make(chan struct{})
			attention := make(chan string, 8)
			stop := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- runWakeLoop(wakeConfig{
					root:        root,
					me:          "codex",
					session:     "session1",
					wakeOwner:   &wakeOwner{},
					debounce:    5 * time.Millisecond,
					previewLen:  80,
					injectMode:  wakeInjectModeNone,
					controlStop: stop,
					onPrepared: func(wakeAdmissionWatcher) error {
						close(ready)
						return nil
					},
					preconditionCheck: func(*wakeConfig) error { return nil },
					attentionIsTTY:    func() bool { return false },
					attentionWrite: func(data []byte) (int, error) {
						attention <- string(data)
						return len(data), nil
					},
				})
			}()
			t.Cleanup(func() {
				close(stop)
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("wake loop did not stop")
				}
			})

			select {
			case <-ready:
			case err := <-done:
				t.Fatalf("wake loop exited before readiness: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("wake loop did not publish readiness")
			}

			test.replace(t, inboxPath)
			deliverWakeWatcherMessageForTest(t, root, "codex", "during-rearm", "during")
			awaitWakeAttentionFrom(t, attention, done, "during")

			if err := os.Remove(filepath.Join(inboxPath, "during-rearm.md")); err != nil {
				t.Fatalf("remove rearm message: %v", err)
			}
			deliverWakeWatcherMessageForTest(t, root, "codex", "after-rearm", "after")
			awaitWakeAttentionFrom(t, attention, done, "after")
		})
	}
}

func TestRunWakeLoopRejectsOrdinaryAgentReplacement(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	agentPath := fsq.AgentBase(root, "codex")
	ready := make(chan struct{})
	attention := make(chan string, 2)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			session:     "session1",
			wakeOwner:   &wakeOwner{},
			debounce:    5 * time.Millisecond,
			previewLen:  80,
			injectMode:  wakeInjectModeNone,
			controlStop: stop,
			onPrepared: func(wakeAdmissionWatcher) error {
				close(ready)
				return nil
			},
			preconditionCheck: func(*wakeConfig) error { return nil },
			attentionIsTTY:    func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attention <- string(data)
				return len(data), nil
			},
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("wake loop exited before readiness: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not publish readiness")
	}
	if err := os.Rename(agentPath, agentPath+".detached"); err != nil {
		t.Fatalf("detach original agent directory: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("create replacement agent directory: %v", err)
	}
	deliverWakeWatcherMessageForTest(t, root, "codex", "replacement", "replacement")

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "agent directory no longer matches retained authority") {
			t.Fatalf("agent replacement error = %v", err)
		}
	case <-time.After(2 * time.Second):
		close(stop)
		t.Fatal("wake loop did not reject replacement agent directory")
	}
	select {
	case output := <-attention:
		t.Fatalf("wake loop read replacement-agent message: %q", output)
	default:
	}
}

func deliverWakeWatcherMessageForTest(
	t *testing.T,
	root, me, id, from string,
) {
	t.Helper()
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      id,
			From:    from,
			To:      []string{me},
			Thread:  "p2p/" + from + "__" + me,
			Subject: id,
			Created: "2026-07-30T08:00:00Z",
		},
		Body: "body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(t, root, me, id+".md", data); err != nil {
		t.Fatal(err)
	}
}

func awaitWakeAttentionFrom(
	t *testing.T,
	attention <-chan string,
	done <-chan error,
	from string,
) {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case output := <-attention:
			if strings.Contains(output, "from "+from) {
				return
			}
		case err := <-done:
			t.Fatalf("wake loop exited before attention from %s: %v", from, err)
		case <-timeout.C:
			t.Fatalf("wake loop did not emit attention from %s", from)
		}
	}
}

func TestRunWakeLoopRetriesPendingDoorbellWithoutOutputFlood(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "pending-reannounce",
			From:    "claude",
			To:      []string{"codex"},
			Thread:  "p2p/claude__codex",
			Subject: "still pending",
			Created: "2026-07-29T07:00:00Z",
		},
		Body: "body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(t, root, "codex", "pending-reannounce.md", data); err != nil {
		t.Fatal(err)
	}

	stubRawInputDrained(t, func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	})
	stubRawInjectSleep(t)
	firstDoorbell := make(chan struct{}, 1)
	extraDoorbell := make(chan struct{}, 1)
	attention := make(chan string, 1)
	stop := make(chan struct{})
	done := make(chan error, 1)
	doorbells := 0

	go func() {
		done <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			session:     "session1",
			wakeOwner:   &wakeOwner{},
			injectMode:  wakeInjectModeRaw,
			controlStop: stop,
			preconditionCheck: func(*wakeConfig) error {
				return nil
			},
			terminalWrite: func(text string) error {
				if strings.Contains(text, coopWakeDoorbell) {
					doorbells++
					if doorbells == 1 {
						firstDoorbell <- struct{}{}
					} else {
						extraDoorbell <- struct{}{}
					}
				}
				return nil
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attention <- string(data)
				return len(data), nil
			},
		})
	}()

	select {
	case <-firstDoorbell:
	case err := <-done:
		t.Fatalf("wake loop exited before initial doorbell: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not submit initial doorbell")
	}

	select {
	case <-extraDoorbell:
	case err := <-done:
		t.Fatalf("wake loop exited before retry: %v", err)
	case <-time.After(wakeDoorbellRetryBase + 2*time.Second):
		t.Fatal("pending doorbell was not retried on its own deadline")
	}
	close(stop)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wake loop stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not stop")
	}
	select {
	case output := <-attention:
		t.Fatalf("retry emitted output-only attention: %q", output)
	default:
	}
}

func TestRunWakeLoopForegroundAuthorityRetryDoesNotCompeteWithExpiredDoorbell(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "expired-foreground-retry",
			From:    "claude",
			To:      []string{"codex"},
			Thread:  "p2p/claude__codex",
			Subject: "foreground handoff",
			Created: "2026-07-30T08:00:00Z",
		},
		Body: "body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(t, root, "codex", "expired-foreground-retry.md", data); err != nil {
		t.Fatal(err)
	}

	originalAuthorityRetryDelay := wakeTerminalAuthorityRetryDelay
	wakeTerminalAuthorityRetryDelay = time.Hour
	t.Cleanup(func() {
		wakeTerminalAuthorityRetryDelay = originalAuthorityRetryDelay
	})

	start := time.Unix(1_800_000_000, 0)
	var nowNanos atomic.Int64
	nowNanos.Store(start.UnixNano())
	var promptWrites atomic.Int64
	initialSubmitted := make(chan struct{}, 1)
	refused := make(chan struct{}, 1)
	extra := make(chan struct{}, 1)
	stop := make(chan struct{})
	stopped := false
	stopLoop := func() {
		if !stopped {
			close(stop)
			stopped = true
		}
	}
	defer stopLoop()
	done := make(chan error, 1)

	go func() {
		done <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			session:     "session1",
			wakeOwner:   &wakeOwner{},
			debounce:    0,
			injectMode:  wakeInjectModePaste,
			controlStop: stop,
			doorbellNow: func() time.Time {
				return time.Unix(0, nowNanos.Load())
			},
			preconditionCheck: func(*wakeConfig) error {
				return nil
			},
			terminalWrite: func(text string) error {
				if strings.Contains(text, coopWakeDoorbellPrefix) {
					switch promptWrites.Add(1) {
					case 1:
						return nil
					case 2:
						refused <- struct{}{}
					default:
						select {
						case extra <- struct{}{}:
						default:
						}
					}
					return newWakeTerminalForegroundPGRPChangedLoss(101, 202)
				}
				if text == "\r" && promptWrites.Load() == 1 {
					select {
					case initialSubmitted <- struct{}{}:
					default:
					}
				}
				return nil
			},
			attentionIsTTY: func() bool { return false },
		})
	}()

	select {
	case <-initialSubmitted:
	case err := <-done:
		t.Fatalf("wake loop exited before initial submit: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not submit initial doorbell")
	}
	nowNanos.Store(start.Add(wakeDoorbellRetryBase).UnixNano())
	message.Header.ID = "trigger-expired-foreground-retry"
	data, err = message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(
		t,
		root,
		"codex",
		"trigger-expired-foreground-retry.md",
		data,
	); err != nil {
		t.Fatal(err)
	}

	select {
	case <-refused:
	case err := <-done:
		t.Fatalf("wake loop exited before foreground refusal: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not reach expired foreground retry")
	}

	message.Header.ID = "debounce-during-foreground-retry"
	data, err = message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(
		t,
		root,
		"codex",
		"debounce-during-foreground-retry.md",
		data,
	); err != nil {
		t.Fatal(err)
	}

	select {
	case <-extra:
		t.Fatal("doorbell or debounce retried alongside terminal-authority timer")
	case err := <-done:
		t.Fatalf("wake loop exited while holding foreground retry: %v", err)
	case <-time.After(75 * time.Millisecond):
	}

	stopLoop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wake loop stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not stop")
	}
}

func TestRunWakeLoopMaintenanceDemotionRetiresForegroundAuthorityHold(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "demote-foreground-retry",
			From:    "claude",
			To:      []string{"codex"},
			Thread:  "p2p/claude__codex",
			Subject: "demote foreground retry",
			Created: "2026-07-30T08:00:00Z",
		},
		Body: "body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(t, root, "codex", "demote-foreground-retry.md", data); err != nil {
		t.Fatal(err)
	}

	originalAuthorityRetryDelay := wakeTerminalAuthorityRetryDelay
	wakeTerminalAuthorityRetryDelay = 200 * time.Millisecond
	t.Cleanup(func() {
		wakeTerminalAuthorityRetryDelay = originalAuthorityRetryDelay
	})

	var promptWrites atomic.Int64
	refused := make(chan struct{}, 1)
	extra := make(chan struct{}, 1)
	demoted := make(chan struct{}, 1)
	attention := make(chan string, 1)
	ticks := make(chan time.Time)
	stop := make(chan struct{})
	stopped := false
	stopLoop := func() {
		if !stopped {
			close(stop)
			stopped = true
		}
	}
	defer stopLoop()
	done := make(chan error, 1)

	go func() {
		done <- runWakeLoop(wakeConfig{
			root:             root,
			me:               "codex",
			session:          "session1",
			wakeOwner:        &wakeOwner{},
			injectMode:       wakeInjectModePaste,
			controlStop:      stop,
			maintenanceTicks: ticks,
			preconditionCheck: func(cfg *wakeConfig) error {
				cfg.injectMode = wakeInjectModeNone
				select {
				case demoted <- struct{}{}:
				default:
				}
				return nil
			},
			terminalWrite: func(text string) error {
				if !strings.Contains(text, coopWakeDoorbellPrefix) {
					return nil
				}
				if promptWrites.Add(1) == 1 {
					refused <- struct{}{}
				} else {
					select {
					case extra <- struct{}{}:
					default:
					}
				}
				return newWakeTerminalForegroundPGRPChangedLoss(101, 202)
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				select {
				case attention <- string(data):
				default:
				}
				return len(data), nil
			},
		})
	}()

	select {
	case <-refused:
	case err := <-done:
		t.Fatalf("wake loop exited before foreground refusal: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not establish foreground-authority hold")
	}
	select {
	case ticks <- time.Now():
	case err := <-done:
		t.Fatalf("wake loop exited before maintenance demotion: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not accept maintenance tick")
	}
	select {
	case <-demoted:
	case err := <-done:
		t.Fatalf("wake loop exited during maintenance demotion: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not complete maintenance demotion")
	}

	select {
	case <-extra:
		t.Fatal("maintenance bypassed the foreground-authority hold")
	case err := <-done:
		t.Fatalf("wake loop exited after maintenance demotion: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case output := <-attention:
		if !strings.Contains(output, "message from claude") {
			t.Fatalf("demotion fallback = %q, want pending message", output)
		}
	case err := <-done:
		t.Fatalf("wake loop exited after maintenance demotion: %v", err)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("maintenance demotion dropped foreground-held notification")
	}
	select {
	case output := <-attention:
		t.Fatalf("retired foreground-authority timer emitted duplicate attention: %q", output)
	case err := <-done:
		t.Fatalf("wake loop exited after maintenance demotion: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	stopLoop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wake loop stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not stop")
	}
}

func TestRunWakeLoopDefersObservationUntilPartialInputCompletes(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "deferred-observation",
			From:    "peer",
			To:      []string{"codex"},
			Thread:  "p2p/codex__peer",
			Subject: "deferred observation",
			Created: "2026-07-30T08:00:00Z",
		},
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	path, err := deliverToInboxForTest(t, root, "codex", "pending.md", data)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	originalRetryDelay := wakeTerminalAuthorityRetryDelay
	wakeTerminalAuthorityRetryDelay = 50 * time.Millisecond
	t.Cleanup(func() { wakeTerminalAuthorityRetryDelay = originalRetryDelay })

	token := coopWakeDoorbellTokenForTests
	payload := buildCoopWakeDoorbell(token)
	const accepted = 7
	promptObserved := make(chan string, 1)
	firstBlocked := make(chan struct{}, 1)
	writes := make(chan string, 4)
	var calls atomic.Int64
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:           root,
			me:             "codex",
			session:        "session1",
			wakeOwner:      &wakeOwner{},
			injectMode:     wakeInjectModePaste,
			controlStop:    stop,
			promptObserved: promptObserved,
			doorbell: wakeDoorbellState{
				phase:       wakeDoorbellAwaitingObservation,
				token:       token,
				cohort:      snapshotWakeFileIdentities(map[string]os.FileInfo{"pending.md": info}),
				attempts:    1,
				nextAttempt: time.Now().Add(-time.Second),
			},
			inputDelivery: wakeInputDeliveryState{
				phase:         wakeInputPayloadPending,
				mode:          wakeInjectModePaste,
				payload:       payload,
				acceptedBytes: accepted,
			},
			preconditionCheck: func(*wakeConfig) error { return nil },
			terminalWrite: func(text string) error {
				if calls.Add(1) == 1 {
					firstBlocked <- struct{}{}
					return newWakeTerminalControlStoppedLoss()
				}
				writes <- text
				return nil
			},
			attentionIsTTY: func() bool { return false },
		})
	}()
	defer func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	}()

	select {
	case <-firstBlocked:
	case err := <-done:
		t.Fatalf("wake loop exited before partial hold: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not retain partial input")
	}
	promptObserved <- token

	select {
	case got := <-writes:
		if want := payload[accepted:]; got != want {
			t.Fatalf("partial retry wrote %q, want exact suffix %q", got, want)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before suffix retry: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not retry retained suffix")
	}
	select {
	case got := <-writes:
		if got != "\r" {
			t.Fatalf("submit retry wrote %q, want CR", got)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before submit retry: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not finish retained sequence")
	}
	select {
	case got := <-writes:
		t.Fatalf("deferred observation replayed terminal input: %q", got)
	case <-time.After(2 * wakeTerminalAuthorityRetryDelay):
	}
}

func TestRunWakeLoopMaintenanceDemotionTransfersPendingDebounce(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")

	initialScan := make(chan struct{})
	var scans atomic.Int64
	inboxNew := fsq.AgentInboxNew(root, "codex")
	reader := wakeScriptedInboxReader{
		readDir: func() ([]os.DirEntry, error) {
			entries, err := os.ReadDir(inboxNew)
			if scans.Add(1) == 1 {
				close(initialScan)
			}
			return entries, err
		},
		readHeader: func(name string) (format.Header, error) {
			return format.ReadHeaderFile(filepath.Join(inboxNew, name))
		},
	}
	pending := make(chan struct{}, 1)
	demoted := make(chan struct{}, 1)
	attention := make(chan string, 2)
	ticks := make(chan time.Time)
	stop := make(chan struct{})
	done := make(chan error, 1)
	var terminalWrites atomic.Int64
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:             root,
			me:               "codex",
			session:          "session1",
			wakeOwner:        &wakeOwner{},
			debounce:         time.Hour,
			injectMode:       wakeInjectModePaste,
			controlStop:      stop,
			maintenanceTicks: ticks,
			retainedInbox:    reader,
			onPendingNotify: func() {
				select {
				case pending <- struct{}{}:
				default:
				}
			},
			preconditionCheck: func(cfg *wakeConfig) error {
				cfg.injectMode = wakeInjectModeNone
				select {
				case demoted <- struct{}{}:
				default:
				}
				return nil
			},
			terminalWrite: func(string) error {
				terminalWrites.Add(1)
				return nil
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attention <- string(data)
				return len(data), nil
			},
		})
	}()
	t.Cleanup(func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	})

	select {
	case <-initialScan:
	case err := <-done:
		t.Fatalf("wake loop exited before initial scan: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not complete initial scan")
	}
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "demote-pending-debounce",
			From:    "claude",
			To:      []string{"codex"},
			Thread:  "p2p/claude__codex",
			Subject: "pending debounce",
			Created: "2026-07-30T08:00:00Z",
		},
		Body: "body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(t, root, "codex", "demote-pending-debounce.md", data); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pending:
	case err := <-done:
		t.Fatalf("wake loop exited before pending debounce: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not observe pending debounce")
	}
	select {
	case ticks <- time.Now():
	case err := <-done:
		t.Fatalf("wake loop exited before maintenance demotion: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not accept maintenance tick")
	}
	select {
	case <-demoted:
	case err := <-done:
		t.Fatalf("wake loop exited during maintenance demotion: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not demote input")
	}
	select {
	case output := <-attention:
		if !strings.Contains(output, "message from claude") {
			t.Fatalf("demotion fallback = %q, want pending message", output)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before demotion fallback: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("maintenance demotion dropped pending debounce")
	}
	if got := terminalWrites.Load(); got != 0 {
		t.Fatalf("pending debounce reached terminal %d times before demotion", got)
	}

	ticks <- time.Now()
	select {
	case duplicate := <-attention:
		t.Fatalf("maintenance emitted duplicate demotion fallback: %q", duplicate)
	case err := <-done:
		t.Fatalf("wake loop exited after demotion fallback: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRunWakeLoopPacesPersistentInboxScanErrors(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")

	originalScanRetryBase := wakeInboxScanRetryBase
	originalScanRetryMax := wakeInboxScanRetryMax
	wakeInboxScanRetryBase = 100 * time.Millisecond
	wakeInboxScanRetryMax = time.Second
	t.Cleanup(func() {
		wakeInboxScanRetryBase = originalScanRetryBase
		wakeInboxScanRetryMax = originalScanRetryMax
	})

	scans := make(chan time.Time, 4)
	var scanCount atomic.Int64
	reader := wakeScriptedInboxReader{
		readDir: func() ([]os.DirEntry, error) {
			count := scanCount.Add(1)
			scans <- time.Now()
			if count <= 3 {
				return nil, syscall.EIO
			}
			return nil, nil
		},
	}
	now := time.Unix(1_800_000_000, 0)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:          root,
			me:            "codex",
			session:       "session1",
			wakeOwner:     &wakeOwner{},
			injectMode:    wakeInjectModePaste,
			controlStop:   stop,
			retainedInbox: reader,
			doorbellNow:   func() time.Time { return now },
			doorbell: wakeDoorbellState{
				phase:       wakeDoorbellAwaitingObservation,
				token:       coopWakeDoorbellTokenForTests,
				cohort:      map[string]*wakeFileIdentity{"pending.md": nil},
				attempts:    1,
				nextAttempt: now.Add(-time.Second),
			},
			preconditionCheck: func(*wakeConfig) error { return nil },
			attentionIsTTY:    func() bool { return false },
		})
	}()
	t.Cleanup(func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	})

	first := awaitWakeScan(t, scans, done)
	for attempt := 2; attempt <= 3; attempt++ {
		at := awaitWakeScan(t, scans, done)
		minimum := wakeInboxScanRetryBase
		if attempt == 3 {
			minimum += 2 * wakeInboxScanRetryBase
		}
		if elapsed := at.Sub(first); elapsed < minimum {
			t.Fatalf(
				"scan attempt %d arrived after %s, want paced retries of at least %s",
				attempt,
				elapsed,
				minimum,
			)
		}
	}
}

func TestRunWakeLoopRecoversAfterTransientInboxScanError(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")

	originalScanRetryBase := wakeInboxScanRetryBase
	originalScanRetryMax := wakeInboxScanRetryMax
	wakeInboxScanRetryBase = 50 * time.Millisecond
	wakeInboxScanRetryMax = time.Second
	t.Cleanup(func() {
		wakeInboxScanRetryBase = originalScanRetryBase
		wakeInboxScanRetryMax = originalScanRetryMax
	})

	scans := make(chan time.Time, 3)
	var scanCount atomic.Int64
	reader := wakeScriptedInboxReader{
		readDir: func() ([]os.DirEntry, error) {
			count := scanCount.Add(1)
			scans <- time.Now()
			if count == 1 {
				return nil, syscall.EIO
			}
			return nil, nil
		},
	}
	now := time.Unix(1_800_000_000, 0)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:          root,
			me:            "codex",
			session:       "session1",
			wakeOwner:     &wakeOwner{},
			injectMode:    wakeInjectModePaste,
			controlStop:   stop,
			retainedInbox: reader,
			doorbellNow:   func() time.Time { return now },
			doorbell: wakeDoorbellState{
				phase:       wakeDoorbellAwaitingObservation,
				token:       coopWakeDoorbellTokenForTests,
				cohort:      map[string]*wakeFileIdentity{"pending.md": nil},
				attempts:    1,
				nextAttempt: now.Add(-time.Second),
			},
			preconditionCheck: func(*wakeConfig) error { return nil },
			attentionIsTTY:    func() bool { return false },
		})
	}()
	t.Cleanup(func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	})

	first := awaitWakeScan(t, scans, done)
	second := awaitWakeScan(t, scans, done)
	if elapsed := second.Sub(first); elapsed < wakeInboxScanRetryBase {
		t.Fatalf("transient scan retried after %s, want at least %s", elapsed, wakeInboxScanRetryBase)
	}
	select {
	case third := <-scans:
		t.Fatalf("successful empty scan left a stale retry armed at %s", third)
	case err := <-done:
		t.Fatalf("wake loop exited after transient inbox scan recovery: %v", err)
	case <-time.After(2 * wakeInboxScanRetryBase):
	}
}

func TestRunWakeLoopDemotionPreservesPendingScanRetry(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "demote-scan-retry",
			From:    "claude",
			To:      []string{"codex"},
			Thread:  "p2p/claude__codex",
			Subject: "demote scan retry",
			Created: "2026-07-30T08:00:00Z",
		},
		Body: "body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(t, root, "codex", "demote-scan-retry.md", data); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	if err != nil {
		t.Fatal(err)
	}

	originalScanRetryBase := wakeInboxScanRetryBase
	originalScanRetryMax := wakeInboxScanRetryMax
	wakeInboxScanRetryBase = 100 * time.Millisecond
	wakeInboxScanRetryMax = 200 * time.Millisecond
	t.Cleanup(func() {
		wakeInboxScanRetryBase = originalScanRetryBase
		wakeInboxScanRetryMax = originalScanRetryMax
	})

	scans := make(chan int, 4)
	var scanCount atomic.Int64
	reader := wakeScriptedInboxReader{
		readDir: func() ([]os.DirEntry, error) {
			count := int(scanCount.Add(1))
			scans <- count
			if count <= 2 {
				return nil, syscall.EIO
			}
			return entries, nil
		},
		readHeader: func(string) (format.Header, error) {
			return message.Header, nil
		},
	}
	demoted := make(chan struct{}, 1)
	attention := make(chan string, 2)
	ticks := make(chan time.Time)
	stop := make(chan struct{})
	done := make(chan error, 1)
	var terminalWrites atomic.Int64
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:             root,
			me:               "codex",
			session:          "session1",
			wakeOwner:        &wakeOwner{},
			injectMode:       wakeInjectModePaste,
			controlStop:      stop,
			maintenanceTicks: ticks,
			retainedInbox:    reader,
			preconditionCheck: func(cfg *wakeConfig) error {
				cfg.injectMode = wakeInjectModeNone
				select {
				case demoted <- struct{}{}:
				default:
				}
				return nil
			},
			terminalWrite: func(string) error {
				terminalWrites.Add(1)
				return nil
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attention <- string(data)
				return len(data), nil
			},
		})
	}()
	t.Cleanup(func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	})

	select {
	case attempt := <-scans:
		if attempt != 1 {
			t.Fatalf("initial scan attempt = %d, want 1", attempt)
		}
	case err := <-done:
		t.Fatalf("wake loop exited on initial scan error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not attempt initial scan")
	}
	select {
	case ticks <- time.Now():
	case err := <-done:
		t.Fatalf("wake loop exited before scan-held demotion: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not accept maintenance tick")
	}
	select {
	case <-demoted:
	case err := <-done:
		t.Fatalf("wake loop exited during scan-held demotion: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not demote during scan retry")
	}
	select {
	case attempt := <-scans:
		if attempt != 2 {
			t.Fatalf("second scan attempt = %d, want 2", attempt)
		}
	case err := <-done:
		t.Fatalf("wake loop exited on repeated scan error: %v", err)
	case <-time.After(time.Second):
		t.Fatal("wake loop did not retry failed scan")
	}
	select {
	case output := <-attention:
		t.Fatalf("failed demotion scan emitted early attention: %q", output)
	default:
	}
	select {
	case attempt := <-scans:
		if attempt != 3 {
			t.Fatalf("recovery scan attempt = %d, want 3", attempt)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before scan recovery: %v", err)
	case <-time.After(time.Second):
		t.Fatal("wake loop did not recover scan after demotion")
	}
	select {
	case output := <-attention:
		if !strings.Contains(output, "message from claude") {
			t.Fatalf("scan recovery fallback = %q, want pending message", output)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before scan recovery fallback: %v", err)
	case <-time.After(time.Second):
		t.Fatal("successful demotion scan did not emit pending fallback")
	}
	if got := terminalWrites.Load(); got != 0 {
		t.Fatalf("scan-held demotion wrote terminal input %d times", got)
	}
	select {
	case duplicate := <-attention:
		t.Fatalf("scan recovery emitted duplicate demotion fallback: %q", duplicate)
	case err := <-done:
		t.Fatalf("wake loop exited after scan recovery: %v", err)
	case <-time.After(2 * wakeInboxScanRetryMax):
	}
}

func TestRunWakeLoopPreservesForegroundRetryAcrossInboxScanError(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "foreground-scan-recovery",
			From:    "claude",
			To:      []string{"codex"},
			Thread:  "p2p/claude__codex",
			Subject: "foreground scan recovery",
			Created: "2026-07-30T08:00:00Z",
		},
		Body: "body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(t, root, "codex", "foreground-scan-recovery.md", data); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	if err != nil {
		t.Fatal(err)
	}

	originalAuthorityRetryDelay := wakeTerminalAuthorityRetryDelay
	originalScanRetryBase := wakeInboxScanRetryBase
	originalScanRetryMax := wakeInboxScanRetryMax
	wakeTerminalAuthorityRetryDelay = 50 * time.Millisecond
	wakeInboxScanRetryBase = 50 * time.Millisecond
	wakeInboxScanRetryMax = time.Second
	t.Cleanup(func() {
		wakeTerminalAuthorityRetryDelay = originalAuthorityRetryDelay
		wakeInboxScanRetryBase = originalScanRetryBase
		wakeInboxScanRetryMax = originalScanRetryMax
	})

	var scanCount atomic.Int64
	scanFailed := make(chan struct{}, 1)
	reader := wakeScriptedInboxReader{
		readDir: func() ([]os.DirEntry, error) {
			if scanCount.Add(1) == 2 {
				scanFailed <- struct{}{}
				return nil, syscall.EIO
			}
			return entries, nil
		},
		readHeader: func(string) (format.Header, error) {
			return message.Header, nil
		},
	}
	var promptWrites atomic.Int64
	firstPrompt := make(chan string, 1)
	recoveredPrompt := make(chan string, 1)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:              root,
			me:                "codex",
			session:           "session1",
			wakeOwner:         &wakeOwner{},
			injectMode:        wakeInjectModePaste,
			controlStop:       stop,
			retainedInbox:     reader,
			preconditionCheck: func(*wakeConfig) error { return nil },
			terminalWrite: func(text string) error {
				if !strings.Contains(text, coopWakeDoorbellPrefix) {
					return nil
				}
				if promptWrites.Add(1) == 1 {
					firstPrompt <- text
					return newWakeTerminalForegroundPGRPChangedLoss(101, 202)
				}
				recoveredPrompt <- text
				return nil
			},
			attentionIsTTY: func() bool { return false },
		})
	}()
	t.Cleanup(func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	})

	var first string
	select {
	case first = <-firstPrompt:
	case err := <-done:
		t.Fatalf("wake loop exited before foreground refusal: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not establish foreground-authority hold")
	}
	select {
	case <-scanFailed:
	case err := <-done:
		t.Fatalf("wake loop exited on transient inbox scan error: %v", err)
	case <-time.After(time.Second):
		t.Fatal("authority retry did not encounter scripted inbox scan error")
	}
	select {
	case recovered := <-recoveredPrompt:
		if recovered != first {
			t.Fatalf("recovered prompt = %q, want preserved token %q", recovered, first)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before scan recovery: %v", err)
	case <-time.After(time.Second):
		t.Fatal("wake loop did not resume foreground retry after scan recovery")
	}
}

func TestWakeInboxScanRetryBackoffCaps(t *testing.T) {
	tests := []struct {
		failures uint
		want     time.Duration
	}{
		{failures: 1, want: wakeInboxScanRetryBase},
		{failures: 2, want: 2 * wakeInboxScanRetryBase},
		{failures: 8, want: wakeInboxScanRetryMax},
		{failures: 32, want: wakeInboxScanRetryMax},
	}
	for _, tc := range tests {
		if got := wakeInboxScanRetryBackoff(tc.failures); got != tc.want {
			t.Fatalf("failures %d backoff = %s, want %s", tc.failures, got, tc.want)
		}
	}
	for _, tc := range []struct {
		name    string
		base    time.Duration
		maximum time.Duration
	}{
		{name: "base-equals-maximum", base: time.Second, maximum: time.Second},
		{name: "base-exceeds-maximum", base: 2 * time.Second, maximum: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cappedExponentialBackoff(1, tc.base, tc.maximum); got != tc.maximum {
				t.Fatalf("backoff = %s, want cap %s", got, tc.maximum)
			}
		})
	}
}

func TestRunWakeLoopMaintenanceDemotionTransfersDormantDoorbellRetry(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "demoted-doorbell",
			From:    "claude",
			To:      []string{"codex"},
			Thread:  "p2p/claude__codex",
			Subject: "demote notifier",
			Created: "2026-07-30T08:00:00Z",
		},
		Body: "body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(t, root, "codex", "demoted-doorbell.md", data); err != nil {
		t.Fatal(err)
	}

	start := time.Unix(1_800_000_000, 0)
	var nowNanos atomic.Int64
	nowNanos.Store(start.UnixNano())
	var promptWrites atomic.Int64
	var initialSubmitted atomic.Bool
	initial := make(chan struct{}, 1)
	demoted := make(chan struct{}, 1)
	attention := make(chan string, 1)
	ticks := make(chan time.Time)
	stop := make(chan struct{})
	stopped := false
	stopLoop := func() {
		if !stopped {
			close(stop)
			stopped = true
		}
	}
	defer stopLoop()
	done := make(chan error, 1)

	go func() {
		done <- runWakeLoop(wakeConfig{
			root:             root,
			me:               "codex",
			session:          "session1",
			wakeOwner:        &wakeOwner{},
			injectMode:       wakeInjectModePaste,
			controlStop:      stop,
			maintenanceTicks: ticks,
			doorbellNow: func() time.Time {
				return time.Unix(0, nowNanos.Load())
			},
			preconditionCheck: func(cfg *wakeConfig) error {
				cfg.injectMode = wakeInjectModeNone
				select {
				case demoted <- struct{}{}:
				default:
				}
				return nil
			},
			terminalWrite: func(text string) error {
				if strings.Contains(text, coopWakeDoorbellPrefix) {
					promptWrites.Add(1)
				}
				if text == "\r" &&
					promptWrites.Load() == 1 &&
					initialSubmitted.CompareAndSwap(false, true) {
					nowNanos.Store(start.Add(wakeDoorbellRetryBase - 100*time.Millisecond).UnixNano())
					initial <- struct{}{}
				}
				return nil
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				select {
				case attention <- string(data):
				default:
				}
				return len(data), nil
			},
		})
	}()

	select {
	case <-initial:
	case err := <-done:
		t.Fatalf("wake loop exited before initial doorbell: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not submit initial doorbell")
	}
	select {
	case ticks <- time.Now():
	case err := <-done:
		t.Fatalf("wake loop exited before capability check: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not accept maintenance tick")
	}
	select {
	case <-demoted:
	case err := <-done:
		t.Fatalf("wake loop exited during capability demotion: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not run capability demotion")
	}

	select {
	case output := <-attention:
		if !strings.Contains(output, "AMQ [session1]") {
			t.Fatalf("demotion attention = %q, want pending AMQ message", output)
		}
	case err := <-done:
		t.Fatalf("wake loop exited after capability demotion: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("capability demotion dropped dormant doorbell retry")
	}
	select {
	case output := <-attention:
		t.Fatalf("capability demotion emitted duplicate attention: %q", output)
	case <-time.After(200 * time.Millisecond):
	}

	select {
	case ticks <- time.Now():
	case err := <-done:
		t.Fatalf("wake loop exited before output-only maintenance tick: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not accept output-only maintenance tick")
	}
	select {
	case <-demoted:
	case err := <-done:
		t.Fatalf("wake loop exited during output-only maintenance tick: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not complete output-only maintenance tick")
	}

	noisePath := filepath.Join(fsq.AgentInboxNew(root, "codex"), ".wake-test-noise.md")
	if err := os.WriteFile(noisePath, []byte("noise"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case output := <-attention:
		t.Fatalf("unchanged cohort redelivered after output-only maintenance: %q", output)
	case err := <-done:
		t.Fatalf("wake loop exited after output-only maintenance: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	stopLoop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wake loop stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not stop")
	}
}

func TestRunWakeLoopMaintenanceDemotionTransfersBeforePersistenceFailure(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "demotion-persistence",
			From:    "peer",
			To:      []string{"codex"},
			Thread:  "p2p/codex__peer",
			Subject: "preserve before exit",
			Created: "2026-07-30T08:00:00Z",
		},
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	path, err := deliverToInboxForTest(t, root, "codex", "pending.md", data)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	persistErr := errors.New("presence unavailable")
	ticks := make(chan time.Time)
	attention := make(chan string, 1)
	done := make(chan error, 1)
	var terminalWrites atomic.Int64
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			session:     "session1",
			wakeOwner:   &wakeOwner{},
			injectMode:  wakeInjectModePaste,
			controlStop: make(chan struct{}),
			doorbell: wakeDoorbellState{
				phase:       wakeDoorbellAwaitingObservation,
				token:       coopWakeDoorbellTokenForTests,
				cohort:      snapshotWakeFileIdentities(map[string]os.FileInfo{"pending.md": info}),
				attempts:    1,
				nextAttempt: time.Now().Add(time.Hour),
			},
			maintenanceTicks: ticks,
			preconditionCheck: func(cfg *wakeConfig) error {
				cfg.injectMode = wakeInjectModeNone
				return persistErr
			},
			terminalWrite: func(text string) error {
				terminalWrites.Add(1)
				return nil
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attention <- string(data)
				return len(data), nil
			},
		})
	}()
	select {
	case ticks <- time.Now():
	case err := <-done:
		t.Fatalf("wake loop exited before maintenance: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not accept maintenance tick")
	}
	select {
	case output := <-attention:
		if !strings.Contains(output, "AMQ [session1]") {
			t.Fatalf("fallback attention = %q", output)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before transferring fallback: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("persistence failure dropped pending attention")
	}
	select {
	case err := <-done:
		if !errors.Is(err, persistErr) {
			t.Fatalf("wake loop error = %v, want persistence failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not surface persistence failure")
	}
	if got := terminalWrites.Load(); got != 0 {
		t.Fatalf("dormant retry wrote %d terminal chunks", got)
	}
}

func TestRunWakeLoopMaintenanceKeepsWatchingAfterConfirmedInputDemotion(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "confirmed-input-demotion",
			From:    "peer",
			To:      []string{"codex"},
			Thread:  "p2p/codex__peer",
			Subject: "retain suffix debt",
			Created: "2026-07-30T08:00:00Z",
		},
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	path, err := deliverToInboxForTest(t, root, "codex", "pending.md", data)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	persistErr := errors.New("presence unavailable")
	stop := make(chan struct{})
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	var attentionWrites atomic.Int64
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			session:     "session1",
			wakeOwner:   &wakeOwner{},
			injectMode:  wakeInjectModePaste,
			controlStop: stop,
			inputDelivery: wakeInputDeliveryState{
				phase:   wakeInputRawRescueQueued,
				mode:    wakeInjectModeRaw,
				payload: buildCoopWakeDoorbell(coopWakeDoorbellTokenForTests),
			},
			doorbell: wakeDoorbellState{
				phase:       wakeDoorbellAwaitingObservation,
				token:       coopWakeDoorbellTokenForTests,
				cohort:      snapshotWakeFileIdentities(map[string]os.FileInfo{"pending.md": info}),
				attempts:    1,
				nextAttempt: time.Now().Add(time.Hour),
			},
			maintenanceTicks: ticks,
			preconditionCheck: func(cfg *wakeConfig) error {
				cfg.injectMode = wakeInjectModeNone
				return persistErr
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attentionWrites.Add(1)
				return len(data), nil
			},
		})
	}()

	select {
	case ticks <- time.Now():
	case err := <-done:
		t.Fatalf("wake loop exited before maintenance: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not accept maintenance tick")
	}
	deadline := time.After(2 * time.Second)
	for attentionWrites.Load() != 1 {
		select {
		case err := <-done:
			t.Fatalf("wake loop exited during confirmed-input recovery: %v", err)
		case <-deadline:
			t.Fatal("wake loop did not emit confirmed-input recovery attention")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case ticks <- time.Now():
	case err := <-done:
		t.Fatalf("wake loop exited before recovery maintenance: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not accept recovery maintenance tick")
	}
	time.Sleep(100 * time.Millisecond)
	if got := attentionWrites.Load(); got != 1 {
		t.Fatalf("unchanged recovery state emitted %d alerts, want one", got)
	}
	close(stop)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wake loop stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not stop")
	}
}

func TestRunWakeLoopKeepsWatchingAfterConfirmedInputBecomesUnsupported(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	writeMessage := func(filename, id string) os.FileInfo {
		t.Helper()
		message := format.Message{
			Header: format.Header{
				Schema:  1,
				ID:      id,
				From:    "peer",
				To:      []string{"codex"},
				Thread:  "p2p/codex__peer",
				Subject: id,
				Created: "2026-07-30T08:00:00Z",
			},
		}
		data, err := message.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		path, err := deliverToInboxForTest(t, root, "codex", filename, data)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	firstInfo := writeMessage("first.md", "first")

	stop := make(chan struct{})
	ticks := make(chan time.Time)
	attention := make(chan string, 3)
	done := make(chan error, 1)
	start := time.Now()
	var nowNanos atomic.Int64
	nowNanos.Store(start.UnixNano())
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			session:     "session1",
			wakeOwner:   &wakeOwner{},
			injectMode:  wakeInjectModePaste,
			controlStop: stop,
			inputDelivery: wakeInputDeliveryState{
				phase:   wakeInputPrimarySubmitPending,
				mode:    wakeInjectModePaste,
				payload: buildCoopWakeDoorbell(coopWakeDoorbellTokenForTests),
			},
			doorbell: wakeDoorbellState{
				phase:  wakeDoorbellAwaitingObservation,
				token:  coopWakeDoorbellTokenForTests,
				cohort: snapshotWakeFileIdentities(map[string]os.FileInfo{"first.md": firstInfo}),
			},
			maintenanceTicks: ticks,
			doorbellNow: func() time.Time {
				return time.Unix(0, nowNanos.Load())
			},
			terminalWrite: func(string) error {
				return newWakeInjectorUnsupportedError(errors.New("injector disabled"))
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attention <- string(data)
				return len(data), nil
			},
		})
	}()

	select {
	case output := <-attention:
		if !strings.Contains(output, "terminal input delivery is incomplete") {
			t.Fatalf("recovery attention = %q, want manual recovery guidance", output)
		}
	case err := <-done:
		t.Fatalf("wake loop exited after confirmed input became unsupported: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not emit recovery attention")
	}

	select {
	case ticks <- time.Now():
	case err := <-done:
		t.Fatalf("wake loop exited before recovery maintenance: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not accept recovery maintenance tick")
	}
	select {
	case duplicate := <-attention:
		t.Fatalf("unchanged recovery cohort emitted duplicate attention: %q", duplicate)
	case err := <-done:
		t.Fatalf("wake loop exited during recovery maintenance: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	nowNanos.Store(start.Add(wakeDoorbellRetryBase).UnixNano())
	writeMessage("second.md", "second")
	select {
	case output := <-attention:
		if !strings.Contains(output, "terminal input delivery is incomplete") {
			t.Fatalf("changed-cohort recovery attention = %q", output)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before changed recovery cohort: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("changed recovery cohort did not emit attention")
	}

	close(stop)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wake loop stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not stop")
	}
}

func TestRunWakeLoopMessageRecoveryAttentionFailureIsFatalWithoutRetry(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "confirmed-input-recovery-attention-failure",
			From:    "peer",
			To:      []string{"codex"},
			Thread:  "p2p/codex__peer",
			Subject: "confirmed-input-recovery-attention-failure",
			Created: "2026-07-30T08:00:00Z",
		},
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	path, err := deliverToInboxForTest(t, root, "codex", "message.md", data)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	attentionSinkErr := errors.New("attention sink unavailable")
	var attentionWrites atomic.Int64
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			session:     "session1",
			wakeOwner:   &wakeOwner{},
			injectMode:  wakeInjectModePaste,
			controlStop: stop,
			inputDelivery: wakeInputDeliveryState{
				phase:   wakeInputPrimarySubmitPending,
				mode:    wakeInjectModePaste,
				payload: buildCoopWakeDoorbell(coopWakeDoorbellTokenForTests),
			},
			doorbell: wakeDoorbellState{
				phase:  wakeDoorbellAwaitingObservation,
				token:  coopWakeDoorbellTokenForTests,
				cohort: snapshotWakeFileIdentities(map[string]os.FileInfo{"message.md": info}),
			},
			terminalWrite: func(string) error {
				return newWakeInjectorUnsupportedError(errors.New("injector disabled"))
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				if attentionWrites.Add(1) == 1 {
					return 0, attentionSinkErr
				}
				return len(data), nil
			},
		})
	}()

	select {
	case err := <-done:
		var attentionErr *wakeAttentionDeliveryError
		if !errors.As(err, &attentionErr) ||
			!errors.Is(err, attentionSinkErr) ||
			!isWakeInputDemotionBlocked(err) {
			t.Fatalf("wake loop error = %v, want blocked input plus attention failure", err)
		}
	case <-time.After(2 * time.Second):
		close(stop)
		<-done
		t.Fatalf(
			"wake loop retried or masked recovery attention failure; writes=%d",
			attentionWrites.Load(),
		)
	}
	if got := attentionWrites.Load(); got != 1 {
		t.Fatalf("recovery attention writes = %d, want exactly 1", got)
	}
}

func TestRunWakeLoopKeepsWatchingWhenDrainedInboxReconciliationBecomesUncertain(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	stop := make(chan struct{})
	ticks := make(chan time.Time)
	attention := make(chan string, 2)
	done := make(chan error, 1)
	var terminalWrites atomic.Int64
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			session:     "session1",
			wakeOwner:   &wakeOwner{},
			injectMode:  wakeInjectModePaste,
			controlStop: stop,
			inputDelivery: wakeInputDeliveryState{
				phase:   wakeInputPrimarySubmitPending,
				mode:    wakeInjectModePaste,
				payload: buildCoopWakeDoorbell(coopWakeDoorbellTokenForTests),
			},
			maintenanceTicks: ticks,
			terminalWrite: func(string) error {
				terminalWrites.Add(1)
				return &wakeTestAcceptedProgressError{
					err:      errors.New("terminal progress unavailable"),
					accepted: -1,
				}
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attention <- string(data)
				return len(data), nil
			},
		})
	}()

	select {
	case output := <-attention:
		if !strings.Contains(output, "terminal input delivery is incomplete") {
			t.Fatalf("reconciliation recovery attention = %q", output)
		}
	case err := <-done:
		t.Fatalf("wake loop exited during drained-inbox reconciliation: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("drained-inbox reconciliation did not emit recovery attention")
	}
	if got := terminalWrites.Load(); got != 1 {
		t.Fatalf("terminal writes = %d, want one uncertain reconciliation attempt", got)
	}

	select {
	case ticks <- time.Now():
	case err := <-done:
		t.Fatalf("wake loop exited before recovery maintenance: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not accept recovery maintenance tick")
	}
	select {
	case duplicate := <-attention:
		t.Fatalf("recovery maintenance emitted duplicate attention: %q", duplicate)
	case err := <-done:
		t.Fatalf("wake loop exited during recovery maintenance: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if got := terminalWrites.Load(); got != 1 {
		t.Fatalf("recovery maintenance retried terminal input: writes=%d", got)
	}

	close(stop)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wake loop stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not stop")
	}
}

func TestRunWakeLoopDrainedInboxRecoveryAttentionFailureIsFatal(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	attentionSinkErr := errors.New("attention sink unavailable")
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			session:     "session1",
			wakeOwner:   &wakeOwner{},
			injectMode:  wakeInjectModePaste,
			controlStop: make(chan struct{}),
			inputDelivery: wakeInputDeliveryState{
				phase:   wakeInputPrimarySubmitPending,
				mode:    wakeInjectModePaste,
				payload: buildCoopWakeDoorbell(coopWakeDoorbellTokenForTests),
			},
			terminalWrite: func(string) error {
				return &wakeTestAcceptedProgressError{
					err:      errors.New("terminal progress unavailable"),
					accepted: -1,
				}
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func([]byte) (int, error) {
				return 0, attentionSinkErr
			},
		})
	}()

	select {
	case err := <-done:
		var uncertainErr *wakeTerminalProgressUncertainError
		var attentionErr *wakeAttentionDeliveryError
		if !errors.As(err, &uncertainErr) ||
			!errors.As(err, &attentionErr) ||
			!errors.Is(err, attentionSinkErr) {
			t.Fatalf("wake loop error = %v, want uncertain input plus attention failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not fail after recovery attention failure")
	}
}

func TestRunWakeLoopBuiltInMaintenanceKeepsWatchingAfterConfirmedInputDemotion(t *testing.T) {
	oldRead := readTIOCSTILegacySysctl
	readTIOCSTILegacySysctl = func() ([]byte, error) {
		return []byte("0\n"), nil
	}
	t.Cleanup(func() {
		readTIOCSTILegacySysctl = oldRead
	})

	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "built-in-confirmed-input-demotion",
			From:    "peer",
			To:      []string{"codex"},
			Thread:  "p2p/codex__peer",
			Subject: "retain suffix debt",
			Created: "2026-07-30T08:00:00Z",
		},
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	path, err := deliverToInboxForTest(t, root, "codex", "pending.md", data)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	ticks := make(chan time.Time)
	done := make(chan error, 1)
	stop := make(chan struct{})
	var attentionWrites atomic.Int64
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			session:     "session1",
			wakeOwner:   &wakeOwner{},
			injectMode:  wakeInjectModeRaw,
			controlStop: stop,
			inputDelivery: wakeInputDeliveryState{
				phase:   wakeInputRawRescuePending,
				mode:    wakeInjectModeRaw,
				payload: buildCoopWakeDoorbell(coopWakeDoorbellTokenForTests),
			},
			doorbell: wakeDoorbellState{
				phase:       wakeDoorbellAwaitingObservation,
				token:       coopWakeDoorbellTokenForTests,
				cohort:      snapshotWakeFileIdentities(map[string]os.FileInfo{"pending.md": info}),
				attempts:    1,
				nextAttempt: time.Now().Add(time.Hour),
			},
			maintenanceTicks: ticks,
			attentionIsTTY:   func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attentionWrites.Add(1)
				return len(data), nil
			},
		})
	}()

	select {
	case ticks <- time.Now():
	case err := <-done:
		t.Fatalf("wake loop exited before built-in maintenance: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not accept built-in maintenance tick")
	}
	deadline := time.After(2 * time.Second)
	for attentionWrites.Load() != 1 {
		select {
		case err := <-done:
			t.Fatalf("wake loop exited during built-in recovery: %v", err)
		case <-deadline:
			t.Fatal("wake loop did not emit built-in recovery attention")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(stop)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wake loop stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not stop")
	}
}

func TestRunWakeLoopFatalDemotionTransfersDuringScanBackoff(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	persistErr := errors.New("presence unavailable")
	originalScanRetryBase := wakeInboxScanRetryBase
	originalScanRetryMax := wakeInboxScanRetryMax
	wakeInboxScanRetryBase = time.Hour
	wakeInboxScanRetryMax = time.Hour
	t.Cleanup(func() {
		wakeInboxScanRetryBase = originalScanRetryBase
		wakeInboxScanRetryMax = originalScanRetryMax
	})

	scanned := make(chan struct{}, 1)
	reader := wakeScriptedInboxReader{
		readDir: func() ([]os.DirEntry, error) {
			select {
			case scanned <- struct{}{}:
			default:
			}
			return nil, syscall.EIO
		},
	}
	ticks := make(chan time.Time)
	attention := make(chan string, 2)
	done := make(chan error, 1)
	var terminalWrites atomic.Int64
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:             root,
			me:               "codex",
			session:          "session1",
			wakeOwner:        &wakeOwner{},
			injectMode:       wakeInjectModePaste,
			controlStop:      make(chan struct{}),
			maintenanceTicks: ticks,
			retainedInbox:    reader,
			preconditionCheck: func(cfg *wakeConfig) error {
				cfg.injectMode = wakeInjectModeNone
				return persistErr
			},
			terminalWrite: func(string) error {
				terminalWrites.Add(1)
				return nil
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attention <- string(data)
				return len(data), nil
			},
		})
	}()
	select {
	case <-scanned:
	case err := <-done:
		t.Fatalf("wake loop exited before initial scan backoff: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not arm scan backoff")
	}
	select {
	case ticks <- time.Now():
	case err := <-done:
		t.Fatalf("wake loop exited before fatal demotion: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not accept maintenance tick")
	}
	select {
	case output := <-attention:
		if !strings.Contains(output, "run amq drain --include-body") {
			t.Fatalf("fatal demotion fallback = %q", output)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before transferring generic fallback: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("fatal demotion dropped scan-held pending work")
	}
	select {
	case err := <-done:
		if !errors.Is(err, persistErr) {
			t.Fatalf("wake loop error = %v, want persistence failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not surface fatal demotion")
	}
	if got := terminalWrites.Load(); got != 0 {
		t.Fatalf("fatal demotion wrote %d terminal chunks", got)
	}
	select {
	case output := <-attention:
		t.Fatalf("fatal demotion emitted duplicate attention: %q", output)
	default:
	}
}

func TestRunWakeLoopFatalScanFallbackSurfacesAttentionWriteFailure(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	persistErr := errors.New("presence unavailable")
	originalScanRetryBase := wakeInboxScanRetryBase
	originalScanRetryMax := wakeInboxScanRetryMax
	wakeInboxScanRetryBase = time.Hour
	wakeInboxScanRetryMax = time.Hour
	t.Cleanup(func() {
		wakeInboxScanRetryBase = originalScanRetryBase
		wakeInboxScanRetryMax = originalScanRetryMax
	})

	scanned := make(chan struct{}, 1)
	reader := wakeScriptedInboxReader{
		readDir: func() ([]os.DirEntry, error) {
			select {
			case scanned <- struct{}{}:
			default:
			}
			return nil, syscall.EIO
		},
	}
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	var attentionWrites atomic.Int64
	attentionSinkErr := errors.New("attention sink unavailable")
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:             root,
			me:               "codex",
			session:          "session1",
			wakeOwner:        &wakeOwner{},
			injectMode:       wakeInjectModePaste,
			controlStop:      make(chan struct{}),
			maintenanceTicks: ticks,
			retainedInbox:    reader,
			preconditionCheck: func(cfg *wakeConfig) error {
				cfg.injectMode = wakeInjectModeNone
				return persistErr
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attentionWrites.Add(1)
				return 0, attentionSinkErr
			},
		})
	}()

	select {
	case <-scanned:
	case err := <-done:
		t.Fatalf("wake loop exited before initial scan backoff: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not arm scan backoff")
	}
	select {
	case ticks <- time.Now():
	case err := <-done:
		t.Fatalf("wake loop exited before fatal demotion: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not accept maintenance tick")
	}
	select {
	case err := <-done:
		var attentionErr *wakeAttentionDeliveryError
		if !errors.Is(err, persistErr) ||
			!errors.As(err, &attentionErr) ||
			!errors.Is(err, attentionSinkErr) {
			t.Fatalf("wake loop error = %v, want persistence plus attention sink failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not surface failed fatal fallback")
	}
	if got := attentionWrites.Load(); got != 1 {
		t.Fatalf("attention writes = %d, want one failed fallback", got)
	}
}

func TestRunWakeLoopFatalDemotionTransfersWhenScanBackoffStartsDuringTransfer(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	persistErr := errors.New("presence unavailable")
	originalScanRetryBase := wakeInboxScanRetryBase
	originalScanRetryMax := wakeInboxScanRetryMax
	wakeInboxScanRetryBase = time.Hour
	wakeInboxScanRetryMax = time.Hour
	t.Cleanup(func() {
		wakeInboxScanRetryBase = originalScanRetryBase
		wakeInboxScanRetryMax = originalScanRetryMax
	})

	initialScan := make(chan struct{})
	var scans atomic.Int64
	inboxNew := fsq.AgentInboxNew(root, "codex")
	reader := wakeScriptedInboxReader{
		readDir: func() ([]os.DirEntry, error) {
			if scans.Add(1) == 1 {
				entries, err := os.ReadDir(inboxNew)
				close(initialScan)
				return entries, err
			}
			return nil, syscall.EIO
		},
		readHeader: func(name string) (format.Header, error) {
			return format.ReadHeaderFile(filepath.Join(inboxNew, name))
		},
	}
	pending := make(chan struct{}, 1)
	ticks := make(chan time.Time)
	attention := make(chan string, 2)
	done := make(chan error, 1)
	var terminalWrites atomic.Int64
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:             root,
			me:               "codex",
			session:          "session1",
			wakeOwner:        &wakeOwner{},
			debounce:         time.Hour,
			injectMode:       wakeInjectModePaste,
			controlStop:      make(chan struct{}),
			maintenanceTicks: ticks,
			retainedInbox:    reader,
			onPendingNotify: func() {
				select {
				case pending <- struct{}{}:
				default:
				}
			},
			preconditionCheck: func(cfg *wakeConfig) error {
				cfg.injectMode = wakeInjectModeNone
				return persistErr
			},
			terminalWrite: func(string) error {
				terminalWrites.Add(1)
				return nil
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attention <- string(data)
				return len(data), nil
			},
		})
	}()

	select {
	case <-initialScan:
	case err := <-done:
		t.Fatalf("wake loop exited before initial scan: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not complete initial scan")
	}
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "demote-new-scan-backoff",
			From:    "claude",
			To:      []string{"codex"},
			Thread:  "p2p/claude__codex",
			Subject: "pending debounce",
			Created: "2026-07-30T08:00:00Z",
		},
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(t, root, "codex", "pending.md", data); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pending:
	case err := <-done:
		t.Fatalf("wake loop exited before pending debounce: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not observe pending debounce")
	}
	select {
	case ticks <- time.Now():
	case err := <-done:
		t.Fatalf("wake loop exited before fatal demotion: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not accept maintenance tick")
	}
	select {
	case output := <-attention:
		if !strings.Contains(output, "run amq drain --include-body") {
			t.Fatalf("fatal demotion fallback = %q", output)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before transferring generic fallback: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("fatal demotion dropped pending work after transfer scan failed")
	}
	select {
	case err := <-done:
		if !errors.Is(err, persistErr) {
			t.Fatalf("wake loop error = %v, want persistence failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not surface fatal demotion")
	}
	if got := terminalWrites.Load(); got != 0 {
		t.Fatalf("fatal demotion wrote %d terminal chunks", got)
	}
	select {
	case output := <-attention:
		t.Fatalf("fatal demotion emitted duplicate attention: %q", output)
	default:
	}
}

func TestRunWakeLoopResumesQueuedSubmitWithoutRetypingDoorbell(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "queued-submit",
			From:    "claude",
			To:      []string{"codex"},
			Thread:  "p2p/claude__codex",
			Subject: "queued while codex is busy",
			Created: "2026-07-29T18:17:38Z",
		},
		Body: "body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(t, root, "codex", "queued-submit.md", data); err != nil {
		t.Fatal(err)
	}

	var readerStalled atomic.Bool
	readerStalled.Store(true)
	stubRawInputDrained(t, func(timeout time.Duration, pollInterval time.Duration) (time.Duration, bool, error) {
		if timeout == rawInjectDrainTimeout {
			return 0, true, nil
		}
		return timeout, !readerStalled.Load(), nil
	})
	stubRawInjectSleep(t)
	writes := make(chan string, 8)
	ticks := make(chan time.Time)
	stop := make(chan struct{})
	done := make(chan error, 1)
	var nowNanos atomic.Int64
	nowNanos.Store(time.Unix(1_800_000_000, 0).UnixNano())

	go func() {
		done <- runWakeLoop(wakeConfig{
			root:             root,
			me:               "codex",
			session:          "session1",
			wakeOwner:        &wakeOwner{},
			injectMode:       wakeInjectModeRaw,
			controlStop:      stop,
			maintenanceTicks: ticks,
			doorbellNow: func() time.Time {
				return time.Unix(0, nowNanos.Load())
			},
			preconditionCheck: func(*wakeConfig) error {
				return nil
			},
			terminalWrite: func(text string) error {
				writes <- text
				return nil
			},
			attentionIsTTY: func() bool { return false },
		})
	}()

	for _, want := range []string{coopWakeDoorbell, "\n", "\r"} {
		select {
		case got := <-writes:
			if got != want {
				t.Fatalf("initial write = %q, want %q", got, want)
			}
		case err := <-done:
			t.Fatalf("wake loop exited during initial doorbell: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatalf("wake loop did not write %q", want)
		}
	}

	ticks <- time.Now()
	select {
	case got := <-writes:
		t.Fatalf("stalled maintenance stacked terminal input %q", got)
	case err := <-done:
		t.Fatalf("wake loop exited while reader stalled: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	readerStalled.Store(false)
	nowNanos.Add(int64(wakeDoorbellRetryBase))
	ticks <- time.Now()
	select {
	case got := <-writes:
		if got != "\r" {
			t.Fatalf("resumed write = %q, want only rescue CR", got)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before reader resumed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not resume queued submit")
	}

	close(stop)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wake loop stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not stop")
	}
	select {
	case got := <-writes:
		t.Fatalf("resumed submission retyped extra input %q", got)
	default:
	}
}

func TestBaselineDLQRetryWithSameFilenameRemainsNotifyEligible(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema: 1, ID: "stale", From: "codex", To: []string{"alice"},
			Thread: "p2p/alice__codex", Subject: "stale", Created: "2026-07-22T00:00:00Z",
		},
		Body: "body",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(root, "alice"), "same.md"), data, 0o600); err != nil {
		t.Fatalf("write message: %v", err)
	}

	cfg, outputPath := injectViaCaptureConfig(t)
	cfg.me = "alice"
	cfg.root = root
	cfg.previewLen = 48
	baseline, err := snapshotWakeExistingMessages(root, "alice")
	if err != nil {
		t.Fatalf("snapshot baseline: %v", err)
	}
	target := mustNewWakeTargetForTest(t, root, "alice", cfg.injectVia, cfg.injectArgs)
	lock := bindWakeLockToTarget(wakeLock{
		Root:       canonicalWakeRoot(root),
		Agent:      "alice",
		Generation: "dlq-retry-generation",
		BootID:     wakeRepairTestBootID,
	}, target)
	floor, err := newWakeRepairFloor(root, "alice", lock, target, baseline)
	if err != nil {
		t.Fatalf("new wake repair floor: %v", err)
	}
	if err := writeWakeRepairFloor(root, "alice", floor); err != nil {
		t.Fatalf("write wake repair floor: %v", err)
	}
	persisted, exists, err := readWakeRepairFloor(root, "alice")
	if err != nil || !exists {
		t.Fatalf("read wake repair floor: exists=%v err=%v", exists, err)
	}
	cfg.baselineExisting = persisted.Existing
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notify stale baseline: %v", err)
	}
	if got, err := os.ReadFile(outputPath); err == nil || !os.IsNotExist(err) || len(got) != 0 {
		t.Fatalf("baseline message notified before DLQ retry: bytes=%d err=%v", len(got), err)
	}

	rootIdentity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatalf("snapshot delivery root: %v", err)
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, rootIdentity)
	if err != nil {
		t.Fatalf("open delivery root: %v", err)
	}
	defer func() { _ = deliveryRoot.Close() }()
	dlqPath, err := fsq.MoveToDLQ(deliveryRoot, "alice", "same.md", "stale", "test", "retry identity")
	if err != nil {
		t.Fatalf("move baseline message to DLQ: %v", err)
	}
	if err := fsq.RetryFromDLQ(deliveryRoot, "alice", filepath.Base(dlqPath), false); err != nil {
		t.Fatalf("retry baseline message from DLQ: %v", err)
	}
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notify DLQ retry: %v", err)
	}
	if got, err := os.ReadFile(outputPath); err != nil || len(got) == 0 {
		t.Fatalf("same-name DLQ retry did not notify: bytes=%d err=%v", len(got), err)
	}
}

func TestRunWakeWithLoopNoneSkipsTTYAndWritesReadyFile(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-mode", "none",
		"--ready-file", readyPath,
	}, func(cfg wakeConfig) error {
		if cfg.injectMode != wakeInjectModeNone {
			t.Fatalf("injectMode = %q, want none", cfg.injectMode)
		}
		if err := cfg.onPrepared(nil); err != nil {
			t.Fatalf("publish readiness: %v", err)
		}
		if _, statErr := os.Stat(readyPath); statErr != nil {
			t.Fatalf("expected ready file after wake preparation: %v", statErr)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopPersistsEffectiveAutoMode(t *testing.T) {
	stubWakeTTYSupport(t)

	for _, tc := range []struct {
		name string
		me   string
		want string
	}{
		{name: "claude uses raw", me: "claude", want: wakeInjectModeRaw},
		{name: "other handles use paste", me: "grok", want: wakeInjectModePaste},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatalf("EnsureRootDirs: %v", err)
			}
			if err := fsq.EnsureAgentDirs(root, tc.me); err != nil {
				t.Fatalf("EnsureAgentDirs: %v", err)
			}

			errDone := errors.New("done")
			err := runWakeWithLoop([]string{
				"--root", root,
				"--me", tc.me,
				"--inject-mode", "auto",
			}, func(cfg wakeConfig) error {
				lockPath := filepath.Join(fsq.AgentBase(root, tc.me), ".wake.lock")
				data, readErr := os.ReadFile(lockPath)
				if readErr != nil {
					t.Fatalf("read wake lock: %v", readErr)
				}
				var lock wakeLock
				if unmarshalErr := json.Unmarshal(data, &lock); unmarshalErr != nil {
					t.Fatalf("unmarshal wake lock: %v", unmarshalErr)
				}
				if lock.WakeMode != tc.want {
					t.Fatalf("WakeMode = %q, want %q", lock.WakeMode, tc.want)
				}
				return errDone
			})
			if !errors.Is(err, errDone) {
				t.Fatalf("expected loop sentinel error, got %v", err)
			}
		})
	}
}

func TestRunWakeWithLoopDisabledTIOCSTIPersistsEffectiveAutoModeNone(t *testing.T) {
	stubWakeTTYSupport(t)
	readTIOCSTILegacySysctl = func() ([]byte, error) {
		return []byte("0\n"), nil
	}

	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "claude",
		"--inject-mode", "auto",
	}, func(cfg wakeConfig) error {
		lockPath := filepath.Join(fsq.AgentBase(root, "claude"), ".wake.lock")
		data, readErr := os.ReadFile(lockPath)
		if readErr != nil {
			t.Fatalf("read wake lock: %v", readErr)
		}
		var lock wakeLock
		if unmarshalErr := json.Unmarshal(data, &lock); unmarshalErr != nil {
			t.Fatalf("unmarshal wake lock: %v", unmarshalErr)
		}
		if lock.WakeMode != wakeInjectModeNone {
			t.Fatalf("WakeMode = %q, want none", lock.WakeMode)
		}
		p, readErr := presence.Read(root, "claude")
		if readErr != nil {
			t.Fatalf("read durable notifier status: %v", readErr)
		}
		if p.NotifierStatus != wakeInjectorUnsupportedStatus ||
			p.NotifierMode != wakeInjectModeRaw ||
			!strings.Contains(p.NotifierReason, tiocstiLegacySysctlPath) {
			t.Fatalf("durable notifier status = %#v", p)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeHelpHidesInternalReadyFlags(t *testing.T) {
	stdout, _, err := captureWakeRepairOutput(t, func() error {
		return runWake([]string{"--help"})
	})
	if err != nil {
		t.Fatalf("runWake --help: %v", err)
	}
	for _, hidden := range []string{"ready-file", "accept-existing-wake", "repair-lineage"} {
		if strings.Contains(stdout, hidden) {
			t.Fatalf("wake help should hide %s:\n%s", hidden, stdout)
		}
	}
	if !strings.Contains(stdout, "inject-cmd") {
		t.Fatalf("wake help should keep --inject-cmd visible:\n%s", stdout)
	}
	if !strings.Contains(stdout, "baseline-existing") {
		t.Fatalf("wake help should keep --baseline-existing visible:\n%s", stdout)
	}
	if !strings.Contains(stdout, "none") || !strings.Contains(stdout, "zero terminal input") {
		t.Fatalf("wake help should document none as zero-input mode:\n%s", stdout)
	}
	if !strings.Contains(stdout, "real SIGINT to the foreground process group") ||
		!strings.Contains(stdout, "can interrupt or crash the agent") {
		t.Fatalf("wake help should explain the destructive ctrl-c opt-in:\n%s", stdout)
	}
}

func TestRunWakeWithLoopWritesInjectViaWakeTarget(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	injector := writeExecutableForTest(t, "injector")
	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--inject-arg", "exec",
	}, func(cfg wakeConfig) error {
		target, exists, targetErr := readWakeTarget(root, "orchestrator")
		if targetErr != nil {
			t.Fatalf("readWakeTarget: %v", targetErr)
		}
		if !exists {
			t.Fatal("expected wake target to be written")
		}
		if target.InjectVia != injector || strings.Join(target.InjectArgs, "|") != "exec" {
			t.Fatalf("unexpected target: %#v", target)
		}
		if target.Owner != nil {
			t.Fatalf("generic inject-via wake target should not record owner: %#v", target.Owner)
		}
		if cfg.wakeOwner != nil {
			t.Fatalf("generic inject-via wake config should not record owner: %#v", cfg.wakeOwner)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopPersistsInjectViaWakeOwnerFromEnv(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	ownerEnv, err := encodeWakeOwnerEnv(owner)
	if err != nil {
		t.Fatalf("encodeWakeOwnerEnv: %v", err)
	}
	t.Setenv(envWakeOwner, ownerEnv)
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		if got != owner {
			t.Fatalf("observed owner = %#v, want %#v", got, owner)
		}
		return liveWakeOwnerObservationForTest(), nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

	injector := writeExecutableForTest(t, "injector")
	errDone := errors.New("done")
	err = runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
	}, func(cfg wakeConfig) error {
		if cfg.wakeOwner == nil || *cfg.wakeOwner != owner {
			t.Fatalf("cfg.wakeOwner = %#v, want %#v", cfg.wakeOwner, owner)
		}
		target, exists, targetErr := readWakeTarget(root, "orchestrator")
		if targetErr != nil {
			t.Fatalf("readWakeTarget: %v", targetErr)
		}
		if !exists {
			t.Fatal("expected wake target to be written")
		}
		if target.Owner == nil || *target.Owner != owner {
			t.Fatalf("target.Owner = %#v, want %#v", target.Owner, owner)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopSupervisesOwnerBeforeGenericLockAndMergesDone(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatal(err)
	}
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	encoded, err := encodeWakeOwnerEnv(owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envWakeOwner, encoded)
	ownerDone := make(chan struct{})
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		if got != owner {
			t.Fatalf("observed owner = %#v, want %#v", got, owner)
		}
		if inspection := inspectWakeLock(root, "orchestrator"); inspection.Exists {
			t.Fatalf("owner observation happened after lock publication: %#v", inspection)
		}
		return wakeOwnerObservation{
			State: wakeOwnerSame,
			done:  ownerDone,
		}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

	errDone := errors.New("done")
	err = runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-mode", wakeInjectModeNone,
	}, func(cfg wakeConfig) error {
		if cfg.wakeOwner == nil || *cfg.wakeOwner != owner {
			t.Fatalf("cfg.wakeOwner = %#v, want %#v", cfg.wakeOwner, owner)
		}
		if cfg.controlStop == nil {
			t.Fatal("owner observation Done was not merged into controlStop")
		}
		close(ownerDone)
		select {
		case <-cfg.controlStop:
		case <-time.After(time.Second):
			t.Fatal("owner exit did not stop the generic wake")
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("wake result = %v, want loop sentinel", err)
	}
}

func TestRunWakeWithLoopDistinguishesOwnerExitFromMonitorFailure(t *testing.T) {
	monitorFailure := errors.New("owner monitor failed")
	loopFailure := errors.New("wake loop failed")
	tests := []struct {
		name       string
		monitorErr error
		loopErr    error
	}{
		{
			name: "owner exit is a normal stop",
		},
		{
			name:       "monitor failure is returned",
			monitorErr: monitorFailure,
		},
		{
			name:       "monitor and loop failures are both returned",
			monitorErr: monitorFailure,
			loopErr:    loopFailure,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
				t.Fatal(err)
			}
			owner := wakeOwner{
				PID:          4242,
				ProcessStart: "12345",
				BootID:       "11111111-1111-1111-1111-111111111111",
				SessionID:    99,
			}
			encoded, err := encodeWakeOwnerEnv(owner)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(envWakeOwner, encoded)

			monitor := newWakeOwnerObservationMonitor(nil)
			oldObserve := observeAuthoritativeWakeOwner
			observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
				if got != owner {
					t.Fatalf("observed owner = %#v, want %#v", got, owner)
				}
				return wakeOwnerObservation{
					State:   wakeOwnerSame,
					monitor: monitor,
				}, nil
			}
			t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

			err = runWakeWithLoop([]string{
				"--root", root,
				"--me", "orchestrator",
				"--inject-mode", wakeInjectModeNone,
			}, func(cfg wakeConfig) error {
				monitor.finish(test.monitorErr)
				select {
				case <-cfg.controlStop:
					return test.loopErr
				case <-time.After(time.Second):
					t.Fatal("owner observation did not stop the wake loop")
					return nil
				}
			})
			if test.monitorErr == nil {
				if err != nil {
					t.Fatalf("normal owner exit returned error: %v", err)
				}
				return
			}
			if !errors.Is(err, test.monitorErr) {
				t.Fatalf("monitor failure result = %v, want %v", err, test.monitorErr)
			}
			if test.loopErr != nil && !errors.Is(err, test.loopErr) {
				t.Fatalf("joined result = %v, want loop failure %v", err, test.loopErr)
			}
		})
	}
}

func TestRunWakeWithLoopRejectsSameOwnerWithoutLifetimeSignalBeforeLock(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatal(err)
	}
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	encoded, err := encodeWakeOwnerEnv(owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envWakeOwner, encoded)
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(wakeOwner) (wakeOwnerObservation, error) {
		return wakeOwnerObservation{State: wakeOwnerSame}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

	loopCalled := false
	err = runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-mode", wakeInjectModeNone,
	}, func(wakeConfig) error {
		loopCalled = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "lifetime signal") {
		t.Fatalf("wake result = %v, want missing-lifetime refusal", err)
	}
	if loopCalled {
		t.Fatal("missing owner lifetime signal reached wake loop")
	}
	if inspection := inspectWakeLock(root, "orchestrator"); inspection.Exists {
		t.Fatalf("missing owner lifetime signal published lock: %#v", inspection)
	}
}

func TestValidateWakeReadyFileAgainstOwnerReobservesGenericOwner(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatal(err)
	}
	cleanup, err := acquireWakeLockWithOptions(root, "orchestrator", wakeLockAcquireOptions{
		wakeMode: wakeInjectModeNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	inspection := inspectWakeLock(root, "orchestrator")
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	if err := writeWakeReadyFile(root, "orchestrator", readyPath, inspection); err != nil {
		t.Fatal(err)
	}
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	observations := 0
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		observations++
		if got != owner {
			t.Fatalf("observed owner = %#v, want %#v", got, owner)
		}
		return liveWakeOwnerObservationForTest(), nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

	ready, err := validateWakeReadyFileAgainstOwner(
		root,
		"orchestrator",
		readyPath,
		&owner,
	)
	if err != nil || !ready {
		t.Fatalf("generic owner readiness = %v, err=%v", ready, err)
	}
	if observations != 1 {
		t.Fatalf("generic readiness owner observations = %d, want 1", observations)
	}
}

func TestRunWakeWithLoopExecutesResolvedInjectViaPath(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	base := secureTempDirForTest(t)
	realDir := filepath.Join(base, "real-bin")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("mkdir real bin: %v", err)
	}
	injector := filepath.Join(realDir, "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write injector: %v", err)
	}
	linkDir := filepath.Join(base, "link-bin")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("symlink bin: %v", err)
	}

	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", filepath.Join(linkDir, "injector"),
	}, func(cfg wakeConfig) error {
		if cfg.injectVia != injector {
			t.Fatalf("cfg.injectVia = %q, want resolved %q", cfg.injectVia, injector)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopPersistsResolvedLeafSymlinkInjectViaPath(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	base := secureTempDirForTest(t)
	cellarDir := filepath.Join(base, "Cellar", "injector", "1.0.0", "bin")
	if err := os.MkdirAll(cellarDir, 0o700); err != nil {
		t.Fatalf("mkdir cellar bin: %v", err)
	}
	injector := filepath.Join(cellarDir, "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write injector: %v", err)
	}
	binDir := filepath.Join(base, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	link := filepath.Join(binDir, "injector")
	if err := os.Symlink(injector, link); err != nil {
		t.Fatalf("symlink injector: %v", err)
	}

	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", link,
	}, func(cfg wakeConfig) error {
		if cfg.injectVia != injector {
			t.Fatalf("cfg.injectVia = %q, want resolved %q", cfg.injectVia, injector)
		}
		target, exists, err := readWakeTarget(root, "orchestrator")
		if err != nil || !exists {
			t.Fatalf("readWakeTarget exists=%v err=%v", exists, err)
		}
		if target.InjectVia != injector {
			t.Fatalf("target inject_via = %q, want resolved %q", target.InjectVia, injector)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopRejectsUnsafeInjectViaBeforeLoop(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	injector := writeExecutableForTest(t, "injector")
	if err := os.Chmod(injector, 0o777); err != nil {
		t.Fatalf("chmod injector: %v", err)
	}
	var runErr error
	_ = captureWakeStderr(t, func() {
		runErr = runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-via", injector,
			"--inject-arg", "exec",
		}, func(cfg wakeConfig) error {
			t.Fatalf("loop should not run with unsafe inject_via: %#v", cfg)
			return nil
		})
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "group/world-writable") {
		t.Fatalf("expected unsafe inject_via rejection, got %v", runErr)
	}
	if _, exists, targetErr := readWakeTarget(root, "orchestrator"); targetErr != nil || exists {
		t.Fatalf("wake target exists=%v err=%v, want absent with no read error", exists, targetErr)
	}
}

func TestInjectViaRevalidatesExecutableBeforeExec(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(t *testing.T, path string)
		wantText string
	}{
		{
			name: "symlink",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				target := writeExecutableForTest(t, "target-injector")
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove injector: %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("symlink injector: %v", err)
				}
			},
			wantText: "must be a resolved path",
		},
		{
			name: "nonregular",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove injector: %v", err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("mkdir injector path: %v", err)
				}
			},
			wantText: "must be a regular file",
		},
		{
			name: "world_writable",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Chmod(path, 0o777); err != nil {
					t.Fatalf("chmod injector: %v", err)
				}
			},
			wantText: "group/world-writable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			injector := writeExecutableForTest(t, "injector-"+tc.name)
			cfg := &wakeConfig{
				injectVia:     injector,
				injectTimeout: time.Second,
			}
			tc.mutate(t, injector)

			err := injectVia(cfg, "payload")
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("expected %q rejection, got %v", tc.wantText, err)
			}
		})
	}
}

func TestRunWakeWithLoopKeepsOldWakeTargetWhenNewTargetIsUnsafe(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	oldInjector := writeExecutableForTest(t, "old-injector")
	if err := writeWakeTarget(root, "orchestrator", mustNewWakeTargetForTest(t, root, "orchestrator", oldInjector, []string{"old"})); err != nil {
		t.Fatalf("write old wake target: %v", err)
	}
	newInjector := writeExecutableForTest(t, "new-injector")
	if err := os.Chmod(newInjector, 0o777); err != nil {
		t.Fatalf("chmod injector: %v", err)
	}
	var runErr error
	_ = captureWakeStderr(t, func() {
		runErr = runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-via", newInjector,
		}, func(cfg wakeConfig) error {
			t.Fatalf("loop should not run with unsafe inject_via: %#v", cfg)
			return nil
		})
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "group/world-writable") {
		t.Fatalf("expected unsafe inject_via rejection, got %v", runErr)
	}
	target, exists, err := readWakeTarget(root, "orchestrator")
	if err != nil || !exists {
		t.Fatalf("wake target exists=%v err=%v, want old target retained", exists, err)
	}
	if target.InjectVia != oldInjector {
		t.Fatalf("wake target inject_via = %q, want old target %q", target.InjectVia, oldInjector)
	}
}

func TestRunWakeWithLoopRejectsInjectorSwappedAfterTargetWrite(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	injector := writeExecutableForTest(t, "injector")
	if err := writeWakeTarget(root, "orchestrator", mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})); err != nil {
		t.Fatalf("write wake target: %v", err)
	}
	if err := os.Remove(injector); err != nil {
		t.Fatalf("remove injector: %v", err)
	}
	nonExecutable := filepath.Join(secureTempDirForTest(t), "non-executable-injector")
	if err := os.WriteFile(nonExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write non-executable injector: %v", err)
	}
	if err := os.Symlink(nonExecutable, injector); err != nil {
		t.Fatalf("swap injector to symlink: %v", err)
	}

	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--inject-arg", "exec",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run after injector swap: %#v", cfg)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("expected swapped injector rejection, got %v", err)
	}
}

func TestRunWakeWithLoopDoesNotWriteReadyFileWhenLockBlocked(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--ready-file", readyPath,
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an existing live wake lock: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected existing wake lock error")
	}
	if !strings.Contains(err.Error(), "wake already running") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file should not exist, statErr=%v", statErr)
	}
}

func TestRunWakeWithLoopWaitsForCurrentPreparedMarkerPastStaleGeneration(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, nil)
	writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		TTY:          "tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		Generation:   "generation-1",
	}, target))
	if err := writeWakeTarget(root, "orchestrator", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	run := func() error {
		return runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-via", injector,
			"--ready-file", readyPath,
			"--accept-existing-wake",
		}, func(cfg wakeConfig) error {
			t.Fatalf("loop should not run with an existing live wake lock: %#v", cfg)
			return nil
		})
	}

	if err := writeWakeGenerationFile(wakePreparedPath(root, "orchestrator"), "wake prepared marker", wakeReady{
		Schema:       wakeReadySchema,
		Generation:   "previous-generation",
		TargetDigest: mustWakeTargetDigest(target),
	}); err != nil {
		t.Fatalf("write previous-generation prepared marker: %v", err)
	}
	inspection := inspectWakeLock(root, "orchestrator")
	if err := writeWakeReadyFileForPreparedWake(root, "orchestrator", readyPath, inspection, time.Now().Add(40*time.Millisecond)); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("stale prepared marker deadline error = %v", err)
	}

	retryEntered := make(chan struct{}, 1)
	originalRetry := waitForWakePreparedRetry
	waitForWakePreparedRetry = func(deadline time.Time) bool {
		select {
		case retryEntered <- struct{}{}:
		default:
		}
		return originalRetry(deadline)
	}
	t.Cleanup(func() { waitForWakePreparedRetry = originalRetry })
	done := make(chan error, 1)
	go func() { done <- run() }()
	select {
	case <-retryEntered:
	case err := <-done:
		t.Fatalf("existing wake returned instead of polling past the stale marker: %v", err)
	case <-time.After(time.Second):
		t.Fatal("existing wake never entered prepared-marker polling")
	}
	writeWakePreparedForTest(t, root, "orchestrator")
	if err := <-done; err != nil {
		t.Fatalf("expected existing usable wake to satisfy ready file, got %v", err)
	}
	if _, statErr := os.Stat(readyPath); statErr != nil {
		t.Fatalf("ready file should exist, statErr=%v", statErr)
	}
	current := inspectWakeLock(root, "orchestrator")
	if err := writeWakeGenerationFile(wakePreparedPath(root, "orchestrator"), "wake prepared marker", wakeReady{
		Schema:       wakeReadySchema,
		Generation:   current.Lock.Generation,
		TargetDigest: "same-generation-wrong-target",
	}); err != nil {
		t.Fatalf("write same-generation invalid marker: %v", err)
	}
	if _, err := validateWakePreparedFileAgainstInspection(root, "orchestrator", current); err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("same-generation target mismatch was not rejected: %v", err)
	}
}

func TestRunWakeWithLoopRetriesCreatingWakeUntilPrepared(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID: pid, Running: true, StartToken: "start-1", BootID: "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	lockPath := filepath.Join(fsq.AgentBase(root, "orchestrator"), ".wake.lock")
	if err := os.WriteFile(lockPath, []byte("{"), 0o600); err != nil {
		t.Fatalf("write creating lock: %v", err)
	}
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, nil)
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	retryEntered := make(chan struct{}, 1)
	originalRetry := waitForWakePreparedRetry
	waitForWakePreparedRetry = func(deadline time.Time) bool {
		select {
		case retryEntered <- struct{}{}:
		default:
		}
		return originalRetry(deadline)
	}
	t.Cleanup(func() { waitForWakePreparedRetry = originalRetry })
	done := make(chan error, 1)
	go func() {
		done <- runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-via", injector,
			"--ready-file", readyPath,
			"--accept-existing-wake",
		}, func(cfg wakeConfig) error {
			t.Errorf("loop should not run with an existing wake: %#v", cfg)
			return nil
		})
	}()
	select {
	case <-retryEntered:
	case err := <-done:
		t.Fatalf("helper returned instead of retrying transient lock creation: %v", err)
	case <-time.After(time.Second):
		t.Fatal("helper never retried transient lock creation")
	}
	if err := writeWakeTarget(root, "orchestrator", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID: wakePID, TTY: "tty", ProcessStart: "start-1", BootID: "boot-1",
		Executable: "/opt/homebrew/bin/amq", Generation: "generation-1",
	}, target))
	writeWakePreparedForTest(t, root, "orchestrator")
	if err := <-done; err != nil {
		t.Fatalf("creating wake did not become reusable: %v", err)
	}
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatalf("ready file missing: %v", err)
	}
}

func TestRunWakeWithLoopNoneRejectsExistingInputWake(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--inject-mode", "auto"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "test-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-mode", "none",
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an existing input wake: %#v", cfg)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "requested --inject-mode none") {
		t.Fatalf("error = %v, want zero-input existing-wake refusal", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file should not exist, statErr=%v", statErr)
	}
}

func TestRunWakeWithLoopNoneAcceptsExistingNoneWake(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--inject-mode", "none"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeInjectModeNone,
		Generation:   "generation-1",
	})
	writeWakePreparedForTest(t, root, "orchestrator")
	stalePath := filepath.Join(fsq.AgentInboxNew(root, "orchestrator"), "stale.md")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale message: %v", err)
	}

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	var err error
	stderr := captureWakeStderr(t, func() {
		err = runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-mode", "none",
			"--baseline-existing",
			"--ready-file", readyPath,
			"--accept-existing-wake",
		}, func(cfg wakeConfig) error {
			t.Fatalf("loop should not run with an existing none wake: %#v", cfg)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("expected existing none wake to satisfy readiness, got %v", err)
	}
	if _, statErr := os.Stat(readyPath); statErr != nil {
		t.Fatalf("ready file should exist, statErr=%v", statErr)
	}
	if !strings.Contains(stderr, "this launch did not re-baseline it, so pending backlog may still notify") {
		t.Fatalf("reuse warning missing from stderr: %q", stderr)
	}
	if _, statErr := os.Stat(stalePath); statErr != nil {
		t.Fatalf("reused wake moved or removed stale backlog: %v", statErr)
	}
}

func TestRequireWakeLockUsableRejectsNoneForNonNone(t *testing.T) {
	inspection := wakeLockInspection{
		Exists:            true,
		Status:            wakeLockValid,
		IdentityConfirmed: true,
		Agent:             "codex",
		Lock:              wakeLock{WakeMode: wakeInjectModeNone, TTY: "test-tty"},
	}
	if err := requireWakeLockUsable(inspection, wakeInjectModeRaw, nil); err == nil {
		t.Fatal("expected a none wake to be rejected for raw mode")
	}
}

func TestRequireWakeLockUsableRawVsPaste(t *testing.T) {
	inspection := wakeLockInspection{
		Exists:            true,
		Status:            wakeLockValid,
		IdentityConfirmed: true,
		Agent:             "codex",
		Lock:              wakeLock{WakeMode: wakeInjectModeRaw, TTY: "test-tty"},
	}
	if err := requireWakeLockUsable(inspection, wakeInjectModePaste, nil); err == nil {
		t.Fatal("expected raw and paste wakes to be incompatible")
	}
	if err := requireWakeLockUsable(inspection, wakeInjectModeRaw, nil); err != nil {
		t.Fatalf("expected matching raw wake to be usable: %v", err)
	}
}

func TestRequireWakeLockUsableLegacyModeCompatibility(t *testing.T) {
	inspection := wakeLockInspection{
		Exists:            true,
		Status:            wakeLockValid,
		IdentityConfirmed: true,
		Agent:             "codex",
		Lock:              wakeLock{TTY: "test-tty"},
	}

	for _, tc := range []struct {
		name         string
		requiredMode string
		wantErr      bool
	}{
		{name: "raw accepted", requiredMode: wakeInjectModeRaw},
		{name: "paste accepted", requiredMode: wakeInjectModePaste},
		{name: "none rejected", requiredMode: wakeInjectModeNone, wantErr: true},
		{name: "inject-via rejected", requiredMode: wakeTargetInjectVia, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireWakeLockUsable(inspection, tc.requiredMode, nil)
			if tc.wantErr && err == nil {
				t.Fatalf("expected legacy wake to reject %q", tc.requiredMode)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected legacy wake to accept %q: %v", tc.requiredMode, err)
			}
		})
	}
}

func TestRequireWakeLockUsableModeMatrix(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "matrix-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec", "fixed"})
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	modes := []string{"", wakeInjectModeNone, wakeInjectModeRaw, wakeInjectModePaste, wakeTargetInjectVia}
	compatible := func(existing, requested string) bool {
		if existing == "" {
			return requested == wakeInjectModeRaw || requested == wakeInjectModePaste
		}
		return existing == requested
	}

	for _, existing := range modes {
		for _, requested := range modes[1:] {
			name := fmt.Sprintf("existing=%s/requested=%s", emptyAsLegacy(existing), requested)
			t.Run(name, func(t *testing.T) {
				lock := wakeLock{
					Root:     canonicalWakeRoot(root),
					Agent:    "codex",
					WakeMode: existing,
					TTY:      "test-tty",
				}
				if existing == wakeTargetInjectVia {
					lock = bindWakeLockToTarget(lock, target)
				}
				inspection := wakeLockInspection{
					Exists:            true,
					Status:            wakeLockValid,
					IdentityConfirmed: true,
					Root:              canonicalWakeRoot(root),
					Agent:             "codex",
					Lock:              lock,
				}
				var requestedTarget *wakeTarget
				if requested == wakeTargetInjectVia {
					requestedTarget = &target
				}
				err := requireWakeLockUsable(inspection, requested, requestedTarget)
				if compatible(existing, requested) && err != nil {
					t.Fatalf("compatible mode pair rejected: %v", err)
				}
				if !compatible(existing, requested) && err == nil {
					t.Fatal("incompatible mode pair accepted")
				}
			})
		}
	}
}

func emptyAsLegacy(mode string) string {
	if mode == "" {
		return "legacy-empty"
	}
	return mode
}

func TestRunWakeWithLoopAcceptExistingWakeRejectsMissingTTY(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "/dev/amq-missing-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeTargetInjectVia,
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an unusable existing wake lock: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected unusable wake lock error")
	}
	if !strings.Contains(err.Error(), "not usable for requested wake readiness") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file should not exist, statErr=%v", statErr)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("existing lock should remain, statErr=%v", statErr)
	}
}

func TestRunWakeWithLoopAcceptExistingWakeRejectsBlankOrUnknownTTY(t *testing.T) {
	for _, tc := range []struct {
		name string
		tty  string
	}{
		{name: "blank", tty: ""},
		{name: "unknown", tty: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const wakePID = 4242
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				if pid == wakePID {
					return wakeProcessInfo{
						PID:        pid,
						Running:    true,
						StartToken: "start-1",
						BootID:     "boot-1",
						Executable: "/opt/homebrew/bin/amq",
						Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
					}
				}
				return wakeProcessInfo{PID: pid}
			})
			root := secureTempDirForTest(t)
			lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
				PID:          wakePID,
				TTY:          tc.tty,
				ProcessStart: "start-1",
				BootID:       "boot-1",
				Executable:   "/opt/homebrew/bin/amq",
				WakeMode:     wakeTargetInjectVia,
			})

			readyPath := filepath.Join(t.TempDir(), "wake.ready")
			injector := writeExecutableForTest(t, "injector")
			err := runWakeWithLoop([]string{
				"--root", root,
				"--me", "orchestrator",
				"--inject-via", injector,
				"--ready-file", readyPath,
				"--accept-existing-wake",
			}, func(cfg wakeConfig) error {
				t.Fatalf("loop should not run with an unusable existing wake lock: %#v", cfg)
				return nil
			})
			if err == nil {
				t.Fatal("expected unusable wake lock error")
			}
			if !strings.Contains(err.Error(), "not usable for requested wake readiness") {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
				t.Fatalf("ready file should not exist, statErr=%v", statErr)
			}
			if _, statErr := os.Stat(lockPath); statErr != nil {
				t.Fatalf("existing lock should remain, statErr=%v", statErr)
			}
		})
	}
}

func TestRunWakeWithLoopAcceptExistingWakeAcceptsInjectViaUnknownTTY(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--inject-via", injector},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		Generation:   "generation-1",
	}, target))
	if err := writeWakeTarget(root, "orchestrator", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	writeWakePreparedForTest(t, root, "orchestrator")

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--inject-arg", "exec",
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an existing live wake lock: %#v", cfg)
		return nil
	})
	if err != nil {
		t.Fatalf("expected inject-via wake to satisfy ready file despite unknown tty, got %v", err)
	}
	if _, statErr := os.Stat(readyPath); statErr != nil {
		t.Fatalf("ready file should exist, statErr=%v", statErr)
	}
}

func TestRunWakeWithLoopAcceptExistingWakeRejectsDifferentInjector(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	existingInjector := writeExecutableForTest(t, "existing-injector")
	requestedInjector := writeExecutableForTest(t, "requested-injector")
	existingTarget := mustNewWakeTargetForTest(t, root, "orchestrator", existingInjector, []string{"exec", "fixed"})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--inject-via", existingInjector},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	}, existingTarget))
	if err := writeWakeTarget(root, "orchestrator", existingTarget); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", requestedInjector,
		"--inject-arg", "exec",
		"--inject-arg", "fixed",
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with a different existing injector: %#v", cfg)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "injector") {
		t.Fatalf("different injector error = %v", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file should not exist, statErr=%v", statErr)
	}
}

func TestRunWakeWithLoopAcceptExistingWakeRejectsSameTTYDifferentSession(t *testing.T) {
	const wakePID = 4242
	ttyPath := filepath.Join(t.TempDir(), "amq-test-tty")
	if err := os.WriteFile(ttyPath, []byte{}, 0o600); err != nil {
		t.Fatalf("write fake tty path: %v", err)
	}
	stubWakeCurrentTTY(t, func() string { return ttyPath })
	sidCalls := 0
	stubWakeProcessSID(t, func(pid int) (int, error) {
		sidCalls++
		if pid == wakePID {
			return 100, nil
		}
		return 200, nil
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          ttyPath,
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeTargetInjectVia,
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an unusable existing wake lock: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected unusable wake lock error")
	}
	if !strings.Contains(err.Error(), "not usable for requested wake readiness") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file should not exist, statErr=%v", statErr)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("existing lock should remain, statErr=%v", statErr)
	}
	if sidCalls < 2 {
		t.Fatalf("expected same-TTY branch to inspect wake and current SIDs, got %d calls", sidCalls)
	}
}

func TestRunWakeWithLoopSupersedesUnverifiedGenericWakeWithoutSignal(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	var signals []os.Signal
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		signals = append(signals, sig)
		return nil
	})
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "test-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	errDone := errors.New("done")
	stderr := captureWakeStderr(t, func() {
		err := runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-via", injector,
			"--ready-file", readyPath,
			"--accept-existing-wake",
		}, func(cfg wakeConfig) error {
			inspection := inspectWakeLock(root, "orchestrator")
			if !inspection.Exists || inspection.Lock.PID != os.Getpid() {
				t.Fatalf("fresh wake was not admitted: %#v", inspection)
			}
			return errDone
		})
		if !errors.Is(err, errDone) {
			t.Fatalf("runWakeWithLoop error = %v, want sentinel", err)
		}
	})
	if len(signals) != 0 {
		t.Fatalf("unverified helper was signaled: %v", signals)
	}
	if count := strings.Count(stderr, "warning:"); count != 1 {
		t.Fatalf("warning count = %d, want 1:\n%s", count, stderr)
	}
	for _, want := range []string{
		"unidentified wake helper",
		"pid 4242",
		"duplicate notifications",
		"stop that helper if duplicates persist",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("warning missing %q:\n%s", want, stderr)
		}
	}
}

func TestRunWakeWithLoopPreservesUnverifiedMode0400Claim(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
	})
	if err := os.Chmod(lockPath, wakeOwnerLockFileMode); err != nil {
		t.Fatalf("chmod owner-bound claim: %v", err)
	}
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read owner-bound claim: %v", err)
	}

	injector := writeExecutableForTest(t, "injector")
	stderr := captureWakeStderr(t, func() {
		err := runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-via", injector,
		}, func(cfg wakeConfig) error {
			t.Fatalf("loop should not run for an owner-bound claim: %#v", cfg)
			return nil
		})
		if err == nil ||
			!strings.Contains(err.Error(), "unverified") ||
			!strings.Contains(err.Error(), "wake recover-owner") {
			t.Fatalf("error = %v, want owner-bound refusal", err)
		}
	})
	if stderr != "" {
		t.Fatalf("owner-bound refusal emitted supersession warning: %q", stderr)
	}
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read preserved owner-bound claim: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("owner-bound claim changed: got %q want %q", after, before)
	}
}

func TestAcquireWakeLockSelfHealsPIDReusedByNonAMQ(t *testing.T) {
	const reusedPID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          reusedPID,
		ProcessStart: "old-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == reusedPID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "new-start",
				BootID:     "boot-1",
				Executable: "/bin/sleep",
				Args:       []string{"/bin/sleep", "100"},
			}
		}
		if pid == os.Getpid() {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "self-start",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
			}
		}
		return wakeProcessInfo{PID: pid}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock should replace stale PID-reuse lock: %v", err)
	}
	defer cleanup()

	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read replacement lock: %v", err)
	}
	var got wakeLock
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal replacement lock: %v", err)
	}
	if got.PID != os.Getpid() {
		t.Fatalf("replacement pid = %d, want %d", got.PID, os.Getpid())
	}
	if got.ProcessStart != "self-start" {
		t.Fatalf("replacement process_start = %q, want self-start", got.ProcessStart)
	}
}

func TestAcquireWakeLockSupersedesUnknownBootIdentity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lock       wakeLock
		process    wakeProcessInfo
		wantReason string
	}{
		{
			name: "boot id mismatch",
			lock: wakeLock{
				PID:          4242,
				ProcessStart: "start-1",
				BootID:       "recorded-boot",
				Executable:   "/opt/homebrew/bin/amq",
			},
			process: wakeProcessInfo{
				Running:    true,
				StartToken: "start-1",
				BootID:     "actual-boot",
				Executable: "/opt/homebrew/bin/amq",
			},
			wantReason: "boot id mismatch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const reusedPID = 4242
			root := secureTempDirForTest(t)
			lockPath := writeWakeLockForTest(t, root, "orchestrator", tc.lock)
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				if pid == reusedPID {
					proc := tc.process
					proc.PID = pid
					proc.Args = []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root}
					return proc
				}
				return wakeProcessInfo{PID: pid}
			})

			cleanup, err := acquireWakeLock(root, "orchestrator", nil)
			if err != nil {
				t.Fatalf("acquireWakeLock should supersede %s: %v", tc.wantReason, err)
			}
			defer cleanup()
			assertWakeLockOwnedByCurrentProcess(t, lockPath)
		})
	}
}

func TestAcquireWakeLockReplacesProvenStartMismatchWhenBootMatches(t *testing.T) {
	const reusedPID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          reusedPID,
		ProcessStart: "old-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == reusedPID {
			return wakeProcessInfo{
				PID:          pid,
				Running:      true,
				StartToken:   "new-start",
				BootID:       "boot-1",
				InspectError: errors.New("executable unavailable"),
			}
		}
		return wakeProcessInfo{PID: pid}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock: %v", err)
	}
	defer cleanup()
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read replacement lock: %v", err)
	}
	var got wakeLock
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal replacement lock: %v", err)
	}
	if got.PID != os.Getpid() {
		t.Fatalf("replacement pid = %d, want %d", got.PID, os.Getpid())
	}
}

func TestAcquireWakeLockSupersedesStartMismatchWhenBootIsUnknown(t *testing.T) {
	const reusedPID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          reusedPID,
		ProcessStart: "old-start",
		BootID:       "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == reusedPID {
			return wakeProcessInfo{
				PID:          pid,
				Running:      true,
				StartToken:   "new-start",
				BootID:       "100.000000000",
				Executable:   "/opt/homebrew/bin/amq",
				Args:         []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
				InspectError: errors.New("boot representations are not comparable"),
			}
		}
		return wakeProcessInfo{PID: pid}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock should supersede unknown boot identity: %v", err)
	}
	defer cleanup()
	assertWakeLockOwnedByCurrentProcess(t, lockPath)
}

func TestAcquireWakeLockSupersedesStartMismatchWithoutBootIdentity(t *testing.T) {
	const reusedPID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          reusedPID,
		ProcessStart: "old-start",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == reusedPID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "new-start",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
			}
		}
		return wakeProcessInfo{PID: pid}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock should supersede missing boot identity: %v", err)
	}
	defer cleanup()
	assertWakeLockOwnedByCurrentProcess(t, lockPath)
}

func TestAcquireWakeLockSupersedesStartReadFailure(t *testing.T) {
	const pid = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          pid,
		ProcessStart: "old-start",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		if gotPID == pid {
			return wakeProcessInfo{
				PID:          gotPID,
				Running:      true,
				Executable:   "/opt/homebrew/bin/amq",
				InspectError: errors.New("permission denied"),
			}
		}
		return wakeProcessInfo{PID: gotPID}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock should supersede process inspection failure: %v", err)
	}
	defer cleanup()
	assertWakeLockOwnedByCurrentProcess(t, lockPath)
}

func TestAcquireWakeLockSupersedesLegacyLiveLock(t *testing.T) {
	const pid = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:        pid,
		Executable: "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		if gotPID == pid {
			return wakeProcessInfo{
				PID:        gotPID,
				Running:    true,
				Executable: "/opt/homebrew/bin/amq",
			}
		}
		return wakeProcessInfo{PID: gotPID}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock should supersede legacy live lock: %v", err)
	}
	defer cleanup()
	assertWakeLockOwnedByCurrentProcess(t, lockPath)
}

func TestRemoveWakeLockIfUnchangedRefusesChangedLock(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{PID: 4242})
	inspection := inspectWakeLock(root, "orchestrator")
	if !inspection.Exists {
		t.Fatal("expected lock inspection")
	}
	changed := wakeLock{
		PID:     4243,
		Root:    canonicalWakeRoot(root),
		Agent:   "orchestrator",
		Started: time.Now().UTC().Format(time.RFC3339),
	}
	data, _ := json.Marshal(changed)
	if err := os.WriteFile(lockPath, data, 0o600); err != nil {
		t.Fatalf("write changed lock: %v", err)
	}

	err := removeWakeLockIfUnchanged(inspection)
	if err == nil {
		t.Fatal("expected changed lock removal error")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("changed lock should remain, stat=%v", statErr)
	}
}

func TestRemoveWakeLockDoesNotDeleteReplacement(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{PID: 4242, Generation: "old"})
	inspection := inspectWakeLock(root, "orchestrator")
	if !inspection.Exists {
		t.Fatal("expected lock inspection")
	}

	replacement := wakeLock{
		PID:        4242,
		Root:       canonicalWakeRoot(root),
		Agent:      "orchestrator",
		Started:    time.Now().UTC().Format(time.RFC3339),
		Generation: "old",
	}
	data, _ := json.Marshal(replacement)
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove original lock: %v", err)
	}
	if err := os.WriteFile(lockPath, data, 0o600); err != nil {
		t.Fatalf("write byte-compatible replacement lock: %v", err)
	}

	err := removeWakeLockIfUnchanged(inspection)
	if err == nil {
		t.Fatal("expected replacement generation removal refusal")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("replacement lock should remain, stat=%v", statErr)
	}
}

func TestWakeCleanupDoesNotDeleteReplacement(t *testing.T) {
	root := secureTempDirForTest(t)
	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock: %v", err)
	}
	lockPath := filepath.Join(fsq.AgentBase(root, "orchestrator"), ".wake.lock")
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read acquired lock: %v", err)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove acquired lock: %v", err)
	}
	if err := os.WriteFile(lockPath, data, 0o600); err != nil {
		t.Fatalf("write byte-compatible replacement: %v", err)
	}

	cleanup()
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("replacement lock should survive old cleanup: %v", statErr)
	}
}

func TestAcquireAndCleanupWaitForWakeLifecycleGuard(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- withWakeLifecycleGuard(root, "orchestrator", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	type acquireResult struct {
		cleanup func()
		err     error
	}
	acquired := make(chan acquireResult, 1)
	go func() {
		cleanup, err := acquireWakeLock(root, "orchestrator", nil)
		acquired <- acquireResult{cleanup: cleanup, err: err}
	}()
	time.Sleep(25 * time.Millisecond)
	lockPath := filepath.Join(fsq.AgentBase(root, "orchestrator"), ".wake.lock")
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("acquire mutated lock before lifecycle guard release: %v", err)
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatalf("guard holder: %v", err)
	}
	result := <-acquired
	if result.err != nil {
		t.Fatalf("acquireWakeLock: %v", result.err)
	}

	entered = make(chan struct{})
	release = make(chan struct{})
	holderDone = make(chan error, 1)
	go func() {
		holderDone <- withWakeLifecycleGuard(root, "orchestrator", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	cleanupDone := make(chan struct{})
	go func() {
		result.cleanup()
		close(cleanupDone)
	}()
	time.Sleep(25 * time.Millisecond)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("cleanup removed lock before lifecycle guard release: %v", err)
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatalf("guard holder: %v", err)
	}
	<-cleanupDone
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove exact lock: %v", err)
	}
}

func TestInspectWakeLockRejectsSymlinkAndFIFO(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, path string)
		wantError string
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "lock.json")
				if err := os.WriteFile(target, []byte(`{"pid":4242}`), 0o600); err != nil {
					t.Fatalf("write target lock: %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("symlink lock: %v", err)
				}
			},
			wantError: "must not be a symlink",
		},
		{
			name: "fifo",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatalf("mkfifo lock: %v", err)
				}
			},
			wantError: "must be a regular file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			agentBase := fsq.AgentBase(root, "orchestrator")
			if err := os.MkdirAll(agentBase, 0o700); err != nil {
				t.Fatalf("mkdir agent base: %v", err)
			}
			tc.setup(t, filepath.Join(agentBase, ".wake.lock"))

			done := make(chan wakeLockInspection, 1)
			go func() {
				done <- inspectWakeLock(root, "orchestrator")
			}()

			select {
			case inspection := <-done:
				if !inspection.Exists || inspection.Status != wakeLockUnverified ||
					!strings.Contains(inspection.Reason, tc.wantError) {
					t.Fatalf("unexpected inspection: %#v", inspection)
				}
			case <-time.After(250 * time.Millisecond):
				t.Fatal("inspectWakeLock blocked")
			}
		})
	}
}

func TestShouldReplaceOrphanedWakeLockSignalsOnlyAfterRevalidation(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "/dev/amq-missing-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeTargetInjectVia,
	})
	killed := false
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			if killed {
				return wakeProcessInfo{PID: pid, Running: false}
			}
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	signals := []os.Signal{}
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		if pid != wakePID {
			t.Fatalf("signal pid = %d, want %d", pid, wakePID)
		}
		signals = append(signals, sig)
		if sig == os.Kill {
			killed = true
		}
		return nil
	})

	inspection := inspectWakeLock(root, "orchestrator")
	replaced, err := shouldReplaceOrphanedWakeLock(inspection)
	if err == nil || !strings.Contains(err.Error(), "no cooperative control endpoint") {
		t.Fatalf("expected missing-control refusal, got %v", err)
	}
	if replaced {
		t.Fatal("legacy inject-via orphan must not be replaced by PID")
	}
	if len(signals) != 0 {
		t.Fatalf("signals = %d, want none", len(signals))
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock should remain after refusal, stat=%v", err)
	}
}

func TestShouldReplaceOrphanedWakeLockKeepsLockWhenKillDoesNotTerminate(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "/dev/amq-missing-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	var signals []os.Signal
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		signals = append(signals, sig)
		return nil
	})

	inspection := inspectWakeLock(root, "orchestrator")
	replaced, err := shouldReplaceOrphanedWakeLock(inspection)
	if err == nil || !strings.Contains(err.Error(), "still alive after SIGKILL") {
		t.Fatalf("expected failed automatic termination, got %v", err)
	}
	if replaced {
		t.Fatal("should not replace lock when old wake remains alive")
	}
	if len(signals) != 2 || signals[0] != syscall.SIGTERM || signals[1] != syscall.SIGKILL {
		t.Fatalf("signals = %v, want SIGTERM then SIGKILL", signals)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("lock should remain after failed kill, stat=%v", statErr)
	}
}

func TestTerminateWakeProcessPreservesLiveWakeOnUnknownBootAfterSignal(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4343
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: wakePID, TTY: "tty", ProcessStart: "start-1", BootID: "boot-1", Executable: "/opt/homebrew/bin/amq"})
	calls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		calls++
		info := wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}}
		if calls >= 3 { // after SIGTERM: still live, but boot identity is unavailable
			return info
		}
		info.BootID = "boot-1"
		return info
	})
	var signals []os.Signal
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error { signals = append(signals, sig); return nil })
	inspection := inspectWakeLock(root, "codex")
	if err := terminateWakeProcess(inspection); err == nil {
		t.Fatal("terminateWakeProcess unexpectedly declared success for live wake with unknown boot")
	}
	if len(signals) != 1 || signals[0] != syscall.SIGTERM {
		t.Fatalf("signals = %v, want only SIGTERM", signals)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock was removed or became unreadable: %v", err)
	}
}

func TestTerminatePreservesLockOnUnknownInspectionAfterSIGTERM(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4350
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: wakePID, TTY: "tty", ProcessStart: "start-1", BootID: "boot-1", Executable: "/opt/homebrew/bin/amq"})
	calls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		calls++
		if calls >= 3 {
			return wakeProcessInfo{PID: pid, Running: true, InspectError: errors.New("sysctl kinfo failed")}
		}
		return wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", BootID: "boot-1", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}}
	})
	var signals []os.Signal
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		signals = append(signals, sig)
		return nil
	})

	inspection := inspectWakeLock(root, "codex")
	err := terminateWakeProcess(inspection)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("terminateWakeProcess error = %v, want unknown-inspection error", err)
	}
	if len(signals) != 1 || signals[0] != syscall.SIGTERM {
		t.Fatalf("signals = %v, want only SIGTERM", signals)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock was removed or became unreadable: %v", err)
	}
}

func TestTerminatePreservesLockOnUnknownInspectionAfterSIGKILL(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4351
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: wakePID, TTY: "tty", ProcessStart: "start-1", BootID: "boot-1", Executable: "/opt/homebrew/bin/amq"})
	calls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		calls++
		if calls >= 5 {
			return wakeProcessInfo{PID: pid, Running: true, InspectError: errors.New("sysctl kinfo failed")}
		}
		return wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", BootID: "boot-1", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}}
	})
	var signals []os.Signal
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		signals = append(signals, sig)
		return nil
	})

	inspection := inspectWakeLock(root, "codex")
	err := terminateWakeProcess(inspection)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("terminateWakeProcess error = %v, want unknown-inspection error", err)
	}
	if len(signals) != 2 || signals[0] != syscall.SIGTERM || signals[1] != syscall.SIGKILL {
		t.Fatalf("signals = %v, want SIGTERM then SIGKILL", signals)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock was removed or became unreadable: %v", err)
	}
}

func TestTerminateWakeProcessPreservesLiveWakeOnShiftedBootClock(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4344
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: wakePID, TTY: "tty", ProcessStart: "start-1", BootID: "100.000000000", Executable: "/opt/homebrew/bin/amq"})
	calls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		calls++
		boot := "100.000000000"
		if calls >= 3 {
			boot = "200.000000000"
		}
		return wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", BootID: boot, Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}}
	})
	var signals []os.Signal
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error { signals = append(signals, sig); return nil })
	inspection := inspectWakeLock(root, "codex")
	if err := terminateWakeProcess(inspection); err == nil {
		t.Fatal("terminateWakeProcess unexpectedly declared success after a shifted boot clock")
	}
	if len(signals) != 1 || signals[0] != syscall.SIGTERM {
		t.Fatalf("signals = %v, want only SIGTERM", signals)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock was removed or became unreadable: %v", err)
	}
}

func TestTerminateWakeProcessPreservesLiveWakeOnShiftedLegacyBootInLegacyField(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4345
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: wakePID, TTY: "tty", ProcessStart: "start-1", BootID: "100.000000000", Executable: "/opt/homebrew/bin/amq"})
	calls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		calls++
		info := wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}}
		if calls >= 3 {
			info.BootID = "9C0682F4-901B-4243-8B5C-287FAFB9AD0E"
			info.LegacyBootID = "200.000000000"
			return info
		}
		info.BootID = "100.000000000"
		return info
	})
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error { return nil })
	inspection := inspectWakeLock(root, "codex")
	if err := terminateWakeProcess(inspection); err == nil {
		t.Fatal("terminateWakeProcess unexpectedly declared success after shifted legacy boot clock")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock was removed or became unreadable: %v", err)
	}
}

func TestShouldReplaceOrphanedWakeLockRevalidatesBeforeSignal(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "/dev/amq-missing-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})
	inspectCalls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			inspectCalls++
			if inspectCalls <= 2 {
				return wakeProcessInfo{
					PID:        pid,
					Running:    true,
					StartToken: "start-1",
					BootID:     "boot-1",
					Executable: "/opt/homebrew/bin/amq",
					Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
				}
			}
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "reused-start",
				BootID:     "boot-1",
				Executable: "/bin/sleep",
				Args:       []string{"/bin/sleep", "100"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		t.Fatalf("must not signal after process identity changes, got pid=%d sig=%v", pid, sig)
		return nil
	})

	inspection := inspectWakeLock(root, "orchestrator")
	replaced, err := shouldReplaceOrphanedWakeLock(inspection)
	if err == nil {
		t.Fatal("expected identity-changed error")
	}
	if replaced {
		t.Fatal("should not replace lock when process identity changes before signal")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("lock should remain after aborted signal, stat=%v", statErr)
	}
}

func TestShouldReplaceOrphanedWakeLockRefusesLegacyOwnerStateWhenOwnerGone(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4242
	const ownerPID = 7777
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	owner := wakeOwner{PID: ownerPID, ProcessStart: "owner-start", BootID: "boot-1"}
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
	target.Owner = &owner
	lockPath := writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "wake-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	}, target))
	if err := writeWakeTarget(root, "orchestrator", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}

	killed := false
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		switch pid {
		case wakePID:
			if killed {
				return wakeProcessInfo{PID: pid, Running: false}
			}
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "wake-start",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root, "--inject-via", injector},
			}
		case ownerPID:
			return wakeProcessInfo{PID: pid, Running: false}
		default:
			return wakeProcessInfo{PID: pid}
		}
	})
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		if pid != wakePID {
			t.Fatalf("signal pid = %d, want %d", pid, wakePID)
		}
		if sig == os.Kill {
			killed = true
		}
		return nil
	})

	inspection := inspectWakeLock(root, "orchestrator")
	replaced, err := shouldReplaceOrphanedWakeLock(inspection)
	if err == nil || !strings.Contains(err.Error(), "recover-owner") {
		t.Fatalf("expected owner-state refusal, got %v", err)
	}
	if replaced || killed {
		t.Fatal("legacy inject-via wake must not be terminated by PID")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock should remain after refusal, stat=%v", err)
	}
}

func TestShouldReplaceOrphanedWakeLockRefusesLegacyOwnerStateWhenOwnerMatches(t *testing.T) {
	const wakePID = 4242
	const ownerPID = 7777
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	owner := wakeOwner{PID: ownerPID, ProcessStart: "owner-start", BootID: "boot-1"}
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
	target.Owner = &owner
	lockPath := writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "wake-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	}, target))
	if err := writeWakeTarget(root, "orchestrator", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}

	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		switch pid {
		case wakePID:
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "wake-start",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root, "--inject-via", injector},
			}
		case ownerPID:
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "owner-start",
				BootID:     "boot-1",
			}
		default:
			return wakeProcessInfo{PID: pid}
		}
	})
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		t.Fatalf("must not signal owner-matched inject-via wake, got pid=%d sig=%v", pid, sig)
		return nil
	})

	inspection := inspectWakeLock(root, "orchestrator")
	replaced, err := shouldReplaceOrphanedWakeLock(inspection)
	if err == nil || !strings.Contains(err.Error(), "recover-owner") {
		t.Fatalf("expected owner-state refusal, got %v", err)
	}
	if replaced {
		t.Fatal("legacy owner-bearing wake should not be replaced")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("lock should remain for owner-matched wake, stat=%v", statErr)
	}
}

func TestRunWakeWithLoopRejectsRecentlyCorruptLock(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	lockPath := filepath.Join(fsq.AgentBase(root, "orchestrator"), ".wake.lock")
	if err := os.WriteFile(lockPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt lock: %v", err)
	}

	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", writeExecutableForTest(t, "injector"),
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with recent corrupt lock: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected recent corrupt lock error")
	}
	if !strings.Contains(err.Error(), "being created") {
		t.Fatalf("expected being-created error, got %v", err)
	}
}

func TestWaitForWakeReadyReturnsWhenReadyFileAppears(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID: os.Getpid(), Root: canonicalWakeRoot(root), Agent: "orchestrator",
		Started: time.Now().UTC().Format(time.RFC3339), Generation: "generation-1",
	})
	inspection := inspectWakeLock(root, "orchestrator")
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	cmd := exec.Command("sh", "-c", "sleep 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
	})

	if err := writeWakeReadyFile(root, "orchestrator", readyPath, inspection); err != nil {
		t.Fatalf("writeWakeReadyFile: %v", err)
	}
	if err := waitForWakeReady(cmd.Process, readyPath, root, "orchestrator", time.Second); err != nil {
		t.Fatalf("waitForWakeReady: %v", err)
	}
}

func TestWaitForWakeReadyFailsWhenWakeExitsBeforeReady(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	cmd := exec.Command("sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}

	err := waitForWakeReady(cmd.Process, readyPath, t.TempDir(), "orchestrator", time.Second)
	if err == nil {
		t.Fatal("expected readiness failure")
	}
	if !strings.Contains(err.Error(), "amq wake exited before becoming ready") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForWakeReadyAcceptsReadyFileWrittenBeforeExit(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID: os.Getpid(), Root: canonicalWakeRoot(root), Agent: "orchestrator",
		Started: time.Now().UTC().Format(time.RFC3339), Generation: "generation-1",
	})
	inspection := inspectWakeLock(root, "orchestrator")
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	if err := writeWakeReadyFile(root, "orchestrator", readyPath, inspection); err != nil {
		t.Fatalf("writeWakeReadyFile: %v", err)
	}
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}

	if err := waitForWakeReady(cmd.Process, readyPath, root, "orchestrator", time.Second); err != nil {
		t.Fatalf("waitForWakeReady should accept ready file written before exit: %v", err)
	}
}

func TestWaitForWakeReadyRefusesLegacyReadyFile(t *testing.T) {
	root := secureTempDirForTest(t)
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	if err := os.WriteFile(readyPath, []byte("ready\n"), 0o600); err != nil {
		t.Fatalf("write legacy ready file: %v", err)
	}
	cmd := exec.Command("sh", "-c", "sleep 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	err := waitForWakeReady(cmd.Process, readyPath, root, "orchestrator", time.Second)
	if err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("expected legacy readiness refusal, got %v", err)
	}
}

func TestWaitForWakeReadyRefusesEmptyReadyFile(t *testing.T) {
	root := secureTempDirForTest(t)
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	if err := os.WriteFile(readyPath, nil, 0o600); err != nil {
		t.Fatalf("write empty ready file: %v", err)
	}
	cmd := exec.Command("sh", "-c", "sleep 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	err := waitForWakeReady(cmd.Process, readyPath, root, "orchestrator", time.Second)
	if err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("expected empty readiness refusal, got %v", err)
	}
}

func TestAcceptExistingReadinessNotPublishedOnReplacement(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID: 4242, Root: canonicalWakeRoot(root), Agent: "orchestrator",
		Started: time.Now().UTC().Format(time.RFC3339), Generation: "generation-1",
	})
	expected := inspectWakeLock(root, "orchestrator")
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove original lock: %v", err)
	}
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID: 4242, Root: canonicalWakeRoot(root), Agent: "orchestrator",
		Started: time.Now().UTC().Format(time.RFC3339), Generation: "generation-2",
	})
	readyPath := filepath.Join(t.TempDir(), "wake.ready")

	err := writeWakeReadyFile(root, "orchestrator", readyPath, expected)
	if err == nil {
		t.Fatal("expected replacement readiness refusal")
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("replacement readiness must not be published, statErr=%v", statErr)
	}
}

func TestCleanupTerminatedWakeLockRemovesOnlyCapturedStaleGeneration(t *testing.T) {
	const wakePID = 4242
	running := true
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != wakePID {
			return wakeProcessInfo{PID: pid}
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    running,
			StartToken: "start-1",
			BootID:     "boot-1",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
		}
	})

	t.Run("captured generation", func(t *testing.T) {
		root := secureTempDirForTest(t)
		lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
			PID: wakePID, ProcessStart: "start-1", BootID: "boot-1",
			Executable: "/opt/homebrew/bin/amq", Generation: "generation-1",
		})
		expected := inspectWakeLock(root, "orchestrator")
		running = false
		if err := cleanupTerminatedWakeLock(expected); err != nil {
			t.Fatalf("cleanupTerminatedWakeLock: %v", err)
		}
		if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
			t.Fatalf("captured stale lock was not removed, statErr=%v", err)
		}
	})

	t.Run("replacement generation", func(t *testing.T) {
		running = true
		root := secureTempDirForTest(t)
		lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
			PID: wakePID, ProcessStart: "start-1", BootID: "boot-1",
			Executable: "/opt/homebrew/bin/amq", Generation: "generation-1",
		})
		expected := inspectWakeLock(root, "orchestrator")
		if err := os.Remove(lockPath); err != nil {
			t.Fatalf("remove captured lock: %v", err)
		}
		writeWakeLockForTest(t, root, "orchestrator", wakeLock{
			PID: wakePID, ProcessStart: "start-1", BootID: "boot-1",
			Executable: "/opt/homebrew/bin/amq", Generation: "generation-2",
		})
		running = false
		if err := cleanupTerminatedWakeLock(expected); err != nil {
			t.Fatalf("cleanup replacement: %v", err)
		}
		if _, err := os.Stat(lockPath); err != nil {
			t.Fatalf("replacement generation was removed: %v", err)
		}
	})
}

func TestTerminateWakeHelperProcessKillsWaitsAndRemovesCapturedLock(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	wakePID := cmd.Process.Pid
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != wakePID {
			return wakeProcessInfo{PID: pid}
		}
		running := syscall.Kill(pid, 0) == nil
		return wakeProcessInfo{
			PID: pid, Running: running, StartToken: "start-1", BootID: "boot-1",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
		}
	})
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID: wakePID, ProcessStart: "start-1", BootID: "boot-1",
		Executable: "/opt/homebrew/bin/amq", Generation: "generation-1",
	})
	waiter := newWakeProcessWaiter(cmd.Process)
	if err := terminateWakeHelperProcess(cmd.Process, waiter, root, "orchestrator"); err != nil {
		t.Fatalf("terminateWakeHelperProcess: %v", err)
	}
	if waiter.state == nil {
		t.Fatalf("wake helper was not waited: state=%v", waiter.state)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("terminated helper lock was not removed, statErr=%v", err)
	}
}

func TestTerminateWakeHelperProcessRemovesLockCommittedAfterFirstInspection(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	root := secureTempDirForTest(t)
	lockPath := filepath.Join(fsq.AgentBase(root, "orchestrator"), ".wake.lock")
	originalKill := killWakeHelperProcess
	killWakeHelperProcess = func(proc *os.Process) error {
		writeWakeLockForTest(t, root, "orchestrator", wakeLock{
			PID: proc.Pid, Generation: "generation-after-first-inspection",
		})
		return originalKill(proc)
	}
	t.Cleanup(func() { killWakeHelperProcess = originalKill })

	waiter := newWakeProcessWaiter(cmd.Process)
	if err := terminateWakeHelperProcess(cmd.Process, waiter, root, "orchestrator"); err != nil {
		t.Fatalf("terminateWakeHelperProcess: %v", err)
	}
	if waiter.state == nil {
		t.Fatal("wake helper was not waited")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("post-inspection child lock was not removed, statErr=%v", err)
	}
}

func TestWaitForWakeReadyRefusesLockReplacement(t *testing.T) {
	root := secureTempDirForTest(t)
	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock: %v", err)
	}
	defer cleanup()
	inspection := inspectWakeLock(root, "orchestrator")
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	if err := writeWakeReadyFile(root, "orchestrator", readyPath, inspection); err != nil {
		t.Fatalf("writeWakeReadyFile: %v", err)
	}
	lockPath := inspection.LockPath
	replacement := inspection.Lock
	replacement.Generation = "replacement-generation"
	data, _ := json.Marshal(replacement)
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove original lock: %v", err)
	}
	if err := os.WriteFile(lockPath, data, 0o600); err != nil {
		t.Fatalf("write replacement lock: %v", err)
	}
	cmd := exec.Command("sh", "-c", "sleep 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	err = waitForWakeReady(cmd.Process, readyPath, root, "orchestrator", time.Second)
	if err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("expected replacement generation refusal, got %v", err)
	}
}

func TestWaitForWakeReadyRefusesTargetReplacement(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
	cleanup, err := acquireWakeLock(root, "orchestrator", &target)
	if err != nil {
		t.Fatalf("acquireWakeLock: %v", err)
	}
	defer cleanup()
	inspection := inspectWakeLock(root, "orchestrator")
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	if err := writeWakeReadyFile(root, "orchestrator", readyPath, inspection); err != nil {
		t.Fatalf("writeWakeReadyFile: %v", err)
	}
	tampered := target
	tampered.InjectArgs = []string{"different"}
	if err := writeWakeTarget(root, "orchestrator", tampered); err != nil {
		t.Fatalf("write replacement target: %v", err)
	}
	cmd := exec.Command("sh", "-c", "sleep 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	err = waitForWakeReady(cmd.Process, readyPath, root, "orchestrator", time.Second)
	if err == nil || !strings.Contains(err.Error(), "does not match wake lock") {
		t.Fatalf("expected replacement target refusal, got %v", err)
	}
}

func TestWaitForWakeReadyRefusesStrayTargetForTargetlessLock(t *testing.T) {
	root := secureTempDirForTest(t)
	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock: %v", err)
	}
	defer cleanup()
	inspection := inspectWakeLock(root, "orchestrator")
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	if err := writeWakeReadyFile(root, "orchestrator", readyPath, inspection); err != nil {
		t.Fatalf("writeWakeReadyFile: %v", err)
	}
	injector := writeExecutableForTest(t, "injector")
	strayTarget := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
	if err := writeWakeTarget(root, "orchestrator", strayTarget); err != nil {
		t.Fatalf("write stray target: %v", err)
	}
	cmd := exec.Command("sh", "-c", "sleep 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	err = waitForWakeReady(cmd.Process, readyPath, root, "orchestrator", time.Second)
	if err == nil || !strings.Contains(err.Error(), "target does not match current wake lock") {
		t.Fatalf("expected stray target refusal for targetless lock, got %v", err)
	}
}

func TestWriteWakeReadyFileRejectsSymlink(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID: os.Getpid(), Root: canonicalWakeRoot(root), Agent: "orchestrator",
		Started: time.Now().UTC().Format(time.RFC3339), Generation: "generation-1",
	})
	inspection := inspectWakeLock(root, "orchestrator")
	dir := t.TempDir()
	target := filepath.Join(dir, "target.ready")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	readyPath := filepath.Join(dir, "wake.ready")
	if err := os.Symlink(target, readyPath); err != nil {
		t.Fatalf("symlink ready file: %v", err)
	}

	err := writeWakeReadyFile(root, "orchestrator", readyPath, inspection)
	if err == nil {
		t.Fatal("expected wake ready symlink rejection")
	}
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != "old\n" {
		t.Fatalf("symlink target changed: data=%q err=%v", got, readErr)
	}
}

func TestConfigureRepairWakeCommandDetachesOutput(t *testing.T) {
	output, err := os.OpenFile(filepath.Join(t.TempDir(), "repair.log"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	defer func() { _ = output.Close() }()

	cmd := exec.Command("amq")
	configureRepairWakeCommand(cmd, output)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("repair wake command should start in a new session: %#v", cmd.SysProcAttr)
	}
	if cmd.Stdout != output || cmd.Stderr != output {
		t.Fatalf("repair wake command should redirect stdout/stderr to repair log")
	}
	if cmd.Stdout == os.Stdout || cmd.Stderr == os.Stderr {
		t.Fatalf("repair wake command must not inherit parent stdout/stderr")
	}
}

func TestOpenWakeRepairOutputCreatesPrivateLog(t *testing.T) {
	root := secureTempDirForTest(t)
	output, err := openWakeRepairOutputForTest(root, "orchestrator")
	if err != nil {
		t.Fatalf("openWakeRepairOutput: %v", err)
	}
	path := output.Name()
	if err := output.Close(); err != nil {
		t.Fatalf("close output: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat repair output: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("repair output mode = %o, want 0600", got)
	}
}

func TestOpenCoopWakeOutputCreatesPrivateDurableLog(t *testing.T) {
	root := secureTempDirForTest(t)
	output, err := openCoopWakeOutput(root, "orchestrator")
	if err != nil {
		t.Fatalf("openCoopWakeOutput: %v", err)
	}
	path := output.Name()
	if _, err := output.WriteString("fatal wake diagnostic\n"); err != nil {
		t.Fatalf("write wake output: %v", err)
	}
	if err := output.Close(); err != nil {
		t.Fatalf("close wake output: %v", err)
	}
	if filepath.Base(path) != ".wake.log" {
		t.Fatalf("wake output path = %q, want .wake.log", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat wake output: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("wake output mode = %o, want 0600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "fatal wake diagnostic\n" {
		t.Fatalf("wake output data = %q err=%v", data, err)
	}
}

func TestOpenCoopWakeOutputRejectsSymlinkLog(t *testing.T) {
	root := secureTempDirForTest(t)
	agentBase := fsq.AgentBase(root, "orchestrator")
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		t.Fatalf("mkdir agent base: %v", err)
	}
	target := filepath.Join(t.TempDir(), "wake.log")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("write target log: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(agentBase, ".wake.log")); err != nil {
		t.Fatalf("symlink wake log: %v", err)
	}

	output, err := openCoopWakeOutput(root, "orchestrator")
	if err == nil {
		_ = output.Close()
		t.Fatal("expected symlink wake log rejection")
	}
	if !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestOpenWakeRepairOutputRejectsSymlinkLog(t *testing.T) {
	root := secureTempDirForTest(t)
	agentBase := fsq.AgentBase(root, "orchestrator")
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		t.Fatalf("mkdir agent base: %v", err)
	}
	target := filepath.Join(t.TempDir(), "repair.log")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("write target log: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(agentBase, ".wake.repair.log")); err != nil {
		t.Fatalf("symlink repair log: %v", err)
	}

	output, err := openWakeRepairOutputForTest(root, "orchestrator")
	if err == nil {
		_ = output.Close()
		t.Fatal("expected symlink repair log rejection")
	}
	if !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestOpenWakeRepairOutputRejectsFIFOWithoutBlocking(t *testing.T) {
	root := secureTempDirForTest(t)
	agentBase := fsq.AgentBase(root, "orchestrator")
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		t.Fatalf("mkdir agent base: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(agentBase, ".wake.repair.log"), 0o600); err != nil {
		t.Fatalf("mkfifo repair log: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		output, err := openWakeRepairOutputForTest(root, "orchestrator")
		if output != nil {
			_ = output.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "must be a regular file") {
			t.Fatalf("expected FIFO rejection, got %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("openWakeRepairOutput blocked on FIFO")
	}
}

func openWakeRepairOutputForTest(root, me string) (*os.File, error) {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return nil, err
	}
	defer func() { _ = agentDir.Close() }()
	return openWakeRepairOutputInDir(agentDir)
}

func TestRunWakeRepairJSONRejectsFIFOLogWithoutBlocking(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
	writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
	}, target))
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	if err := writeWakeTarget(root, "orchestrator", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	writeWakeRepairFloorForTest(t, root, "orchestrator", target, nil)
	agentBase := fsq.AgentBase(root, "orchestrator")
	if err := syscall.Mkfifo(filepath.Join(agentBase, ".wake.repair.log"), 0o600); err != nil {
		t.Fatalf("mkfifo repair log: %v", err)
	}

	stdout, _, runErr := captureWakeRepairOutput(t, func() error {
		return runWakeRepair([]string{"--root", root, "--me", "orchestrator", "--json"})
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "regular file") {
		t.Fatalf("runWakeRepair error = %v, want regular-file refusal", runErr)
	}
	var result wakeRepairResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal output: %v\nstdout: %s", err, stdout)
	}
	if result.Status != "error" || !strings.Contains(result.Reason, "regular file") {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestRunWakeWithLoopRejectsInjectArgWithoutInjectVia(t *testing.T) {
	err := runWakeWithLoop([]string{
		"--root", t.TempDir(),
		"--me", "orchestrator",
		"--inject-arg", "exec",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with invalid flags: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected usage error")
	}
	if !strings.Contains(err.Error(), "--inject-arg requires --inject-via") {
		t.Fatalf("expected inject-arg usage error, got %v", err)
	}
}

func TestRunWakeWithLoopRejectsInvalidRepairLineageFlags(t *testing.T) {
	injector := writeExecutableForTest(t, "injector")
	tests := []struct {
		name     string
		args     []string
		ownerEnv string
		want     string
	}{
		{
			name: "blank lineage",
			args: []string{"--repair-lineage= "},
			want: "--repair-lineage must not be blank",
		},
		{
			name: "requires inject via",
			args: []string{"--repair-lineage", "dead-generation"},
			want: "--repair-lineage requires --inject-via",
		},
		{
			name: "conflicts with baseline existing",
			args: []string{
				"--repair-lineage", "dead-generation",
				"--inject-via", injector,
				"--baseline-existing",
			},
			want: "--repair-lineage cannot be combined with --baseline-existing",
		},
		{
			name: "requires private handoff before owner inspection",
			args: []string{
				"--repair-lineage", "dead-generation",
				"--inject-via", injector,
			},
			ownerEnv: `{"pid":4242}`,
			want:     "wake repair requires a private source/admission handoff",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envWakeOwner, tc.ownerEnv)
			args := []string{"--root", secureTempDirForTest(t), "--me", "orchestrator"}
			args = append(args, tc.args...)
			err := runWakeWithLoop(args, func(cfg wakeConfig) error {
				t.Fatalf("loop should not run with invalid repair lineage: %#v", cfg)
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRunWakeWithLoopRejectsNoneWithInputTransports(t *testing.T) {
	injector := writeExecutableForTest(t, "injector")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "inject via", args: []string{"--inject-via", injector}, want: "--inject-via"},
		{name: "inject arg", args: []string{"--inject-arg", "exec"}, want: "--inject-arg"},
		{name: "inject cmd", args: []string{"--inject-cmd", "amq drain"}, want: "--inject-cmd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"--root", t.TempDir(), "--me", "orchestrator", "--inject-mode", "none"}
			args = append(args, tt.args...)
			err := runWakeWithLoop(args, func(cfg wakeConfig) error {
				t.Fatalf("loop should not run with invalid flags: %#v", cfg)
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "none") {
				t.Fatalf("error = %v, want none-mode conflict mentioning %s", err, tt.want)
			}
		})
	}
}

func TestRunWakeWithLoopRejectsNonPositiveInjectTimeout(t *testing.T) {
	err := runWakeWithLoop([]string{
		"--root", t.TempDir(),
		"--me", "orchestrator",
		"--inject-via", writeExecutableForTest(t, "injector"),
		"--inject-timeout", "0",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with invalid timeout: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected usage error")
	}
	if !strings.Contains(err.Error(), "--inject-timeout must be > 0") {
		t.Fatalf("expected inject-timeout usage error, got %v", err)
	}
}

func TestWakeInjectionPreconditionCheckSkipsTTYForInjectVia(t *testing.T) {
	cfg := wakeConfig{injectVia: "/tmp/injector"}
	err := wakeInjectionPreconditionCheck(&cfg, func() bool {
		return false
	})
	if err != nil {
		t.Fatalf("expected external injection health check to skip TTY, got %v", err)
	}
}

func TestWakeInjectionPreconditionCheckSkipsTTYForNoneMode(t *testing.T) {
	cfg := wakeConfig{injectMode: wakeInjectModeNone}
	err := wakeInjectionPreconditionCheck(&cfg, func() bool {
		return false
	})
	if err != nil {
		t.Fatalf("expected none mode health check to skip TTY, got %v", err)
	}
}

func TestWakeInjectionPreconditionCheckExitsWhenInjectViaOwnerGone(t *testing.T) {
	owner := wakeOwner{PID: 4242, ProcessStart: "owner-start", BootID: "boot-1"}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	cfg := wakeConfig{injectVia: "/tmp/injector", wakeOwner: &owner}
	err := wakeInjectionPreconditionCheck(&cfg, func() bool {
		return false
	})
	if err == nil {
		t.Fatal("expected owner liveness failure")
	}
	if !strings.Contains(err.Error(), "owner pid 4242 is not running") {
		t.Fatalf("unexpected owner liveness error: %v", err)
	}
}

func TestWakeInjectionPreconditionCheckExitsWhenInjectViaOwnerIdentityChanges(t *testing.T) {
	owner := wakeOwner{PID: 4242, ProcessStart: "owner-start", BootID: "boot-1", SessionID: 99}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "other-start",
			BootID:     "boot-1",
		}
	})

	cfg := wakeConfig{injectVia: "/tmp/injector", wakeOwner: &owner}
	err := wakeInjectionPreconditionCheck(&cfg, func() bool {
		return false
	})
	if err == nil {
		t.Fatal("expected owner identity failure")
	}
	if !strings.Contains(err.Error(), "owner process start changed") {
		t.Fatalf("unexpected owner identity error: %v", err)
	}
}

func TestWakeInjectionPreconditionCheckKeepsInjectViaWhenOwnerMatches(t *testing.T) {
	owner := wakeOwner{PID: 4242, ProcessStart: "owner-start", BootID: "boot-1"}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "owner-start",
			BootID:     "boot-1",
		}
	})

	cfg := wakeConfig{injectVia: "/tmp/injector", wakeOwner: &owner}
	err := wakeInjectionPreconditionCheck(&cfg, func() bool {
		return false
	})
	if err != nil {
		t.Fatalf("expected owner-matched inject-via health check to pass, got %v", err)
	}
}

func TestWakeCommandEnvCarriesOwnerToken(t *testing.T) {
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	env, err := wakeCommandEnv([]string{
		"PATH=/bin",
		envRoot + "=/old/root",
		envWakeOwner + `={"pid":111}`,
	}, "/new/root", &owner)
	if err != nil {
		t.Fatalf("wakeCommandEnv: %v", err)
	}
	if got := testEnvValue(env, envRoot); got != "/new/root" {
		t.Fatalf("%s = %q, want /new/root", envRoot, got)
	}
	var decoded wakeOwner
	if err := json.Unmarshal([]byte(testEnvValue(env, envWakeOwner)), &decoded); err != nil {
		t.Fatalf("decode %s: %v", envWakeOwner, err)
	}
	if decoded != owner {
		t.Fatalf("decoded owner = %#v, want %#v", decoded, owner)
	}

	env, err = wakeCommandEnv(env, "/raw/root", nil)
	if err != nil {
		t.Fatalf("wakeCommandEnv without owner: %v", err)
	}
	if got := testEnvValue(env, envRoot); got != "/raw/root" {
		t.Fatalf("%s = %q, want /raw/root", envRoot, got)
	}
	if got := testEnvValue(env, envWakeOwner); got != "" {
		t.Fatalf("%s should be cleared without owner, got %q", envWakeOwner, got)
	}
}

func TestWakeInjectionPreconditionCheckReportsOnlyOpenability(t *testing.T) {
	cfg := wakeConfig{}
	err := wakeInjectionPreconditionCheck(&cfg, func() bool {
		return false
	})
	if err == nil {
		t.Fatal("expected controlling-terminal precondition failure")
	}
	if err.Error() != "controlling terminal is no longer openable; TIOCSTI injectability was not tested" {
		t.Fatalf("unexpected precondition error: %v", err)
	}
}

func TestWakeInjectionPreconditionCheckSurfacesLegacyCapabilityChangeWithoutInjecting(t *testing.T) {
	oldRead := readTIOCSTILegacySysctl
	oldInject := tiocstiInject
	legacyControl := "1\n"
	legacyReads := 0
	readTIOCSTILegacySysctl = func() ([]byte, error) {
		legacyReads++
		return []byte(legacyControl), nil
	}
	tiocstiInject = func(text string) error {
		t.Fatalf("periodic precondition check test-injected %q", text)
		return nil
	}
	t.Cleanup(func() {
		readTIOCSTILegacySysctl = oldRead
		tiocstiInject = oldInject
	})

	var status, mode, reason string
	cfg := wakeConfig{
		me:         "codex",
		injectMode: wakeInjectModePaste,
		inputDelivery: wakeInputDeliveryState{
			phase:   wakeInputRawRescueQueued,
			mode:    wakeInjectModeRaw,
			payload: "stale doorbell",
		},
		doorbell: wakeDoorbellState{
			phase:            wakeDoorbellObserved,
			token:            "11111111111111111111111111111111",
			attempts:         2,
			nextAttempt:      time.Unix(1_800_000_000, 0),
			observationUntil: time.Unix(1_800_000_100, 0),
		},
		recordNotifierStatus: func(gotStatus, gotMode, gotReason string) error {
			status, mode, reason = gotStatus, gotMode, gotReason
			return nil
		},
	}
	openChecks := 0
	openable := func() bool {
		openChecks++
		return true
	}
	if err := wakeInjectionPreconditionCheck(&cfg, openable); err != nil {
		t.Fatalf("initial precondition check: %v", err)
	}
	if cfg.injectMode != wakeInjectModePaste || status != "" {
		t.Fatalf("unchanged capability altered notifier: mode=%q status=%q", cfg.injectMode, status)
	}

	legacyControl = "0\n"
	previousInput := cfg.inputDelivery
	previousDoorbell := cfg.doorbell
	err := wakeInjectionPreconditionCheck(&cfg, openable)
	var demotionErr *wakeInputDemotionBlockedError
	if !errors.As(err, &demotionErr) {
		t.Fatalf("changed precondition error = %v, want blocked input demotion", err)
	}
	if openChecks != 1 {
		t.Fatalf("controlling terminal checks = %d, want 1 before capability became unsupported", openChecks)
	}
	if cfg.injectMode != wakeInjectModePaste {
		t.Fatalf("inject mode = %q, want retained paste mode", cfg.injectMode)
	}
	if cfg.doorbell.phase != previousDoorbell.phase ||
		cfg.doorbell.token != previousDoorbell.token ||
		cfg.doorbell.attempts != previousDoorbell.attempts ||
		!cfg.doorbell.nextAttempt.Equal(previousDoorbell.nextAttempt) ||
		!cfg.doorbell.observationUntil.Equal(previousDoorbell.observationUntil) ||
		cfg.doorbell.observationUsed != previousDoorbell.observationUsed {
		t.Fatalf("blocked demotion mutated doorbell: %#v", cfg.doorbell)
	}
	if cfg.inputDelivery != previousInput {
		t.Fatalf("blocked demotion mutated input state: %#v", cfg.inputDelivery)
	}
	if status != wakeInjectorUnsupportedStatus || mode != wakeInjectModePaste {
		t.Fatalf("status/mode = %q/%q, want %q/%q", status, mode, wakeInjectorUnsupportedStatus, wakeInjectModePaste)
	}
	for _, want := range []string{
		tiocstiLegacySysctlPath + " is 0",
		"observed after wake binding",
	} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason = %q, want %q", reason, want)
		}
	}
	if legacyReads != 2 {
		t.Fatalf("legacy capability reads = %d, want 2 through blocked demotion", legacyReads)
	}
}

func TestWakeInjectionPreconditionCheckSurfacesPersistenceFailureAfterDemotion(t *testing.T) {
	oldRead := readTIOCSTILegacySysctl
	oldInject := tiocstiInject
	readTIOCSTILegacySysctl = func() ([]byte, error) {
		return []byte("0\n"), nil
	}
	tiocstiInject = func(text string) error {
		t.Fatalf("periodic precondition check test-injected %q", text)
		return nil
	}
	t.Cleanup(func() {
		readTIOCSTILegacySysctl = oldRead
		tiocstiInject = oldInject
	})

	persistErr := errors.New("presence unavailable")
	cfg := wakeConfig{
		me:         "codex",
		injectMode: wakeInjectModeRaw,
		recordNotifierStatus: func(status, mode, reason string) error {
			return persistErr
		},
	}
	err := wakeInjectionPreconditionCheck(&cfg, func() bool {
		t.Fatal("unsupported capability should demote before checking the terminal")
		return false
	})
	if !errors.Is(err, persistErr) {
		t.Fatalf("error = %v, want persistence failure", err)
	}
	if cfg.injectMode != wakeInjectModeNone {
		t.Fatalf("inject mode = %q, want immediate safety demotion", cfg.injectMode)
	}
	if !strings.Contains(err.Error(), "after safety demotion") {
		t.Fatalf("error does not surface demotion state: %v", err)
	}
}

func testEnvValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}
