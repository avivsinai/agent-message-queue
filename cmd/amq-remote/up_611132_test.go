//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"bytes"
	"context"
	"io"
	"os"
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
	// SIGKILL. The pre-B4 failure mode was 10s+ and still counting. Bound =
	// delay + 1s (review r3 P2-2): the broken build fails at ~2.5s, so the
	// red margin must stay well clear of it — with delay+2s a parent trap
	// wait that overruns ~130ms on a loaded runner let the mutation pass.
	if elapsed > delay+time.Second {
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
			{code: 1},                              // immediate crash: escalates restart to 1
			{code: 1},                              // immediate crash: escalates restart to 2
			{code: 1, onWait: advance(healthyRun)}, // healthy-lived crash AFTER escalation
			{code: 0},                              // clean exit ends supervision
		},
	}
	cfg := upConfig{
		maxRestarts: 5,
		backoffBase: base,
		backoffMax:  2 * time.Second,
		serveArgs:   []string{"serve"},
		now:         now,
	}
	var log bytes.Buffer
	code, err := runUpLoop(context.Background(), cfg, sp, &log)
	if err != nil {
		t.Fatalf("runUpLoop: %v", err)
	}
	if code != 0 {
		t.Fatalf("up exited %d, want 0", code)
	}
	out := log.String()
	if !strings.Contains(out, "resetting backoff series") {
		t.Fatalf("expected healthy-uptime reset announcement, got:\n%s", out)
	}
	// Pin the RESET, not just the announcement: the healthy run happens only
	// after two escalated crashes (restart=2, next wait would be 2*base), so
	// without the reset assignment the respawn following the reset
	// escalates further (verifier r3 P1: with the healthy child first,
	// `restart = 0` was a no-op and this test passed with the fix removed).
	// Assert ORDER: the base-step respawn must come AFTER the reset
	// announcement (the pre-reset respawns include base too, so a bare
	// Contains would pass even with the reset deleted).
	escalated := 2 * base
	resetIdx := strings.Index(out, "resetting backoff series")
	postReset := out[resetIdx:]
	if !strings.Contains(out, "respawning in "+escalated.String()) {
		t.Fatalf("expected the escalated wait %s before the healthy run, got:\n%s", escalated, out)
	}
	if !strings.Contains(postReset, "respawning in "+base.String()) {
		t.Fatalf("expected the respawn after the healthy run to be the base step %s, got:\n%s", base, postReset)
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
		maxRestarts: 2,
		backoffBase: 20 * time.Millisecond,
		backoffMax:  100 * time.Millisecond,
		serveArgs:   []string{"serve"},
		now:         now,
	}
	var log bytes.Buffer
	code, err := runUpLoop(context.Background(), cfg, sp, &log)
	if err == nil {
		t.Errorf("runUpLoop: want max-restarts error, got nil")
	}
	if code != 1 {
		t.Fatalf("up exited %d, want 1", code)
	}
	out := log.String()
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
	sp := &fakeSpawner{
		procs: []fakeProc{{code: 2, delay: 5 * time.Millisecond}},
	}
	cfg := upConfig{
		maxRestarts: 5,
		backoffBase: time.Millisecond,
		backoffMax:  10 * time.Millisecond,
		serveArgs:   []string{"serve"},
	}
	code, err := runUpLoop(context.Background(), cfg, sp, io.Discard)
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

// TestUpReclaimsPhantomOnRealEntryPoint (611.13.2 d, P1 phantom): the real
// up() entry point reclaims a row whose lifetime lock is not held (here no
// lock file exists at all: a kill -9'd up) before spawning, with a
// clean-exit spawner so no serve runs. A row whose lock IS held survives:
// TestUpTargetOwnedByAnotherRegistryEntryExitsActionRequired.
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

	clean := func(string) spawner {
		return &fakeSpawner{procs: []fakeProc{{code: 0, delay: 5 * time.Millisecond}}}
	}
	code, err := up([]string{"--root", root, "--me", "agent-c", "--registry", regPath}, io.Discard, io.Discard, clean)
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
