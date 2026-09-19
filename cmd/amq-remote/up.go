package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/supervisor"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// spawner is the interface for launching a serve child process. The default
// implementation uses exec.Command; tests inject a fake to avoid real
// processes (Claude's correction: no child processes in tests).
type spawner interface {
	// Spawn starts serve with the given args and returns a handle that can be
	// waited on. The returned process exposes ExitCode() (nil if still running).
	Spawn(ctx context.Context, args []string) (process, error)
}

// process is a waited-on child.
type process interface {
	// Wait blocks until the process exits, returning its exit code.
	Wait() (int, error)
	// Signal sends a signal to the process.
	Signal(sig os.Signal) error
}

// execProcess wraps exec.Cmd.
type execProcess struct{ cmd *exec.Cmd }

func (p *execProcess) Wait() (int, error) {
	err := p.cmd.Wait()
	if p.cmd.ProcessState != nil {
		return p.cmd.ProcessState.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func (p *execProcess) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return errors.New("no process")
	}
	return p.cmd.Process.Signal(sig)
}

// execSpawner launches the amq-remote binary as a serve child.
type execSpawner struct {
	binary string
}

func (s *execSpawner) Spawn(ctx context.Context, args []string) (process, error) {
	cmd := exec.CommandContext(ctx, s.binary, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd}, nil
}

// mustDefaultRegistryPath returns the keepalive companion registry path.
func mustDefaultRegistryPath() string {
	path, err := registry.DefaultPath()
	if err != nil {
		return ""
	}
	return path
}

// executablePath returns the path to the current amq-remote binary.
func executablePath() string {
	path, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	return path
}

// serveFlags is the exact set of flags the serve command defines (main.go).
// up forwards any of these the user passed; anything else is a usage error
// so a typo is never silently dropped (codex P2: `up --fake` must reach
// serve, and an unknown flag must be rejected, not swallowed).
var serveFlags = map[string]bool{
	"root":          true,
	"json":          true,
	"me":            true,
	"fake":          true,
	"codex-socket":  true,
	"codex-thread":  true,
	"codex-approve": true,
	"manifest":      true,
	"discover":      true,
	"poll":          true,
}

// newUpFlagSet registers up's flags: common plus up-specific. up declares
// serve's flags too so their values can be forwarded verbatim to the child;
// the declared-but-forwarded set is validated against serveFlags below so a
// name present here but absent from serve is still a usage error.
func newUpFlagSet(output io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(output)
	addCommon(fs)
	fs.String("me", "", "")
	fs.Bool("fake", false, "")
	fs.String("codex-socket", "", "")
	fs.String("codex-thread", "", "")
	fs.Bool("codex-approve", false, "")
	fs.String("manifest", "", "")
	fs.Bool("discover", false, "")
	fs.Duration("poll", 0, "")
	return fs
}

// up runs serve under supervision: spawn, respawn on non-zero exit with
// exponential backoff (reusing keepalive's failure-backoff constants), and
// stop on clean exit 0.
//
// Lifetime ownership (codex P1): the registry's per-write flock protects one
// WRITE, not the process lifetime — two sequential Upserts from two ups both
// succeed and the second exit's Forget empties the registry under the live
// first endpoint. up therefore acquires a process-lifetime flock on
// <registry>.up-<entryid>.lock BEFORE spawning. The kernel releases it when
// the up process dies; a second up on the same root fails the non-blocking
// flock and refuses. Only the owner removes the registration (its deferred
// Forget runs under the lifetime lock it still holds).
func up(args []string, stdout, stderr io.Writer) (int, error) {
	fs := newUpFlagSet(stderr)
	c := addCommon(fs)
	me := fs.String("me", "amq-remote", "companion handle for registry identification")
	registryPath := fs.String("registry", mustDefaultRegistryPath(), "keepalive companion registry file path")
	maxRestarts := fs.Int("max-restarts", 0, "maximum respawns before giving up (0 = unlimited)")
	self := fs.String("self", executablePath(), "amq-remote executable path to spawn serve")
	if err := fs.Parse(args); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	if len(fs.Args()) > 0 {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "up takes no positional arguments (got %q)", fs.Args()[0])
	}
	var unknown []string
	fs.Visit(func(f *flag.Flag) {
		if !serveFlags[f.Name] && f.Name != "self" && f.Name != "registry" && f.Name != "max-restarts" {
			unknown = append(unknown, "--"+f.Name)
		}
	})
	if len(unknown) > 0 {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "up: unsupported flag(s) %v: only serve's flags are forwarded", unknown)
	}
	regPath := *registryPath
	var err error
	if regPath == "" {
		regPath, err = registry.DefaultPath()
		if err != nil {
			return 0, fmt.Errorf("registry path: %w", err)
		}
	}
	store := registry.New(regPath)
	entryID := registry.EntryID(c.root, *me, "remote", c.root)
	entry := registry.Entry{
		ID:      entryID,
		Root:    c.root,
		Agent:   *me,
		Adapter: "remote",
		Target:  c.root,
		State:   registry.StateActive,
	}

	// Process-lifetime ownership before anything else is spawned.
	lifetime, err := acquireLifetimeLock(regPath, entryID)
	if err != nil {
		return protocol.ExitActionRequired, protocol.Refuse(protocol.CodeEndpointAlreadyRunning,
			"up: another up already supervises this root (registry %s): %v", regPath, err)
	}
	defer func() { _ = lifetime.Close() }()

	if _, err := store.Upsert(entry); err != nil {
		return 0, fmt.Errorf("register companion in keepalive registry: %w", err)
	}
	defer func() {
		// Only the lifetime owner reaches this Forget: the lock above is
		// released after the registration is removed, so a racing second up
		// can claim the slot only after it is actually empty.
		_, _ = store.Forget(entryID)
	}()
	say(stderr, "amq-remote up: supervising serve for root %s (registry %s)\n", c.root, regPath)

	// Serve args: forward --root, --me, and the serve flags the user passed
	// (e.g. --fake, --poll, --manifest). Up-specific flags are stripped.
	serveArgs := buildServeArgs(fs, c.root, *me)

	sp := &execSpawner{binary: *self}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg := upConfig{
		root:         c.root,
		me:           *me,
		registryPath: regPath,
		maxRestarts:  *maxRestarts,
		backoffBase:  supervisor.DefaultFailureBackoffBase,
		backoffMax:   supervisor.DefaultFailureBackoffMax,
		serveArgs:    serveArgs,
	}
	return runUpLoop(ctx, cfg, sp)
}

// acquireLifetimeLock takes an exclusive non-blocking flock on a dedicated
// lock file beside the registry. The lock lives for the process lifetime (the
// returned *os.File is held until Close), which is what makes a second up on
// the same root/agent refused even though both would Upsert fine.
func acquireLifetimeLock(regPath, entryID string) (*os.File, error) {
	lockPath := lockFilePath(regPath, entryID)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lifetime lock: %w", err)
	}
	if err := flockLifetime(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// lockFilePath derives the per-entry lifetime lock path from the registry
// path so tests and production share one derivation.
func lockFilePath(regPath, entryID string) string {
	return regPath + ".up-" + entryID + ".lock"
}

// upConfig holds the parameters for the supervision loop, separated so tests
// can inject a fake spawner and short backoff without real processes.
type upConfig struct {
	root         string
	me           string
	registryPath string
	maxRestarts  int
	backoffBase  time.Duration
	backoffMax   time.Duration
	serveArgs    []string
}

// runUpLoop is the supervision loop: spawn, respawn on non-zero exit with
// exponential backoff (reusing keepalive's constants), stop on clean exit 0.
// The spawner interface lets tests inject a fake (no real process).
func runUpLoop(ctx context.Context, cfg upConfig, sp spawner) (int, error) {
	restart := 0
	for {
		proc, err := sp.Spawn(ctx, cfg.serveArgs)
		if err != nil {
			say(os.Stderr, "amq-remote up: spawn failed: %v\n", err)
		} else {
			code, werr := proc.Wait()
			if ctx.Err() != nil {
				// Context cancelled (SIGINT/SIGTERM to up): exit.
				say(os.Stderr, "amq-remote up: shutting down\n")
				return 0, nil
			}
			if werr != nil {
				say(os.Stderr, "amq-remote up: serve wait error: %v\n", werr)
			}
			if code == 0 {
				// Clean exit: up ends.
				say(os.Stderr, "amq-remote up: serve exited cleanly (0)\n")
				return 0, nil
			}
			say(os.Stderr, "amq-remote up: serve exited %d\n", code)
		}
		restart++
		if cfg.maxRestarts > 0 && restart > cfg.maxRestarts {
			say(os.Stderr, "amq-remote up: max-restarts (%d) exceeded\n", cfg.maxRestarts)
			return 1, fmt.Errorf("max-restarts exceeded")
		}
		// Backoff: reuse keepalive's failure-backoff constants (exponential,
		// capped). Do not invent a second table.
		delay := backoff(restart, cfg.backoffBase, cfg.backoffMax)
		say(os.Stderr, "amq-remote up: respawning in %s (attempt %d)\n", delay, restart)
		select {
		case <-ctx.Done():
			return 0, nil
		case <-time.After(delay):
		}
	}
}

// backoff computes an exponential backoff capped at maxDelay, matching
// keepalive's Reconciler.backoff / exponentialBackoff policy.
func backoff(failureCount int, base, maxDelay time.Duration) time.Duration {
	if failureCount < 1 {
		failureCount = 1
	}
	delay := base
	for i := 1; i < failureCount; i++ {
		delay *= 2
		if delay >= maxDelay {
			return maxDelay
		}
	}
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

// buildServeArgs constructs the args to pass to serve, forwarding --root and
// --me plus the serve flags the user passed. Boolean flags are forwarded as
// bare --name (no value); value flags as --name=value.
func buildServeArgs(fs *flag.FlagSet, root, me string) []string {
	args := []string{"serve", "--root", root, "--me", me}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "self", "registry", "max-restarts", "root", "me":
			// handled explicitly or up-specific
		default:
			if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
				args = append(args, "--"+f.Name)
				return
			}
			args = append(args, "--"+f.Name, f.Value.String())
		}
	})
	return args
}
