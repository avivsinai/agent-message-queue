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
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
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
//
// B4 (611.13.2): waitDelay bounds how long os/exec waits after the context
// is cancelled (SIGTERM delivered) before killing the child with SIGKILL.
// Without it, a serve that ignores SIGTERM keeps the up process alive for
// an unbounded time - observed 10s+ and still counting. The delay is the
// shutdown bound the supervisor contract promises: SIGTERM, then a grace
// period, then the kernel takes the child and up exits.
type execSpawner struct {
	binary    string
	waitDelay time.Duration
	// env, when non-nil, replaces the child's environment. nil inherits.
	env []string
}

func (s *execSpawner) Spawn(ctx context.Context, args []string) (process, error) {
	cmd := exec.CommandContext(ctx, s.binary, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if s.env != nil {
		cmd.Env = s.env
	}
	if s.waitDelay > 0 {
		cmd.WaitDelay = s.waitDelay
	}
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

// errUpUnsupportedFlag marks a forwarded-flag set the parser accepted but
// serve does not define. It keeps the usage refusal typed for tests.
var errUpUnsupportedFlag = errors.New("up: unsupported flag(s): only serve's flags are forwarded")

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
	// ONE FlagSet declares up's flags plus serve's forwardable set. The
	// previous recut re-registered root/json/me after newUpFlagSet had
	// already added them, panicking "flag redefined: root" on every real
	// invocation (codex P1, reproduced with the built binary).
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	sf := defineServeFlags(fs, "remote")
	me := sf.me
	registryPath := fs.String("registry", mustDefaultRegistryPath(), "keepalive companion registry file path")
	maxRestarts := fs.Int("max-restarts", 0, "maximum respawns before giving up (0 = unlimited)")
	self := fs.String("self", executablePath(), "amq-remote executable path to spawn serve")
	selfFlag = self
	if err := fs.Parse(args); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	if len(fs.Args()) > 0 {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "up takes no positional arguments (got %q)", fs.Args()[0])
	}
	var unknown []string
	fs.Visit(func(f *flag.Flag) {
		if !serveFlagNames[f.Name] && f.Name != "self" && f.Name != "registry" && f.Name != "max-restarts" {
			unknown = append(unknown, "--"+f.Name)
		}
	})
	if len(unknown) > 0 {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v: %v", errUpUnsupportedFlag, unknown)
	}
	regPath := *registryPath
	var err error
	if regPath == "" {
		regPath, err = registry.DefaultPath()
		if err != nil {
			return 0, fmt.Errorf("registry path: %w", err)
		}
	}
	// Stable identity before any derivation or persistence (codex r3 P1):
	// deriving the registry/lifetime identity from a NOT-YET-EXISTING root
	// canonicalizes lexically (macOS: /tmp/...), but once serve creates the
	// root the same path resolves through its symlink (/private/tmp/...) and
	// the persisted entry ID no longer matches its identity — a second up
	// then hit "registry file is corrupt" instead of the ownership refusal.
	// Create the root layout first and canonicalize ONCE; every identity
	// derivation, the registry row, and the serve child all use the same
	// resolved path afterwards.
	if c.root == "" {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "up: --root or AM_ROOT is required")
	}
	if !filepath.IsAbs(c.root) {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "up: --root must be absolute")
	}
	if err := fsq.EnsureRootDirs(c.root); err != nil {
		return 0, fmt.Errorf("prepare root %s: %w", c.root, err)
	}
	canonical, err := registry.CanonicalRoot(c.root)
	if err != nil {
		return 0, fmt.Errorf("canonicalize root %s: %w", c.root, err)
	}
	c.root = canonical
	entryID := registry.EntryID(c.root, *me, "remote", c.root)

	// Process-lifetime ownership before anything is spawned or written.
	// The secure parent directory is prepared first (codex P2): creating the
	// lock used to fail ENOENT on first use when ~/.amq-keepalive did not
	// exist yet, and that FS error was mislabeled "another owner".
	if err := prepareSecureDir(filepath.Dir(regPath)); err != nil {
		return 0, fmt.Errorf("prepare registry directory: %w", err)
	}
	lifetime, err := acquireLifetimeLock(regPath, entryID)
	if err != nil {
		if errors.Is(err, errLifetimeHeld) {
			// N3 (review-b5): name the running owner so the operator has
			// something to inspect or kill — the lock holder's pid when the
			// kernel can discover it, 0 meaning unknown, plus the entry id.
			return protocol.ExitActionRequired, protocol.Refuse(protocol.CodeEndpointAlreadyRunning,
				"up: another up already supervises root %s (registry %s, owner pid %d, entry %s)",
				c.root, regPath, lifetimeOwnerPid(regPath, entryID), entryID)
		}
		return 0, fmt.Errorf("acquire lifetime lock: %w", err)
	}
	defer func() { _ = lifetime.Close() }()

	store := registry.New(regPath)
	// D (611.13.2): reclaim phantom companion rows before registering. A
	// kill -9 on up skips its deferred Forget, leaving a row that supervise
	// skips, GC keeps as not-detached, and doctor reports active forever -
	// and whose target ownership BLOCKS the fresh up at Upsert. A row whose
	// lifetime lock is NOT held has no living supervisor; reclaim it under
	// the registration lock. Probe errors fail closed (the row stays and
	// the error surfaces; never a silent removal).
	reclaimed, rerr := reclaimPhantomCompanions(store, regPath, entryID)
	if rerr != nil {
		return 0, fmt.Errorf("reclaim phantom companion rows: %w", rerr)
	}
	for _, id := range reclaimed {
		say(stderr, "amq-remote up: reclaimed stale companion row %s (lifetime lock not held)\n", id)
	}
	entry := registry.Entry{
		ID:      entryID,
		Root:    c.root,
		Agent:   *me,
		Adapter: "remote",
		Target:  c.root,
		State:   registry.StateActive,
	}
	if _, err := store.Upsert(entry); err != nil {
		// N2 (review-b5): a target owned by ANOTHER up refuses this up —
		// the same "one up per root" condition the lifetime flock enforces,
		// so it gets the same positive refusal shape (exit 6,
		// endpoint_already_running), not an untyped exit-1 error surface
		// that scripts reading exit 6 miss. The registry error text carries
		// the existing_owner/existing_id detail.
		if errors.Is(err, registry.ErrTargetOwned) {
			return protocol.ExitActionRequired, protocol.Refuse(protocol.CodeEndpointAlreadyRunning,
				"up: another up already owns root %s (registry %s): %v", c.root, regPath, err)
		}
		return 0, fmt.Errorf("register companion in keepalive registry: %w", err)
	}
	defer func() {
		// Only the lifetime owner reaches this Forget: the lock above is
		// released after the registration is removed, so a racing second up
		// can claim the slot only after it is actually empty. A cleanup
		// failure is never silent (codex r3): a leftover row would present a
		// dead endpoint as active to every registry consumer.
		if _, ferr := store.Forget(entryID); ferr != nil {
			say(stderr, "amq-remote up: WARNING: could not remove registration %s from %s: %v\n", entryID, regPath, ferr)
		}
	}()
	say(stderr, "amq-remote up: supervising serve for root %s (registry %s)\n", c.root, regPath)

	// Serve args: forward --root and --me plus every serve flag the user
	// passed, preserving parsed values (codex P1: --fake=false used to be
	// forwarded bare as --fake, flipping an explicit opt-out back on).
	serveArgs := buildServeArgs(fs, c.root, *me)

	sp := newUpSpawner()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg := upConfig{
		maxRestarts: *maxRestarts,
		backoffBase: supervisor.DefaultFailureBackoffBase,
		backoffMax:  supervisor.DefaultFailureBackoffMax,
		serveArgs:   serveArgs,
	}
	return runUpLoop(ctx, cfg, sp)
}

// upSpawnerFactory lets tests override the spawner used by the real up
// entry point (default: an execSpawner running the --self binary with the
// default SIGTERM grace, B4 611.13.2).
var upSpawnerFactory = func(self string) spawner {
	return &execSpawner{binary: self, waitDelay: upWaitDelay}
}

func newUpSpawner() spawner { return upSpawnerFactory(*selfFlag) }

// selfFlag is bound in up(); it lives at package scope so the factory can
// read it without threading a parameter through every call site.
var selfFlag *string

// prepareSecureDir creates dir (and parents) with 0700 and tightens an
// EXISTING directory to 0700 as well (N6, review-b5): the creation-only
// chmod left a pre-existing 0755 ~/.amq-keepalive loose, and the later
// registry refusal pointed at the registry instead of the directory the
// caller must fix.
func prepareSecureDir(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return os.Chmod(dir, 0o700)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	// Existing directory: enforce the same 0700 guarantee the name promises.
	return os.Chmod(dir, 0o700)
}

// lifetimeOwnerPid reports the pid recorded by the entry's lifetime lock
// holder (N3, review-b5: the exit-6 refusal must name the owner). This is
// a best-effort recorded pid, not verified process identity — never an
// authority to kill a process, and the flock stays the only ownership
// authority. 0 means unknown; never an error source.
func lifetimeOwnerPid(regPath, entryID string) int {
	return lifetimeOwnerPidOS(regPath, entryID)
}

// acquireLifetimeLock takes an exclusive non-blocking flock on a dedicated
// lock file beside the registry. The lock lives for the process lifetime (the
// returned *os.File is held until Close), which is what makes a second up on
// the same root/agent refused even though both would Upsert fine.
func acquireLifetimeLock(regPath, entryID string) (*os.File, error) {
	lockPath := lockFilePath(regPath, entryID)
	// Create the parent directory first (codex P2): a first use with a
	// not-yet-existing registry directory hit ENOENT on open and was
	// reported as "another up already supervises" — a filesystem error
	// mislabeled as lock contention.
	if dir := filepath.Dir(lockPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("prepare lifetime lock directory: %w", err)
		}
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lifetime lock: %w", err)
	}
	if err := flockLifetime(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	// review-828-r1 P1: BSD flock does not record a pid — the previous
	// F_GETLK probe could never see this flock(2) holder (different lock
	// namespaces; it reported pid 0 live on darwin and by documented
	// semantics on Linux). So the holder self-registers: truncate the body
	// (AFTER the flock — never on open, that would erase a live holder's
	// record) and write the complete pid. Truncation matters: WriteString
	// alone leaves a suffix when a shorter pid replaces a longer one
	// ("123456\n" then "123\n" reads back "123\n56\n" → unknown), so the
	// refusal would print 0 for a real holder (codex review). The pid is
	// best-effort recorded text, NOT verified process identity — a failed
	// truncate can leave old numeric text and a partial write can parse as
	// a number — so it must never be used as authority to kill a process.
	// The flock, not the content, stays the only ownership authority: a
	// stale pid from a hard kill is harmless advisory text.
	if werr := f.Truncate(0); werr == nil {
		if _, werr := f.WriteString(strconv.Itoa(os.Getpid()) + "\n"); werr != nil {
			_ = werr // advisory only; unknown is an honest refusal value
		}
	}
	return f, nil
}

// reclaimPhantomCompanions removes companion (adapter "remote") rows whose
// process-lifetime lock is no longer held (611.13.2 D): the owning up died
// hard - kill -9 - and its deferred Forget never ran. Rows are reclaimed
// under the registration lock; the caller's own row and any row whose lock
// IS held (a live companion) are untouched. A probe error fails closed: the
// row survives and the error surfaces to the caller.
func reclaimPhantomCompanions(store *registry.Store, regPath, ownEntryID string) ([]string, error) {
	var reclaimed []string
	err := store.WithRegistrationLock(func() error {
		file, err := store.Load()
		if err != nil {
			return err
		}
		var stale []string
		for _, e := range file.Entries {
			if e.Adapter != "remote" || e.ID == ownEntryID {
				continue
			}
			held, err := registry.ProbeLifetimeLock(regPath, e.ID)
			if err != nil {
				// Fail closed: an unprobeable row is not provably dead.
				return fmt.Errorf("probe lifetime lock for %s: %w", e.ID, err)
			}
			if !held {
				stale = append(stale, e.ID)
			}
		}
		if len(stale) == 0 {
			return nil
		}
		reclaimed = stale
		_, err = store.ForgetMany(stale)
		return err
	})
	return reclaimed, err
}

// lockFilePath derives the per-entry lifetime lock path from the registry
// path so tests and production share one derivation. Delegates to the
// registry package so doctor --ops probes the same file (611.13.2 D).
func lockFilePath(regPath, entryID string) string {
	return registry.LifetimeLockPath(regPath, entryID)
}

// upWaitDelay is the default SIGTERM grace before SIGKILL for a real serve
// child (B4, 611.13.2). Serve's own shutdown is fast when it cooperates;
// the delay only binds when it ignores SIGTERM.
const upWaitDelay = 10 * time.Second

// upUsageExitCode is the exit class that terminates supervision: serve
// refused its own arguments (B6, 611.13.2). Respawning a usage error loops
// forever - observed 'respawning in 1m0s' for a child exiting 2.
const upUsageExitCode = 2

// upHealthyUptime is the runtime after which a child counts as having run
// healthily (B5, 611.13.2): the failure counter resets so the next crash
// waits the base backoff again, not an escalated step. Two times the base
// keeps the bar low in tests (inject a tiny base) while staying meaningful
// in production (a healthy serve runs far longer than two base steps).
func upHealthyUptime(base time.Duration) time.Duration { return 2 * base }

// upConfig holds the parameters for the supervision loop, separated so tests
// can inject a fake spawner and short backoff without real processes.
type upConfig struct {
	maxRestarts int
	backoffBase time.Duration
	backoffMax  time.Duration
	serveArgs   []string
	// now returns the current time for uptime measurement; nil means
	// time.Now. Injected by tests so the healthy-uptime logic is instant
	// (611.13.2 review P2-2) instead of wall-clock.
	now func() time.Time
}

// runUpLoop is the supervision loop: spawn, respawn on non-zero exit with
// exponential backoff (reusing keepalive's constants), stop on clean exit 0.
// The spawner interface lets tests inject a fake (no real process).
func runUpLoop(ctx context.Context, cfg upConfig, sp spawner) (int, error) {
	now := cfg.now
	if now == nil {
		now = time.Now
	}
	// restart is the BACKOFF index: it resets on healthy uptime so the next
	// crash waits the base step again. budget is the LIFETIME respawn count
	// --max-restarts bounds (611.13.2 review P1-2): one counter, two
	// obligations made the cap forgettable after any healthy-lived child.
	restart := 0
	budget := 0
	for {
		start := now()
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
			if code == upUsageExitCode {
				// B6 (611.13.2): serve refused its arguments. Respawning
				// replays the same refusal forever - observed 'respawning in
				// 1m0s' for a child exiting 2. Propagate the code; up ends.
				say(os.Stderr, "amq-remote up: serve exited %d (usage error); not respawning\n", code)
				return code, nil
			}
			uptime := now().Sub(start)
			say(os.Stderr, "amq-remote up: serve exited %d (ran %s)\n", code, uptime.Round(time.Millisecond))
			// B5 (611.13.2): a child that ran healthy-long did not fail
			// immediately - the crash it just suffered starts a fresh failure
			// series, so the counter resets and the next wait is the base
			// step, not an escalated one.
			if uptime >= upHealthyUptime(cfg.backoffBase) {
				say(os.Stderr, "amq-remote up: healthy uptime %s; resetting backoff series\n", uptime.Round(time.Second))
				restart = 0
			}
		}
		restart++
		budget++
		if cfg.maxRestarts > 0 && budget > cfg.maxRestarts {
			say(os.Stderr, "amq-remote up: max-restarts (%d) exceeded\n", cfg.maxRestarts)
			return 1, fmt.Errorf("max-restarts exceeded")
		}
		// Backoff: reuse keepalive's failure-backoff constants (exponential,
		// capped). Do not invent a second table. The index (restart), not
		// the budget, drives escalation.
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
// --me plus every serve flag the user set, preserving parsed values —
// including explicit false. Go's flag package accepts --flag=false, so serve
// parses the forwarded form fine; dropping the value would silently flip an
// explicit --fake=false or --codex-approve=false back to opt-in (codex P1).
func buildServeArgs(fs *flag.FlagSet, root, me string) []string {
	args := []string{"serve", "--root", root, "--me", me}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "self", "registry", "max-restarts", "root", "me":
			// handled explicitly or up-specific
		default:
			args = append(args, "--"+f.Name+"="+f.Value.String())
		}
	})
	return args
}
