#!/bin/bash
# AMQ Claude SessionStart hook
# Phase 1: Sets AM_ROOT/AM_ME for Claude Code via CLAUDE_ENV_FILE.
# Phase 2: Emits coop preamble as additionalContext for context re-injection
#           after /clear or compaction (see issue #71).

set -euo pipefail

PROJECT_DIR="${CLAUDE_PROJECT_DIR:-.}"
ME="${AM_ME:-claude}"
RESOLVED_ROOT="${AM_ROOT:-}"

# --- Phase 1: Environment injection (existing behavior) ---

if [ -n "${CLAUDE_ENV_FILE:-}" ]; then
    touch "$CLAUDE_ENV_FILE"

    if command -v amq &> /dev/null; then
        ENV_OUT=$(cd "$PROJECT_DIR" && amq env --me "$ME" 2>/dev/null) || ENV_OUT=""

        if [ -n "$ENV_OUT" ]; then
            # Append AM_ROOT if not already set
            if ! grep -q '^export AM_ROOT=' "$CLAUDE_ENV_FILE"; then
                ROOT_LINE=$(echo "$ENV_OUT" | grep '^export AM_ROOT=' || true)
                if [ -n "$ROOT_LINE" ]; then
                    echo "$ROOT_LINE" >> "$CLAUDE_ENV_FILE"
                    # Capture resolved root for phase 2
                    RESOLVED_ROOT=$(echo "$ROOT_LINE" | sed "s/^export AM_ROOT=//; s/^'//; s/'$//")
                fi
            fi
            # Append AM_ME if not already set
            if ! grep -q '^export AM_ME=' "$CLAUDE_ENV_FILE"; then
                ME_LINE=$(echo "$ENV_OUT" | grep '^export AM_ME=' || true)
                if [ -n "$ME_LINE" ]; then
                    echo "$ME_LINE" >> "$CLAUDE_ENV_FILE"
                    ME=$(echo "$ME_LINE" | sed "s/^export AM_ME=//; s/^'//; s/'$//")
                fi
            fi
        fi
    fi
fi

# --- Phase 2: Context re-injection ---
# Emit coop preamble so Claude retains session awareness after /clear.
# Uses the resolved root/me from phase 1 to avoid re-resolution divergence.

if command -v amq &> /dev/null; then
    # Build flags from resolved values so phase 2 uses the same root as phase 1
    CTX_FLAGS=(integration claude context --json)
    [ -n "$RESOLVED_ROOT" ] && CTX_FLAGS+=(--root "$RESOLVED_ROOT")
    [ -n "$ME" ] && CTX_FLAGS+=(--me "$ME")

    CONTEXT_JSON=$(cd "$PROJECT_DIR" && amq "${CTX_FLAGS[@]}" 2>/dev/null) || CONTEXT_JSON=""

    if [ -n "$CONTEXT_JSON" ]; then
        # Extract preamble, peer count, and inbox count from JSON in a single python call
        python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
    preamble = d.get('preamble', '')
    if not preamble:
        sys.exit(0)
    peers = d.get('peers', [])
    inbox_new = d.get('inbox', {}).get('new', 0)

    banner = 'AMQ coop: %d peer(s)' % len(peers)
    if inbox_new:
        banner += ', %d unread' % inbox_new

    payload = {
        'hookSpecificOutput': {
            'additionalContext': preamble
        },
        'systemMessage': banner
    }
    json.dump(payload, sys.stdout)
except Exception:
    pass
" <<< "$CONTEXT_JSON"
    fi
fi
