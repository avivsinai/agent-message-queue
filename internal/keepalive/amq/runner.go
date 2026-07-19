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

const defaultWakeReadyTimeout = 10 * time.Second

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

type WakeRetireResult struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Root   string `json:"root,omitempty"`
	PID    int    `json:"pid,omitempty"`
	Error  string `json:"error,omitempty"`
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

type RetireWakeRequest struct {
	Root      string
	Me        string
	InjectVia string
	Adapter   string
	Target    string
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

func (c CLI) RetireWake(ctx context.Context, req RetireWakeRequest) (WakeRetireResult, error) {
	if req.InjectVia == "" {
		return WakeRetireResult{}, errors.New("inject-via executable is required")
	}
	if req.Adapter == "" {
		return WakeRetireResult{}, errors.New("adapter is required")
	}
	if req.Target == "" {
		return WakeRetireResult{}, errors.New("target is required")
	}
	args := []string{"wake", "retire", "-json"}
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
	)
	stdout, stderr, err := c.run(ctx, args...)
	var result WakeRetireResult
	if parseErr := json.Unmarshal(stdout, &result); parseErr != nil {
		if err != nil {
			return WakeRetireResult{Status: "error", Error: strings.TrimSpace(stderr)},
				fmt.Errorf("amq wake retire failed: %w: %s", err, strings.TrimSpace(stderr))
		}
		return WakeRetireResult{}, fmt.Errorf("parse amq wake retire: %w", parseErr)
	}
	if result.Error == "" && len(stderr) > 0 {
		result.Error = strings.TrimSpace(stderr)
	}
	if err != nil {
		return result, fmt.Errorf("amq wake retire failed: %w: %s", err, strings.TrimSpace(stderr))
	}
	return result, nil
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
	readyDir, err := os.MkdirTemp("", "amq-keepalive-wake-*")
	if err != nil {
		return fmt.Errorf("create wake readiness directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(readyDir) }()
	readyFile := filepath.Join(readyDir, "ready")

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

	cmd := exec.CommandContext(ctx, c.Path, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "already") {
			return ErrAlreadyRunning
		}
		return err
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	if processDone, err := waitForWakeReady(ctx, done, readyFile, req.Timeout); err != nil {
		if !processDone && cmd.Process != nil {
			_ = cmd.Process.Kill()
			<-done
		}
		return err
	}
	return nil
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

func waitForWakeReady(ctx context.Context, done <-chan error, readyFile string, timeout time.Duration) (bool, error) {
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
		case err := <-done:
			if wakeReadyFileExists(readyFile) {
				return true, nil
			}
			if err == nil {
				return true, errors.New("amq wake exited before becoming ready")
			}
			if strings.Contains(strings.ToLower(err.Error()), "already") {
				return true, ErrAlreadyRunning
			}
			return true, fmt.Errorf("amq wake exited before becoming ready: %w", err)
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
