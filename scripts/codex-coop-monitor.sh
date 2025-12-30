#!/usr/bin/env bash
# Codex CLI co-op mode monitor script (optional helper)
# Run this in a background terminal session (requires unified_exec=true in Codex config).
# Note: Background terminal output does NOT wake Codex; use the notify hook for reliable alerts.
# This script is for /ps visibility and manual diagnostics only.
#
# Usage:
#   1. Enable background terminals in ~/.codex/config.toml:
#      [features]
#      unified_exec = true
#
#   2. Run in Codex background terminal:
#      ./scripts/codex-coop-monitor.sh
#
#   3. Or with custom settings:
#      AM_ROOT=/path/to/root AM_ME=codex ./scripts/codex-coop-monitor.sh

set -euo pipefail

: "${AM_ROOT:=.agent-mail}"
: "${AM_ME:=codex}"

# Find amq binary - try local build first, then PATH
AMQ_BIN="./amq"
if [[ ! -x "$AMQ_BIN" ]]; then
    AMQ_BIN="$(command -v amq 2>/dev/null || true)"
fi

if [[ -z "$AMQ_BIN" || ! -x "$AMQ_BIN" ]]; then
    echo "Error: amq binary not found. Run 'make build' first." >&2
    exit 1
fi

echo "[AMQ Co-op Monitor] Starting for agent: $AM_ME"
echo "[AMQ Co-op Monitor] Root: $AM_ROOT"
echo "[AMQ Co-op Monitor] Waiting for messages (peek mode)..."

HAVE_JQ=0
if command -v jq &>/dev/null; then
    HAVE_JQ=1
fi

wait_for_drain() {
    while true; do
        if [[ "$HAVE_JQ" -eq 1 ]]; then
            COUNT=$("$AMQ_BIN" list --me "$AM_ME" --root "$AM_ROOT" --new --json 2>/dev/null | jq -r 'length // 0' 2>/dev/null || echo "0")
            if [[ "$COUNT" =~ ^[0-9]+$ ]] && [[ "$COUNT" -eq 0 ]]; then
                return 0
            fi
        else
            if "$AMQ_BIN" list --me "$AM_ME" --root "$AM_ROOT" --new 2>/dev/null | grep -q "^No messages\\.$"; then
                return 0
            fi
        fi
        sleep 0.5
    done
}

# Continuous monitor loop - respawns after each message
# Exit with Ctrl+C to stop
while true; do
    "$AMQ_BIN" monitor --me "$AM_ME" --root "$AM_ROOT" --timeout 0 --include-body --json --peek
    # Avoid repeated notifications for the same messages; wait until drained.
    wait_for_drain
done
