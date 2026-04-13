#!/usr/bin/env bash
set -euo pipefail

# Clear env vars that could interfere with explicit --root/--me flags.
unset AM_ROOT AM_ME 2>/dev/null || true

ROOT_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "$ROOT_DIR"
}
trap cleanup EXIT

BIN="$ROOT_DIR/amq"
go build -o "$BIN" ./cmd/amq

QUEUE_ROOT="$ROOT_DIR/agent-mail"

"$BIN" init --root "$QUEUE_ROOT" --agents codex,claude

send_json="$("$BIN" send --root "$QUEUE_ROOT" --me codex --to claude --body "hello" --json)"
msg_id="$(printf '%s\n' "$send_json" | awk -F'"' '/"id":/ {print $4; exit}')"
thread_id="$(printf '%s\n' "$send_json" | awk -F'"' '/"thread":/ {print $4; exit}')"
if [[ -z "$msg_id" || -z "$thread_id" ]]; then
  echo "failed to parse send output"
  exit 1
fi

"$BIN" list --root "$QUEUE_ROOT" --me claude --new | grep -q "$msg_id"

read_out="$("$BIN" read --root "$QUEUE_ROOT" --me claude --id "$msg_id")"
printf '%s' "$read_out" | grep -q "hello"

"$BIN" list --root "$QUEUE_ROOT" --me claude --cur | grep -q "$msg_id"

# read already moved the message to cur, which emits a drained receipt
test -f "$QUEUE_ROOT/agents/claude/receipts/${msg_id}__claude__drained.json"

thread_json="$("$BIN" thread --root "$QUEUE_ROOT" --id "$thread_id" --json)"
thread_msg="$(printf '%s\n' "$thread_json" | awk -F'"' '/"id":/ {print $4; exit}')"
if [[ "$thread_msg" != "$msg_id" ]]; then
  echo "thread output missing message"
  exit 1
fi

"$BIN" presence set --root "$QUEUE_ROOT" --me codex --status busy
"$BIN" presence list --root "$QUEUE_ROOT" | grep -q "^codex"

tmpfile="$QUEUE_ROOT/agents/codex/inbox/tmp/old.tmp"
mkdir -p "$(dirname "$tmpfile")"
printf 'tmp' > "$tmpfile"
sleep 1
"$BIN" cleanup --root "$QUEUE_ROOT" --tmp-older-than 1ms --yes
if [[ -f "$tmpfile" ]]; then
  echo "cleanup did not remove tmp file"
  exit 1
fi

# --- .amqrc root detection (literal root, no default_session) ---
AMQRC_DIR="$(mktemp -d)"
amqrc_cleanup() {
  rm -rf "$AMQRC_DIR"
}
trap 'cleanup; amqrc_cleanup' EXIT

# .amqrc root is literal — init queue at custom-root directly
AMQRC_ROOT="$AMQRC_DIR/custom-root"
printf '{"root": "custom-root"}\n' > "$AMQRC_DIR/.amqrc"
"$BIN" init --root "$AMQRC_ROOT" --agents alice,bob

# Run list with explicit --root (deterministic routing requires AM_ROOT or --root)
(cd "$AMQRC_DIR" && "$BIN" list --root "$AMQRC_ROOT" --me alice --new >/dev/null 2>&1)
echo ".amqrc detection ok"

# --- coop exec bash (defaults to --session collab) ---
EXEC_DIR="$(mktemp -d)"
exec_cleanup() {
  rm -rf "$EXEC_DIR"
}
trap 'cleanup; amqrc_cleanup; exec_cleanup' EXIT

# Init at literal root agent-mail
EXEC_ROOT="$EXEC_DIR/agent-mail"
"$BIN" init --root "$EXEC_ROOT" --agents bash
printf '{"root": "agent-mail"}\n' > "$EXEC_DIR/.amqrc"

exec_out="$(cd "$EXEC_DIR" && "$BIN" coop exec --no-wake -y bash -- -c 'echo $AM_ROOT:$AM_ME' 2>/dev/null)"
# The output should contain the default session "collab" and handle "bash"
if ! printf '%s' "$exec_out" | grep -q "bash"; then
  echo "coop exec did not set AM_ME=bash"
  echo "got: $exec_out"
  exit 1
fi
if ! printf '%s' "$exec_out" | grep -q "agent-mail/collab"; then
  echo "coop exec did not default to --session collab"
  echo "got: $exec_out"
  exit 1
fi
echo "coop exec ok (default session=collab)"

# --- coop exec --session with existing .amqrc ---
ISODIR="$(mktemp -d)"
iso_cleanup() {
  rm -rf "$ISODIR"
}
trap 'cleanup; amqrc_cleanup; exec_cleanup; iso_cleanup' EXIT

# Set up literal root + .amqrc
ISODEFAULT="$ISODIR/agent-mail"
"$BIN" init --root "$ISODEFAULT" --agents claude,codex
printf '{"root": "agent-mail"}\n' > "$ISODIR/.amqrc"

# Now exec with --session feature-x — should create isolated session under base
iso_out="$(cd "$ISODIR" && "$BIN" coop exec --session feature-x --no-wake -y bash -- -c 'echo $AM_ROOT:$AM_ME' 2>/dev/null)"
if ! printf '%s' "$iso_out" | grep -q "feature-x"; then
  echo "coop exec --session did not use isolated root"
  echo "got: $iso_out"
  exit 1
fi
echo "coop exec --session isolation ok"

# --- Python hook session-name tests ---
if command -v python3 >/dev/null 2>&1; then
  # Clear env to avoid interference with session detection tests
  unset AM_BASE_ROOT 2>/dev/null || true
  python3 scripts/test_session_name.py
  echo "python session-name tests ok"
fi

# --- SessionStart hook test (claude-session-start.sh) ---
HOOK_TMPDIR="$(mktemp -d)"
hook_tmpdir_cleanup() {
  rm -rf "$HOOK_TMPDIR"
}
trap 'cleanup; amqrc_cleanup; exec_cleanup; iso_cleanup; hook_tmpdir_cleanup' EXIT

# Fixture: 1 peer (codex, active presence) + 2 unread messages for claude
HOOK_ROOT="$HOOK_TMPDIR/agent-mail/collab"
"$BIN" init --root "$HOOK_ROOT" --agents claude,codex
"$BIN" presence set --root "$HOOK_ROOT" --me codex --status active
"$BIN" send --root "$HOOK_ROOT" --me codex --to claude --body "msg one" >/dev/null 2>&1
"$BIN" send --root "$HOOK_ROOT" --me codex --to claude --body "msg two" >/dev/null 2>&1

# Write .amqrc so amq env can resolve project/session
printf '{"root": "agent-mail"}\n' > "$HOOK_TMPDIR/.amqrc"

HOOK_ENV_FILE="$HOOK_TMPDIR/claude-env"

HOOK_OUTPUT=$(
  CLAUDE_ENV_FILE="$HOOK_ENV_FILE" \
  CLAUDE_PROJECT_DIR="$HOOK_TMPDIR" \
  AM_ROOT="$HOOK_ROOT" \
  AM_ME="claude" \
  PATH="$(dirname "$BIN"):$PATH" \
  bash scripts/claude-session-start.sh 2>/dev/null
)

# Phase 1: env file should contain AM_ROOT export
grep -q '^export AM_ROOT=' "$HOOK_ENV_FILE"
echo "  hook phase 1: AM_ROOT in env file ok"

# Phase 2: stdout should be valid JSON with required fields
if command -v python3 >/dev/null 2>&1; then
  python3 - "$HOOK_OUTPUT" <<'PYEOF'
import json, sys

data = json.loads(sys.argv[1])
hso = data["hookSpecificOutput"]
assert hso["hookEventName"] == "SessionStart", f"hookEventName={hso.get('hookEventName')}"
ctx = hso["additionalContext"]
assert ctx, "additionalContext is empty"
assert "me=claude" in ctx, f"missing me=claude in: {ctx}"
assert "codex" in ctx, f"missing peer codex in: {ctx}"
sm = data["systemMessage"]
assert "peer" in sm.lower(), f"systemMessage missing peer mention: {sm}"
assert "2 unread" in sm, f"systemMessage missing unread count: {sm}"
print("  hook phase 2: JSON assertions passed")
PYEOF
else
  echo "  hook phase 2: python3 not available, skipping JSON assertions"
fi

# Test /clear scenario: env file already has AM_ROOT, but AM_ROOT not in parent env.
# Phase 1 must read RESOLVED_ROOT from existing env file for phase 2 to work.
printf "export AM_ROOT='%s'\n" "$HOOK_ROOT" > "$HOOK_ENV_FILE"

HOOK_OUTPUT2=$(
  CLAUDE_ENV_FILE="$HOOK_ENV_FILE" \
  CLAUDE_PROJECT_DIR="$HOOK_TMPDIR" \
  AM_ME="claude" \
  PATH="$(dirname "$BIN"):$PATH" \
  bash scripts/claude-session-start.sh 2>/dev/null
)

if command -v python3 >/dev/null 2>&1; then
  python3 - "$HOOK_OUTPUT2" <<'PYEOF'
import json, sys

data = json.loads(sys.argv[1])
hso = data["hookSpecificOutput"]
assert hso["hookEventName"] == "SessionStart", f"hookEventName={hso.get('hookEventName')}"
ctx = hso["additionalContext"]
assert ctx, "additionalContext is empty"
assert "me=claude" in ctx, f"missing me=claude in: {ctx}"
print("  hook /clear scenario: JSON assertions passed")
PYEOF
fi

echo "claude-session-start.sh hook test ok"

echo "smoke test ok"
