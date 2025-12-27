#!/bin/bash
# AMQ Co-op Stop Hook
# Blocks stop if there are pending messages in inbox
# Safe fallback: approves if amq/jq unavailable or co-op not configured

ROOT="${AM_ROOT:-.agent-mail}"
ME="${AM_ME:-claude}"

# Fast path: approve immediately if co-op not set up
if [ ! -d "$ROOT/agents/$ME/inbox/new" ]; then
    echo '{"decision": "approve"}'
    exit 0
fi

# Safe fallback if dependencies missing
if ! command -v amq &> /dev/null || ! command -v jq &> /dev/null; then
    echo '{"decision": "approve"}'
    exit 0
fi

# Check for pending messages (safe fallback on any error)
COUNT=$(amq list --root "$ROOT" --me "$ME" --new --json 2>/dev/null | jq -r 'length // 0' 2>/dev/null || echo "0")

# Sanitize COUNT to ensure it's a number
if ! [[ "$COUNT" =~ ^[0-9]+$ ]]; then
    COUNT=0
fi

if [ "$COUNT" -gt 0 ]; then
    echo '{"decision": "block", "reason": "You have '"$COUNT"' pending message(s). Ask me to drain the inbox before stopping."}'
    exit 0
fi

echo '{"decision": "approve"}'
