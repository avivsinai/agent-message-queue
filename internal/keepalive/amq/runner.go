package amq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var ErrAlreadyRunning = errors.New("amq wake already running")
var ErrWakeReadinessUncertain = errors.New("amq wake readiness is uncertain; child was left unsignaled")

const defaultWakeReadyTimeout = 10 * time.Second
const staleWakeReadyMarkerAge = 24 * time.Hour

type Env struct {
	SchemaVersion int               `json:"schema_version"`
	AMQVersion    string            `json:"amq_version"`
	Root          string            `json:"root"`
	BaseRoot      string            `json:"base_root"`
	SessionName   string            `json:"session_name"`
	InSession     bool              `json:"in_session"`
	Me            string            `json:"me"`
	Project       string            `json:"project"`
	RootSource    string            `json:"root_source"`
	Peers         map[string]string `json:"peers"`
	Shell         string            `json:"shell"`
}

type StartWakeRequest struct {
	Root      string
	Me        string
	InjectVia string
	Adapter   string
	Target    string
	Timeout   time.Duration
}

// RetireWakeRequest names the exact inject-via identity that AMQ must match
// before it stops a wake. Root, Me, Adapter, and Target reproduce the fixed
// argv the wake was started with (inject <adapter> <target>), so AMQ retires
// only a wake whose saved target is identical.
type RetireWakeRequest struct {
	Root      string
	Me        string
	InjectVia string
	Adapter   string
	Target    string
}

// RetireWakeResult mirrors `amq wake retire -json`. Only Status == "retired"
// is a positive confirmation; every other status ("refused", "error",
// "unknown") means the wake was not stopped and its registry entry must be
// preserved.
type RetireWakeResult struct {
	Status string `json:"status"`
	Agent  string `json:"agent"`
	Root   string `json:"root"`
	Lock   string `json:"lock"`
	Target string `json:"target"`
	PID    int    `json:"pid"`
	Reason string `json:"reason"`
}

// Retired reports whether AMQ positively confirmed the wake was stopped (or its
// exactly-bound proven-stale lock removed).
func (r RetireWakeResult) Retired() bool {
	return r.Status == "retired"
}

// ErrWakeRetireNotConfirmed wraps every non-retired outcome so callers can log
// it loudly and leave the registry entry for the next pass. A wake that AMQ
// refused, could not identify, or exited non-zero on is never treated as
// retired.
var ErrWakeRetireNotConfirmed = errors.New("amq wake retirement was not confirmed")

type CLI struct {
	Path string
}

func NewCLI(path string) CLI {
	if path == "" {
		path = "amq"
	}
	return CLI{Path: path}
}

func (c CLI) Env(ctx context.Context) (Env, error) {
	stdout, stderr, err := c.run(ctx, "env", "--json")
	if err != nil {
		return Env{}, fmt.Errorf("amq env failed: %w: %s", err, strings.TrimSpace(stderr))
	}
	var env Env
	if err := json.Unmarshal(stdout, &env); err != nil {
		return Env{}, fmt.Errorf("parse amq env: %w", err)
	}
	return env, nil
}

func (c CLI) StartWake(ctx context.Context, req StartWakeRequest) error {
	if req.InjectVia == "" {
		return errors.New("inject-via executable is required")
	}
	if req.Adapter == "" {
		return errors.New("adapter is required")
	}
	if req.Target == "" {
		return errors.New("target is required")
	}

	args := []string{"wake"}
	_, readyFile, err := newWakeReadyPath()
	if err != nil {
		return err
	}

	if req.Root != "" {
		args = append(args, "-root", req.Root)
	}
	if req.Me != "" {
		args = append(args, "-me", req.Me)
	}
	// Do NOT baseline existing inbox mail by default.
	//
	// keepalive only reaches StartWake when no live wake owns this agent, so
	// anything sitting in inbox/new arrived while nothing was notifying and has
	// therefore never been announced. Passing --baseline-existing here made the
	// replacement wake re-snapshot inbox/new and permanently suppress those
	// downtime arrivals: the message stayed queued, no injection ever fired, and
	// the peer looked like it had failed to send. This mirrors the guarantee the
	// repair path already documents ("downtime arrivals remain eligible") — a
	// wake that starts *after* downtime must not swallow what landed during it.
	//
	// Undrained mail in inbox/new is unread by definition, so re-announcing it is
	// correct; the cost is at most one duplicate notice when a wake is replaced
	// while mail is still undrained, which clears as soon as the agent drains.
	// Set AMQ_KEEPALIVE_BASELINE_EXISTING=1 to restore the old suppressing
	// behavior without a rebuild.
	if baselineExistingEnabled() {
		args = append(args, "--baseline-existing")
	}
	args = append(args,
		"-inject-via", req.InjectVia,
		"-inject-arg", "inject",
		"-inject-arg", req.Adapter,
		"-inject-arg", req.Target,
		"--accept-existing-wake",
		"-ready-file", readyFile,
	)

	// The wake process is intentionally longer-lived than this invocation (and
	// than the supervisor which launched it). Do not use exec.CommandContext:
	// its cancellation goroutine would kill an already-ready wake when the
	// supervisor receives SIGTERM. After spawning, cancellation never signals
	// the child; a durable registry reservation lets a later pass converge.
	cmd := exec.Command(c.Path, args...)
	configureWakeProcess(cmd)
	if err := ctx.Err(); err != nil {
		_ = os.Remove(readyFile)
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = os.Remove(readyFile)
		if strings.Contains(strings.ToLower(err.Error()), "already") {
			return ErrAlreadyRunning
		}
		return err
	}
	done := make(chan wakeProcessResult, 1)
	go func() {
		err := cmd.Wait()
		ready := wakeReadyFileExists(readyFile)
		_ = os.Remove(readyFile)
		done <- wakeProcessResult{Err: err, Ready: ready}
	}()
	if processDone, err := waitForWakeReady(ctx, done, readyFile, req.Timeout); err != nil {
		// Readiness wins a cancellation/timeout race. Once AMQ has published the
		// ready file, ownership of the long-lived process has transferred to AMQ
		// and this caller must never kill it.
		if wakeReadyFileExists(readyFile) {
			_ = os.Remove(readyFile)
			return nil
		}
		if !processDone {
			select {
			case result := <-done:
				processDone = true
				if result.Ready {
					return nil
				}
			default:
			}
		}
		if !processDone {
			// Never signal a spawned wake from helper cancellation. It may be
			// between lock acquisition and atomic ready-file publication; killing
			// here could terminate an established or accepted wake. The durable
			// registry reservation lets a later supervisor pass converge safely.
			return fmt.Errorf("%w: %w", ErrWakeReadinessUncertain, err)
		}
		return err
	}
	// The shared readiness directory must survive AMQ's post-rename directory
	// fsync, but the marker itself is no longer needed after acknowledgement.
	_ = os.Remove(readyFile)
	return nil
}

// RetireWake asks AMQ to stop an identity-confirmed live inject-via wake (or
// remove its exactly-bound proven-stale lock) whose saved target matches the
// requested adapter/target exactly. It returns nil only when AMQ reports
// status "retired"; any other outcome — a refusal (including owner-bound
// claims), an unparseable or empty JSON body, or a non-zero exit — returns a
// descriptive error wrapping ErrWakeRetireNotConfirmed so the caller never
// forgets the registry entry.
func (c CLI) RetireWake(ctx context.Context, req RetireWakeRequest) (RetireWakeResult, error) {
	if req.Me == "" {
		return RetireWakeResult{}, errors.New("me is required")
	}
	if req.InjectVia == "" {
		return RetireWakeResult{}, errors.New("inject-via executable is required")
	}
	if req.Adapter == "" {
		return RetireWakeResult{}, errors.New("adapter is required")
	}
	if req.Target == "" {
		return RetireWakeResult{}, errors.New("target is required")
	}

	args := []string{"wake", "retire", "--me", req.Me}
	if req.Root != "" {
		args = append(args, "--root", req.Root)
	}
	args = append(args,
		"--inject-via", req.InjectVia,
		"--inject-arg", "inject",
		"--inject-arg", req.Adapter,
		"--inject-arg", req.Target,
		"-json",
	)

	stdout, stderr, runErr := c.run(ctx, args...)
	var result RetireWakeResult
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) > 0 {
		if err := json.Unmarshal(trimmed, &result); err != nil {
			return RetireWakeResult{}, fmt.Errorf(
				"%w: parse amq wake retire output: %v: stdout=%q stderr=%q",
				ErrWakeRetireNotConfirmed, err, strings.TrimSpace(string(stdout)), strings.TrimSpace(stderr),
			)
		}
	}

	if result.Retired() {
		return result, nil
	}

	// Not retired. Never mutate the registry on any of these branches.
	switch {
	case result.Status != "":
		msg := fmt.Sprintf("amq wake retire returned status %q", result.Status)
		if reason := strings.TrimSpace(result.Reason); reason != "" {
			msg += ": " + reason
		}
		return result, fmt.Errorf("%w: %s", ErrWakeRetireNotConfirmed, msg)
	case runErr != nil:
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = runErr.Error()
		}
		return result, fmt.Errorf("%w: amq wake retire failed: %v: %s", ErrWakeRetireNotConfirmed, runErr, detail)
	default:
		return result, fmt.Errorf("%w: amq wake retire produced no status", ErrWakeRetireNotConfirmed)
	}
}

// baselineExistingEnabled reports whether a replacement wake should suppress
// inbox mail that is already queued when it starts. Default is false so that
// arrivals during wake downtime still get announced; see StartWake.
func baselineExistingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AMQ_KEEPALIVE_BASELINE_EXISTING"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func newWakeReadyPath() (string, string, error) {
	cacheDir := strings.TrimSpace(os.Getenv("AMQ_KEEPALIVE_CACHE_DIR"))
	if cacheDir == "" {
		var err error
		cacheDir, err = os.UserCacheDir()
		if err != nil {
			return "", "", fmt.Errorf("resolve user cache directory for wake readiness: %w", err)
		}
	}
	dir := filepath.Join(cacheDir, "amq-keepalive", "readiness")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("create wake readiness directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", "", fmt.Errorf("inspect wake readiness directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", "", fmt.Errorf("wake readiness path %q must be a real directory", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("secure wake readiness directory: %w", err)
	}
	scavengeStaleWakeReadyMarkers(dir, time.Now())
	placeholder, err := os.CreateTemp(dir, "wake-*")
	if err != nil {
		return "", "", fmt.Errorf("reserve wake readiness path: %w", err)
	}
	path := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		_ = os.Remove(path)
		return "", "", fmt.Errorf("close wake readiness placeholder: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return "", "", fmt.Errorf("prepare wake readiness destination: %w", err)
	}
	return dir, path, nil
}

func scavengeStaleWakeReadyMarkers(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "wake-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < staleWakeReadyMarkerAge {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}

type wakeProcessResult struct {
	Err   error
	Ready bool
}

func (c CLI) run(ctx context.Context, args ...string) ([]byte, string, error) {
	cmd := exec.CommandContext(ctx, c.Path, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.String(), err
}

func waitForWakeReady(ctx context.Context, done <-chan wakeProcessResult, readyFile string, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		timeout = defaultWakeReadyTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		if wakeReadyFileExists(readyFile) {
			return false, nil
		}
		select {
		case result := <-done:
			if result.Ready || wakeReadyFileExists(readyFile) {
				return true, nil
			}
			if result.Err == nil {
				return true, errors.New("amq wake exited before becoming ready")
			}
			if strings.Contains(strings.ToLower(result.Err.Error()), "already") {
				return true, ErrAlreadyRunning
			}
			return true, fmt.Errorf("amq wake exited before becoming ready: %w", result.Err)
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return false, fmt.Errorf("timed out after %s waiting for amq wake readiness", timeout)
		case <-ticker.C:
		}
	}
}

func wakeReadyFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
