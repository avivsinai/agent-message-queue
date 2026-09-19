//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
)

// captureStderr swaps os.Stderr for a pipe, runs fn, and returns everything
// the code under test printed. runUpLoop writes to os.Stderr directly, so
// the tests assert on the supervisor's own log lines (the same lines an
// operator sees).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stderr = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// TestUpWaitDelayKillsSigtermIgnoringChild (611.13.2 a, B4): a serve child
// that ignores SIGTERM must not keep up alive past the WaitDelay grace. The
// child traps SIGTERM and sleeps; the real execSpawner delivers SIGTERM via
// cmd.Cancel and SIGKILL after WaitDelay. Wait must return within the bound
// plus slack.
func TestUpWaitDelayKillsSigtermIgnoringChild(t *testing.T) {
	if testing.Short() {
		t.Skip("real child process")
	}
	// The helper gates on this env var; the child inherits the parent's
	// environment (execSpawner does not scrub it).
	t.Setenv("GO_TEST_HELPER_UP611132", "1")
	// Helper child: this test binary in helper mode.
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const delay = 400 * time.Millisecond
	sp := &execSpawner{binary: bin, waitDelay: delay, env: append(os.Environ(), "GO_TEST_HELPER_UP611132=1")}
	cmdArgs := []string{"-test.run=TestHelperSigtermIgnoringChild$"}

	ctx, cancel := context.WithCancel(context.Background())
	proc, err := sp.Spawn(ctx, cmdArgs)
	if err != nil {
		t.Fatal(err)
	}
	// Give the child time to install its SIGTERM trap.
	time.Sleep(500 * time.Millisecond)
	start := time.Now()
	cancel() // SIGTERM to the child
	_, _ = proc.Wait()
	elapsed := time.Since(start)

	// The child ignores SIGTERM, so the kill lands only via WaitDelay's
	// SIGKILL. The pre-B4 failure mode was 10s+ and still counting; the
	// bound is delay + modest slack (611.13.2 review P2-1: the helper only
	// sleeps 3s, so a broken build fails fast instead of burning 30s).
	if elapsed > delay+2*time.Second {
		t.Fatalf("up waited %s for a SIGTERM-ignoring child; WaitDelay=%s not enforced", elapsed.Round(time.Millisecond), delay)
	}
}

// TestHelperSigtermIgnoringChild is the B4 helper process: it ignores SIGTERM
// and sleeps. Selected by -test.run from TestUpWaitDelayKillsSigtermIgnoringChild.
func TestHelperSigtermIgnoringChild(t *testing.T) {
	if os.Getenv("GO_TEST_HELPER_UP611132") != "1" {
		t.Skip("helper only")
	}
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for range c {
			// Ignore: the whole point.
		}
	}()
	// 3s: comfortably longer than the parent's 500ms trap-install wait plus
	// the 400ms WaitDelay, but bounded so a broken parent build fails fast
	// instead of burning 30s of CI (611.13.2 review P2-1).
	time.Sleep(3 * time.Second)
}

// TestUpBackoffResetsAfterHealthyUptime (611.13.2 b, B5): a child that ran
// longer than the healthy-uptime threshold did not fail immediately — the
// next wait must be the BASE backoff step, not an escalated one. Asserted on
// the supervisor's own log lines for determinism. Time is injected
// (611.13.2 review P2-2): the first child's Wait advances the fake clock by
// a healthy-lived span, so the scenario is instant and wall-clock-free.
func TestUpBackoffResetsAfterHealthyUptime(t *testing.T) {
	root, err := os.MkdirTemp("", "amqup-b5")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	const base = 200 * time.Millisecond
	const healthyRun = 500 * time.Millisecond // >= upHealthyUptime(base) = 400ms
	var mu sync.Mutex
	clock := time.Unix(0, 0)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	advance := func(d time.Duration) func() {
		return func() {
			mu.Lock()
			defer mu.Unlock()
			clock = clock.Add(d)
		}
	}
	sp := &fakeSpawner{
		procs: []fakeProc{
			{code: 1, onWait: advance(healthyRun)}, // healthy-lived crash
			{code: 1},                              // immediate crash, no advance
			{code: 0},                              // clean exit ends supervision
		},
	}
	cfg := upConfig{
		root:         root,
		me:           "amq-remote",
		registryPath: filepath.Join(root, "registry.json"),
		maxRestarts:  5,
		backoffBase:  base,
		backoffMax:   2 * time.Second,
		serveArgs:    []string{"serve"},
		now:          now,
	}
	var code int
	out := captureStderr(t, func() {
		var lerr error
		code, lerr = runUpLoop(context.Background(), cfg, sp)
		if lerr != nil {
			t.Errorf("runUpLoop: %v", lerr)
		}
	})
	if code != 0 {
		t.Fatalf("up exited %d, want 0", code)
	}
	if !strings.Contains(out, "resetting backoff series") {
		t.Fatalf("expected healthy-uptime reset announcement, got:\n%s", out)
	}
	if !strings.Contains(out, "respawning in "+base.String()) {
		t.Fatalf("expected the first respawn after the healthy run to be the base step %s, got:\n%s", base, out)
	}
}

// TestUpMaxRestartsBoundsLifetimeAcrossHealthyResets pins review-b8 P1-2
// (611.13.2): --max-restarts bounds the LIFETIME respawn budget, not the
// current backoff series. Eight children each live healthy-long (the fake
// clock advances past the healthy threshold on every Wait) then exit 1;
// with cap 2 supervision must stop at the cap, even though the backoff
// index resets on every healthy run. Before the recut this loop never
// ended (spawns=9, "max-restarts exceeded" never printed).
func TestUpMaxRestartsBoundsLifetimeAcrossHealthyResets(t *testing.T) {
	root, err := os.MkdirTemp("", "amqup-budget")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	clock := time.Unix(0, 0)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	// Every child lives healthy-long: Wait advances the clock past
	// upHealthyUptime(base) = 2 * 20ms, then the child exits 1.
	advance := func() {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(50 * time.Millisecond)
	}
	procs := make([]fakeProc, 8)
	for i := range procs {
		procs[i] = fakeProc{code: 1, onWait: advance}
	}
	sp := &fakeSpawner{procs: procs}
	cfg := upConfig{
		root:         root,
		me:           "amq-remote",
		registryPath: filepath.Join(root, "registry.json"),
		maxRestarts:  2,
		backoffBase:  20 * time.Millisecond,
		backoffMax:   100 * time.Millisecond,
		serveArgs:    []string{"serve"},
		now:          now,
	}
	var code int
	out := captureStderr(t, func() {
		var lerr error
		code, lerr = runUpLoop(context.Background(), cfg, sp)
		if lerr == nil {
			t.Errorf("runUpLoop: want max-restarts error, got nil")
		}
	})
	if code != 1 {
		t.Fatalf("up exited %d, want 1", code)
	}
	if !strings.Contains(out, "max-restarts (2) exceeded") {
		t.Fatalf("expected the max-restarts announcement, got:\n%s", out)
	}
	// 1 initial spawn + 2 budgeted respawns = 3 spawns, no more.
	if sp.spawns != 3 {
		t.Fatalf("spawns = %d, want 3 (cap 2 bounds lifetime respawns)", sp.spawns)
	}
}

// TestUpUsageErrorIsTerminal (611.13.2 c, B6): a child exiting 2 (usage
// refusal) ends supervision with the same code — no respawn loop.
func TestUpUsageErrorIsTerminal(t *testing.T) {
	root, err := os.MkdirTemp("", "amqup-b6")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	sp := &fakeSpawner{
		procs: []fakeProc{{code: 2, delay: 5 * time.Millisecond}},
	}
	cfg := upConfig{
		root:         root,
		me:           "amq-remote",
		registryPath: filepath.Join(root, "registry.json"),
		maxRestarts:  5,
		backoffBase:  time.Millisecond,
		backoffMax:   10 * time.Millisecond,
		serveArgs:    []string{"serve"},
	}
	code, err := runUpLoop(context.Background(), cfg, sp)
	if err != nil {
		t.Fatalf("runUpLoop: %v", err)
	}
	if code != 2 {
		t.Fatalf("up exited %d, want 2 (usage error propagated)", code)
	}
	if sp.spawns != 1 {
		t.Fatalf("spawns=%d, want 1 (usage error must not respawn)", sp.spawns)
	}
}

// TestUpExitOneStillRespawns (611.13.2 c negative arm): exit 1 still
// respawns — pinned so the B6 change cannot over-trigger.
func TestUpExitOneStillRespawns(t *testing.T) {
	root, err := os.MkdirTemp("", "amqup-b6n")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	sp := &fakeSpawner{
		procs: []fakeProc{
			{code: 1, delay: 5 * time.Millisecond},
			{code: 0, delay: 5 * time.Millisecond},
		},
	}
	cfg := upConfig{
		root:         root,
		me:           "amq-remote",
		registryPath: filepath.Join(root, "registry.json"),
		maxRestarts:  5,
		backoffBase:  time.Millisecond,
		backoffMax:   10 * time.Millisecond,
		serveArgs:    []string{"serve"},
	}
	code, err := runUpLoop(context.Background(), cfg, sp)
	if err != nil {
		t.Fatalf("runUpLoop: %v", err)
	}
	if code != 0 {
		t.Fatalf("up exited %d, want 0", code)
	}
	if sp.spawns != 2 {
		t.Fatalf("spawns=%d, want 2 (exit 1 respawns)", sp.spawns)
	}
}

// TestReclaimPhantomCompanionRow (611.13.2 d, P1 phantom): a registry row
// whose lifetime lock is NOT held is reclaimed by the next up; a live row
// (lock held) and the caller's own row are untouched.
func TestReclaimPhantomCompanionRow(t *testing.T) {
	root, err := os.MkdirTemp("", "amqup-phantom")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	regPath := filepath.Join(root, "registry.json")
	store := registry.New(regPath)

	// Three rows, each owning its own root (the registry refuses two
	// agents on one target): a phantom (lock unheld), a live peer (lock
	// held by this test), and the caller's own row. Roots must exist so
	// canonicalization inside Upsert matches.
	rootA := filepath.Join(root, "a")
	rootB := filepath.Join(root, "b")
	rootC := filepath.Join(root, "c")
	for _, r := range [3]string{rootA, rootB, rootC} {
		if err := fsq.EnsureRootDirs(r); err != nil {
			t.Fatal(err)
		}
	}
	var phantomID, liveID, ownID string
	rows := []struct {
		root  string
		agent string
		slot  *string
	}{
		{rootA, "agent-a", &phantomID},
		{rootB, "agent-b", &liveID},
		{rootC, "agent-c", &ownID},
	}
	for _, r := range rows {
		up, err := store.Upsert(registry.Entry{Root: r.root, Agent: r.agent, Adapter: "remote", Target: r.root, State: registry.StateActive})
		if err != nil {
			t.Fatal(err)
		}
		*r.slot = up.ID
	}
	// Hold the live row's lock the way a live up would.
	lockFile, err := os.OpenFile(registry.LifetimeLockPath(regPath, liveID), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockFile.Close() }()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) }()

	reclaimed, err := reclaimPhantomCompanions(store, regPath, ownID)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0] != phantomID {
		t.Fatalf("reclaimed=%v, want [%s] only", reclaimed, phantomID)
	}
	file, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, e := range file.Entries {
		if e.ID == phantomID {
			t.Fatalf("phantom row %s survived reclaim", phantomID)
		}
		found[e.ID] = true
	}
	if !found[liveID] || !found[ownID] {
		t.Fatalf("live row or own row lost: live=%v own=%v", found[liveID], found[ownID])
	}
}

// TestProbeLifetimeLockOutcomes pins the probe's three-state contract:
// missing file = not held; held lock = held; released lock = not held.
func TestProbeLifetimeLockOutcomes(t *testing.T) {
	root, err := os.MkdirTemp("", "amqup-probe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	regPath := filepath.Join(root, "registry.json")

	held, err := registry.ProbeLifetimeLock(regPath, "no-such-entry")
	if err != nil || held {
		t.Fatalf("missing file: held=%v err=%v, want false/nil", held, err)
	}

	f, err := os.OpenFile(registry.LifetimeLockPath(regPath, "entry-x"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	held, err = registry.ProbeLifetimeLock(regPath, "entry-x")
	if err != nil || !held {
		t.Fatalf("held lock: held=%v err=%v, want true/nil", held, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	held, err = registry.ProbeLifetimeLock(regPath, "entry-x")
	if err != nil || held {
		t.Fatalf("released lock: held=%v err=%v, want false/nil", held, err)
	}
}

// TestUpReclaimsPhantomOnRealEntryPoint (611.13.2 live-check rehearsal):
// the real up() entry point reclaims a phantom row against the same root
// before spawning, with a clean-exit spawner so no serve runs.
func TestUpReclaimsPhantomOnRealEntryPoint(t *testing.T) {
	root, err := os.MkdirTemp("", "amqup-reclaim")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	regPath := filepath.Join(root, "registry.json")
	store := registry.New(regPath)
	phantomID := registry.EntryID(root, "ghost", "remote", root)
	if _, err := store.Upsert(registry.Entry{ID: phantomID, Root: root, Agent: "ghost", Adapter: "remote", Target: root, State: registry.StateActive}); err != nil {
		t.Fatal(err)
	}

	restore := upSpawnerFactory
	upSpawnerFactory = func(self string) spawner {
		return &fakeSpawner{procs: []fakeProc{{code: 0, delay: 5 * time.Millisecond}}}
	}
	t.Cleanup(func() { upSpawnerFactory = restore })
	selfSaved := selfFlag
	self := filepath.Join(root, "amq-remote-self")
	selfFlag = &self
	t.Cleanup(func() { selfFlag = selfSaved })

	code, err := up([]string{"--root", root, "--me", "agent-c", "--registry", regPath}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if code != 0 {
		t.Fatalf("up exited %d, want 0", code)
	}
	file, err := registry.New(regPath).Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range file.Entries {
		if e.ID == phantomID {
			t.Fatalf("phantom row %s survived a fresh up", phantomID)
		}
	}
}

// keep imports honest: exec and errors are production-code imports surfaced
// here so the test file compiles against the same package surface.
var (
	_ = exec.Command
	_ = errors.New
)
