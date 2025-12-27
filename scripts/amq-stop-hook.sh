#!/bin/bash
# AMQ Co-op Stop Hook
# Blocks stop if there are pending messages in inbox
# Safe fallback: approves if amq/jq unavailable

ROOT="${AM_ROOT:-.agent-mail}"
ME="${AM_ME:-claude}"

# Safe fallback if dependencies missing
if ! command -v amq &> /dev/null || ! command -v jq &> /dev/null; then
    echo '{"decision": "approve"}'
    exit 0
fi

# Check for pending messages (safe fallback on any error)
COUNT=$(amq list --root "$ROOT" --me "$ME" --new --json 2>/dev/null | jq -r 'length // 0' 2>/dev/null || echo "0")

if [ "$COUNT" -gt 0 ] 2>/dev/null; then
    echo '{"decision": "block", "reason": "You have '"$COUNT"' pending message(s). Run: amq drain --include-body"}'
    exit 0
fi

echo '{"decision": "approve"}'
