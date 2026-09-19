package main

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
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

// blockingProc blocks in Wait until released, then exits with its code.
type blockingProc struct {
	release chan struct{}
	code    int
}

func (p *blockingProc) Wait() (int, error) {
	<-p.release
	return p.code, nil
}
func (p *blockingProc) Signal(os.Signal) error { return nil }

// upEntryFakeSpawner adapts the spawner hook to run the REAL up() entry point
// without a real serve child: it records the forwarded args and blocks in
// Wait until the test releases it. Release is idempotent (safe from both the
// test body and cleanup).
type upEntryFakeSpawner struct {
	mu         sync.Mutex
	args       []string
	proc       *blockingProc
	releaseOne sync.Once
	spawned    chan struct{}
	released   chan struct{}
}

func newUpEntryFakeSpawner(exitCode int) *upEntryFakeSpawner {
	return &upEntryFakeSpawner{
		spawned:  make(chan struct{}),
		released: make(chan struct{}),
		proc:     &blockingProc{release: make(chan struct{}), code: exitCode},
	}
}

func (s *upEntryFakeSpawner) Spawn(ctx context.Context, args []string) (process, error) {
	s.mu.Lock()
	s.args = append([]string(nil), args...)
	s.mu.Unlock()
	close(s.spawned)
	return s.proc, nil
}

func (s *upEntryFakeSpawner) forwardedArgs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.args
}

// releaseProc unblocks the serve child exactly once; calling it again (test
// body after cleanup, or double cleanup) is a no-op.
func (s *upEntryFakeSpawner) releaseProc() {
	s.releaseOne.Do(func() { close(s.proc.release) })
}

// TestUpFreshRootStableIdentityRealEntryPath reproduces the observed defect
// (codex #815 r3 P1): up derived and persisted the registry identity from a
// NOT-YET-EXISTING root, which canonicalizes lexically (/tmp/...); once the
// supervised serve created the root, the same path resolved through its
// symlink (/private/tmp/... on macOS) and the persisted entry ID no longer
// matched its identity — a second identical up exited 1 "registry file is
// corrupt / entry id does not match identity" instead of the ownership
// refusal. The fix prepares + canonicalizes the root BEFORE deriving the
// lifetime key or persisting the entry. The regression drives the REAL up()
// with the spawner hook (no real child process).
func TestUpFreshRootStableIdentityRealEntryPath(t *testing.T) {
	base := t.TempDir()
	// A symlinked fresh path makes the lexical/canonical divergence
	// deterministic on every platform (on macOS any /tmp path diverges the
	// same way via /private/tmp): the fresh root is reached through a
	// symlink, so before up creates it the only possible spelling is the
	// lexical one, and after creation CanonicalRoot resolves the link.
	if err := os.Mkdir(filepath.Join(base, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	// Deliberately NOT created: up must prepare the root itself.
	root := filepath.Join(base, "link", "fresh-root")
	regPath := filepath.Join(base, "fresh-registry", "registry.json")

	sp := newUpEntryFakeSpawner(0)
	prevFactory := upSpawnerFactory
	upSpawnerFactory = func(self string) spawner { return sp }

	firstDone := make(chan error, 1)
	go func() {
		code, err := up([]string{
			"--root", root,
			"--registry", regPath,
			"--fake",
			"--self", "unused-by-fake-spawner",
		}, io.Discard, io.Discard)
		if code != 0 {
			firstDone <- fmt.Errorf("first up: code %d err %v", code, err)
			return
		}
		firstDone <- err
	}()

	// Idempotent release-and-join BEFORE any assertion (codex r4): an
	// assertion failure between spawn and the clean-stop section used to
	// leave the first up blocked in Wait, holding its lifetime lock and
	// registry entry while temp-root cleanup ran. The cleanup releases the
	// child and joins the goroutine (bounded); joinOnce makes the join a
	// no-op when the test body already consumed the result.
	var joinOnce sync.Once
	var joinMu sync.Mutex
	var joinSet bool
	var joinRes error
	join := func(fail string) error {
		joinOnce.Do(func() {
			select {
			case err := <-firstDone:
				joinMu.Lock()
				joinSet, joinRes = true, err
				joinMu.Unlock()
			case <-time.After(5 * time.Second):
				t.Error(fail)
			}
		})
		joinMu.Lock()
		defer joinMu.Unlock()
		if joinSet {
			return joinRes
		}
		return fmt.Errorf("up goroutine result unknown (join not completed)")
	}
	t.Cleanup(func() {
		upSpawnerFactory = prevFactory
		sp.releaseProc()
		_ = join("first up goroutine did not exit within 5s of release")
	})

	// Wait for the spawn OR an early up failure — an unconditional <-sp.spawned
	// never observed an early error and hung the test until package timeout
	// (codex r4).
	select {
	case <-sp.spawned:
	case err := <-firstDone:
		t.Fatalf("first up exited before spawning: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first up to spawn serve")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("up did not prepare fresh root: %v", err)
	}
	stored, err := registry.New(regPath).LoadSnapshot()
	if err != nil {
		t.Fatalf("load registry after first up spawn: %v", err)
	}
	if len(stored.Entries) != 1 {
		t.Fatalf("entries=%d, want exactly the first up's companion", len(stored.Entries))
	}
	entry := stored.Entries[0]
	canonical, err := registry.CanonicalRoot(root)
	if err != nil {
		t.Fatalf("canonicalize root now that it exists: %v", err)
	}
	if entry.Root != canonical {
		t.Fatalf("persisted root %q != canonical %q — identity derived from the lexical fresh path", entry.Root, canonical)
	}
	if entry.ID != registry.EntryID(canonical, "remote", "remote", canonical) {
		t.Fatalf("persisted entry id %q does not match canonical identity — second up would see corrupt registry", entry.ID)
	}

	// The persisted identity stays valid while the first up lives: a second
	// identical up must refuse with the ownership code, never a corrupt-
	// registry error.
	secondCode, secondErr := up([]string{
		"--root", root,
		"--registry", regPath,
		"--fake",
		"--self", "unused-by-fake-spawner",
	}, io.Discard, io.Discard)
	if secondCode != protocol.ExitActionRequired {
		t.Fatalf("second up: code %d err %v, want %d (endpoint_already_running)", secondCode, secondErr, protocol.ExitActionRequired)
	}
	if protocol.RefusalCode(secondErr) != protocol.CodeEndpointAlreadyRunning {
		t.Fatalf("second up err = %v, want endpoint_already_running refusal", secondErr)
	}

	// Clean stop of the first up must remove its registration — and a
	// cleanup failure must not be silent. Bounded join: the child returns
	// once released, so a hang here is a defect, not patience (codex r4).
	sp.releaseProc()
	if err := join("timed out waiting for first up to exit after release"); err != nil {
		t.Fatalf("first up did not exit cleanly: %v", err)
	}
	after, err := registry.New(regPath).LoadSnapshot()
	if err != nil {
		t.Fatalf("load registry after clean stop: %v", err)
	}
	for _, e := range after.Entries {
		if e.ID == entry.ID {
			t.Fatalf("registration %s survived clean stop of its owner", e.ID)
		}
	}
}

// TestUpForwardedServeArgsThroughRealEntryPoint pins the forwarded serve
// arguments as observed at the spawner boundary of the REAL up() entry point
// (the spawner hook was previously unused by any test — codex r3 note): the
// supervision loop must receive serve + root + me + the user's flags with
// parsed values preserved.
func TestUpForwardedServeArgsThroughRealEntryPoint(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "args-root")
	regPath := filepath.Join(base, "args-registry", "registry.json")
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("ensure root: %v", err)
	}

	sp := newUpEntryFakeSpawner(0)
	prevFactory := upSpawnerFactory
	upSpawnerFactory = func(self string) spawner { return sp }

	done := make(chan error, 1)
	go func() {
		code, err := up([]string{
			"--root", root,
			"--registry", regPath,
			"--fake=false",
			"--poll", "2s",
			"--self", "unused-by-fake-spawner",
		}, io.Discard, io.Discard)
		if code != 0 {
			done <- fmt.Errorf("up: code %d err %v", code, err)
			return
		}
		done <- err
	}()

	// Idempotent release-and-join cleanup before assertions (codex r4),
	// same contract as the fresh-root test above; joinOnce makes the join a
	// no-op when the body already consumed the result.
	var joinOnce sync.Once
	var joinMu sync.Mutex
	var joinSet bool
	var joinRes error
	join := func(fail string) error {
		joinOnce.Do(func() {
			select {
			case err := <-done:
				joinMu.Lock()
				joinSet, joinRes = true, err
				joinMu.Unlock()
			case <-time.After(5 * time.Second):
				t.Error(fail)
			}
		})
		joinMu.Lock()
		defer joinMu.Unlock()
		if joinSet {
			return joinRes
		}
		return fmt.Errorf("up goroutine result unknown (join not completed)")
	}
	t.Cleanup(func() {
		upSpawnerFactory = prevFactory
		sp.releaseProc()
		_ = join("up goroutine did not exit within 5s of release")
	})

	// Spawn OR early failure, never an unconditional wait (codex r4).
	select {
	case <-sp.spawned:
	case err := <-done:
		t.Fatalf("up exited before spawning: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for up to spawn serve")
	}
	sp.releaseProc()
	if err := join("timed out waiting for up to exit after release"); err != nil {
		t.Fatalf("up did not exit cleanly: %v", err)
	}

	canonical, err := registry.CanonicalRoot(root)
	if err != nil {
		t.Fatalf("canonicalize root: %v", err)
	}
	got := sp.forwardedArgs()
	want := []string{"serve", "--root", canonical, "--me", "remote", "--fake=false", "--poll=2s"}
	if !slices.Equal(got, want) {
		t.Fatalf("serve args = %v, want %v (canonical root, verbatim values)", got, want)
	}
}
