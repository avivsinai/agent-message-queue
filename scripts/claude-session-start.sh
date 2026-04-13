#!/bin/bash
# AMQ Claude SessionStart hook
# Phase 1: Sets AM_ROOT/AM_ME for Claude Code via CLAUDE_ENV_FILE.
# Phase 2: Emits coop preamble as additionalContext for context re-injection
#           after /clear or compaction (see issue #71).

set -euo pipefail

PROJECT_DIR="${CLAUDE_PROJECT_DIR:-.}"
ME="${AM_ME:-claude}"
RESOLVED_ROOT="${AM_ROOT:-}"
HOOK_LOG="${TMPDIR:-/tmp}/amq-hook-${USER:-unknown}.log"

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
            elif [ -z "$RESOLVED_ROOT" ]; then
                # Already in env file — capture for phase 2 (/clear scenario)
                RESOLVED_ROOT=$(grep '^export AM_ROOT=' "$CLAUDE_ENV_FILE" | head -1 | sed "s/^export AM_ROOT=//; s/^'//; s/'$//")
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
# Compose existing CLI primitives to build a coop preamble that restores
# session awareness after /clear or compaction (see issue #71).

if command -v amq &> /dev/null; then
    ROOT_FLAGS=()
    [ -n "$RESOLVED_ROOT" ] && ROOT_FLAGS=(--root "$RESOLVED_ROOT")

    ENV_JSON=$(cd "$PROJECT_DIR" && amq env --me "$ME" "${ROOT_FLAGS[@]}" --json 2>>"$HOOK_LOG") || ENV_JSON=""
    PRESENCE_JSON=$(amq presence list "${ROOT_FLAGS[@]}" --json 2>>"$HOOK_LOG") || PRESENCE_JSON="[]"
    # Count unread via pipe to avoid passing large JSON as argv
    INBOX_COUNT=$(amq list --me "$ME" "${ROOT_FLAGS[@]}" --new --json 2>>"$HOOK_LOG" \
        | python3 -c "import json,sys;print(len(json.load(sys.stdin)))" 2>>"$HOOK_LOG") || INBOX_COUNT=0

    if [ -n "$ENV_JSON" ]; then
        python3 - "$ME" "$ENV_JSON" "$PRESENCE_JSON" "$INBOX_COUNT" 2>>"$HOOK_LOG" <<'PYEOF' || true
import json, sys
from datetime import datetime, timezone, timedelta

try:
    me = sys.argv[1]
    env = json.loads(sys.argv[2])
    presence = json.loads(sys.argv[3])
    unread = int(sys.argv[4])

    session = env.get("session_name", "")
    project = env.get("project", "")

    # Build peer list with activity status from presence
    now = datetime.now(timezone.utc)
    peers = []
    for p in presence:
        handle = p.get("handle", "")
        if not handle or handle == me:
            continue
        last_seen = p.get("last_seen", "")
        active = False
        if last_seen:
            try:
                ts = datetime.fromisoformat(last_seen.replace("Z", "+00:00"))
                active = (now - ts) < timedelta(minutes=10)
            except (ValueError, TypeError):
                pass
        status = "active" if active else "stale"
        peers.append({"handle": handle, "status": status})

    peer_str = ",".join(f'{p["handle"]}({p["status"]})' for p in peers)

    # Line 1: identity + session
    if session:
        line1 = f"AMQ coop active: me={me} session={session}"
    else:
        line1 = f"AMQ available: me={me}"
    if project:
        line1 += f" project={project}"
    if peer_str:
        line1 += f" peers={peer_str}"
    line1 += "."

    parts = [line1]
    if unread:
        parts.append(f"Inbox: {unread} unread message(s). Run: amq drain --me {me}")
    parts.append("Use amq send/reply/drain for peer coordination. Preserve thread IDs on replies.")

    preamble = "\n".join(parts)

    banner = f"AMQ coop: {len(peers)} peer(s)"
    if unread:
        banner += f", {unread} unread"

    print(json.dumps({
        "hookSpecificOutput": {
            "hookEventName": "SessionStart",
            "additionalContext": preamble,
        },
        "systemMessage": banner,
    }))
except (json.JSONDecodeError, KeyError, TypeError):
    pass
PYEOF
    fi
fi
