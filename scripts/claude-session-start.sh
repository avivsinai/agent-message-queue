#!/bin/bash
# AMQ Claude SessionStart hook
# Phase 1: Sets AM_ROOT/AM_ME for Claude Code via CLAUDE_ENV_FILE.
# Phase 2: Emits coop preamble as additionalContext for context re-injection
#           after /clear or compaction (see issue #71).

set -euo pipefail

# --- Phase 1: Environment injection (existing behavior) ---

if [ -n "${CLAUDE_ENV_FILE:-}" ]; then
    touch "$CLAUDE_ENV_FILE"

    PROJECT_DIR="${CLAUDE_PROJECT_DIR:-.}"
    ME="${AM_ME:-claude}"

    if command -v amq &> /dev/null; then
        ENV_OUT=$(cd "$PROJECT_DIR" && amq env --me "$ME" 2>/dev/null) || ENV_OUT=""

        if [ -n "$ENV_OUT" ]; then
            # Append AM_ROOT if not already set
            if ! grep -q '^export AM_ROOT=' "$CLAUDE_ENV_FILE"; then
                ROOT_LINE=$(echo "$ENV_OUT" | grep '^export AM_ROOT=' || true)
                [ -n "$ROOT_LINE" ] && echo "$ROOT_LINE" >> "$CLAUDE_ENV_FILE"
            fi
            # Append AM_ME if not already set
            if ! grep -q '^export AM_ME=' "$CLAUDE_ENV_FILE"; then
                ME_LINE=$(echo "$ENV_OUT" | grep '^export AM_ME=' || true)
                [ -n "$ME_LINE" ] && echo "$ME_LINE" >> "$CLAUDE_ENV_FILE"
            fi
        fi
    fi
fi

# --- Phase 2: Context re-injection ---
# Emit coop preamble so Claude retains session awareness after /clear.
# Requires amq binary and an active coop session (AM_ROOT or .amqrc).

if command -v amq &> /dev/null; then
    PROJECT_DIR="${CLAUDE_PROJECT_DIR:-.}"
    CONTEXT_JSON=$(cd "$PROJECT_DIR" && amq integration claude context --json 2>/dev/null) || CONTEXT_JSON=""

    if [ -n "$CONTEXT_JSON" ]; then
        # Extract preamble and peer count from JSON
        PREAMBLE=$(echo "$CONTEXT_JSON" | python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
    print(d.get('preamble', ''))
except Exception:
    pass
" 2>/dev/null) || PREAMBLE=""

        PEER_COUNT=$(echo "$CONTEXT_JSON" | python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
    print(len(d.get('peers', [])))
except Exception:
    print('0')
" 2>/dev/null) || PEER_COUNT="0"

        INBOX_NEW=$(echo "$CONTEXT_JSON" | python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
    print(d.get('inbox', {}).get('new', 0))
except Exception:
    print('0')
" 2>/dev/null) || INBOX_NEW="0"

        if [ -n "$PREAMBLE" ]; then
            # Build banner line
            BANNER="AMQ coop: ${PEER_COUNT} peer(s)"
            if [ "$INBOX_NEW" != "0" ]; then
                BANNER="${BANNER}, ${INBOX_NEW} unread"
            fi

            # Emit Claude Code hook JSON with both additionalContext and systemMessage
            python3 -c "
import json, sys
preamble = sys.argv[1]
banner = sys.argv[2]
payload = {
    'hookSpecificOutput': {
        'additionalContext': preamble
    },
    'systemMessage': banner
}
json.dump(payload, sys.stdout)
" "$PREAMBLE" "$BANNER"
        fi
    fi
fi
