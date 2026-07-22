//go:build darwin

package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func TestDarwinControlRootAliasSupportsStartAndCooperativeStop(t *testing.T) {
	realRoot := filepath.Join(secureTempDirForTest(t), "real-root")
	if err := fsq.EnsureAgentDirs(realRoot, "codex"); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(secureTempDirForTest(t), "root-alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("root symlinks unsupported: %v", err)
	}
	lock := wakeLock{
		PID:        os.Getpid(),
		Root:       canonicalWakeRoot(aliasRoot),
		Agent:      "codex",
		WakeMode:   wakeTargetInjectVia,
		Generation: "0123456789abcdef0123456789abcdef",
	}
	lock.ControlSocket = wakeControlSocketPath(aliasRoot, "codex", lock.Generation)
	if got, want := filepath.Dir(lock.ControlSocket), fsq.AgentBase(realRoot, "codex"); got != want {
		t.Fatalf("control socket directory = %q, want canonical agent directory %q", got, want)
	}
	writeWakeLockExactForTest(t, aliasRoot, "codex", lock)

	cleanup, stopped, markStopped, err := startWakeControlListener(aliasRoot, "codex", lock)
	if err != nil {
		t.Fatalf("start through root alias: %v", err)
	}
	defer cleanup()
	go func() { <-stopped; markStopped() }()
	replaced, err := cooperativeStopInjectVia(inspectWakeLock(aliasRoot, "codex"))
	if err != nil || !replaced {
		t.Fatalf("stop through root alias=(%v,%v)", replaced, err)
	}
	if current := inspectWakeLock(realRoot, "codex"); current.Exists {
		t.Fatal("canonical generation remained after cooperative stop")
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
	if current := inspectWakeLock(root, agent); !current.Exists || current.Lock.Generation != lock.Generation {
		t.Fatal("listener unpublished its generation before loop exit")
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

func TestDarwinCooperativeStopKeepsGenerationPublishedUntilLoopExit(t *testing.T) {
	root, agent, lock := testDarwinControlLock(t)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", agent},
		}
	})
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
	defer markStopped()

	current := inspectWakeLock(root, agent)
	if !current.Exists || current.Lock.Generation != lock.Generation {
		t.Fatalf("stopping generation was unpublished before loop exit: %#v", current)
	}
	concurrentCleanup, acquireErr := acquireWakeLock(root, agent, nil)
	if concurrentCleanup != nil {
		concurrentCleanup()
	}
	if acquireErr == nil {
		t.Fatalf("concurrent acquire started a second generation before the old loop exited; stopping=%#v", current)
	}
	if !strings.Contains(acquireErr.Error(), "already running") {
		t.Fatalf("concurrent acquire error = %v, want already-running refusal", acquireErr)
	}

	markStopped()
	select {
	case got := <-resultCh:
		if got.err != nil || !got.stopped {
			t.Fatalf("stop=(%v,%v)", got.stopped, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not complete after loop exit")
	}
}

func TestDarwinCooperativeStopCompletionOutlivesAuthenticationDeadline(t *testing.T) {
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

	// Authentication is complete, but the synchronous injector may still be
	// running for longer than the authentication deadline.
	time.Sleep(2200 * time.Millisecond)
	markStopped()
	select {
	case got := <-resultCh:
		if got.err != nil || !got.stopped {
			t.Fatalf("stop=(%v,%v), want completion after delayed loop exit", got.stopped, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not complete after delayed loop exit")
	}
}

func TestDarwinControlTokenMismatchRefused(t *testing.T) {
	root, agent, lock := testDarwinControlLock(t)
	cleanup, _, _, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	name, err := darwinControlSocketName(agentDir, lock.ControlSocket)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialDarwinUnixAt(agentDir, name, time.Second)
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

func TestDarwinControlStartPinsAuthorizedAgentDirectory(t *testing.T) {
	parent := secureTempDirForTest(t)
	root := filepath.Join(parent, "root")
	parkedRoot := filepath.Join(parent, "authorized-root")
	replacementRoot := filepath.Join(parent, "replacement-root")
	const agent = "codex"
	lock := wakeLock{PID: os.Getpid(), WakeMode: wakeTargetInjectVia, Generation: "0123456789abcdef0123456789abcdef"}
	lock.ControlSocket = wakeControlSocketPath(root, agent, lock.Generation)
	writeWakeLockForTest(t, root, agent, lock)
	if err := fsq.EnsureAgentDirs(replacementRoot, agent); err != nil {
		t.Fatal(err)
	}
	replacementSocket := filepath.Join(fsq.AgentBase(replacementRoot, agent), filepath.Base(lock.ControlSocket))
	const sentinel = "replacement-tree-sentinel"
	if err := os.WriteFile(replacementSocket, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	var swapOnce sync.Once
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		swapOnce.Do(func() {
			if err := os.Rename(root, parkedRoot); err != nil {
				t.Fatalf("park authorized root: %v", err)
			}
			if err := os.Rename(replacementRoot, root); err != nil {
				t.Fatalf("install replacement root: %v", err)
			}
		})
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", agent},
		}
	})

	cleanup, _, _, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatalf("start control listener: %v", err)
	}
	parkedSocket := filepath.Join(fsq.AgentBase(parkedRoot, agent), filepath.Base(lock.ControlSocket))
	if info, err := os.Lstat(parkedSocket); err != nil || info.Mode()&os.ModeSocket == 0 {
		cleanup()
		t.Fatalf("listener was not bound in authorized agent directory: info=%v err=%v", info, err)
	}
	if got, err := os.ReadFile(filepath.Join(fsq.AgentBase(root, agent), filepath.Base(lock.ControlSocket))); err != nil || string(got) != sentinel {
		cleanup()
		t.Fatalf("replacement tree was modified: got=%q err=%v", got, err)
	}

	cleanup()
	if _, err := os.Lstat(parkedSocket); !os.IsNotExist(err) {
		t.Fatalf("authorized socket survived cleanup: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(fsq.AgentBase(root, agent), filepath.Base(lock.ControlSocket))); err != nil || string(got) != sentinel {
		t.Fatalf("cleanup modified replacement tree: got=%q err=%v", got, err)
	}
}

func TestDarwinControlCleanupPinsAuthorizedAgentDirectory(t *testing.T) {
	parent := secureTempDirForTest(t)
	root := filepath.Join(parent, "root")
	parkedRoot := filepath.Join(parent, "authorized-root")
	replacementRoot := filepath.Join(parent, "replacement-root")
	const agent = "codex"
	lock := wakeLock{PID: os.Getpid(), WakeMode: wakeTargetInjectVia, Generation: "0123456789abcdef0123456789abcdef"}
	lock.ControlSocket = wakeControlSocketPath(root, agent, lock.Generation)
	writeWakeLockForTest(t, root, agent, lock)

	cleanup, _, _, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatalf("start control listener: %v", err)
	}
	if err := fsq.EnsureAgentDirs(replacementRoot, agent); err != nil {
		cleanup()
		t.Fatal(err)
	}
	replacementSocket := filepath.Join(fsq.AgentBase(replacementRoot, agent), filepath.Base(lock.ControlSocket))
	const sentinel = "replacement-tree-sentinel"
	if err := os.WriteFile(replacementSocket, []byte(sentinel), 0o600); err != nil {
		cleanup()
		t.Fatal(err)
	}
	if err := os.Rename(root, parkedRoot); err != nil {
		cleanup()
		t.Fatalf("park authorized root: %v", err)
	}
	if err := os.Rename(replacementRoot, root); err != nil {
		cleanup()
		t.Fatalf("install replacement root: %v", err)
	}

	cleanup()
	parkedSocket := filepath.Join(fsq.AgentBase(parkedRoot, agent), filepath.Base(lock.ControlSocket))
	if _, err := os.Lstat(parkedSocket); !os.IsNotExist(err) {
		t.Fatalf("authorized socket survived cleanup: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(fsq.AgentBase(root, agent), filepath.Base(lock.ControlSocket))); err != nil || string(got) != sentinel {
		t.Fatalf("cleanup modified replacement tree: got=%q err=%v", got, err)
	}
}

func TestDarwinControlAuthenticationPinsAuthorizedAgentDirectory(t *testing.T) {
	parent := secureTempDirForTest(t)
	root := filepath.Join(parent, "root")
	parkedRoot := filepath.Join(parent, "authorized-root")
	replacementRoot := filepath.Join(parent, "replacement-root")
	const agent = "codex"
	lock := wakeLock{
		PID:        os.Getpid(),
		Root:       canonicalWakeRoot(root),
		Agent:      agent,
		WakeMode:   wakeTargetInjectVia,
		Generation: "0123456789abcdef0123456789abcdef",
	}
	lock.ControlSocket = wakeControlSocketPath(root, agent, lock.Generation)
	writeWakeLockExactForTest(t, root, agent, lock)
	writeWakeLockExactForTest(t, replacementRoot, agent, lock)

	var inspections atomic.Int32
	swapErr := make(chan error, 1)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if inspections.Add(1) == 2 {
			if err := os.Rename(root, parkedRoot); err != nil {
				swapErr <- fmt.Errorf("park authorized root: %w", err)
			} else if err := os.Rename(replacementRoot, root); err != nil {
				swapErr <- fmt.Errorf("install replacement root: %w", err)
			}
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", agent},
		}
	})

	cleanup, stopped, markStopped, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatalf("start control listener: %v", err)
	}
	defer cleanup()
	type stopResult struct {
		stopped bool
		err     error
	}
	resultCh := make(chan stopResult, 1)
	go func() {
		stopped, err := cooperativeStopInjectVia(wakeLockInspection{Root: root, Agent: agent, Lock: lock})
		resultCh <- stopResult{stopped: stopped, err: err}
	}()
	select {
	case <-stopped:
	case err := <-swapErr:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not request stop after authenticated ancestor swap")
	}
	markStopped()
	select {
	case got := <-resultCh:
		if got.err != nil || !got.stopped {
			t.Fatalf("stop=(%v,%v)", got.stopped, got.err)
		}
	case err := <-swapErr:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not ACK after authenticated ancestor swap")
	}

	if _, err := os.Lstat(filepath.Join(fsq.AgentBase(parkedRoot, agent), ".wake.lock")); !os.IsNotExist(err) {
		t.Fatalf("authorized generation survived cooperative cleanup: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(fsq.AgentBase(root, agent), ".wake.lock")); err != nil {
		t.Fatalf("replacement-tree generation was modified: %v", err)
	}
}
