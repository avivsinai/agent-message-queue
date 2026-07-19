#!/usr/bin/env bash
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
