package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

const (
	claudePrintName                = "claude-print"
	claudePrintTargetSessionPrefix = "claude-print:session:"
	// Live-verified 2026-08-26 on Claude Code 2.1.246: dontAsk denies Bash even
	// with --allowedTools Bash. auto + scoped allow lets the doorbell run
	// `amq drain` without --dangerously-skip-permissions.
	claudePrintPermissionMode = "auto"
	claudePrintAllowedTools   = "Bash(amq *)"
	claudePrintAckWait        = 60 * time.Second
	claudePrintJSONLCwdTail   = 1 << 20
)

// ClaudePrint is the honest submitted seat into an existing Claude Code
// session. It spawns `claude -p --resume <uuid>` with stream-json replay ack,
// never raises a GUI (ActivationNone), and never claims the turn finished —
// the first matching isReplay user event is submitted.
//
// Claude CLI does not enforce a one-owner mutex (a second -p --resume appends
// to the same jsonl). Probe refuses when ~/.claude/sessions/<pid>.json names
// this uuid and that pid is alive. Stale pid files are ignored, never deleted.
type ClaudePrint struct {
	Runner   CommandRunner
	LookPath func(string) (string, error)
	Spawner  processSpawner
	// ConfigDir is CLAUDE_CONFIG_DIR (default ~/.claude). Ambient, not part of
	// injector identity (docs/wake-lifecycle.md §9.4).
	ConfigDir string
	StateDir  string
	AckWait   time.Duration
	Now       func() time.Time
}

func (ClaudePrint) Name() string { return claudePrintName }

func (ClaudePrint) Capability() Capability {
	return Capability{
		Activation:    ActivationNone,
		Delivery:      DeliverySubmitted,
		Session:       SessionExistingExact,
		RequiresHuman: false,
	}
}

func (c ClaudePrint) CapabilityForTarget(target string) (Capability, error) {
	if _, err := parseClaudePrintSessionUUID(target); err != nil {
		return Capability{}, err
	}
	return c.Capability(), nil
}

func (ClaudePrint) NormalizeTarget(target string) (string, error) {
	id, err := parseClaudePrintSessionUUID(target)
	if err != nil {
		return "", err
	}
	return claudePrintTargetSessionPrefix + id, nil
}

func (c ClaudePrint) Probe(ctx context.Context, target string) error {
	_, _, _, err := c.probe(ctx, target)
	return err
}

func (c ClaudePrint) Inject(ctx context.Context, target string, payload string) error {
	uuid, claudePath, cwd, err := c.probe(ctx, target)
	if err != nil {
		return fmt.Errorf("inject Claude print: %w", err)
	}
	logPath, err := c.streamLogPath(uuid)
	if err != nil {
		return fmt.Errorf("inject Claude print: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fmt.Errorf("inject Claude print: create log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("inject Claude print: open log: %w", err)
	}
	if err := logFile.Close(); err != nil {
		return fmt.Errorf("inject Claude print: close log: %w", err)
	}
	stdin, err := encodeClaudePrintUserMessage(payload)
	if err != nil {
		return fmt.Errorf("inject Claude print: encode payload: %w", err)
	}
	spec := processSpec{
		Path:    claudePath,
		Args:    claudePrintArgv(uuid),
		Dir:     cwd,
		Stdin:   stdin,
		LogPath: logPath,
	}
	proc, err := c.spawner().Start(ctx, spec)
	if err != nil {
		return fmt.Errorf("inject Claude print: spawn: %w", err)
	}
	wait := c.ackWait()
	deadline := time.Now().Add(wait)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	waitCh := make(chan error, 1)
	go func() { waitCh <- proc.Wait() }()
	var waitErr error
	exited := false
	for {
		matched, parseErr := claudePrintAckSeen(logPath, payload)
		if parseErr != nil {
			_ = proc.KillGroup()
			return fmt.Errorf("inject Claude print: read ack log: %w", parseErr)
		}
		if matched {
			// Submitted: leave the child running; the turn is its concern.
			return nil
		}
		if !exited {
			select {
			case waitErr = <-waitCh:
				exited = true
			default:
			}
		}
		if exited {
			tail := lastLogLines(logPath, 20)
			return fmt.Errorf("inject Claude print: child exited before ack: %v: %s", waitErr, tail)
		}
		if time.Now().After(deadline) {
			return abortAfterKill(proc, logPath, payload, "ack timeout")
		}
		select {
		case <-ctx.Done():
			err := abortAfterKill(proc, logPath, payload, "context canceled")
			if errors.Is(err, ErrInjectUncertain) {
				return err
			}
			return fmt.Errorf("inject Claude print: %w", ctx.Err())
		case waitErr = <-waitCh:
			exited = true
		case <-ticker.C:
		}
	}
}

func claudePrintArgv(uuid string) []string {
	return []string{
		"-p",
		"--verbose",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--replay-user-messages",
		"--resume", uuid,
		"--permission-mode", claudePrintPermissionMode,
		"--allowedTools", claudePrintAllowedTools,
	}
}

func encodeClaudePrintUserMessage(payload string) ([]byte, error) {
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]string{
				{"type": "text", "text": payload},
			},
		},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(msg); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (c ClaudePrint) probe(ctx context.Context, target string) (uuid, claudePath, cwd string, err error) {
	normalized, err := c.NormalizeTarget(target)
	if err != nil {
		return "", "", "", err
	}
	claudePath, err = c.pinExecutable(ctx)
	if err != nil {
		return "", "", "", err
	}
	uuid, err = parseClaudePrintSessionUUID(normalized)
	if err != nil {
		return "", "", "", err
	}
	configDir, err := c.configDir()
	if err != nil {
		return "", "", "", err
	}
	jsonl, err := findClaudePrintJSONL(configDir, uuid)
	if err != nil {
		return "", "", "", err
	}
	cwd, err = lastJSONLCwd(jsonl)
	if err != nil {
		return "", "", "", err
	}
	if st, statErr := os.Stat(cwd); statErr != nil || !st.IsDir() {
		return "", "", "", fmt.Errorf("session %s recorded cwd %s is gone; resume that session from a live checkout or pick another uuid", uuid, cwd)
	}
	if err := c.refuseLiveOwner(configDir, uuid); err != nil {
		return "", "", "", err
	}
	return uuid, claudePath, cwd, nil
}

func (c ClaudePrint) pinExecutable(ctx context.Context) (string, error) {
	lookPath := c.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	looked, err := lookPath("claude")
	if err != nil {
		return "", fmt.Errorf("claude not found on PATH (%w); put claude on PATH", err)
	}
	if !filepath.IsAbs(looked) {
		abs, absErr := filepath.Abs(looked)
		if absErr != nil {
			return "", fmt.Errorf("resolve claude executable: %w; put claude on PATH", absErr)
		}
		looked = abs
	}
	if launch.ProviderForExecutable(looked) != launch.ClaudeProvider {
		return "", fmt.Errorf("claude at %s has basename %q, want %q; put claude on PATH", looked, filepath.Base(looked), launch.ClaudeProvider)
	}
	resolved, err := filepath.EvalSymlinks(looked)
	if err != nil {
		return "", fmt.Errorf("resolve claude at %s: %w; put claude on PATH", looked, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat claude at %s: %w; put claude on PATH", resolved, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("claude at %s is not an executable regular file; put claude on PATH", resolved)
	}
	out, err := c.runner().Run(ctx, resolved, "--help")
	help := string(out)
	if err != nil || !strings.Contains(help, "--resume") || !strings.Contains(help, "--output-format") || !strings.Contains(help, "--replay-user-messages") {
		return "", fmt.Errorf("claude at %s lacks print/resume stream-json; put claude on PATH", resolved)
	}
	return resolved, nil
}

func (c ClaudePrint) configDir() (string, error) {
	if strings.TrimSpace(c.ConfigDir) != "" {
		return c.ConfigDir, nil
	}
	if v := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve CLAUDE_CONFIG_DIR: %w", err)
	}
	return filepath.Join(home, ".claude"), nil
}

func (c ClaudePrint) stateDir() (string, error) {
	if strings.TrimSpace(c.StateDir) != "" {
		return c.StateDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve keepalive state dir: %w", err)
	}
	return filepath.Join(home, ".amq-keepalive"), nil
}

func (c ClaudePrint) streamLogPath(uuid string) (string, error) {
	root, err := c.stateDir()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}
	name := now.Format("20060102T150405.000Z") + ".stream.jsonl"
	return filepath.Join(root, "claude-print", uuid, name), nil
}

func (c ClaudePrint) ackWait() time.Duration {
	if c.AckWait > 0 {
		return c.AckWait
	}
	return claudePrintAckWait
}

func (c ClaudePrint) runner() CommandRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}

func (c ClaudePrint) spawner() processSpawner {
	if c.Spawner != nil {
		return c.Spawner
	}
	return platformProcessSpawner{}
}

func abortAfterKill(proc startedProcess, logPath, payload, why string) error {
	_ = proc.KillGroup()
	matched, err := claudePrintAckSeen(logPath, payload)
	if err != nil {
		return fmt.Errorf("inject Claude print: read ack log: %w", err)
	}
	if matched {
		return fmt.Errorf("%w: inject Claude print: %s after the payload was accepted", ErrInjectUncertain, why)
	}
	tail := lastLogLines(logPath, 20)
	return fmt.Errorf("inject Claude print: %s: %s", why, tail)
}

func (c ClaudePrint) refuseLiveOwner(configDir, uuid string) error {
	dir := filepath.Join(configDir, "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read Claude sessions index: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !isPIDJSON(name) {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			continue
		}
		var rec claudeSessionOwner
		if json.Unmarshal(raw, &rec) != nil {
			continue
		}
		if rec.SessionID != uuid {
			continue
		}
		alive, aliveErr := pidAlive(rec.PID)
		if aliveErr != nil {
			return fmt.Errorf("inspect live owner pid %d: %w", rec.PID, aliveErr)
		}
		if !alive {
			continue
		}
		kind := strings.TrimSpace(rec.Kind)
		if kind == "" {
			kind = "unknown"
		}
		ep := strings.TrimSpace(rec.Entrypoint)
		if ep == "" {
			ep = "unknown"
		}
		return fmt.Errorf("%w: session %s has a live owner pid %d (%s/%s); use the TTY seat for a live terminal, or wait for the turn to finish", ErrTargetDegraded, uuid, rec.PID, kind, ep)
	}
	return nil
}

func isPIDJSON(name string) bool {
	base, ok := strings.CutSuffix(name, ".json")
	if !ok || base == "" {
		return false
	}
	_, err := strconv.Atoi(base)
	return err == nil
}

type claudeSessionOwner struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	Kind       string `json:"kind"`
	Entrypoint string `json:"entrypoint"`
}

func findClaudePrintJSONL(configDir, uuid string) (string, error) {
	// Scan projects/*/<uuid>.jsonl rather than decoding the escaped directory
	// name: slash-to-dash escaping is lossy (cannot round-trip).
	matches, err := filepath.Glob(filepath.Join(configDir, "projects", "*", uuid+".jsonl"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("%w: no Claude session jsonl for %s under %s", ErrTargetNotFound, uuid, configDir)
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("%w: want exactly one session jsonl for %s under %s, found %d", ErrTargetNotFound, uuid, configDir, len(matches))
	}
	return matches[0], nil
}

func lastJSONLCwd(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > claudePrintJSONLCwdTail {
		if _, err := f.Seek(-claudePrintJSONLCwdTail, io.SeekEnd); err != nil {
			return "", err
		}
		cwd, err := scanJSONLCwd(f, true)
		if err != nil {
			return "", err
		}
		if cwd != "" {
			return cwd, nil
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
	}
	cwd, err := scanJSONLCwd(f, false)
	if err != nil {
		return "", err
	}
	if cwd == "" {
		return "", fmt.Errorf("session jsonl %s has no cwd field", path)
	}
	return cwd, nil
}

func scanJSONLCwd(r io.Reader, dropPartialFirstLine bool) (string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	skip := dropPartialFirstLine
	var cwd string
	for scanner.Scan() {
		if skip {
			skip = false
			continue
		}
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			continue
		}
		if rec.Cwd != "" {
			cwd = rec.Cwd
		}
	}
	return cwd, scanner.Err()
}

func claudePrintAckSeen(logPath, payload string) (bool, error) {
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev claudeStreamEvent
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type != "user" || !ev.IsReplay {
			continue
		}
		if claudeStreamUserText(ev) == payload {
			return true, nil
		}
	}
	return false, scanner.Err()
}

type claudeStreamEvent struct {
	Type     string          `json:"type"`
	Subtype  string          `json:"subtype"`
	IsReplay bool            `json:"isReplay"`
	Message  json.RawMessage `json:"message"`
}

func claudeStreamUserText(ev claudeStreamEvent) string {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(ev.Message, &msg) != nil {
		return ""
	}
	var asString string
	if json.Unmarshal(msg.Content, &asString) == nil {
		return asString
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type == "text" {
			return b.Text
		}
	}
	return ""
}

func lastLogLines(path string, n int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return string(bytes.Join(lines, []byte("\n")))
}

func parseClaudePrintSessionUUID(target string) (string, error) {
	id, ok := strings.CutPrefix(strings.TrimSpace(target), claudePrintTargetSessionPrefix)
	if !ok {
		return "", fmt.Errorf("unsupported Claude print target %q; use %s<uuid>", target, claudePrintTargetSessionPrefix)
	}
	id = strings.TrimSpace(id)
	if !lowercaseThreadUUIDRe.MatchString(id) {
		return "", fmt.Errorf("invalid Claude print session uuid %q; want an exact lowercase 8-4-4-4-12 hex uuid", id)
	}
	return id, nil
}

type processSpec struct {
	Path    string
	Args    []string
	Dir     string
	Stdin   []byte
	LogPath string
}

type startedProcess interface {
	PID() int
	Wait() error
	KillGroup() error
}

type processSpawner interface {
	Start(ctx context.Context, spec processSpec) (startedProcess, error)
}
