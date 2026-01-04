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

echo "smoke test ok"
