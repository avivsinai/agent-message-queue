package hookinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	// amqOwnedCommandPrefix marks every SessionStart hook command the AMQ
	// keepalive installer writes. buildHookCommand stamps each command with an
	// AMQ_KEEPALIVE_BIN= env assignment, so its presence is a reliable ownership
	// signal that does not require a separate marker file.
	amqOwnedCommandPrefix = "AMQ_KEEPALIVE_BIN="
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
SLEEP_CMD="${AMQ_KEEPALIVE_SLEEP:-sleep}"

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
SOURCE=""
if command -v jq >/dev/null 2>&1 && [[ -n "$INPUT" ]]; then
    CWD="$(printf '%s' "$INPUT" | jq -r '.cwd // .workdir // .working_directory // empty' 2>/dev/null || true)"
    SOURCE="$(printf '%s' "$INPUT" | jq -r '.source // empty' 2>/dev/null || true)"
fi
if [[ -n "$CWD" && -d "$CWD" ]]; then
    cd "$CWD" 2>/dev/null || true
fi

SOURCE_LOWER="$(printf '%s' "$SOURCE" | tr '[:upper:]' '[:lower:]')"
case "$SOURCE_LOWER" in
    clear|compact)
        log "skip: SessionStart source=$SOURCE"
        printf '{}\n'
        exit 0
        ;;
esac

if [[ -z "$TARGET" ]]; then
    log "skip: no exact terminal target adapter=$ADAPTER"
    printf '{}\n'
    exit 0
fi

if ! command -v "$BIN" >/dev/null 2>&1; then
    log "skip: amq-keepalive binary not found: $BIN"
    printf '{}\n'
    exit 0
fi

args=(reattach --adapter "$ADAPTER" --amq "$AMQ_BIN" --wake-ready-timeout "${WAKE_TIMEOUT_MILLISECONDS}ms" --retire-detached)
[[ -n "${AMQ_KEEPALIVE_SELF:-}" ]] && args+=(--self "$SELF_BIN")
args+=(--target "$TARGET")
[[ -n "$REGISTRY" ]] && args+=(--registry "$REGISTRY")
[[ -n "$ROOT" ]] && args+=(--root "$ROOT")
[[ -n "$BASE_ROOT" ]] && args+=(--base-root "$BASE_ROOT")
[[ -n "$SESSION_NAME" ]] && args+=(--session "$SESSION_NAME")
[[ -n "$ME" ]] && args+=(--me "$ME")
[[ "${AMQ_KEEPALIVE_NO_START:-0}" == "1" ]] && args+=(--no-start)

run_reattach() {
    "$BIN" "${args[@]}" >> "$LOG_PATH" 2>&1
}

# GOLDEN-CHANGE: private exclusive timeout marker (mktemp 0700 dir) and process-group kill.
timeout_dir="$(mktemp -d "${TMPDIR:-/tmp}/amq-keepalive-timeout.XXXXXX" 2>/dev/null)" || {
    log "timeout marker: mktemp failed"
    printf '{}\n'
    exit 0
}
chmod 0700 "$timeout_dir" 2>/dev/null || true
timeout_marker="${timeout_dir}/flag"
trap 'rm -rf -- "$timeout_dir"' EXIT

set -m
(
    trap 'exit 143' TERM
    run_reattach
) 2>> "$LOG_PATH" &
reattach_pid=$!
set +m
(
    "$SLEEP_CMD" "$TIMEOUT_SECONDS"
    if kill -0 "$reattach_pid" 2>/dev/null; then
        : > "$timeout_marker" 2>/dev/null || true
        kill -TERM -- -"$reattach_pid" 2>/dev/null || true
        "$SLEEP_CMD" 1
        kill -KILL -- -"$reattach_pid" 2>/dev/null || true
    fi
) >/dev/null 2>&1 &
watchdog_pid=$!

wait "$reattach_pid" 2>/dev/null
status=$?
pkill -TERM -P "$watchdog_pid" 2>/dev/null || true
kill "$watchdog_pid" 2>/dev/null || true
wait "$watchdog_pid" 2>/dev/null || true

if [[ -f "$timeout_marker" ]]; then
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

	if normalized.Agent == AgentClaude || normalized.Agent == AgentBoth {
		result.Commands[AgentClaude] = command
		result.Snippets[AgentClaude] = claudeSessionStartEntry(command, hookTimeoutSeconds)
	}
	if normalized.Agent == AgentCodex || normalized.Agent == AgentBoth {
		result.Commands[AgentCodex] = command
		result.Snippets[AgentCodex] = codexSessionStartEntry(command, hookTimeoutSeconds)
	}

	scriptResult := FileResult{Path: normalized.ScriptPath}
	if !normalized.DryRun {
		if err := preflightHookConfigs(normalized, command, hookTimeoutSeconds); err != nil {
			return Result{}, err
		}
		changed, err := writeExecutableIfChanged(normalized.ScriptPath, []byte(SessionStartScript))
		if err != nil {
			return Result{}, err
		}
		scriptResult.Changed = changed
	}
	result.Script = scriptResult

	if !normalized.DryRun && (normalized.Agent == AgentClaude || normalized.Agent == AgentBoth) {
		fileResult, err := installClaudeHook(normalized.ClaudeConfig, command, hookTimeoutSeconds)
		if err != nil {
			return result, err
		}
		result.Configs[AgentClaude] = fileResult
	}
	if !normalized.DryRun && (normalized.Agent == AgentCodex || normalized.Agent == AgentBoth) {
		fileResult, err := installCodexHook(normalized.CodexConfig, command, hookTimeoutSeconds)
		if err != nil {
			if result.Configs[AgentClaude].Changed {
				return result, fmt.Errorf("partial hookinstall commit: %s updated; %s failed: %w", AgentClaude, AgentCodex, err)
			}
			return result, err
		}
		result.Configs[AgentCodex] = fileResult
	}
	return result, nil
}

func preflightHookConfigs(opts Options, command string, hookTimeoutSeconds int) error {
	if opts.Agent == AgentClaude || opts.Agent == AgentBoth {
		if _, _, err := prepareClaudeHook(opts.ClaudeConfig, command, hookTimeoutSeconds); err != nil {
			return err
		}
	}
	if opts.Agent == AgentCodex || opts.Agent == AgentBoth {
		if _, _, err := prepareCodexHook(opts.CodexConfig, command, hookTimeoutSeconds); err != nil {
			return err
		}
	}
	return nil
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
	doc, changed, err := prepareClaudeHook(path, command, hookTimeoutSeconds)
	if err != nil {
		return FileResult{}, err
	}
	return saveJSONIfChanged(path, doc, changed)
}

func installCodexHook(path, command string, hookTimeoutSeconds int) (FileResult, error) {
	doc, changed, err := prepareCodexHook(path, command, hookTimeoutSeconds)
	if err != nil {
		return FileResult{}, err
	}
	return saveJSONIfChanged(path, doc, changed)
}

func prepareClaudeHook(path, command string, hookTimeoutSeconds int) (map[string]interface{}, bool, error) {
	doc, err := loadJSONObject(path)
	if err != nil {
		return nil, false, err
	}
	hooks, err := objectField(doc, "hooks")
	if err != nil {
		return nil, false, err
	}
	sessionStart, err := arrayField(hooks, "SessionStart")
	if err != nil {
		return nil, false, err
	}
	if err := validateSessionStartEntries(sessionStart); err != nil {
		return nil, false, err
	}
	sessionStart, healed := selfHealSessionStart(sessionStart)
	hooks["SessionStart"] = sessionStart
	doc["hooks"] = hooks
	if claudeHasCommand(sessionStart, command) {
		return doc, healed, nil
	}
	sessionStart = append(sessionStart, claudeSessionStartEntry(command, hookTimeoutSeconds))
	hooks["SessionStart"] = sessionStart
	doc["hooks"] = hooks
	return doc, true, nil
}

func prepareCodexHook(path, command string, hookTimeoutSeconds int) (map[string]interface{}, bool, error) {
	doc, err := loadJSONObject(path)
	if err != nil {
		return nil, false, err
	}
	hooks, err := objectField(doc, "hooks")
	if err != nil {
		return nil, false, err
	}
	sessionStart, err := arrayField(hooks, "SessionStart")
	if err != nil {
		return nil, false, err
	}
	if err := validateSessionStartEntries(sessionStart); err != nil {
		return nil, false, err
	}
	sessionStart, healed := selfHealSessionStart(sessionStart)
	hooks["SessionStart"] = sessionStart
	doc["hooks"] = hooks
	if codexHasCommand(sessionStart, command) {
		return doc, healed, nil
	}
	hook := codexHook(command, hookTimeoutSeconds)
	if len(sessionStart) == 0 {
		sessionStart = append(sessionStart, map[string]interface{}{"hooks": []interface{}{hook}})
	} else {
		first, ok := sessionStart[0].(map[string]interface{})
		if !ok {
			sessionStart = append(sessionStart, map[string]interface{}{"hooks": []interface{}{hook}})
		} else {
			hookList, err := innerHookList(first)
			if err != nil {
				return nil, false, err
			}
			first["hooks"] = append(hookList, hook)
		}
	}
	hooks["SessionStart"] = sessionStart
	doc["hooks"] = hooks
	return doc, true, nil
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
		"timeout": hookTimeoutSeconds,
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

// isAMQOwnedCommand reports whether a SessionStart hook command was written by
// the AMQ keepalive installer, by detecting the AMQ_KEEPALIVE_BIN= env prefix
// that buildHookCommand stamps on every command it emits. The prefix is
// matched at the start of the command so a foreign hook that merely echoes the
// env var name (e.g. `echo AMQ_KEEPALIVE_BIN=x`) is not misclassified as owned.
func isAMQOwnedCommand(command string) bool {
	return strings.HasPrefix(strings.TrimSpace(command), amqOwnedCommandPrefix)
}

// hookClass is the shared classification of a single hook command used by both
// the install-time self-heal and the read-only doctor scan so the two never
// disagree. owned is true for AMQ-written commands; scriptPath is the parsed
// installed script path (empty when the command could not be parsed); stale is
// true only when the path parsed and the script does not exist on disk. An
// owned command that cannot be parsed is NOT stale: self-heal and doctor never
// delete or report what they cannot read.
type hookClass struct {
	owned      bool
	scriptPath string
	stale      bool
}

// classifyHook inspects a single hook object and returns its classification.
// It is the single source of truth for ownership, path extraction, and
// staleness shared by pruneStaleAMQHooks (install-time self-heal) and
// StaleSessionStartHooks (doctor scan).
func classifyHook(hookObj map[string]interface{}) hookClass {
	command := strings.TrimSpace(fmt.Sprint(hookObj["command"]))
	if !isAMQOwnedCommand(command) {
		return hookClass{}
	}
	scriptPath, ok := scriptPathFromAMQCommand(command)
	if !ok || scriptPath == "" {
		// Owned but unparseable: preserve, do not report stale.
		return hookClass{owned: true}
	}
	_, err := os.Stat(scriptPath)
	stale := os.IsNotExist(err)
	// An unreadable path (e.g. EACCES) is not stale: unreadable is not the
	// same as missing, so we do not prune or report it.
	return hookClass{owned: true, scriptPath: scriptPath, stale: stale}
}

// scriptPathFromAMQCommand extracts the installed script path from an AMQ-owned
// SessionStart hook command. buildHookCommand emits a fixed layout of three
// single-quoted tokens: AMQ_KEEPALIVE_BIN=<q> AMQ_KEEPALIVE_TIMEOUT_SECONDS=<q>
// <q>, where the third token is the script path. This is a forward decoder of
// that fixed shape: it consumes one single-quoted token at a time (decoding the
// 4-byte escaped-quote sequence that shellQuote emits for an embedded quote), skips the
// single space separators, and returns the third token. Any leftover non-space
// bytes after the third token, or the wrong number of tokens, means the command
// does not match the shape buildHookCommand produces and is not parseable.
// Returns the unescaped path and true on success; false (without dropping
// anything) otherwise.
func scriptPathFromAMQCommand(command string) (string, bool) {
	s := command
	// Token 1: AMQ_KEEPALIVE_BIN=<quoted>, then a space.
	rest, ok := strings.CutPrefix(s, amqOwnedCommandPrefix)
	if !ok {
		return "", false
	}
	token1, rest, ok := shellUnquoteToken(rest)
	if !ok || token1 == "" {
		return "", false
	}
	rest, ok = consumeSingleSpace(rest)
	if !ok {
		return "", false
	}
	// Token 2: AMQ_KEEPALIVE_TIMEOUT_SECONDS=<quoted>, then a space. The prefix
	// must be present exactly (CutPrefix, not TrimPrefix) and the decoded token
	// must be all digits, because buildHookCommand emits shellQuote(fmt.Sprintf("%d",...)).
	rest, ok = strings.CutPrefix(rest, "AMQ_KEEPALIVE_TIMEOUT_SECONDS=")
	if !ok {
		return "", false
	}
	timeoutToken, rest, ok := shellUnquoteToken(rest)
	if !ok || timeoutToken == "" {
		return "", false
	}
	if !isAllDigits(timeoutToken) {
		return "", false
	}
	rest, ok = consumeSingleSpace(rest)
	if !ok {
		return "", false
	}
	// Token 3: the script path.
	path, left, ok := shellUnquoteToken(rest)
	if !ok {
		return "", false
	}
	// Any leftover non-space bytes mean a 4th token is present: not parseable.
	if strings.TrimSpace(left) != "" {
		return "", false
	}
	return path, true
}

// isAllDigits reports whether s is non-empty and consists only of ASCII digits,
// matching the shape buildHookCommand emits for the timeout (an int seconds
// value rendered by fmt.Sprintf("%d", ...)).
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// shellUnquoteToken consumes one single-quoted token from the start of s and
// returns its decoded value, the unconsumed remainder, and ok. It is the exact
// inverse of shellQuote, which produces a token of the form '<value>' where
// every embedded single quote is encoded as a 4-byte close-escape-reopen
// sequence: close the quote, a backslash-escaped quote, reopen the quote.
// Decoding therefore strips the outer quotes and replaces each such sequence
// with a literal single quote.
// ok is false if s does not begin with a complete, well-formed token.
func shellUnquoteToken(s string) (token, rest string, ok bool) {
	if len(s) == 0 || s[0] != '\'' {
		return "", s, false
	}
	// Find the closing quote that ends this token. A token is a run of
	// '<...>' segments joined by escaped-quote sequences; the token ends at the
	// first single quote that is not followed by the rest of an escape.
	i := 1
	for i < len(s) {
		if s[i] != '\'' {
			i++
			continue
		}
		// s[i] == quote. If this is the start of an escaped-quote sequence (close,
		// backslash, quote, reopen), skip the 4 bytes and continue inside the token.
		if i+3 < len(s) && s[i+1] == '\\' && s[i+2] == '\'' && s[i+3] == '\'' {
			i += 4
			continue
		}
		// This quote closes the token.
		break
	}
	if i >= len(s) {
		return "", s, false // unterminated quote
	}
	// s[1:i] is the raw token body; decode escaped-quote sequences back to
	// single quotes.
	raw := s[1:i]
	token = strings.ReplaceAll(raw, "'\\''", "'")
	return token, s[i+1:], true
}

// consumeSingleSpace consumes exactly one ASCII space from the start of s. It
// returns the remainder and ok; ok is false if s does not start with a space.
func consumeSingleSpace(s string) (rest string, ok bool) {
	if len(s) == 0 || s[0] != ' ' {
		return s, false
	}
	return s[1:], true
}

// selfHealSessionStart prunes stale AMQ-owned hooks from every SessionStart
// entry. Foreign hooks are preserved verbatim. AMQ-owned hooks whose script
// path does not exist are dropped, and duplicate AMQ-owned hooks referencing
// the same existing script path are de-duplicated to the first occurrence.
// Owned hooks that cannot be parsed are preserved (never deleted unread).
// Returns the (possibly same) slice and whether any entry changed.
func selfHealSessionStart(sessionStart []interface{}) ([]interface{}, bool) {
	changed := false
	for i, entry := range sessionStart {
		obj, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		inner, err := innerHookList(obj)
		if err != nil || len(inner) == 0 {
			continue
		}
		pruned, entryChanged := pruneStaleAMQHooks(inner)
		if entryChanged {
			obj["hooks"] = pruned
			sessionStart[i] = obj
			changed = true
		}
	}
	return sessionStart, changed
}

// pruneStaleAMQHooks filters the inner hooks list of a single SessionStart
// entry. Non-AMQ hooks are preserved verbatim. AMQ-owned hooks whose script
// path is missing are dropped; duplicates referencing the same existing script
// path are collapsed to the first occurrence. Owned hooks that cannot be parsed
// are preserved, not dropped. Uses classifyHook so it agrees with the doctor
// scan.
func pruneStaleAMQHooks(hooks []interface{}) ([]interface{}, bool) {
	// Duplicate collapse keys on the exact full command string, the same
	// identity the installer's claudeHasCommand uses: two LIVE owned hooks that
	// share a script path but differ in AMQ_KEEPALIVE_BIN or timeout are distinct
	// commands and must both be preserved; only byte-identical commands collapse.
	seen := make(map[string]bool)
	filtered := make([]interface{}, 0, len(hooks))
	changed := false
	for _, hook := range hooks {
		hookObj, ok := hook.(map[string]interface{})
		if !ok {
			filtered = append(filtered, hook)
			continue
		}
		class := classifyHook(hookObj)
		if !class.owned {
			filtered = append(filtered, hook)
			continue
		}
		if class.scriptPath == "" {
			// Owned but unparseable: preserve verbatim.
			filtered = append(filtered, hook)
			continue
		}
		command := strings.TrimSpace(fmt.Sprint(hookObj["command"]))
		if seen[command] {
			changed = true
			continue
		}
		if class.stale {
			changed = true
			continue
		}
		seen[command] = true
		filtered = append(filtered, hook)
	}
	return filtered, changed
}

// StaleSessionStartHook describes an AMQ-owned SessionStart hook command whose
// script path does not exist. It is used by amq doctor to flag dead hooks.
type StaleSessionStartHook struct {
	ConfigPath string `json:"config_path"`
	Agent      string `json:"agent"`
	ScriptPath string `json:"script_path"`
}

// StaleSessionStartHooks scans a Claude or Codex hook config file for AMQ-owned
// SessionStart hook commands whose script path does not exist. It does not
// modify the file. Returns one entry per stale hook. Uses classifyHook so it
// agrees with the install-time self-heal. Owned hooks that cannot be parsed are
// not reported (they are preserved, not stale).
func StaleSessionStartHooks(configPath, agent string) ([]StaleSessionStartHook, error) {
	doc, err := loadJSONObject(configPath)
	if err != nil {
		return nil, err
	}
	hooks, ok := doc["hooks"].(map[string]interface{})
	if !ok {
		return nil, nil
	}
	sessionStart, ok := hooks["SessionStart"].([]interface{})
	if !ok {
		return nil, nil
	}
	var stale []StaleSessionStartHook
	for _, entry := range sessionStart {
		obj, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		for _, hook := range interfaceArray(obj["hooks"]) {
			hookObj, ok := hook.(map[string]interface{})
			if !ok {
				continue
			}
			class := classifyHook(hookObj)
			if class.owned && class.stale {
				stale = append(stale, StaleSessionStartHook{
					ConfigPath: configPath,
					Agent:      agent,
					ScriptPath: class.scriptPath,
				})
			}
		}
	}
	return stale, nil
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
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parse %s: trailing JSON value", path)
		}
		return nil, fmt.Errorf("parse %s: trailing JSON: %w", path, err)
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
	ts := time.Now().UTC().Format("20060102T150405Z")
	for i := 0; i < 100; i++ {
		backup := fmt.Sprintf("%s.bak-%s", path, ts)
		if i > 0 {
			backup = fmt.Sprintf("%s.bak-%s-%d", path, ts, i)
		}
		err := writeExclusiveRegularFile(backup, data, 0o600)
		if err == nil {
			return backup, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("create backup %s.bak-%s: names exhausted", path, ts)
}

func writeExclusiveRegularFile(path string, data []byte, mode os.FileMode) (err error) {
	file, err := os.OpenFile(path, exclusiveCreateFlags(), mode)
	if err != nil {
		return err
	}
	defer func() {
		cerr := file.Close()
		if err != nil {
			_ = os.Remove(path)
			return
		}
		if cerr != nil {
			_ = os.Remove(path)
			err = cerr
		}
	}()
	if err = file.Chmod(mode); err != nil {
		return err
	}
	if _, err = file.Write(data); err != nil {
		return err
	}
	return file.Sync()
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

func validateSessionStartEntries(entries []interface{}) error {
	for _, entry := range entries {
		obj, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		if _, err := innerHookList(obj); err != nil {
			return err
		}
	}
	return nil
}

func innerHookList(entry map[string]interface{}) ([]interface{}, error) {
	raw, exists := entry["hooks"]
	if !exists || raw == nil {
		return []interface{}{}, nil
	}
	values, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("%q must be a JSON array", "hooks")
	}
	return values, nil
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
