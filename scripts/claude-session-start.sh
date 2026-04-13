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

first_export_line() {
    local var_name="$1"
    local source_text="${2:-}"

    printf '%s\n' "$source_text" | grep "^export ${var_name}=" | head -1 || true
}

decode_export_line() {
    local var_name="$1"
    local export_line="${2:-}"

    case "$export_line" in
        "export ${var_name}="*)
            env -i EXPORT_LINE="$export_line" VAR_NAME="$var_name" /bin/sh -c \
                'eval "$EXPORT_LINE"; eval "printf %s \"\${$VAR_NAME-}\""' 2>/dev/null || true
            ;;
        *)
            return 0
            ;;
    esac
}

# --- Phase 1: Environment injection (existing behavior) ---

if [ -n "${CLAUDE_ENV_FILE:-}" ]; then
    touch "$CLAUDE_ENV_FILE"

    if command -v amq &> /dev/null; then
        ENV_OUT=$(cd "$PROJECT_DIR" && amq env --me "$ME" 2>/dev/null) || ENV_OUT=""

        if [ -n "$ENV_OUT" ]; then
            ROOT_LINE=$(first_export_line AM_ROOT "$ENV_OUT")
            ME_LINE=$(first_export_line AM_ME "$ENV_OUT")

            # Append AM_ROOT if not already set
            if ! grep -q '^export AM_ROOT=' "$CLAUDE_ENV_FILE"; then
                if [ -n "$ROOT_LINE" ]; then
                    printf '%s\n' "$ROOT_LINE" >> "$CLAUDE_ENV_FILE"
                fi
            fi
            # Append AM_ME if not already set
            if ! grep -q '^export AM_ME=' "$CLAUDE_ENV_FILE"; then
                if [ -n "$ME_LINE" ]; then
                    printf '%s\n' "$ME_LINE" >> "$CLAUDE_ENV_FILE"
                fi
            fi
        fi

        if [ -z "$RESOLVED_ROOT" ]; then
            ROOT_LINE=$(grep '^export AM_ROOT=' "$CLAUDE_ENV_FILE" | head -1 || true)
            if [ -z "$ROOT_LINE" ]; then
                ROOT_LINE=$(first_export_line AM_ROOT "$ENV_OUT")
            fi
            RESOLVED_ROOT=$(decode_export_line AM_ROOT "$ROOT_LINE")
        fi

        if [ -z "${AM_ME:-}" ]; then
            ME_LINE=$(grep '^export AM_ME=' "$CLAUDE_ENV_FILE" | head -1 || true)
            if [ -z "$ME_LINE" ]; then
                ME_LINE=$(first_export_line AM_ME "$ENV_OUT")
            fi
            DECODED_ME=$(decode_export_line AM_ME "$ME_LINE")
            if [ -n "$DECODED_ME" ]; then
                ME="$DECODED_ME"
            fi
        fi
    fi
fi

# --- Phase 2: Context re-injection ---
# Compose existing CLI primitives to build a coop preamble that restores
# session awareness after /clear or compaction (see issue #71).

if command -v amq &> /dev/null; then
    if [ -n "$RESOLVED_ROOT" ]; then
        ENV_JSON=$(cd "$PROJECT_DIR" && amq env --me "$ME" --root "$RESOLVED_ROOT" --json 2>>"$HOOK_LOG") || ENV_JSON=""
        PRESENCE_JSON=$(amq presence list --root "$RESOLVED_ROOT" --json 2>>"$HOOK_LOG") || PRESENCE_JSON="[]"
        # Count unread via pipe to avoid passing large JSON as argv
        INBOX_COUNT=$(amq list --me "$ME" --root "$RESOLVED_ROOT" --new --json 2>>"$HOOK_LOG" \
            | python3 -c "import json,sys;print(len(json.load(sys.stdin)))" 2>>"$HOOK_LOG") || INBOX_COUNT=0
    else
        ENV_JSON=$(cd "$PROJECT_DIR" && amq env --me "$ME" --json 2>>"$HOOK_LOG") || ENV_JSON=""
        PRESENCE_JSON=$(amq presence list --json 2>>"$HOOK_LOG") || PRESENCE_JSON="[]"
        # Count unread via pipe to avoid passing large JSON as argv
        INBOX_COUNT=$(amq list --me "$ME" --new --json 2>>"$HOOK_LOG" \
            | python3 -c "import json,sys;print(len(json.load(sys.stdin)))" 2>>"$HOOK_LOG") || INBOX_COUNT=0
    fi

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
