//go:build darwin

package cli

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func testDarwinControlLock(t *testing.T) (string, string, wakeLock) {
	t.Helper()
	root := secureTempDirForTest(t)
	const agent = "codex"
	lock := wakeLock{PID: os.Getpid(), WakeMode: wakeTargetInjectVia, Generation: "0123456789abcdef0123456789abcdef"}
	lock.ControlSocket = wakeControlSocketPath(root, agent, lock.Generation)
	writeWakeLockForTest(t, root, agent, lock)
	return root, agent, lock
}

func TestDarwinCooperativeStopACKAfterLockRemoval(t *testing.T) {
	root, agent, lock := testDarwinControlLock(t)
	cleanup, stopped, markStopped, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	go func() { <-stopped; markStopped() }()
	inspection := inspectWakeLock(root, agent)
	replaced, err := cooperativeStopInjectVia(inspection)
	if err != nil || !replaced {
		t.Fatalf("stop=(%v,%v)", replaced, err)
	}
	if current := inspectWakeLock(root, agent); current.Exists {
		t.Fatal("ACK arrived before lock removal")
	}
}

func TestDarwinCooperativeStopACKWaitsForLoopExit(t *testing.T) {
	root, agent, lock := testDarwinControlLock(t)
	cleanup, stopped, markStopped, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	type result struct {
		stopped bool
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		stopped, err := cooperativeStopInjectVia(inspectWakeLock(root, agent))
		resultCh <- result{stopped: stopped, err: err}
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not request loop stop")
	}
	if current := inspectWakeLock(root, agent); current.Exists {
		t.Fatal("listener requested loop stop before removing its lock")
	}
	if err := withWakeLifecycleGuard(root, agent, func() error { return nil }); err != nil {
		t.Fatalf("listener held lifecycle guard while waiting for loop exit: %v", err)
	}
	select {
	case got := <-resultCh:
		t.Fatalf("stop returned before loop exit: %+v", got)
	default:
	}
	markStopped()
	select {
	case got := <-resultCh:
		if got.err != nil || !got.stopped {
			t.Fatalf("stop=(%v,%v)", got.stopped, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not ACK after loop exit")
	}
}

func TestDarwinControlTokenMismatchRefused(t *testing.T) {
	root, agent, lock := testDarwinControlLock(t)
	cleanup, _, _, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	conn, err := dialDarwinUnix(lock.ControlSocket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = conn.Write([]byte("wrong-token\n"))
	line, _ := bufio.NewReader(conn).ReadString('\n')
	if strings.TrimSpace(line) == "ACK" {
		t.Fatal("mismatched token acknowledged")
	}
	if !inspectWakeLock(root, agent).Exists {
		t.Fatal("mismatched token removed lock")
	}
}

func TestDarwinMissingControlMetadataRefused(t *testing.T) {
	_, err := cooperativeStopInjectVia(wakeLockInspection{Lock: wakeLock{Generation: "g"}})
	if err == nil {
		t.Fatal("legacy inject-via wake accepted without control metadata")
	}
}

func TestDarwinControlRemovesStaleSocketBeforeListen(t *testing.T) {
	root, agent, lock := testDarwinControlLock(t)
	oldSocket := filepath.Join(fsq.AgentBase(root, agent), ".w.stale-generation")
	if err := os.WriteFile(oldSocket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock.ControlSocket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanup, _, _, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatalf("stale socket prevented listener: %v", err)
	}
	if _, err := os.Lstat(oldSocket); !os.IsNotExist(err) {
		t.Fatalf("old generation socket still exists: %v", err)
	}
	cleanup()
}
