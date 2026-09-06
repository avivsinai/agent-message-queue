//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/notificationattempt"
	"github.com/avivsinai/agent-message-queue/internal/presence"
	"github.com/fsnotify/fsnotify"
)

const (
	wakeSIGPIPEHelperEnv = "AMQ_TEST_WAKE_SIGPIPE_HELPER"
	wakeSIGPIPERootEnv   = "AMQ_TEST_WAKE_SIGPIPE_ROOT"
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

func TestWakeSurvivesClosedDiagnosticPipe(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")

	diagnosticReader, diagnosticWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	gateReader, gateWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = diagnosticReader.Close()
		_ = diagnosticWriter.Close()
		_ = gateReader.Close()
		_ = gateWriter.Close()
		_ = readyReader.Close()
		_ = readyWriter.Close()
	})

	cmd := exec.Command(os.Args[0], "-test.run=^TestWakeSIGPIPEHelper$")
	cmd.Env = append(
		os.Environ(),
		wakeSIGPIPEHelperEnv+"=1",
		wakeSIGPIPERootEnv+"="+root,
	)
	cmd.ExtraFiles = []*os.File{gateReader, readyWriter}
	cmd.Stderr = diagnosticWriter
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = gateReader.Close()
	_ = readyWriter.Close()
	_ = diagnosticWriter.Close()
	if _, err := io.ReadFull(readyReader, make([]byte, 1)); err != nil {
		t.Fatalf("wait for production wake loop: %v; stdout=%q", err, stdout.String())
	}
	_ = diagnosticReader.Close()
	if _, err := gateWriter.Write([]byte{1}); err != nil {
		t.Fatalf("release production wake loop: %v", err)
	}
	_ = gateWriter.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("production wake died after diagnostic reader closed: %v; stdout=%q", err, stdout.String())
	}
	if got := stdout.String(); !strings.Contains(got, "survived\n") {
		t.Fatalf("stdout = %q, want survived marker", got)
	}
}

func TestWakeSIGPIPEHelper(t *testing.T) {
	if os.Getenv(wakeSIGPIPEHelperEnv) != "1" {
		t.Skip("subprocess helper")
	}

	ignoredBefore := signal.Ignored(syscall.SIGPIPE)
	gate := os.NewFile(3, "wake-sigpipe-gate")
	ready := os.NewFile(4, "wake-sigpipe-ready")
	helperDone := errors.New("wake SIGPIPE helper complete")
	err := runWakeWithLoop(
		[]string{
			"--root", os.Getenv(wakeSIGPIPERootEnv),
			"--me", "codex",
			"--inject-mode", wakeInjectModeNone,
			"--interrupt=false",
		},
		func(wakeConfig) error {
			if _, err := ready.Write([]byte{1}); err != nil {
				return fmt.Errorf("signal SIGPIPE test readiness: %w", err)
			}
			if _, err := io.ReadFull(gate, make([]byte, 1)); err != nil {
				return fmt.Errorf("await closed diagnostic pipe: %w", err)
			}
			_, _ = fmt.Fprintln(os.Stderr, "broken-pipe-probe")
			_, _ = fmt.Fprintln(os.Stdout, "survived")
			return helperDone
		},
	)
	if !errors.Is(err, helperDone) {
		t.Fatal(err)
	}
	if ignoredAfter := signal.Ignored(syscall.SIGPIPE); ignoredAfter != ignoredBefore {
		t.Fatalf("SIGPIPE ignored state after wake = %t, want baseline %t", ignoredAfter, ignoredBefore)
	}
	probeReader, probeWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = probeReader.Close()
	_, err = probeWriter.Write([]byte{1})
	_ = probeWriter.Close()
	if !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("post-wake broken pipe error = %v, want EPIPE", err)
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
			now := time.Unix(1_800_000_000, 0)
			attentionWrites := 0
			cfg := &wakeConfig{
				root:           root,
				me:             tc.me,
				session:        "session1",
				wakeOwner:      &wakeOwner{},
				injectMode:     tc.mode,
				doorbellNow:    func() time.Time { return now },
				attentionIsTTY: func() bool { return false },
				attentionWrite: func(data []byte) (int, error) {
					attentionWrites++
					return len(data), nil
				},
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
			if tc.failAt == 1 {
				if !isWakeTerminalForegroundPGRPChanged(err) {
					t.Fatalf("zero-progress refusal = %T %v, want foreground-PGRP change", err, err)
				}
				if attentionWrites != 1 {
					t.Fatalf("zero-progress attention writes = %d, want 1", attentionWrites)
				}
			} else if !isWakeTerminalForegroundPGRPChanged(err) {
				t.Fatalf("first notify error = %T %v, want foreground-PGRP change", err, err)
			}
			if got := strings.Join(writes, "|"); got != strings.Join(tc.firstWant, "|") {
				t.Fatalf("first writes = %q, want %q", got, strings.Join(tc.firstWant, "|"))
			}
			wantAttempts := uint(0)
			if cfg.doorbell.attempts != wantAttempts {
				t.Fatalf(
					"attempts after foreground refusal = %d, want %d",
					cfg.doorbell.attempts,
					wantAttempts,
				)
			}
			if !cfg.doorbell.nextAttempt.IsZero() {
				t.Fatalf(
					"foreground refusal armed delivery ladder: %#v",
					cfg.doorbell,
				)
			}

			writes = nil
			if err := notifyNewMessages(cfg); err != nil {
				t.Fatalf("retry notify: %v", err)
			}
			if got := strings.Join(writes, "|"); got != strings.Join(tc.retryWant, "|") {
				t.Fatalf("retry writes = %q, want %q", got, strings.Join(tc.retryWant, "|"))
			}
			wantAttempts++
			if cfg.doorbell.attempts != wantAttempts {
				t.Fatalf(
					"attempts after completed retry = %d, want %d",
					cfg.doorbell.attempts,
					wantAttempts,
				)
			}
			wantAttentionWrites := 0
			if tc.failAt == 1 {
				wantAttentionWrites = 1
			}
			if attentionWrites != wantAttentionWrites {
				t.Fatalf(
					"attention writes after input retry = %d, want %d",
					attentionWrites,
					wantAttentionWrites,
				)
			}
		})
	}
}

func stubSignalWakeProcess(t *testing.T, fn func(pid int, sig os.Signal) error) {
	t.Helper()
	oldSignal := signalWakeProcess
	oldGrace := wakeTerminateGrace
	oldKillConfirm := wakeTerminateKillConfirm
	oldGracefulExitConfirm := wakeTerminateGracefulExitConfirm
	signalWakeProcess = fn
	wakeTerminateGrace = 0
	wakeTerminateKillConfirm = 0
	wakeTerminateGracefulExitConfirm = 0
	t.Cleanup(func() {
		signalWakeProcess = oldSignal
		wakeTerminateGrace = oldGrace
		wakeTerminateKillConfirm = oldKillConfirm
		wakeTerminateGracefulExitConfirm = oldGracefulExitConfirm
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
	return writeExecutableScriptForTest(t, name, "#!/bin/sh\nexit 0\n")
}

func writeExecutableScriptForTest(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(secureTempDirForTest(t), name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	return path
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
	var maintenanceInspection wakeLockInspection
	errDone := errors.New("done")
	injector := writeExecutableForTest(t, "inject tool")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--retry-until", "injected",
		"--inject-arg", "exec",
		"--inject-arg", "Team Alpha",
		"--inject-timeout", "250ms",
	}, func(cfg wakeConfig) error {
		got = cfg
		if cfg.inspectTerminalGeneration != nil {
			maintenanceInspection = cfg.inspectTerminalGeneration()
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
	if got.injectVia != injector {
		t.Fatalf("expected inject executable with spaces, got %q", got.injectVia)
	}
	if got.retryUntil != wakeRetryUntilInjected {
		t.Fatalf("retry until = %q, want %q", got.retryUntil, wakeRetryUntilInjected)
	}
	if strings.Join(got.injectArgs, "|") != "exec|Team Alpha" {
		t.Fatalf("expected fixed inject args, got %#v", got.injectArgs)
	}
	if got.injectTimeout != 250*time.Millisecond {
		t.Fatalf("expected inject timeout 250ms, got %s", got.injectTimeout)
	}
	if got.inspectTerminalGeneration == nil {
		t.Fatal("--inject-via wake has no maintenance lock-health inspection")
	}
	if !maintenanceInspection.Exists ||
		maintenanceInspection.fileInfo == nil ||
		maintenanceInspection.Lock.Generation == "" {
		t.Fatalf("--inject-via maintenance lock inspection = %+v", maintenanceInspection)
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

func TestRunWakeWithLoopWaitsForAcquiredInboxToReturnWithoutRecreatingIt(t *testing.T) {
	stubFastWakeInboxRetry(t)
	retryObserved := make(chan struct{}, 1)
	originalWaitWakeRetry := waitWakeRetry
	waitWakeRetry = func(
		controlStop <-chan struct{},
		signals <-chan os.Signal,
		delay time.Duration,
	) bool {
		select {
		case retryObserved <- struct{}{}:
		default:
		}
		return originalWaitWakeRetry(controlStop, signals, delay)
	}
	t.Cleanup(func() { waitWakeRetry = originalWaitWakeRetry })

	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "injector")
	inboxPath := fsq.AgentInboxNew(root, "codex")
	heldInboxPath := inboxPath + ".held"
	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "codex",
		"--inject-via", injector,
	}, func(cfg wakeConfig) error {
		if err := os.Rename(inboxPath, heldInboxPath); err != nil {
			t.Fatal(err)
		}
		inboxHeld := true
		t.Cleanup(func() {
			if inboxHeld {
				_ = os.Rename(heldInboxPath, inboxPath)
			}
		})

		stop := make(chan struct{})
		stopClosed := false
		defer func() {
			if !stopClosed {
				close(stop)
			}
		}()
		prepared := make(chan struct{})
		done := make(chan error, 1)
		cfg.controlStop = stop
		cfg.onPrepared = func(wakeAdmissionWatcher) error {
			close(prepared)
			return nil
		}
		go func() {
			done <- runWakeLoop(cfg)
		}()

		select {
		case <-retryObserved:
		case loopErr := <-done:
			t.Fatalf("wake loop exited while the acquired inbox was unavailable: %v", loopErr)
		case <-time.After(2 * time.Second):
			t.Fatal("wake loop did not retry while the acquired inbox was unavailable")
		}
		if _, statErr := os.Stat(inboxPath); !os.IsNotExist(statErr) {
			t.Fatalf("missing acquired inbox was recreated: %v", statErr)
		}

		if err := os.Rename(heldInboxPath, inboxPath); err != nil {
			t.Fatal(err)
		}
		inboxHeld = false
		select {
		case <-prepared:
		case loopErr := <-done:
			t.Fatalf("wake loop exited instead of recovering its acquired inbox: %v", loopErr)
		case <-time.After(2 * time.Second):
			t.Fatal("wake loop did not recover after its acquired inbox returned")
		}

		close(stop)
		stopClosed = true
		if loopErr := <-done; loopErr != nil {
			t.Fatalf("wake loop stop: %v", loopErr)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
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

func TestRunWakeLoopRetriesPendingDoorbellWithSubmitOnlyNudge(t *testing.T) {
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
	retypedDoorbell := make(chan struct{}, 1)
	reminderSubmit := make(chan struct{}, 1)
	attention := make(chan string, 1)
	stop := make(chan struct{})
	done := make(chan error, 1)
	loopDone := make(chan struct{})
	defer func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		select {
		case <-loopDone:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop during cleanup")
		}
	}()
	doorbellPayloads := 0
	submitKeys := 0
	preludeLFs := 0

	go func() {
		defer close(loopDone)
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
				if text == coopWakeDoorbell {
					doorbellPayloads++
					if doorbellPayloads == 1 {
						firstDoorbell <- struct{}{}
					} else {
						retypedDoorbell <- struct{}{}
					}
				}
				if text == "\r" {
					submitKeys++
					if submitKeys == 4 {
						reminderSubmit <- struct{}{}
					}
				}
				if text == "\n" {
					preludeLFs++
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
	case <-reminderSubmit:
	case err := <-done:
		t.Fatalf("wake loop exited before retry: %v", err)
	case <-time.After(wakeDoorbellRetryBase + 2*time.Second):
		t.Fatal("pending doorbell did not receive a submit-only nudge on its own deadline")
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
	case <-retypedDoorbell:
		t.Fatal("submit-only reminder retyped the fixed doorbell payload")
	default:
	}
	select {
	case output := <-attention:
		t.Fatalf("retry emitted output-only attention: %q", output)
	default:
	}
	if preludeLFs != 1 {
		t.Fatalf("raw LF preludes = %d, want exactly the initial full-presentation prelude", preludeLFs)
	}
}

func TestNotifyNewMessagesAttentionOnlyRetryCadence(t *testing.T) {
	root := secureTempDirForTest(t)
	deliverPartialWakeMessageForTest(t, root, "codex", "attention-cadence")

	now := time.Unix(1_800_000_000, 0)
	writes := 0
	cfg := &wakeConfig{
		root:           root,
		me:             "codex",
		session:        "session1",
		injectMode:     wakeInjectModeNone,
		doorbellNow:    func() time.Time { return now },
		attentionIsTTY: func() bool { return false },
		attentionWrite: func(data []byte) (int, error) {
			writes++
			return len(data), nil
		},
	}

	wantDelays := []time.Duration{
		30 * time.Second,
		time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		15 * time.Minute,
		15 * time.Minute,
	}
	for attempt, wantDelay := range wantDelays {
		if err := notifyNewMessages(cfg); err != nil {
			t.Fatalf("attention attempt %d: %v", attempt+1, err)
		}
		if writes != attempt+1 {
			t.Fatalf(
				"attention writes after attempt %d = %d, want %d",
				attempt+1,
				writes,
				attempt+1,
			)
		}
		if cfg.doorbell.attempts != uint(attempt+1) {
			t.Fatalf(
				"recorded attempts after attempt %d = %d, want %d",
				attempt+1,
				cfg.doorbell.attempts,
				attempt+1,
			)
		}
		wantDeadline := now.Add(wantDelay)
		if deadline, ok := cfg.doorbell.nextDeadline(); !ok || !deadline.Equal(wantDeadline) {
			t.Fatalf(
				"attention deadline after attempt %d = %s, ok=%v; want %s",
				attempt+1,
				deadline,
				ok,
				wantDeadline,
			)
		}
		if err := notifyNewMessages(cfg); err != nil {
			t.Fatalf("early attention check %d: %v", attempt+1, err)
		}
		if writes != attempt+1 {
			t.Fatalf("attention repeated before deadline %d: writes=%d", attempt+1, writes)
		}
		now = wantDeadline
	}
}

func TestRunWakeLoopCoalescesAdditionThenRearmsAfterDrain(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	deliverWakeWatcherMessageForTest(t, root, "codex", "a", "claude")
	aPath := filepath.Join(fsq.AgentInboxNew(root, "codex"), "a.md")
	aInfo, err := os.Stat(aPath)
	if err != nil {
		t.Fatal(err)
	}

	stubRawInputDrained(t, func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	})
	stubRawInjectSleep(t)

	startedAt := time.Now()
	floorAt := startedAt.Add(500 * time.Millisecond)
	lastAttempt := floorAt.Add(-wakeDoorbellRetryBase)
	decayedRetryAt := startedAt.Add(15 * time.Minute)
	doorbells := make(chan struct{}, 3)
	pending := make(chan struct{}, 1)
	baselineReady := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			session:     "session1",
			wakeOwner:   &wakeOwner{},
			injectMode:  wakeInjectModeRaw,
			controlStop: stop,
			debounce:    10 * time.Millisecond,
			doorbell: wakeDoorbellState{
				phase:                wakeDoorbellRetrying,
				cohort:               snapshotWakeFileIdentities(map[string]os.FileInfo{"a.md": aInfo}),
				attempts:             6,
				nextAttempt:          decayedRetryAt,
				additionAttemptFloor: lastAttempt.Add(wakeDoorbellRetryBase),
			},
			preconditionCheck: func(*wakeConfig) error { return nil },
			onBaselineReady: func(map[string]wakeFileIdentity) error {
				close(baselineReady)
				return nil
			},
			terminalWrite: func(text string) error {
				if strings.Contains(text, coopWakeDoorbell) {
					doorbells <- struct{}{}
				}
				return nil
			},
			attentionIsTTY: func() bool { return false },
			onPendingNotify: func() {
				select {
				case pending <- struct{}{}:
				default:
				}
			},
		})
	}()
	defer func() {
		close(stop)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("wake loop stop: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	}()

	select {
	case <-baselineReady:
	case err := <-done:
		t.Fatalf("wake loop exited before baseline readiness: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not publish baseline readiness")
	}
	deliverWakeWatcherMessageForTest(t, root, "codex", "b", "claude")
	select {
	case <-pending:
	case err := <-done:
		t.Fatalf("wake loop exited before observing addition: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not observe first addition")
	}
	deliverWakeWatcherMessageForTest(t, root, "codex", "c", "claude")

	if earlyWait := time.Until(floorAt) / 2; earlyWait > 0 {
		select {
		case <-doorbells:
			t.Fatal("added-message burst emitted before the delivery floor")
		case err := <-done:
			t.Fatalf("wake loop exited before delivery floor: %v", err)
		case <-time.After(earlyWait):
		}
	}
	select {
	case <-doorbells:
	case err := <-done:
		t.Fatalf("wake loop exited before delivery floor: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("coalesced cohort waited for the decayed retry deadline")
	}
	select {
	case <-doorbells:
		t.Fatal("one debounce burst emitted more than one doorbell")
	case <-time.After(100 * time.Millisecond):
	}

	if drained := runDrainJSON(t, root, "codex", 1, false); drained.Count != 1 {
		t.Fatalf("drained count = %d, want 1", drained.Count)
	}
	select {
	case <-doorbells:
	case err := <-done:
		t.Fatalf("wake loop exited before post-drain rearm: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("draining one coalesced message did not immediately rearm the remainder")
	}
	select {
	case <-doorbells:
		t.Fatal("post-drain rearm emitted more than one consolidated doorbell")
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
				phase:       wakeDoorbellRetrying,
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
				if strings.Contains(text, coopWakeDoorbell) {
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

func TestDeliverNewMessageNotificationInjectViaUncertainDoesNotReplay(t *testing.T) {
	current := wakeDoorbellTestFiles(t, "pending.md")
	logPath := filepath.Join(secureTempDirForTest(t), "inject.log")
	injector := writeExecutableScriptForTest(t, "uncertain-injector", fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$1" >> %q
echo AMQ_INJECT_PROGRESS=uncertain >&2
exit 1
`, logPath))
	attentionWrites := 0
	cfg := &wakeConfig{
		me:             "codex",
		wakeOwner:      &wakeOwner{},
		injectMode:     wakeInjectModePaste,
		injectVia:      injector,
		injectTimeout:  time.Second,
		attentionIsTTY: func() bool { return false },
		attentionWrite: func(data []byte) (int, error) {
			attentionWrites++
			return len(data), nil
		},
	}

	err := deliverNewMessageNotification(
		cfg,
		peerWakeNotification("pending message"),
		false,
		current,
	)
	if err != nil {
		t.Fatalf("delivery error = %v, want live recovery transition", err)
	}
	if !cfg.inputDelivery.acceptanceUncertain {
		t.Fatalf("uncertain acceptance was not retained: %#v", cfg.inputDelivery)
	}
	if !cfg.inputRecoveryRequired {
		t.Fatal("uncertain input did not enter recovery-required mode")
	}
	if attentionWrites != 1 {
		t.Fatalf("uncertain input emitted %d recovery alerts, want one", attentionWrites)
	}
	first, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read inject log: %v", readErr)
	}
	if got, want := string(first), coopWakeDoorbell+"\n"; got != want {
		t.Fatalf("inject log = %q, want payload once (%q)", got, want)
	}

	if err := deliverNewMessageNotification(
		cfg,
		peerWakeNotification("pending message"),
		false,
		current,
	); err != nil {
		t.Fatalf("second delivery error = %v", err)
	}
	second, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read inject log after second delivery: %v", readErr)
	}
	if string(second) != string(first) {
		t.Fatalf("second delivery replayed payload: first=%q second=%q", first, second)
	}
}

func TestDeliverNewMessageNotificationInjectViaOrdinaryFailureFallsBack(t *testing.T) {
	current := wakeDoorbellTestFiles(t, "pending.md")
	logPath := filepath.Join(secureTempDirForTest(t), "inject.log")
	injector := writeExecutableScriptForTest(t, "ordinary-fail-injector", fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$1" >> %q
echo failed >&2
exit 1
`, logPath))
	attentionWrites := 0
	cfg := &wakeConfig{
		me:             "codex",
		wakeOwner:      &wakeOwner{},
		injectMode:     wakeInjectModePaste,
		injectVia:      injector,
		injectTimeout:  time.Second,
		attentionIsTTY: func() bool { return false },
		attentionWrite: func(data []byte) (int, error) {
			attentionWrites++
			return len(data), nil
		},
	}

	err := deliverNewMessageNotification(
		cfg,
		peerWakeNotification("pending message"),
		false,
		current,
	)
	if err != nil {
		t.Fatalf("ordinary inject-via failure error = %v, want attention fallback", err)
	}
	if cfg.inputDelivery.acceptanceUncertain {
		t.Fatalf("ordinary inject-via failure marked uncertain: %#v", cfg.inputDelivery)
	}
	if cfg.inputRecoveryRequired {
		t.Fatal("ordinary inject-via failure entered recovery")
	}
	if attentionWrites != 1 {
		t.Fatalf("ordinary inject-via failure emitted %d attention writes, want one", attentionWrites)
	}
}

func TestInjectViaDeferredThenAcceptedUsesOneAttemptAndNoDuplicate(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	countPath := filepath.Join(secureTempDirForTest(t), "inject-count")
	injector := writeExecutableScriptForTest(t, "deferred-then-accepted", fmt.Sprintf(`#!/bin/sh
count=0
if [ -f %q ]; then count=$(cat %q); fi
count=$((count + 1))
printf '%%s' "$count" > %q
if [ "$count" -eq 1 ]; then
  echo AMQ_INJECT_PROGRESS=deferred >&2
  exit 1
fi
echo AMQ_INJECT_PROGRESS=accepted >&2
exit 0
`, countPath, countPath, countPath))
	attentionWrites := 0
	cfg := protocolWakeConfigForTest(t, root, injector, &attentionWrites)
	cfg.retryUntil = wakeRetryUntilInjected
	current := wakeDoorbellTestFiles(t, "pending.md")
	messageIDs := []string{"msg-deferred-accepted"}
	notice := peerWakeNotification("pending message")

	if err := deliverWithNotificationLedger(cfg, messageIDs, notice, false, current); !isWakeInjectorDeferred(err) {
		t.Fatalf("first delivery error = %v, want deferred", err)
	}
	first := listNotificationAttemptsForTest(t, root, messageIDs[0])
	if len(first) != 1 || first[0].State != notificationattempt.StateDeferred {
		t.Fatalf("first notification attempts = %#v, want one deferred attempt", first)
	}
	cfg.doorbell.makeDue(cfg.wakeDoorbellNow())
	if err := deliverWithNotificationLedger(cfg, messageIDs, notice, false, current); err != nil {
		t.Fatalf("accepted retry error = %v", err)
	}

	data, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatalf("read injector count: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "2" {
		t.Fatalf("injector calls = %q, want deferred plus one accepted call", got)
	}
	if attentionWrites != 0 || cfg.doorbell.phase != wakeDoorbellAnnounced {
		t.Fatalf("post-accept attention/state = %d/%#v, want 0/announced", attentionWrites, cfg.doorbell)
	}
	accepted := listNotificationAttemptsForTest(t, root, messageIDs[0])
	if len(accepted) != 1 || accepted[0].State != notificationattempt.StateAccepted {
		t.Fatalf("accepted notification attempts = %#v, want one terminal accepted attempt", accepted)
	}
	if accepted[0].Prepared.AttemptID != first[0].Prepared.AttemptID || len(accepted[0].History) != 4 {
		t.Fatalf("accepted lifecycle = %#v, want same ID and attempt/deferred/retried/accepted history", accepted[0])
	}
	if err := deliverWithNotificationLedger(cfg, messageIDs, notice, false, current); err != nil {
		t.Fatalf("post-accept scan error = %v", err)
	}
	data, err = os.ReadFile(countPath)
	if err != nil {
		t.Fatalf("read injector count after post-accept scan: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "2" {
		t.Fatalf("post-accept injector calls = %q, want 2", got)
	}
}

func TestInjectViaLegacyExitZeroIsWrittenButNotAccepted(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	countPath := filepath.Join(secureTempDirForTest(t), "inject-count")
	injector := writeExecutableScriptForTest(t, "legacy-injector", fmt.Sprintf(`#!/bin/sh
count=0
if [ -f %q ]; then count=$(cat %q); fi
count=$((count + 1))
printf '%%s' "$count" > %q
exit 0
`, countPath, countPath, countPath))
	attentionWrites := 0
	cfg := protocolWakeConfigForTest(t, root, injector, &attentionWrites)
	cfg.retryUntil = wakeRetryUntilInjected
	current := wakeDoorbellTestFiles(t, "pending.md")

	if err := deliverWithNotificationLedger(cfg, []string{"msg-legacy"}, peerWakeNotification("pending message"), false, current); err != nil {
		t.Fatalf("legacy delivery error = %v, want legacy success", err)
	}
	if attentionWrites != 0 || cfg.inputRecoveryRequired || cfg.doorbell.phase != wakeDoorbellAnnounced {
		t.Fatalf("legacy loop state = attention %d recovery %v doorbell %#v; want no recovery and announced", attentionWrites, cfg.inputRecoveryRequired, cfg.doorbell)
	}
	attempts := listNotificationAttemptsForTest(t, root, "msg-legacy")
	if len(attempts) != 1 || attempts[0].State != notificationattempt.OutcomeWritten {
		t.Fatalf("legacy notification attempts = %#v, want written byte evidence", attempts)
	}
	if attempts[0].State == notificationattempt.StateAccepted || attempts[0].Result == nil || !strings.Contains(attempts[0].Result.Detail, "uncertain") {
		t.Fatalf("legacy result = %#v, want uncertain non-accepted detail", attempts[0].Result)
	}
	data, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatalf("read legacy injector count: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "1" {
		t.Fatalf("legacy injector calls = %q, want one call under retry-until-injected", got)
	}
}

func TestRawTIOCSTIRecordsWrittenWithoutAcceptanceClaim(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	var writes []string
	cfg := &wakeConfig{
		me:         "codex",
		root:       root,
		wakeOwner:  &wakeOwner{},
		injectMode: wakeInjectModeRaw,
		terminalWrite: func(chunk string) error {
			writes = append(writes, chunk)
			return nil
		},
	}
	current := wakeDoorbellTestFiles(t, "pending.md")
	if err := deliverWithNotificationLedger(cfg, []string{"msg-raw"}, peerWakeNotification("pending message"), false, current); err != nil {
		t.Fatalf("raw delivery error = %v", err)
	}
	if len(writes) == 0 {
		t.Fatal("raw TIOCSTI test injected no terminal bytes")
	}
	attempts := listNotificationAttemptsForTest(t, root, "msg-raw")
	if len(attempts) != 1 || attempts[0].State != notificationattempt.OutcomeWritten {
		t.Fatalf("raw notification attempts = %#v, want written byte evidence", attempts)
	}
	if attempts[0].State == notificationattempt.StateAccepted || cfg.doorbell.phase == wakeDoorbellAnnounced {
		t.Fatalf("raw delivery claimed acceptance: attempt=%#v doorbell=%#v", attempts[0], cfg.doorbell)
	}
}

func protocolWakeConfigForTest(t *testing.T, root, injector string, attentionWrites *int) *wakeConfig {
	t.Helper()
	return &wakeConfig{
		me:             "codex",
		root:           root,
		wakeOwner:      &wakeOwner{},
		injectMode:     wakeInjectModePaste,
		injectVia:      injector,
		injectTimeout:  time.Second,
		attentionIsTTY: func() bool { return false },
		attentionWrite: func(data []byte) (int, error) {
			if attentionWrites != nil {
				(*attentionWrites)++
			}
			return len(data), nil
		},
	}
}

func listNotificationAttemptsForTest(t *testing.T, root, messageID string) []notificationattempt.Attempt {
	t.Helper()
	attempts, err := notificationattempt.List(root, "codex", messageID)
	if err != nil {
		t.Fatalf("List notification attempts: %v", err)
	}
	return attempts
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
	retryRelease := make(chan struct{})
	releaseRetry := func() {
		select {
		case <-retryRelease:
		default:
			close(retryRelease)
		}
	}
	t.Cleanup(releaseRetry)
	originalRetry := waitForWakePreparedRetry
	waitForWakePreparedRetry = func(time.Time) bool {
		select {
		case retryEntered <- struct{}{}:
		default:
		}
		<-retryRelease
		return true
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
		Executable: "/opt/homebrew/bin/amq", Generation: "11111111111111111111111111111111",
	}, target))
	writeWakePreparedForTest(t, root, "orchestrator")
	// The retry must observe the complete target/lock/prepared publication, not
	// an accidental mid-replacement snapshot created by the test goroutine.
	releaseRetry()
	if err := <-done; err != nil {
		t.Fatalf("creating wake did not become reusable: %v", err)
	}
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatalf("ready file missing: %v", err)
	}
}

func TestRunWakeWithLoopAcceptExistingWakeAcceptsInjectViaUnknownTTY(t *testing.T) {
	tests := []struct {
		name             string
		mutatePersisted  func(t *testing.T, root string, target wakeTarget)
		wantErrSubstring string
	}{
		{name: "matching target"},
		{
			name: "missing target",
			mutatePersisted: func(t *testing.T, root string, _ wakeTarget) {
				t.Helper()
				if err := os.Remove(wakeTargetPath(root, "orchestrator")); err != nil {
					t.Fatal(err)
				}
			},
			wantErrSubstring: "target",
		},
		{
			name: "target digest mismatch",
			mutatePersisted: func(t *testing.T, root string, target wakeTarget) {
				t.Helper()
				target.Created = "2026-08-09T00:00:00Z"
				if err := writeWakeTarget(root, "orchestrator", target); err != nil {
					t.Fatal(err)
				}
			},
			wantErrSubstring: "target",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const wakePID = 4242
			root := secureTempDirForTest(t)
			injector := writeExecutableForTest(t, "injector")
			target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				if pid == wakePID {
					return wakeProcessInfo{
						PID:                      pid,
						Running:                  true,
						StartToken:               "start-1",
						BootID:                   "boot-1",
						Executable:               "/opt/homebrew/bin/amq",
						Args:                     []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--inject-via", injector},
						ControllingTerminalKnown: true,
						HasControllingTerminal:   false,
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
				Generation:   "11111111111111111111111111111111",
			}, target))
			if err := writeWakeTarget(root, "orchestrator", target); err != nil {
				t.Fatalf("writeWakeTarget: %v", err)
			}
			writeWakePreparedForTest(t, root, "orchestrator")
			if test.mutatePersisted != nil {
				test.mutatePersisted(t, root, target)
				originalRetry := waitForWakePreparedRetry
				waitForWakePreparedRetry = func(time.Time) bool { return false }
				t.Cleanup(func() { waitForWakePreparedRetry = originalRetry })
			}

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
			if test.wantErrSubstring == "" {
				if err != nil {
					t.Fatalf("expected inject-via wake to satisfy ready file despite unknown tty, got %v", err)
				}
				if _, statErr := os.Stat(readyPath); statErr != nil {
					t.Fatalf("ready file should exist, statErr=%v", statErr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErrSubstring) {
				t.Fatalf("runWakeWithLoop() error = %v, want %q", err, test.wantErrSubstring)
			}
			if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
				t.Fatalf("ready file should not exist, statErr=%v", statErr)
			}
		})
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
				PID:                      pid,
				Running:                  true,
				StartToken:               "start-1",
				BootID:                   "boot-1",
				Executable:               "/opt/homebrew/bin/amq",
				Args:                     []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--inject-via", existingInjector},
				ControllingTerminalKnown: true,
				HasControllingTerminal:   false,
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
	originalRetry := waitForWakePreparedRetry
	retryCalls := 0
	waitForWakePreparedRetry = func(time.Time) bool {
		retryCalls++
		return false
	}
	t.Cleanup(func() { waitForWakePreparedRetry = originalRetry })

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
	if retryCalls != 0 {
		t.Fatalf("conclusive injector mismatch retried %d times", retryCalls)
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

func TestRemoveWakeLockIfUnchangedRefusesChangedLock(t *testing.T) {
	root := secureTempDirForTest(t)
	establishDoctorWakeLifecycleGuardForTest(t, root, "orchestrator")
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

	agentDir, err := openWakeDirectory(filepath.Dir(lockPath), "wake agent directory")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	err = withExistingWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		return removeWakeLockIfUnchangedGuardedAt(scope, inspection)
	})
	if err == nil {
		t.Fatal("expected changed lock removal error")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("changed lock should remain, stat=%v", statErr)
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

func TestShouldReplaceOrphanedWakeLockKeepsLockWhenKillDoesNotTerminate(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4242
	root := secureTempDirForTest(t)
	establishDoctorWakeLifecycleGuardForTest(t, root, "orchestrator")
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
	waiter := newWakeProcessWaiter(cmd.Process)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = waiter.waitForExit(time.Second)
	})

	if err := writeWakeReadyFile(root, "orchestrator", readyPath, inspection); err != nil {
		t.Fatalf("writeWakeReadyFile: %v", err)
	}
	if err := waitForWakeReadyWithWaiter(waiter, readyPath, root, "orchestrator", time.Second); err != nil {
		t.Fatalf("waitForWakeReady: %v", err)
	}
}

func TestTerminateWakeHelperProcessKillsWaitsAndRemovesCapturedLock(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	waiter := newWakeProcessWaiter(cmd.Process)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = waiter.waitForExit(time.Second)
	})
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
	establishDoctorWakeLifecycleGuardForTest(t, root, "orchestrator")
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID: wakePID, ProcessStart: "start-1", BootID: "boot-1",
		Executable: "/opt/homebrew/bin/amq", Generation: "generation-1",
	})
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
	if got := classifyWakeFailure(err); got != wakeFailureFatal {
		t.Fatalf("owner death disposition = %v, want fatal", got)
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
			phase:       wakeDoorbellRetrying,
			attempts:    2,
			nextAttempt: time.Unix(1_800_000_000, 0),
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
		cfg.doorbell.attempts != previousDoorbell.attempts ||
		!cfg.doorbell.nextAttempt.Equal(previousDoorbell.nextAttempt) {
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
