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

type WakeRepairResult struct {
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (r WakeRepairResult) Text() string {
	return strings.TrimSpace(strings.Join([]string{r.Status, r.Reason, r.Message, r.Error}, " "))
}

type StartWakeRequest struct {
	Root      string
	Me        string
	InjectVia string
	Adapter   string
	Target    string
	Timeout   time.Duration
}

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

func (c CLI) RepairWake(ctx context.Context, root, me string) (WakeRepairResult, error) {
	args := []string{"wake", "repair", "-json"}
	if root != "" {
		args = append(args, "-root", root)
	}
	if me != "" {
		args = append(args, "-me", me)
	}
	stdout, stderr, err := c.run(ctx, args...)
	result, parseErr := parseWakeRepair(stdout)
	if parseErr != nil {
		if err != nil {
			return WakeRepairResult{
				Status: "error",
				Error:  strings.TrimSpace(strings.Join([]string{err.Error(), stderr}, ": ")),
			}, err
		}
		return WakeRepairResult{}, parseErr
	}
	if result.Error == "" && len(stderr) > 0 {
		result.Error = strings.TrimSpace(stderr)
	}
	return result, err
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

func parseWakeRepair(data []byte) (WakeRepairResult, error) {
	var result WakeRepairResult
	if len(bytes.TrimSpace(data)) == 0 {
		return WakeRepairResult{}, errors.New("empty amq wake repair output")
	}
	if err := json.Unmarshal(data, &result); err != nil {
		var raw map[string]any
		if rawErr := json.Unmarshal(data, &raw); rawErr != nil {
			return WakeRepairResult{}, err
		}
		result.Status = stringField(raw, "status")
		result.Reason = stringField(raw, "reason")
		result.Message = stringField(raw, "message")
		result.Error = stringField(raw, "error")
	}
	result.Status = strings.TrimSpace(result.Status)
	return result, nil
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

func stringField(raw map[string]any, key string) string {
	value, ok := raw[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}
