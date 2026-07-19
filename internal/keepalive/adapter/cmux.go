package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	cmuxSurfaceTargetPrefix = "cmux:surface:"
	defaultCmuxSettleDelay  = 150 * time.Millisecond
)

type Cmux struct {
	Runner       CommandRunner
	Path         string
	Getenv       func(string) string
	LookPath     func(string) (string, error)
	UserHomeDir  func() (string, error)
	IsExecutable func(string) bool
	Sleep        func(context.Context, time.Duration) error
	SettleDelay  time.Duration
}

func (Cmux) Name() string {
	return "cmux"
}

func (c Cmux) Discover(_ context.Context) (string, error) {
	if err := requireCmuxPlatform(); err != nil {
		return "", err
	}
	id, err := normalizeCmuxSurfaceID(c.getenv("CMUX_SURFACE_ID"))
	if err != nil {
		return "", fmt.Errorf("discover cmux surface from CMUX_SURFACE_ID: %w", err)
	}
	return cmuxSurfaceTargetPrefix + id, nil
}

func (Cmux) NormalizeTarget(target string) (string, error) {
	id, err := parseCmuxSurfaceTarget(target)
	if err != nil {
		return "", err
	}
	return cmuxSurfaceTargetPrefix + id, nil
}

func (c Cmux) Probe(ctx context.Context, target string) error {
	if err := requireCmuxPlatform(); err != nil {
		return err
	}
	id, err := parseCmuxSurfaceTarget(target)
	if err != nil {
		return err
	}
	params, err := json.Marshal(map[string]any{
		"lines":      1,
		"surface_id": id,
	})
	if err != nil {
		return fmt.Errorf("encode cmux probe parameters: %w", err)
	}
	path, err := c.executable()
	if err != nil {
		return err
	}
	out, err := c.runner().Run(ctx, path, "rpc", "surface.read_text", string(params))
	if err != nil {
		message := strings.TrimSpace(string(out))
		if strings.Contains(message, "not_found:") && strings.Contains(message, "Workspace not found") {
			return fmt.Errorf("%w: probe cmux target %q: %v: %s", ErrTargetNotFound, target, err, message)
		}
		return fmt.Errorf("probe cmux target %q: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (c Cmux) Inject(ctx context.Context, target string, payload string) error {
	if err := requireCmuxPlatform(); err != nil {
		return err
	}
	id, err := parseCmuxSurfaceTarget(target)
	if err != nil {
		return err
	}
	path, err := c.executable()
	if err != nil {
		return err
	}

	payload = sanitizePayloadForSubmit(payload)
	textParams, err := json.Marshal(map[string]string{
		"surface_id": id,
		"text":       payload,
	})
	if err != nil {
		return fmt.Errorf("encode cmux text parameters: %w", err)
	}
	if out, err := c.runner().Run(ctx, path, "rpc", "surface.send_text", string(textParams)); err != nil {
		return fmt.Errorf("inject text into cmux target %q: %w: %s", target, err, strings.TrimSpace(string(out)))
	}

	if err := c.sleep(ctx, c.settleDelay()); err != nil {
		return fmt.Errorf("wait before submitting cmux target %q: %w", target, err)
	}

	keyParams, err := json.Marshal(map[string]string{
		"key":        "enter",
		"surface_id": id,
	})
	if err != nil {
		return fmt.Errorf("encode cmux key parameters: %w", err)
	}
	if out, err := c.runner().Run(ctx, path, "rpc", "surface.send_key", string(keyParams)); err != nil {
		return fmt.Errorf("submit cmux target %q: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (c Cmux) runner() CommandRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}

func (c Cmux) executable() (string, error) {
	if strings.TrimSpace(c.Path) != "" {
		return filepath.Clean(strings.TrimSpace(c.Path)), nil
	}
	if override := strings.TrimSpace(c.getenv("AMQ_KEEPALIVE_CMUX")); override != "" {
		path, err := c.resolveCandidate(override)
		if err != nil {
			return "", fmt.Errorf("resolve AMQ_KEEPALIVE_CMUX: %w", err)
		}
		return path, nil
	}
	if bundled := strings.TrimSpace(c.getenv("CMUX_BUNDLED_CLI_PATH")); bundled != "" && c.isExecutable(bundled) {
		return filepath.Clean(bundled), nil
	}
	if path, err := c.lookPath("cmux"); err == nil {
		return filepath.Clean(path), nil
	}

	candidates := []string{"/Applications/cmux.app/Contents/Resources/bin/cmux"}
	if home, err := c.userHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		candidates = append(candidates, filepath.Join(home, "Applications", "cmux.app", "Contents", "Resources", "bin", "cmux"))
	}
	for _, candidate := range candidates {
		if c.isExecutable(candidate) {
			return filepath.Clean(candidate), nil
		}
	}
	return "", fmt.Errorf("cmux CLI not found; set AMQ_KEEPALIVE_CMUX or install the bundled CLI at %s", candidates[0])
}

func (c Cmux) resolveCandidate(candidate string) (string, error) {
	if filepath.IsAbs(candidate) || strings.ContainsRune(candidate, filepath.Separator) {
		if !c.isExecutable(candidate) {
			return "", fmt.Errorf("%q is not an executable file", candidate)
		}
		return filepath.Clean(candidate), nil
	}
	path, err := c.lookPath(candidate)
	if err != nil {
		return "", err
	}
	return filepath.Clean(path), nil
}

func (c Cmux) getenv(key string) string {
	if c.Getenv != nil {
		return c.Getenv(key)
	}
	return os.Getenv(key)
}

func (c Cmux) lookPath(file string) (string, error) {
	if c.LookPath != nil {
		return c.LookPath(file)
	}
	return exec.LookPath(file)
}

func (c Cmux) userHomeDir() (string, error) {
	if c.UserHomeDir != nil {
		return c.UserHomeDir()
	}
	return os.UserHomeDir()
}

func (c Cmux) isExecutable(path string) bool {
	if c.IsExecutable != nil {
		return c.IsExecutable(path)
	}
	return isExecutableFile(path)
}

func (c Cmux) sleep(ctx context.Context, delay time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c Cmux) settleDelay() time.Duration {
	if c.SettleDelay < 0 {
		return 0
	}
	if c.SettleDelay == 0 {
		return defaultCmuxSettleDelay
	}
	return c.SettleDelay
}

func requireCmuxPlatform() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("cmux adapter requires macOS, got %s", runtime.GOOS)
	}
	return nil
}

func parseCmuxSurfaceTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", errors.New("cmux adapter target is required")
	}
	id, ok := strings.CutPrefix(target, cmuxSurfaceTargetPrefix)
	if !ok {
		return "", fmt.Errorf("unsupported cmux target %q; reattach required: run reattach --adapter cmux from the target surface", target)
	}
	id, err := normalizeCmuxSurfaceID(id)
	if err != nil {
		return "", fmt.Errorf("invalid cmux surface target: %w", err)
	}
	return strings.ToUpper(id), nil
}

func normalizeCmuxSurfaceID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("cmux surface id is empty")
	}
	if len(id) != 36 {
		return "", fmt.Errorf("cmux surface id %q is not a UUID", id)
	}
	for i, r := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return "", fmt.Errorf("cmux surface id %q is not a UUID", id)
			}
			continue
		}
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return "", fmt.Errorf("cmux surface id %q is not a UUID", id)
		}
	}
	return id, nil
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}
