package launch

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

func (b *TmuxBackend) hasSession(ctx context.Context, name string) (bool, error) {
	_, err := b.run(ctx, b.args("has-session", "-t", "="+name)...)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func (b *TmuxBackend) args(args ...string) []string {
	if b.socketName == "" || b.socketName == "default" {
		if socketPath := b.socketPath(); b.socketFromActive && socketPath != "" {
			return append([]string{"-S", socketPath}, args...)
		}
		return args
	}
	return append([]string{"-L", b.socketName}, args...)
}

func (b *TmuxBackend) runCommand(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, b.binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (b *TmuxBackend) socketPath() string {
	b.socketOnce.Do(func() {
		b.socketPathValue, b.socketFromActive = b.resolveSocketPath()
	})
	return b.socketPathValue
}

func (b *TmuxBackend) resolveSocketPath() (string, bool) {
	if active := strings.TrimSpace(b.getenv("TMUX")); b.socketName == "" && active != "" {
		if path, _, ok := strings.Cut(active, ","); ok && filepath.IsAbs(path) {
			return filepath.Clean(path), true
		}
	}
	base := strings.TrimSpace(b.getenv("TMUX_TMPDIR"))
	if base == "" {
		base = "/tmp"
	}
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	name := b.socketName
	if name == "" {
		name = "default"
	}
	return filepath.Join(base, "tmux-"+b.uid(), name), false
}

func currentUserID() string {
	current, err := user.Current()
	if err != nil || strings.TrimSpace(current.Uid) == "" {
		return "unknown"
	}
	return current.Uid
}

func supportedTmuxVersion(raw string) bool {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) < 2 || fields[0] != "tmux" {
		return false
	}
	version := strings.TrimRightFunc(fields[1], func(r rune) bool { return r < '0' || r > '9' })
	parts := strings.SplitN(version, ".", 2)
	if len(parts) != 2 {
		return false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	return errMajor == nil && errMinor == nil && major == 3 && minor >= 2
}
