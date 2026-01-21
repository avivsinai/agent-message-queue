#!/bin/bash
# AMQ Claude SessionStart hook
# Sets AM_ROOT/AM_ME for Claude Code by appending to CLAUDE_ENV_FILE.

set -euo pipefail

[ -z "${CLAUDE_ENV_FILE:-}" ] && exit 0

touch "$CLAUDE_ENV_FILE"

# Run amq env from project dir (finds .amqrc there)
# If CLAUDE_PROJECT_DIR not set, fall back to cwd
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
