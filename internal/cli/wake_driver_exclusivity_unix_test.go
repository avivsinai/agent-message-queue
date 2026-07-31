//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// An unreadable lock parks the incumbent without surrendering its admitted
// backlog. The first conclusive read either resumes the same generation or,
// as exercised here, proves replacement and terminates the stale driver.
func TestUnreadableWakeLockParksIncumbentUntilConclusiveReplacement(t *testing.T) {
	fixture := installWakeTerminalAuthorityFixture(t)
	stop := make(chan struct{})
	authority, err := bindWakeTerminalAuthority(fixture.generation, stop)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	var injections atomic.Int64
	injectWakeTerminalFD = func(uintptr, string) error {
		injections.Add(1)
		return nil
	}

	unreadable := wakeLockInspection{
		Exists:   true,
		Status:   wakeLockUnverified,
		Reason:   "cannot read lock: too many open files",
		Root:     fixture.generation.Root,
		Agent:    fixture.generation.Agent,
		LockPath: fixture.generation.LockPath,
	}
	fixture.current = unreadable

	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	deliverWakeWatcherMessageForTest(t, root, "codex", "during-outage", "claude")

	attention := make(chan string, 4)
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:                root,
			me:                  "codex",
			session:             "session1",
			wakeOwner:           &wakeOwner{},
			debounce:            5 * time.Millisecond,
			injectMode:          wakeInjectModeRaw,
			controlStop:         stop,
			beforeTerminalWrite: authority.BeforeWrite,
			terminalWrite:       authority.Inject,
			attentionIsTTY:      func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				select {
				case attention <- string(data):
				default:
				}
				return len(data), nil
			},
		})
	}()
	var exited atomic.Bool
	t.Cleanup(func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		if exited.Load() {
			return
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	})

	select {
	case <-attention:
	case err := <-done:
		t.Fatalf("unreadable lock killed the wake loop: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("parked wake emitted no attention during lock outage")
	}
	select {
	case err := <-done:
		t.Fatalf("unreadable lock killed the wake loop: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if got := injections.Load(); got != 0 {
		t.Fatalf("incumbent injected %d chunks while its lock was unreadable", got)
	}

	if err := os.Remove(filepath.Join(
		fsq.AgentInboxNew(root, "codex"), "during-outage.md",
	)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	lockPath := fixture.generation.LockPath
	replacementRaw := []byte(`{"generation":"replacement-generation"}`)
	if err := os.WriteFile(lockPath+".replacement", replacementRaw, 0o400); err != nil {
		t.Fatal(err)
	}
	replacementInfo, err := os.Stat(lockPath + ".replacement")
	if err != nil {
		t.Fatal(err)
	}
	fixture.current = wakeLockInspection{
		Exists:   true,
		Root:     fixture.generation.Root,
		Agent:    fixture.generation.Agent,
		LockPath: lockPath,
		Lock:     wakeLock{Generation: "replacement-generation"},
		raw:      replacementRaw,
		fileInfo: replacementInfo,
	}
	deliverWakeWatcherMessageForTest(t, root, "codex", "after-recovery", "claude")

	select {
	case err := <-done:
		exited.Store(true)
		if !isWakeTerminalAuthorityLoss(err) ||
			!strings.Contains(err.Error(), "wake generation changed") {
			t.Fatalf("incumbent exit = %v, want positive generation-changed loss", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf(
			"first successful read of replacement lock did not converge the incumbent (injections=%d)",
			injections.Load(),
		)
	}
	if got := injections.Load(); got != 0 {
		t.Fatalf("incumbent injected %d chunks alongside the replacement", got)
	}
}

// Paths without a bound terminal authority still fail closed at each write:
// neither an unreadable lock nor a proven replacement can authorize input.
func TestPlatformWriteGateRejectsUnreadableAndReplacedWakeLock(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	lockPath := filepath.Join(root, "agents", "codex", ".wake.lock")
	if err := os.WriteFile(lockPath, []byte(`{"generation":"mine","tty":"unknown"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &wakeConfig{
		root:               root,
		me:                 "codex",
		terminalGeneration: "mine",
		terminalTTY:        "unknown",
	}
	if !authorizeTerminalWritePlatform(cfg) {
		t.Fatal("gate refused the incumbent's own readable lock")
	}

	if err := os.Chmod(lockPath, 0); err != nil {
		t.Fatal(err)
	}
	if authorizeTerminalWritePlatform(cfg) {
		t.Fatal("gate authorized a write with an unreadable lock")
	}
	if err := os.Chmod(lockPath, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(lockPath, []byte(`{"generation":"replacement","tty":"unknown"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if authorizeTerminalWritePlatform(cfg) {
		t.Fatal("gate authorized a write against a replacement's lock")
	}
}
