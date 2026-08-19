package amq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrWakeReadinessUncertain = errors.New("amq wake readiness is uncertain; child was left unsignaled")
var ErrWakeImageIdentityInconclusive = errors.New("amq wake image identity is inconclusive")

const defaultWakeReadyTimeout = 10 * time.Second
const staleWakeReadyMarkerAge = 24 * time.Hour
const maxWakeStartupStderrBytes = 16 * 1024
const keepaliveWakeRetryUntil = "injected"
const wakeCheckSchemaV1 = 1

const wakeStderrDrainExitGrace = 250 * time.Millisecond

// wakeProcessExitGrace gives exec.Wait and the bounded stderr drain enough time
// to publish a child exit which raced the caller's timeout or cancellation.
// It must remain longer than wakeStderrDrainExitGrace or a concrete child exit
// can be mislabeled as uncertain while its diagnostics are still draining.
const wakeProcessExitGrace = wakeStderrDrainExitGrace + 50*time.Millisecond
const maxWakeStderrDrainDiagnosticBytes = 4 * 1024

const (
	wakeImageCurrent   = "current"
	wakeImageDifferent = "different"
)

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

type WakeCheckResult struct {
	Schema      int    `json:"schema"`
	LiveWake    bool   `json:"live_wake"`
	ImageStatus string `json:"image_status"`
	Generation  string `json:"wake_generation"`
}

type wakeCheckWireResult struct {
	Schema      *int    `json:"schema"`
	LiveWake    *bool   `json:"live_wake"`
	ImageStatus *string `json:"image_status"`
	Generation  *string `json:"wake_generation"`
}

// RetireWakeRequest names the exact inject-via identity that AMQ must match
// before it stops a wake. Root, Me, Adapter, and Target reproduce the fixed
// argv the wake was started with (inject <adapter> <target>), so AMQ retires
// only a wake whose saved target is identical. Generation is the CAS token
// from amq wake check; when set, AMQ refuses if a replacement generation was
// published in between.
type RetireWakeRequest struct {
	Root       string
	Me         string
	InjectVia  string
	Adapter    string
	Target     string
	Generation string
}

// RetireWakeResult mirrors `amq wake retire -json`. Both "retired" and
// "retired_with_residue" positively confirm that the wake was stopped. The
// latter preserves cleanup residue for AMQ's next acquisition to converge;
// keepalive must still release its obsolete registry row. Every other status
// means retirement was not confirmed and the row must be preserved.
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
	return r.Status == "retired" || r.Status == "retired_with_residue"
}

// ErrWakeRetireNotConfirmed wraps every non-retired outcome so callers can log
// it loudly and leave the registry entry for the next pass. A wake that AMQ
// refused, could not identify, or exited non-zero on is never treated as
// retired.
var ErrWakeRetireNotConfirmed = errors.New("amq wake retirement was not confirmed")

type CLI struct {
	Path string
}

var newWakeStartupStderrForStart = newWakeStartupStderr

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
	if req.Me == "" {
		return errors.New("me is required")
	}
	if req.InjectVia == "" {
		return errors.New("inject-via executable is required")
	}
	if req.Adapter == "" {
		return errors.New("adapter is required")
	}
	if req.Target == "" {
		return errors.New("target is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.refreshWakeImage(ctx, req); err != nil {
		return err
	}

	args := []string{"wake"}
	readyDir, readyFile, err := newWakeReadyPath()
	if err != nil {
		return err
	}
	startupStderr, err := newWakeStartupStderrForStart(readyDir)
	if err != nil {
		_ = os.Remove(readyFile)
		return fmt.Errorf("prepare wake startup stderr capture: %w", err)
	}
	defer startupStderr.Close()

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
		"--retry-until", keepaliveWakeRetryUntil,
		"--accept-existing-wake",
		"-ready-file", readyFile,
	)

	// The wake process is intentionally longer-lived than this invocation (and
	// than the supervisor which launched it). Do not use exec.CommandContext:
	// its cancellation goroutine would kill an already-ready wake when the
	// supervisor receives SIGTERM. After spawning, cancellation never signals
	// the child; a durable registry reservation lets a later pass converge.
	cmd := exec.Command(c.Path, args...)
	// keepalive owns this managed wake. A caller may itself be running under
	// `amq coop exec` and therefore carry an AMQ_WAKE_OWNER token for a different
	// agent/root. Forwarding that token would bind the new wake to the caller's
	// lifecycle. Plain keepalive-managed wakes must remain ownerless.
	cmd.Env = environmentWithout(os.Environ(), "AMQ_WAKE_OWNER")
	configureWakeProcess(cmd)
	// Capture bounded startup diagnostics without inheriting the caller's PTY.
	// A detached drain process owns the pipe reader after this launcher returns,
	// so later wake diagnostics cannot block, grow storage without bound, or
	// terminate the wake with SIGPIPE.
	cmd.Stderr = startupStderr.writer
	// Cancellation can race image refresh and diagnostic-helper setup. Recheck
	// at the irreversible boundary so shutdown never creates a new durable,
	// ownerless wake after the caller has stopped asking for one.
	if err := ctx.Err(); err != nil {
		_ = os.Remove(readyFile)
		return err
	}
	if err := cmd.Start(); err != nil {
		startupStderr.closeWriter()
		_ = os.Remove(readyFile)
		return err
	}
	startupStderr.closeWriter()
	done := make(chan wakeProcessResult, 1)
	go func() {
		err := cmd.Wait()
		ready := wakeReadyFileExists(readyFile)
		_ = os.Remove(readyFile)
		drainErr := startupStderr.waitForDrain(wakeStderrDrainExitGrace)
		stderr := wakeStartupStderrDetail(startupStderr, drainErr)
		done <- wakeProcessResult{Err: err, Ready: ready, Stderr: stderr}
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
				if result.Ready {
					return nil
				}
				return wakeProcessExitError(result)
			default:
			}
		}
		if !processDone {
			// Never signal a spawned wake from helper cancellation. It may be
			// between lock acquisition and atomic ready-file publication; killing
			// here could terminate an established or accepted wake. The durable
			// registry reservation lets a later supervisor pass converge safely.
			readinessErr := fmt.Errorf("%w: %w", ErrWakeReadinessUncertain, err)
			if detail := wakeStartupStderrDetail(startupStderr, nil); detail != "" {
				return fmt.Errorf("%w; stderr: %s", readinessErr, detail)
			}
			return readinessErr
		}
		return err
	}
	// The shared readiness directory must survive AMQ's post-rename directory
	// fsync, but the marker itself is no longer needed after acknowledgement.
	_ = os.Remove(readyFile)
	return nil
}

// refreshWakeImage replaces a conclusively stale live wake before StartWake's
// existing target-aware acquisition runs. The read-only check compares the
// running image to this CLI's image; retirement then revalidates the exact
// inject-via target before stopping anything. Unknown image identity is never
// treated as stale or current.
func (c CLI) refreshWakeImage(ctx context.Context, req StartWakeRequest) error {
	check, err := c.checkWake(ctx, req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWakeImageIdentityInconclusive, err)
	}
	if !check.LiveWake {
		return nil
	}

	switch check.ImageStatus {
	case wakeImageCurrent:
		return nil
	case wakeImageDifferent:
		if check.Generation == "" {
			return fmt.Errorf("%w: live wake omitted generation", ErrWakeImageIdentityInconclusive)
		}
		_, err := c.RetireWake(ctx, RetireWakeRequest{
			Root:       req.Root,
			Me:         req.Me,
			InjectVia:  req.InjectVia,
			Adapter:    req.Adapter,
			Target:     req.Target,
			Generation: check.Generation,
		})
		if err != nil {
			return fmt.Errorf("retire stale amq wake image: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("%w: live wake reported image status %q", ErrWakeImageIdentityInconclusive, check.ImageStatus)
	}
}

func (c CLI) CheckWake(ctx context.Context, req StartWakeRequest) (WakeCheckResult, error) {
	return c.checkWake(ctx, req)
}

func (c CLI) checkWake(ctx context.Context, req StartWakeRequest) (WakeCheckResult, error) {
	args := []string{"wake", "check", "--me", req.Me}
	if req.Root != "" {
		args = append(args, "--root", req.Root)
	}
	args = append(args, "--json", "--json-schema", "1")

	stdout, stderr, err := c.run(ctx, args...)
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return WakeCheckResult{}, fmt.Errorf("amq wake check failed: %v: %s", err, detail)
	}
	var wire wakeCheckWireResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &wire); err != nil {
		return WakeCheckResult{}, fmt.Errorf("parse amq wake check output: %w", err)
	}
	if wire.Schema == nil || wire.LiveWake == nil || wire.ImageStatus == nil {
		return WakeCheckResult{}, errors.New("amq wake check omitted required schema, live_wake, or image_status field")
	}
	if *wire.Schema != wakeCheckSchemaV1 {
		return WakeCheckResult{}, fmt.Errorf("amq wake check returned schema %d, want %d", *wire.Schema, wakeCheckSchemaV1)
	}
	result := WakeCheckResult{
		Schema:      *wire.Schema,
		LiveWake:    *wire.LiveWake,
		ImageStatus: *wire.ImageStatus,
	}
	if wire.Generation != nil {
		result.Generation = strings.TrimSpace(*wire.Generation)
	}
	return result, nil
}

// RetireWake asks AMQ to stop an identity-confirmed live inject-via wake (or
// remove its exactly-bound proven-stale lock) whose saved target matches the
// requested adapter/target exactly. It returns nil only when AMQ reports
// status "retired" or "retired_with_residue"; any other outcome — a refusal
// (including owner-bound claims), an unparseable or empty JSON body, or a
// non-zero exit — returns a descriptive error wrapping
// ErrWakeRetireNotConfirmed so the caller never forgets the registry entry.
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
	)
	if req.Generation != "" {
		args = append(args, "--if-generation", req.Generation)
	}
	args = append(args,
		"--retry-until", keepaliveWakeRetryUntil,
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

	if result.Retired() && runErr != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = runErr.Error()
		}
		return RetireWakeResult{}, fmt.Errorf(
			"%w: amq wake retire reported %q but exited unsuccessfully: %v: %s",
			ErrWakeRetireNotConfirmed, result.Status, runErr, detail,
		)
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
	Err    error
	Ready  bool
	Stderr string
}

type wakeStartupStderr struct {
	mu              sync.Mutex
	file            *os.File
	diagnosticFile  *os.File
	writer          *os.File
	closeWriterOnce sync.Once
	drainResult     <-chan error
}

func newWakeStartupStderr(dir string) (_ *wakeStartupStderr, retErr error) {
	return newWakeStartupStderrWithSetup(dir, wakeStderrSetup{
		createTemp: os.CreateTemp,
		chmod:      func(file *os.File, mode os.FileMode) error { return file.Chmod(mode) },
		pipe:       os.Pipe,
		executable: os.Executable,
		start:      func(cmd *exec.Cmd) error { return cmd.Start() },
	})
}

type wakeStderrSetup struct {
	createTemp func(string, string) (*os.File, error)
	chmod      func(*os.File, os.FileMode) error
	pipe       func() (*os.File, *os.File, error)
	executable func() (string, error)
	start      func(*exec.Cmd) error
}

func newWakeStartupStderrWithSetup(dir string, setup wakeStderrSetup) (_ *wakeStartupStderr, retErr error) {
	var reader, writer, diagnosticFile *os.File
	file, err := setup.createTemp(dir, "wake-stderr-*")
	if err != nil {
		return nil, fmt.Errorf("create wake startup stderr file: %w", err)
	}
	// Every failure below owns the same progressively-created resource set.
	// One unwind point keeps future additions from leaking an fd or private
	// diagnostic file on only one of several setup branches.
	defer func() {
		if retErr == nil {
			return
		}
		if diagnosticFile != nil {
			_ = diagnosticFile.Close()
			_ = os.Remove(diagnosticFile.Name())
		}
		if reader != nil {
			_ = reader.Close()
		}
		if writer != nil {
			_ = writer.Close()
		}
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()
	if err := setup.chmod(file, 0o600); err != nil {
		return nil, fmt.Errorf("secure wake startup stderr file: %w", err)
	}
	reader, writer, err = setup.pipe()
	if err != nil {
		return nil, fmt.Errorf("create wake startup stderr pipe: %w", err)
	}
	diagnosticFile, err = setup.createTemp(dir, "wake-stderr-drain-*")
	if err != nil {
		return nil, fmt.Errorf("create wake stderr drain diagnostic file: %w", err)
	}
	if err := setup.chmod(diagnosticFile, 0o600); err != nil {
		return nil, fmt.Errorf("secure wake stderr drain diagnostic file: %w", err)
	}
	executable, err := setup.executable()
	if err != nil {
		return nil, fmt.Errorf("resolve wake stderr drain executable: %w", err)
	}
	drain := exec.Command(executable, "__wake-stderr-drain")
	drain.Env = append(environmentWithout(os.Environ(), wakeStderrDrainMode), wakeStderrDrainMode+"=1")
	drain.ExtraFiles = []*os.File{reader, file}
	configureWakeProcess(drain)
	// configureWakeProcess intentionally clears inherited stdio. Attach the
	// private diagnostic destination afterwards so helper failures remain
	// observable without retaining the caller's PTY.
	drain.Stderr = diagnosticFile
	if err := setup.start(drain); err != nil {
		return nil, fmt.Errorf("start wake stderr drain: %w", err)
	}
	_ = reader.Close()
	reader = nil
	drainResult := make(chan error, 1)
	go func() { drainResult <- drain.Wait() }()
	return &wakeStartupStderr{
		file: file, diagnosticFile: diagnosticFile, writer: writer, drainResult: drainResult,
	}, nil
}

func (capture *wakeStartupStderr) closeWriter() {
	if capture == nil {
		return
	}
	capture.closeWriterOnce.Do(func() {
		if capture.writer != nil {
			_ = capture.writer.Close()
		}
	})
}

func (capture *wakeStartupStderr) waitForDrain(timeout time.Duration) error {
	if capture == nil || capture.drainResult == nil {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-capture.drainResult:
		return err
	case <-timer.C:
		return fmt.Errorf("timed out after %s", timeout)
	}
}

func (capture *wakeStartupStderr) Close() {
	if capture == nil {
		return
	}
	capture.closeWriter()
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.file == nil {
		return
	}
	path := capture.file.Name()
	diagnosticPath := capture.diagnosticFile.Name()
	_ = capture.file.Close()
	_ = os.Remove(path)
	_ = capture.diagnosticFile.Close()
	_ = os.Remove(diagnosticPath)
	capture.file = nil
	capture.diagnosticFile = nil
}

func wakeStartupStderrDetail(capture *wakeStartupStderr, drainErr error) string {
	detail := capture.String()
	if drainErr != nil {
		if detail != "" {
			detail += "\n"
		}
		detail += fmt.Sprintf("[stderr capture incomplete: %v]", drainErr)
	}
	return detail
}

func (capture *wakeStartupStderr) String() string {
	if capture == nil {
		return ""
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.file == nil {
		return "[stderr capture unavailable: capture is closed]"
	}
	text := readBoundedWakeDiagnostic(capture.file, maxWakeStartupStderrBytes, "stderr")
	drainDiagnostic := readBoundedWakeDiagnostic(
		capture.diagnosticFile, maxWakeStderrDrainDiagnosticBytes, "stderr drain diagnostic",
	)
	if drainDiagnostic == "" {
		return text
	}
	if text == "" {
		return drainDiagnostic
	}
	return text + "\n" + drainDiagnostic
}

func readBoundedWakeDiagnostic(file *os.File, limit int, label string) string {
	if file == nil {
		return ""
	}
	data := make([]byte, limit+1)
	n, err := file.ReadAt(data, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Sprintf("[%s unavailable: %v]", label, err)
	}
	truncated := n > limit
	if truncated {
		n = limit
	}
	text := strings.TrimSpace(string(data[:n]))
	if !truncated {
		return text
	}
	marker := fmt.Sprintf("[%s truncated after %d bytes]", label, limit)
	if text == "" {
		return marker
	}
	return text + "\n" + marker
}

func environmentWithout(environment []string, name string) []string {
	prefix := name + "="
	filtered := make([]string, 0, len(environment))
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
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

type wakeReadyTimer struct {
	channel <-chan time.Time
	stop    func()
}

type wakeReadyTimerFactory func(time.Duration) wakeReadyTimer
type wakeGraceObserver func(<-chan wakeProcessResult, string, time.Duration) (bool, bool, error)

func newRealWakeReadyTimer(timeout time.Duration) wakeReadyTimer {
	timer := time.NewTimer(timeout)
	return wakeReadyTimer{channel: timer.C, stop: func() { timer.Stop() }}
}

func waitForWakeReady(ctx context.Context, done <-chan wakeProcessResult, readyFile string, timeout time.Duration) (bool, error) {
	return waitForWakeReadyWith(ctx, done, readyFile, timeout, newRealWakeReadyTimer, observeWakeDuringGrace)
}

func waitForWakeReadyWith(
	ctx context.Context,
	done <-chan wakeProcessResult,
	readyFile string,
	timeout time.Duration,
	newTimer wakeReadyTimerFactory,
	observe wakeGraceObserver,
) (bool, error) {
	if timeout <= 0 {
		timeout = defaultWakeReadyTimeout
	}
	timer := newTimer(timeout)
	defer timer.stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		if wakeReadyFileExists(readyFile) {
			return false, nil
		}
		select {
		case result := <-done:
			return finishWakeProcess(result, readyFile)
		case <-ctx.Done():
			if processDone, observed, err := observe(done, readyFile, wakeProcessExitGrace); observed {
				return processDone, err
			}
			return false, ctx.Err()
		case <-timer.channel:
			if processDone, observed, err := observe(done, readyFile, wakeProcessExitGrace); observed {
				return processDone, err
			}
			return false, fmt.Errorf("timed out after %s waiting for amq wake readiness", timeout)
		case <-ticker.C:
		}
	}
}

func observeWakeDuringGrace(done <-chan wakeProcessResult, readyFile string, grace time.Duration) (processDone, observed bool, err error) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		if wakeReadyFileExists(readyFile) {
			return false, true, nil
		}
		select {
		case result := <-done:
			processDone, err := finishWakeProcess(result, readyFile)
			return processDone, true, err
		case <-timer.C:
			if wakeReadyFileExists(readyFile) {
				return false, true, nil
			}
			if result, ok := pollWakeProcess(done); ok {
				processDone, err := finishWakeProcess(result, readyFile)
				return processDone, true, err
			}
			return false, false, nil
		case <-ticker.C:
		}
	}
}

func pollWakeProcess(done <-chan wakeProcessResult) (wakeProcessResult, bool) {
	select {
	case result := <-done:
		return result, true
	default:
		return wakeProcessResult{}, false
	}
}

func finishWakeProcess(result wakeProcessResult, readyFile string) (bool, error) {
	if result.Ready || wakeReadyFileExists(readyFile) {
		return true, nil
	}
	return true, wakeProcessExitError(result)
}

func wakeProcessExitError(result wakeProcessResult) error {
	detail := strings.TrimSpace(result.Stderr)
	var err error
	if result.Err == nil {
		err = errors.New("amq wake exited before becoming ready")
	} else {
		err = fmt.Errorf("amq wake exited before becoming ready: %w", result.Err)
	}
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w; stderr: %s", err, detail)
}
