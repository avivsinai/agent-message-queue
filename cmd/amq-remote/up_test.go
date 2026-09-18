package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// fakeProc is a process that exits with a configured code after a delay.
type fakeProc struct {
	code    int
	delay   time.Duration
	started chan struct{}
}

func (p *fakeProc) Wait() (int, error) {
	time.Sleep(p.delay)
	return p.code, nil
}
func (p *fakeProc) Signal(os.Signal) error { return nil }

// fakeSpawner returns a sequence of processes from a queue.
type fakeSpawner struct {
	mu     sync.Mutex
	procs  []fakeProc
	spawns int
}

func (s *fakeSpawner) Spawn(ctx context.Context, args []string) (process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.spawns
	s.spawns++
	if idx >= len(s.procs) {
		// Should not happen in the test; return a clean exit.
		return &fakeProc{code: 0, delay: 0}, nil
	}
	p := s.procs[idx]
	if p.started != nil {
		close(p.started)
	}
	return &p, nil
}

// TestUpRespawnsOnceAfterNonZeroExit (611.13 PR2) pins that up respawns serve
// after a non-zero exit, then stops on a clean exit 0. Uses an injected
// spawner (no real process). The backoff is bypassed via a short base.
func TestUpRespawnsOnceAfterNonZeroExit(t *testing.T) {
	root, err := os.MkdirTemp("", "amqup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	// Use a temp registry so we don't touch the real keepalive registry.
	regPath := filepath.Join(root, "registry.json")

	// Injected spawner: first serve exits 1 (crash), second exits 0 (clean).
	sp := &fakeSpawner{
		procs: []fakeProc{
			{code: 1, delay: 5 * time.Millisecond},
			{code: 0, delay: 5 * time.Millisecond},
		},
	}

	// Build the up command manually (bypass flag parsing for the spawner).
	// We test the supervision loop directly, not the flag wiring.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	upCfg := upConfig{
		root:         root,
		me:           "amq-remote",
		registryPath: regPath,
		maxRestarts:  5,
		backoffBase:  time.Millisecond, // short for the test
		backoffMax:   10 * time.Millisecond,
		serveArgs:    []string{"serve", "--root", root},
	}
	code, err := runUpLoop(ctx, upCfg, sp)
	if err != nil {
		t.Fatalf("runUpLoop: %v", err)
	}
	if code != 0 {
		t.Fatalf("up exited %d, want 0", code)
	}
	if sp.spawns != 2 {
		t.Fatalf("spawns=%d, want 2 (one crash + one clean)", sp.spawns)
	}

	// Verify the loop spawned twice (one crash + one clean).
}
