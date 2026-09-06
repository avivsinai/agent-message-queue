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
	replacementInfo, err := os.Stat(replacementPath)
	if err != nil {
		t.Fatal(err)
	}
	replacement, ok := captureWakeFileIdentity(replacementInfo)
	if !ok {
		t.Fatal("capture replacement terminal identity")
	}
	originalInfo, err := authority.tty.Stat()
	if err != nil {
		t.Fatal(err)
	}
	original, ok := captureWakeFileIdentity(originalInfo)
	if !ok {
		t.Fatal("capture original terminal identity")
	}
	if original.Device == replacement.Device && original.Inode == replacement.Inode {
		t.Fatalf("replacement did not change terminal Dev/Ino: original=%+v replacement=%+v", original, replacement)
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
	loss := newWakeTerminalAuthorityLoss("test authority loss")
	terminalWriteCalled := false
	attentionWrites := 0
	err = runWakeLoop(wakeConfig{
		root:        root,
		me:          "codex",
		injectMode:  wakeInjectModeRaw,
		controlStop: make(chan struct{}),
		attentionIsTTY: func() bool {
			return false
		},
		attentionWrite: func(data []byte) (int, error) {
			attentionWrites++
			return len(data), nil
		},
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
	if isWakeTerminalForegroundPGRPChanged(err) {
		t.Fatalf("generic authority loss classified as foreground-pgrp change: %v", err)
	}
	if terminalWriteCalled {
		t.Fatal("wake loop wrote after terminal authority loss")
	}
	// Conclusive authority loss means the terminal is no longer ours. Preserve
	// the typed fatal exit without narrating into the lost destination.
	if attentionWrites != 0 {
		t.Fatalf("wake loop authority-loss attention writes = %d, want 0", attentionWrites)
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
