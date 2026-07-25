//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

type wakeTerminalInjection struct {
	fd   uintptr
	text string
}

type wakeTerminalAuthorityFixture struct {
	generation     wakeLockInspection
	current        wakeLockInspection
	currentTTYPath string
	foregroundPGRP int
	injections     []wakeTerminalInjection
}

func TestWakeTerminalAuthorityInjectsThroughRetainedFD(t *testing.T) {
	fixture := installWakeTerminalAuthorityFixture(t)
	stop := make(chan struct{})
	authority, err := bindWakeTerminalAuthority(fixture.generation, stop)
	if err != nil {
		t.Fatal(err)
	}
	retainedFD := authority.fd
	t.Cleanup(func() { _ = authority.Close() })

	if err := authority.BeforeWrite(); err != nil {
		t.Fatalf("validate retained terminal authority: %v", err)
	}
	if err := authority.Inject("doorbell"); err != nil {
		t.Fatalf("inject through retained terminal authority: %v", err)
	}
	if len(fixture.injections) != 1 ||
		fixture.injections[0].fd != retainedFD ||
		fixture.injections[0].text != "doorbell" {
		t.Fatalf("retained-fd injections = %#v, want fd=%d text=doorbell", fixture.injections, retainedFD)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestWakeTerminalAuthorityRefusesSamePathForegroundPGRPHandoff(t *testing.T) {
	fixture := installWakeTerminalAuthorityFixture(t)
	authority, err := bindWakeTerminalAuthority(fixture.generation, make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	fixture.foregroundPGRP++
	err = authority.Inject("must-not-arrive")
	if !isWakeTerminalAuthorityLoss(err) ||
		!strings.Contains(err.Error(), "foreground process group changed") {
		t.Fatalf("same-path foreground-pgrp handoff error = %v", err)
	}
	if len(fixture.injections) != 0 {
		t.Fatalf("same-path foreground-pgrp handoff injected: %#v", fixture.injections)
	}
}

func TestWakeTerminalAuthorityRefusesChangedRetainedFDIdentity(t *testing.T) {
	fixture := installWakeTerminalAuthorityFixture(t)
	authority, err := bindWakeTerminalAuthority(fixture.generation, make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	replacementPath := filepath.Join(t.TempDir(), "replacement-tty")
	if err := os.WriteFile(replacementPath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := authority.tty.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.OpenFile(replacementPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	authority.tty = replacement
	fixture.currentTTYPath = replacementPath

	err = authority.Inject("must-not-arrive")
	if !isWakeTerminalAuthorityLoss(err) ||
		(!strings.Contains(err.Error(), "descriptor changed") &&
			!strings.Contains(err.Error(), "identity changed")) {
		t.Fatalf("changed retained-fd identity error = %v", err)
	}
	if len(fixture.injections) != 0 {
		t.Fatalf("changed retained-fd identity injected: %#v", fixture.injections)
	}
}

func TestWakeTerminalAuthorityRefusesChangedCurrentTTYIdentity(t *testing.T) {
	fixture := installWakeTerminalAuthorityFixture(t)
	authority, err := bindWakeTerminalAuthority(fixture.generation, make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	replacementPath := filepath.Join(t.TempDir(), "replacement-current-tty")
	if err := os.WriteFile(replacementPath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.currentTTYPath = replacementPath

	err = authority.Inject("must-not-arrive")
	if !isWakeTerminalAuthorityLoss(err) ||
		!strings.Contains(err.Error(), "current controlling-terminal identity changed") {
		t.Fatalf("changed current tty identity error = %v", err)
	}
	if len(fixture.injections) != 0 {
		t.Fatalf("changed current tty identity injected: %#v", fixture.injections)
	}
}

func TestWakeTerminalAuthorityRefusesChangedGenerationAndStoppedControl(t *testing.T) {
	t.Run("generation", func(t *testing.T) {
		fixture := installWakeTerminalAuthorityFixture(t)
		authority, err := bindWakeTerminalAuthority(fixture.generation, make(chan struct{}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = authority.Close() })

		fixture.current.Lock.Generation = "replacement-generation"
		err = authority.Inject("must-not-arrive")
		if !isWakeTerminalAuthorityLoss(err) ||
			!strings.Contains(err.Error(), "wake generation changed") {
			t.Fatalf("changed generation error = %v", err)
		}
		if len(fixture.injections) != 0 {
			t.Fatalf("changed generation injected: %#v", fixture.injections)
		}
	})

	t.Run("control stopped", func(t *testing.T) {
		fixture := installWakeTerminalAuthorityFixture(t)
		stop := make(chan struct{})
		authority, err := bindWakeTerminalAuthority(fixture.generation, stop)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = authority.Close() })

		close(stop)
		err = authority.Inject("must-not-arrive")
		var loss *wakeTerminalAuthorityLossError
		if !errors.As(err, &loss) || !strings.Contains(loss.Reason, "control stopped") {
			t.Fatalf("closed control-stop error = %v", err)
		}
		if len(fixture.injections) != 0 {
			t.Fatalf("closed control-stop injected: %#v", fixture.injections)
		}
	})
}

func TestWakeTerminalControlStoppedClassificationIsStructural(t *testing.T) {
	stopped := newWakeTerminalControlStoppedLoss()
	if !isWakeTerminalControlStopped(stopped) {
		t.Fatalf("typed stopped-control loss was not classified: %v", stopped)
	}

	sameReasonOnly := newWakeTerminalAuthorityLoss("wake control stopped", nil)
	if isWakeTerminalControlStopped(sameReasonOnly) {
		t.Fatalf("reason-only authority loss was classified as stopped control: %v", sameReasonOnly)
	}
	if !isWakeTerminalAuthorityLoss(sameReasonOnly) {
		t.Fatalf("reason-only loss stopped being an authority loss: %v", sameReasonOnly)
	}
}

func TestWakeTerminalAuthorityStopClosingAtWriteBoundariesIsNonfatal(t *testing.T) {
	oldWait := waitForRawInputDrained
	oldSleep := rawInjectSleep
	waitForRawInputDrained = func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	}
	rawInjectSleep = func(time.Duration) {}
	t.Cleanup(func() {
		waitForRawInputDrained = oldWait
		rawInjectSleep = oldSleep
	})

	t.Run("between outer stop check and before-write validation", func(t *testing.T) {
		fixture := installWakeTerminalAuthorityFixture(t)
		stop := make(chan struct{})
		authority, err := bindWakeTerminalAuthority(fixture.generation, stop)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = authority.Close() })

		beforeCalls := 0
		err = injectNotification(&wakeConfig{
			me:          "codex",
			injectMode:  wakeInjectModeRaw,
			controlStop: stop,
			beforeTerminalWrite: func() error {
				beforeCalls++
				close(stop)
				return authority.BeforeWrite()
			},
			terminalWrite: authority.Inject,
		}, "dynamic", false)
		if err != nil {
			t.Fatalf("stop before retained validation: %v", err)
		}
		if beforeCalls != 1 {
			t.Fatalf("before-write calls = %d, want 1", beforeCalls)
		}
		if len(fixture.injections) != 0 {
			t.Fatalf("stopped authority injected terminal bytes: %#v", fixture.injections)
		}
	})

	t.Run("between before-write validation and retained inject", func(t *testing.T) {
		fixture := installWakeTerminalAuthorityFixture(t)
		stop := make(chan struct{})
		authority, err := bindWakeTerminalAuthority(fixture.generation, stop)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = authority.Close() })

		beforeCalls := 0
		injectCalls := 0
		err = injectNotification(&wakeConfig{
			me:          "codex",
			injectMode:  wakeInjectModeRaw,
			controlStop: stop,
			beforeTerminalWrite: func() error {
				beforeCalls++
				if err := authority.BeforeWrite(); err != nil {
					return err
				}
				close(stop)
				return nil
			},
			terminalWrite: func(chunk string) error {
				injectCalls++
				return authority.Inject(chunk)
			},
		}, "dynamic", false)
		if err != nil {
			t.Fatalf("stop before retained inject: %v", err)
		}
		if beforeCalls != 1 || injectCalls != 1 {
			t.Fatalf("boundary calls before=%d inject=%d, want 1/1", beforeCalls, injectCalls)
		}
		if len(fixture.injections) != 0 {
			t.Fatalf("stopped authority injected terminal bytes: %#v", fixture.injections)
		}
	})
}

func TestRunWakeLoopTerminatesOnTerminalAuthorityLoss(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "terminal-authority-loss",
			From:    "sender",
			To:      []string{"codex"},
			Thread:  "p2p/sender__codex",
			Subject: "wake",
			Created: time.Now().UTC().Format(time.RFC3339),
		},
		Body: "durable body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	loss := newWakeTerminalAuthorityLoss("test authority loss", nil)
	terminalWriteCalled := false
	err = runWakeLoop(wakeConfig{
		root:        root,
		me:          "codex",
		injectMode:  wakeInjectModeRaw,
		controlStop: make(chan struct{}),
		beforeTerminalWrite: func() error {
			return loss
		},
		terminalWrite: func(string) error {
			terminalWriteCalled = true
			return nil
		},
		onPrepared: func(wakeAdmissionWatcher) error {
			_, deliverErr := deliverToInboxForTest(
				t,
				root,
				"codex",
				"terminal-authority-loss.md",
				data,
			)
			return deliverErr
		},
	})
	if !isWakeTerminalAuthorityLoss(err) || !errors.Is(err, loss) {
		t.Fatalf("wake loop authority-loss result = %v", err)
	}
	if terminalWriteCalled {
		t.Fatal("wake loop wrote after terminal authority loss")
	}
}

func installWakeTerminalAuthorityFixture(t *testing.T) *wakeTerminalAuthorityFixture {
	t.Helper()
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".wake.lock")
	lockRaw := []byte(`{"generation":"terminal-authority-generation"}`)
	if err := os.WriteFile(lockPath, lockRaw, 0o400); err != nil {
		t.Fatal(err)
	}
	lockInfo, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	ttyPath := filepath.Join(dir, "tty")
	if err := os.WriteFile(ttyPath, []byte("tty"), 0o600); err != nil {
		t.Fatal(err)
	}
	generation := wakeLockInspection{
		Exists:   true,
		Root:     dir,
		Agent:    "codex",
		LockPath: lockPath,
		Lock: wakeLock{
			Generation: "terminal-authority-generation",
		},
		raw:      lockRaw,
		fileInfo: lockInfo,
	}
	fixture := &wakeTerminalAuthorityFixture{
		generation:     generation,
		current:        generation,
		currentTTYPath: ttyPath,
		foregroundPGRP: 4242,
	}

	originalOpen := openWakeControllingTerminal
	originalPGRP := wakeTerminalForegroundPGRP
	originalInspect := inspectWakeTerminalGeneration
	originalInject := injectWakeTerminalFD
	openWakeControllingTerminal = func() (*os.File, error) {
		return os.OpenFile(fixture.currentTTYPath, os.O_RDWR, 0)
	}
	wakeTerminalForegroundPGRP = func(uintptr) (int, error) {
		return fixture.foregroundPGRP, nil
	}
	inspectWakeTerminalGeneration = func(root, agent string) wakeLockInspection {
		if root != generation.Root || agent != generation.Agent {
			t.Fatalf("inspect generation root=%q agent=%q", root, agent)
		}
		return fixture.current
	}
	injectWakeTerminalFD = func(fd uintptr, text string) error {
		fixture.injections = append(fixture.injections, wakeTerminalInjection{fd: fd, text: text})
		return nil
	}
	t.Cleanup(func() {
		openWakeControllingTerminal = originalOpen
		wakeTerminalForegroundPGRP = originalPGRP
		inspectWakeTerminalGeneration = originalInspect
		injectWakeTerminalFD = originalInject
	})
	return fixture
}
