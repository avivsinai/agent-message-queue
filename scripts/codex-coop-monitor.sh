#!/usr/bin/env bash
# Codex CLI co-op mode monitor script
# Run this in a background terminal session (requires unified_exec=true in Codex config)
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
echo "[AMQ Co-op Monitor] Waiting for messages..."

# Single-shot monitor - exits when message arrives
# Codex should respawn this script or check the output
exec "$AMQ_BIN" monitor --me "$AM_ME" --root "$AM_ROOT" --timeout 0 --include-body --json
