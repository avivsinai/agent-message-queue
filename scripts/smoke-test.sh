#!/usr/bin/env bash
set -euo pipefail

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

"$BIN" ack --root "$QUEUE_ROOT" --me claude --id "$msg_id"
test -f "$QUEUE_ROOT/agents/claude/acks/sent/${msg_id}.json"
test -f "$QUEUE_ROOT/agents/codex/acks/received/${msg_id}.json"

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

# --- .amqrc root detection (base + default session) ---
AMQRC_DIR="$(mktemp -d)"
amqrc_cleanup() {
  rm -rf "$AMQRC_DIR"
}
trap 'cleanup; amqrc_cleanup' EXIT

# .amqrc points to base; init the queue root at base/team
AMQRC_ROOT="$AMQRC_DIR/custom-root/team"
printf '{"root": "custom-root", "default_session": "team"}\n' > "$AMQRC_DIR/.amqrc"
"$BIN" init --root "$AMQRC_ROOT" --agents alice,bob

# Run list from the .amqrc dir — should resolve to custom-root/team automatically
(cd "$AMQRC_DIR" && AM_ROOT= "$BIN" list --me alice --new >/dev/null 2>&1)
echo ".amqrc detection ok"

# --- coop exec bash (resolves base + default session) ---
EXEC_DIR="$(mktemp -d)"
exec_cleanup() {
  rm -rf "$EXEC_DIR"
}
trap 'cleanup; amqrc_cleanup; exec_cleanup' EXIT

# Init the queue root at agent-mail/team (base + default session)
EXEC_ROOT="$EXEC_DIR/agent-mail/team"
"$BIN" init --root "$EXEC_ROOT" --agents bash
printf '{"root": "agent-mail", "default_session": "team"}\n' > "$EXEC_DIR/.amqrc"

exec_out="$(cd "$EXEC_DIR" && "$BIN" coop exec --no-wake -y bash -- -c 'echo $AM_ROOT:$AM_ME' 2>/dev/null)"
# The output should contain the resolved root (with /team) and handle "bash"
if ! printf '%s' "$exec_out" | grep -q "bash"; then
  echo "coop exec did not set AM_ME=bash"
  echo "got: $exec_out"
  exit 1
fi
if ! printf '%s' "$exec_out" | grep -q "agent-mail/team"; then
  echo "coop exec did not set AM_ROOT containing agent-mail/team"
  echo "got: $exec_out"
  exit 1
fi
echo "coop exec ok"

# --- coop exec --session with existing .amqrc ---
ISODIR="$(mktemp -d)"
iso_cleanup() {
  rm -rf "$ISODIR"
}
trap 'cleanup; amqrc_cleanup; exec_cleanup; iso_cleanup' EXIT

# Set up default root + .amqrc
ISODEFAULT="$ISODIR/agent-mail/team"
"$BIN" init --root "$ISODEFAULT" --agents claude,codex
printf '{"root": "agent-mail", "default_session": "team"}\n' > "$ISODIR/.amqrc"

# Now exec with --session feature-x — should create isolated session under base
iso_out="$(cd "$ISODIR" && "$BIN" coop exec --session feature-x --no-wake -y bash -- -c 'echo $AM_ROOT:$AM_ME' 2>/dev/null)"
if ! printf '%s' "$iso_out" | grep -q "feature-x"; then
  echo "coop exec --session did not use isolated root"
  echo "got: $iso_out"
  exit 1
fi
echo "coop exec --session isolation ok"

echo "smoke test ok"
