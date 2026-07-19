package hookinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	AgentBoth   = "both"
	AgentClaude = "claude"
	AgentCodex  = "codex"

	DefaultTimeout = 10 * time.Second
)

const SessionStartScript = `#!/usr/bin/env bash
# SessionStart hook wrapper for amq-keepalive.
#
# This hook is intentionally non-blocking: it logs reattach failures and always
# returns an empty hook response so agent startup is not held hostage by AMQ.

set -u

BIN="${AMQ_KEEPALIVE_BIN:-amq-keepalive}"
ADAPTER="${AMQ_KEEPALIVE_ADAPTER:-}"
TARGET="${AMQ_KEEPALIVE_TARGET:-}"
REGISTRY="${AMQ_KEEPALIVE_REGISTRY:-}"
AMQ_BIN="${AMQ_KEEPALIVE_AMQ:-amq}"
SELF_BIN="${AMQ_KEEPALIVE_SELF:-$BIN}"
ROOT="${AMQ_KEEPALIVE_ROOT:-}"
BASE_ROOT="${AMQ_KEEPALIVE_BASE_ROOT:-}"
SESSION_NAME="${AMQ_KEEPALIVE_SESSION:-}"
ME="${AMQ_KEEPALIVE_ME:-}"
LOG_PATH="${AMQ_KEEPALIVE_LOG:-$HOME/.amq-keepalive/session-start.log}"
DEFAULT_TIMEOUT_SECONDS="${AMQ_KEEPALIVE_DEFAULT_TIMEOUT_SECONDS:-10}"
TIMEOUT_SECONDS="${AMQ_KEEPALIVE_TIMEOUT_SECONDS:-$DEFAULT_TIMEOUT_SECONDS}"
STDIN_TIMEOUT_SECONDS="${AMQ_KEEPALIVE_STDIN_TIMEOUT_SECONDS:-1}"
WAKE_TIMEOUT_MILLISECONDS="${AMQ_KEEPALIVE_WAKE_TIMEOUT_MILLISECONDS:-}"

if [[ -z "$ADAPTER" ]]; then
    if [[ -n "${CMUX_SURFACE_ID:-}" ]]; then
        ADAPTER="cmux"
    else
        ADAPTER="ghostty"
    fi
fi
if [[ "$ADAPTER" == "cmux" && -z "$TARGET" && -n "${CMUX_SURFACE_ID:-}" ]]; then
    TARGET="cmux:surface:${CMUX_SURFACE_ID}"
fi

if [[ "${AMQ_KEEPALIVE_DISABLED:-0}" == "1" ]]; then
    printf '{}\n'
    exit 0
fi

mkdir -p "$(dirname "$LOG_PATH")" 2>/dev/null || true

log() {
    printf '%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >> "$LOG_PATH" 2>/dev/null || true
}

if ! [[ "$DEFAULT_TIMEOUT_SECONDS" =~ ^[0-9]+$ && "$DEFAULT_TIMEOUT_SECONDS" -gt 0 ]]; then
    DEFAULT_TIMEOUT_SECONDS=10
fi
if ! [[ "$TIMEOUT_SECONDS" =~ ^[0-9]+$ && "$TIMEOUT_SECONDS" -gt 0 ]]; then
    log "invalid timeout ${TIMEOUT_SECONDS}; using ${DEFAULT_TIMEOUT_SECONDS}s"
    TIMEOUT_SECONDS="$DEFAULT_TIMEOUT_SECONDS"
fi
if ! [[ "$STDIN_TIMEOUT_SECONDS" =~ ^[0-9]+$ && "$STDIN_TIMEOUT_SECONDS" -gt 0 ]]; then
    STDIN_TIMEOUT_SECONDS=1
fi

outer_timeout_milliseconds=$((TIMEOUT_SECONDS * 1000))
default_wake_timeout_milliseconds=$((outer_timeout_milliseconds - 2000))
if [[ "$default_wake_timeout_milliseconds" -le 0 ]]; then
    default_wake_timeout_milliseconds=$((outer_timeout_milliseconds / 2))
fi
if [[ "$default_wake_timeout_milliseconds" -le 0 ]]; then
    default_wake_timeout_milliseconds=100
fi
if ! [[ "$WAKE_TIMEOUT_MILLISECONDS" =~ ^[0-9]+$ && "$WAKE_TIMEOUT_MILLISECONDS" -gt 0 ]]; then
    [[ -n "$WAKE_TIMEOUT_MILLISECONDS" ]] && log "invalid wake timeout ${WAKE_TIMEOUT_MILLISECONDS}ms; using ${default_wake_timeout_milliseconds}ms"
    WAKE_TIMEOUT_MILLISECONDS="$default_wake_timeout_milliseconds"
fi
if [[ "$WAKE_TIMEOUT_MILLISECONDS" -ge "$outer_timeout_milliseconds" ]]; then
    clamped_wake_timeout_milliseconds=$((outer_timeout_milliseconds - 500))
    if [[ "$clamped_wake_timeout_milliseconds" -le 0 ]]; then
        clamped_wake_timeout_milliseconds=100
    fi
    log "wake timeout ${WAKE_TIMEOUT_MILLISECONDS}ms must be shorter than outer ${outer_timeout_milliseconds}ms; using ${clamped_wake_timeout_milliseconds}ms"
    WAKE_TIMEOUT_MILLISECONDS="$clamped_wake_timeout_milliseconds"
fi

read_hook_input() {
    local line=""
    IFS= read -r -t "$STDIN_TIMEOUT_SECONDS" line || true
    printf '%s' "$line"
}

INPUT="$(read_hook_input 2>/dev/null || true)"

CWD=""
if command -v jq >/dev/null 2>&1 && [[ -n "$INPUT" ]]; then
    CWD="$(printf '%s' "$INPUT" | jq -r '.cwd // .workdir // .working_directory // empty' 2>/dev/null || true)"
fi
if [[ -n "$CWD" && -d "$CWD" ]]; then
    cd "$CWD" 2>/dev/null || true
fi

if ! command -v "$BIN" >/dev/null 2>&1; then
    log "skip: amq-keepalive binary not found: $BIN"
    printf '{}\n'
    exit 0
fi

args=(reattach --adapter "$ADAPTER" --amq "$AMQ_BIN" --wake-ready-timeout "${WAKE_TIMEOUT_MILLISECONDS}ms")
[[ -n "${AMQ_KEEPALIVE_SELF:-}" ]] && args+=(--self "$SELF_BIN")
[[ -n "$TARGET" ]] && args+=(--target "$TARGET")
[[ -n "$REGISTRY" ]] && args+=(--registry "$REGISTRY")
[[ -n "$ROOT" ]] && args+=(--root "$ROOT")
[[ -n "$BASE_ROOT" ]] && args+=(--base-root "$BASE_ROOT")
[[ -n "$SESSION_NAME" ]] && args+=(--session "$SESSION_NAME")
[[ -n "$ME" ]] && args+=(--me "$ME")
[[ "${AMQ_KEEPALIVE_NO_START:-0}" == "1" ]] && args+=(--no-start)

run_reattach() {
    "$BIN" "${args[@]}" >> "$LOG_PATH" 2>&1
}

timeout_marker="${TMPDIR:-/tmp}/amq-keepalive-timeout.$$"
rm -f "$timeout_marker" 2>/dev/null || true

(
    trap 'exit 143' TERM
    run_reattach
) 2>> "$LOG_PATH" &
reattach_pid=$!
(
    sleep "$TIMEOUT_SECONDS"
    if kill -0 "$reattach_pid" 2>/dev/null; then
        : > "$timeout_marker" 2>/dev/null || true
        pkill -TERM -P "$reattach_pid" 2>/dev/null || true
        kill -TERM "$reattach_pid" 2>/dev/null || true
        sleep 1
        pkill -KILL -P "$reattach_pid" 2>/dev/null || true
        kill -KILL "$reattach_pid" 2>/dev/null || true
    fi
) >/dev/null 2>&1 &
watchdog_pid=$!

wait "$reattach_pid" 2>/dev/null
status=$?
pkill -TERM -P "$watchdog_pid" 2>/dev/null || true
kill "$watchdog_pid" 2>/dev/null || true
wait "$watchdog_pid" 2>/dev/null || true

if [[ -f "$timeout_marker" ]]; then
    rm -f "$timeout_marker" 2>/dev/null || true
    log "reattach timed out after ${TIMEOUT_SECONDS}s adapter=$ADAPTER target=${TARGET:-auto}"
    printf '{}\n'
    exit 0
fi

if [[ "$status" -eq 0 ]]; then
    log "reattach ok adapter=$ADAPTER target=${TARGET:-auto}"
else
    log "reattach failed status=$status adapter=$ADAPTER target=${TARGET:-auto}"
fi

printf '{}\n'
exit 0
`

type Options struct {
	Agent        string
	ScriptPath   string
	BinaryPath   string
	ClaudeConfig string
	CodexConfig  string
	Timeout      time.Duration
	DryRun       bool
}

type FileResult struct {
	Path    string `json:"path"`
	Changed bool   `json:"changed"`
	Backup  string `json:"backup,omitempty"`
}

type Result struct {
	Agent              string                 `json:"agent"`
	Script             FileResult             `json:"script"`
	Configs            map[string]FileResult  `json:"configs"`
	Commands           map[string]string      `json:"commands"`
	Snippets           map[string]interface{} `json:"snippets"`
	TimeoutSeconds     int                    `json:"timeout_seconds"`
	HookTimeoutSeconds int                    `json:"hook_timeout_seconds"`
	DryRun             bool                   `json:"dry_run"`
}

func DefaultScriptPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".amq-keepalive", "hooks", "amq-keepalive-session-start.sh"), nil
}

func DefaultClaudeConfig() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func DefaultCodexConfig() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "hooks.json"), nil
}

func Install(opts Options) (Result, error) {
	normalized, err := NormalizeOptions(opts)
	if err != nil {
		return Result{}, err
	}
	command := buildHookCommand(normalized.BinaryPath, normalized.ScriptPath, normalized.Timeout)
	hookTimeoutSeconds := int(normalized.Timeout.Seconds()) + 5
	result := Result{
		Agent:              normalized.Agent,
		Configs:            map[string]FileResult{},
		Commands:           map[string]string{},
		Snippets:           map[string]interface{}{},
		TimeoutSeconds:     int(normalized.Timeout.Seconds()),
		HookTimeoutSeconds: hookTimeoutSeconds,
		DryRun:             normalized.DryRun,
	}

	scriptResult := FileResult{Path: normalized.ScriptPath}
	if !normalized.DryRun {
		changed, err := writeExecutableIfChanged(normalized.ScriptPath, []byte(SessionStartScript))
		if err != nil {
			return Result{}, err
		}
		scriptResult.Changed = changed
	}
	result.Script = scriptResult

	if normalized.Agent == AgentClaude || normalized.Agent == AgentBoth {
		snippet := claudeSessionStartEntry(command, hookTimeoutSeconds)
		result.Commands[AgentClaude] = command
		result.Snippets[AgentClaude] = snippet
		if !normalized.DryRun {
			fileResult, err := installClaudeHook(normalized.ClaudeConfig, command, hookTimeoutSeconds)
			if err != nil {
				return Result{}, err
			}
			result.Configs[AgentClaude] = fileResult
		}
	}
	if normalized.Agent == AgentCodex || normalized.Agent == AgentBoth {
		snippet := codexSessionStartEntry(command, hookTimeoutSeconds)
		result.Commands[AgentCodex] = command
		result.Snippets[AgentCodex] = snippet
		if !normalized.DryRun {
			fileResult, err := installCodexHook(normalized.CodexConfig, command, hookTimeoutSeconds)
			if err != nil {
				return Result{}, err
			}
			result.Configs[AgentCodex] = fileResult
		}
	}
	return result, nil
}

func NormalizeOptions(opts Options) (Options, error) {
	switch opts.Agent {
	case "", AgentBoth:
		opts.Agent = AgentBoth
	case AgentClaude, AgentCodex:
	default:
		return Options{}, fmt.Errorf("--agent must be one of %s, %s, %s", AgentClaude, AgentCodex, AgentBoth)
	}
	if opts.ScriptPath == "" {
		path, err := DefaultScriptPath()
		if err != nil {
			return Options{}, err
		}
		opts.ScriptPath = path
	}
	scriptPath, err := absPath(opts.ScriptPath)
	if err != nil {
		return Options{}, fmt.Errorf("resolve script path: %w", err)
	}
	opts.ScriptPath = scriptPath

	if opts.BinaryPath == "" {
		return Options{}, errors.New("binary path is required")
	}
	binaryPath, err := resolveExecutable(opts.BinaryPath)
	if err != nil {
		return Options{}, fmt.Errorf("resolve binary path: %w", err)
	}
	opts.BinaryPath = binaryPath

	if opts.ClaudeConfig == "" {
		path, err := DefaultClaudeConfig()
		if err != nil {
			return Options{}, err
		}
		opts.ClaudeConfig = path
	}
	opts.ClaudeConfig, err = absPath(opts.ClaudeConfig)
	if err != nil {
		return Options{}, fmt.Errorf("resolve Claude config: %w", err)
	}

	if opts.CodexConfig == "" {
		path, err := DefaultCodexConfig()
		if err != nil {
			return Options{}, err
		}
		opts.CodexConfig = path
	}
	opts.CodexConfig, err = absPath(opts.CodexConfig)
	if err != nil {
		return Options{}, fmt.Errorf("resolve Codex config: %w", err)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Timeout < time.Second {
		return Options{}, errors.New("--timeout must be at least 1s")
	}
	return opts, nil
}

func installClaudeHook(path, command string, hookTimeoutSeconds int) (FileResult, error) {
	doc, err := loadJSONObject(path)
	if err != nil {
		return FileResult{}, err
	}
	hooks, err := objectField(doc, "hooks")
	if err != nil {
		return FileResult{}, err
	}
	sessionStart, err := arrayField(hooks, "SessionStart")
	if err != nil {
		return FileResult{}, err
	}
	changed := false
	if !claudeHasCommand(sessionStart, command) {
		sessionStart = append(sessionStart, claudeSessionStartEntry(command, hookTimeoutSeconds))
		hooks["SessionStart"] = sessionStart
		doc["hooks"] = hooks
		changed = true
	}
	return saveJSONIfChanged(path, doc, changed)
}

func installCodexHook(path, command string, hookTimeoutSeconds int) (FileResult, error) {
	doc, err := loadJSONObject(path)
	if err != nil {
		return FileResult{}, err
	}
	hooks, err := objectField(doc, "hooks")
	if err != nil {
		return FileResult{}, err
	}
	sessionStart, err := arrayField(hooks, "SessionStart")
	if err != nil {
		return FileResult{}, err
	}
	changed := false
	if !codexHasCommand(sessionStart, command) {
		hook := codexHook(command, hookTimeoutSeconds)
		if len(sessionStart) == 0 {
			sessionStart = append(sessionStart, map[string]interface{}{"hooks": []interface{}{hook}})
		} else {
			first, ok := sessionStart[0].(map[string]interface{})
			if !ok {
				sessionStart = append(sessionStart, map[string]interface{}{"hooks": []interface{}{hook}})
				hooks["SessionStart"] = sessionStart
				doc["hooks"] = hooks
				return saveJSONIfChanged(path, doc, true)
			}
			hookList := interfaceArray(first["hooks"])
			hookList = append(hookList, hook)
			first["hooks"] = hookList
		}
		hooks["SessionStart"] = sessionStart
		doc["hooks"] = hooks
		changed = true
	}
	return saveJSONIfChanged(path, doc, changed)
}

func claudeSessionStartEntry(command string, hookTimeoutSeconds int) map[string]interface{} {
	return map[string]interface{}{
		"matcher": "*",
		"hooks": []interface{}{
			map[string]interface{}{
				"type":          "command",
				"command":       command,
				"timeout":       hookTimeoutSeconds,
				"statusMessage": "Reattaching AMQ wake...",
			},
		},
	}
}

func codexSessionStartEntry(command string, hookTimeoutSeconds int) map[string]interface{} {
	return map[string]interface{}{
		"hooks": []interface{}{
			codexHook(command, hookTimeoutSeconds),
		},
	}
}

func codexHook(command string, hookTimeoutSeconds int) map[string]interface{} {
	return map[string]interface{}{
		"type":    "command",
		"command": command,
		"timeout": hookTimeoutSeconds * 1000,
	}
}

func claudeHasCommand(entries []interface{}, command string) bool {
	for _, entry := range entries {
		obj, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		for _, hook := range interfaceArray(obj["hooks"]) {
			hookObj, ok := hook.(map[string]interface{})
			if ok && strings.TrimSpace(fmt.Sprint(hookObj["command"])) == command {
				return true
			}
		}
	}
	return false
}

func codexHasCommand(entries []interface{}, command string) bool {
	return claudeHasCommand(entries, command)
}

func loadJSONObject(path string) (map[string]interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]interface{}{}, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]interface{}{}, nil
	}
	var doc map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc == nil {
		doc = map[string]interface{}{}
	}
	return doc, nil
}

func saveJSONIfChanged(path string, doc map[string]interface{}, changed bool) (FileResult, error) {
	result := FileResult{Path: path, Changed: changed}
	if !changed {
		return result, nil
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return FileResult{}, err
	}
	data = append(data, '\n')
	backup, err := backupIfExists(path)
	if err != nil {
		return FileResult{}, err
	}
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return FileResult{}, err
	}
	result.Backup = backup
	return result, nil
}

func backupIfExists(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	backup := fmt.Sprintf("%s.bak-%s", path, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.WriteFile(backup, data, 0o600); err != nil {
		return "", err
	}
	return backup, nil
}

func writeExecutableIfChanged(path string, data []byte) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return false, os.Chmod(path, 0o755)
	} else if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return true, writeFileAtomic(path, data, 0o755)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".amq-keepalive-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func objectField(doc map[string]interface{}, key string) (map[string]interface{}, error) {
	if raw, ok := doc[key]; ok {
		if obj, ok := raw.(map[string]interface{}); ok {
			return obj, nil
		}
		return nil, fmt.Errorf("%q must be a JSON object", key)
	}
	obj := map[string]interface{}{}
	doc[key] = obj
	return obj, nil
}

func arrayField(doc map[string]interface{}, key string) ([]interface{}, error) {
	if raw, ok := doc[key]; ok {
		values, ok := raw.([]interface{})
		if !ok {
			return nil, fmt.Errorf("%q must be a JSON array", key)
		}
		return values, nil
	}
	values := []interface{}{}
	doc[key] = values
	return values, nil
}

func interfaceArray(raw interface{}) []interface{} {
	if values, ok := raw.([]interface{}); ok {
		return values
	}
	return []interface{}{}
}

func buildHookCommand(binaryPath, scriptPath string, timeout time.Duration) string {
	return fmt.Sprintf("AMQ_KEEPALIVE_BIN=%s AMQ_KEEPALIVE_TIMEOUT_SECONDS=%s %s",
		shellQuote(binaryPath),
		shellQuote(fmt.Sprintf("%d", int(timeout.Seconds()))),
		shellQuote(scriptPath),
	)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func resolveExecutable(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return "", err
	}
	return absPath(resolved)
}

func absPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Abs(path)
}
