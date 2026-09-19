package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"flag"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
)

type stdFlag = flag.Flag

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

// TestUpSecondProcessRefusedWhileFirstHoldsLifetimeLock pins the observed
// defect (codex P1): the registry flock protects one write, not the process
// lifetime, so two ups on the same root both registered and the second exit
// emptied the registry under the live first endpoint. The lifetime flock
// makes the second up refuse before any spawn.
func TestUpSecondProcessRefusedWhileFirstHoldsLifetimeLock(t *testing.T) {
	root := t.TempDir()
	regPath := filepath.Join(root, "registry.json")
	entryID := registry.EntryID(root, "amq-remote", "remote", root)

	first, err := acquireLifetimeLock(regPath, entryID)
	if err != nil {
		t.Fatalf("first acquireLifetimeLock: %v", err)
	}
	defer func() { _ = first.Close() }()

	second, lockErr := acquireLifetimeLock(regPath, entryID)
	if lockErr == nil {
		_ = second.Close()
		t.Fatal("second acquireLifetimeLock succeeded, want refusal while first holds the lock")
	}
	if !errors.Is(lockErr, errLifetimeHeld) {
		t.Fatalf("second lock error = %v, want errLifetimeHeld", lockErr)
	}

	// The first owner can still re-acquire its own lock file handle (idempotent
	// within the same process/fd) and the registry path derivation is stable.
	if got := lockFilePath(regPath, entryID); got != regPath+".up-"+entryID+".lock" {
		t.Fatalf("lockFilePath = %q", got)
	}
}

// TestUpForwardsServeFlagsAndRejectsUnknown pins the observed defects
// (codex P1+P2): the complete FlagSet must parse without redefinition
// panics, accept serve's flags, reject unknown flags as usage errors, and
// forward values verbatim.
func TestUpForwardsServeFlagsAndRejectsUnknown(t *testing.T) {
	// The REAL entry-point shape: one FlagSet, addCommon + defineServeFlags +
	// up-specific flags, exactly as up() builds it. Reproducing the
	// construction here is what catches "flag redefined" panics that
	// helper-only tests miss (codex reproduced the panic with the binary).
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addCommon(fs)
	defineServeFlags(fs, "remote")
	fs.String("registry", "", "")
	fs.Int("max-restarts", 0, "")
	fs.String("self", "", "")
	args := []string{"--root", "/tmp/r", "--fake", "--poll", "250ms", "--me", "amq-remote"}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	var unknown []string
	fs.Visit(func(f *stdFlag) {
		if !serveFlagNames[f.Name] && f.Name != "self" && f.Name != "registry" && f.Name != "max-restarts" {
			unknown = append(unknown, "--"+f.Name)
		}
	})
	if len(unknown) != 0 {
		t.Fatalf("serve flags rejected as unknown: %v", unknown)
	}
	got := buildServeArgs(fs, "/tmp/r", "amq-remote")
	want := []string{"serve", "--root", "/tmp/r", "--me", "amq-remote", "--fake=true", "--poll=250ms"}
	if !slices.Equal(got, want) {
		t.Fatalf("buildServeArgs = %v, want %v", got, want)
	}

	// A flag serve does not define is a parse-time usage error (ContinueOnError
	// refuses "flag provided but not defined"), never forwarded or dropped.
	fs2 := flag.NewFlagSet("up", flag.ContinueOnError)
	fs2.SetOutput(io.Discard)
	addCommon(fs2)
	defineServeFlags(fs2, "remote")
	if err := fs2.Parse([]string{"--no-such-flag"}); err == nil {
		t.Fatal("parse of unknown flag succeeded, want usage error")
	}
}

// TestUpBooleanValuesPreserved pins the observed defect (codex P1): explicit
// boolean values were dropped and the flag sent bare, so --fake=false turned
// INTO --fake and --codex-approve=false silently opted INTO approval. Go's
// flag package accepts --flag=false, so values must be forwarded verbatim.
func TestUpBooleanValuesPreserved(t *testing.T) {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addCommon(fs)
	defineServeFlags(fs, "remote")
	if err := fs.Parse([]string{"--root", "/tmp/r", "--fake=false", "--discover=false", "--codex-approve=false"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := buildServeArgs(fs, "/tmp/r", "amq-remote")
	want := []string{"serve", "--root", "/tmp/r", "--me", "amq-remote", "--codex-approve=false", "--discover=false", "--fake=false"}
	if !slices.Equal(got, want) {
		t.Fatalf("buildServeArgs = %v, want %v", got, want)
	}
}

// TestUpLifetimeLockCreatesMissingRegistryDir pins the observed defect
// (codex P2): the lifetime-lock file was opened before the registry created
// its parent directory, so a first use pointed at a fresh directory failed
// with ENOENT reported as "another up already supervises".
func TestUpLifetimeLockCreatesMissingRegistryDir(t *testing.T) {
	root := t.TempDir()
	regPath := filepath.Join(root, "fresh-dir", "registry.json")
	entryID := registry.EntryID(root, "amq-remote", "remote", root)

	f, err := acquireLifetimeLock(regPath, entryID)
	if err != nil {
		t.Fatalf("acquireLifetimeLock on missing parent dir: %v", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := os.Stat(filepath.Join(root, "fresh-dir")); err != nil {
		t.Fatalf("parent dir not created: %v", err)
	}
}
