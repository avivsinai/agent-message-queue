#!/usr/bin/env bash
# Regression tests for the fixed Bot-chat submit wrapper.
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
wrapper="$script_dir/amq-bridge-bot-enqueue.sh"
chmod +x "$wrapper"

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/amq-bridge-bot-enqueue-test.XXXXXX")
tmp_dir=$(CDPATH= cd -- "$tmp_dir" && pwd)
trap 'rm -rf "$tmp_dir"' EXIT

amq_bridge_bin="$tmp_dir/amq-bridge"
(cd "$repo_root" && go build -o "$amq_bridge_bin" ./cmd/amq-bridge)
chmod +x "$amq_bridge_bin"

spool_root="$tmp_dir/root"
mkdir -p "$spool_root"
spool_root=$(CDPATH= cd -- "$spool_root" && pwd)
spool_dir="$spool_root/bridge/outbox/codex/new"

config="$tmp_dir/config.json"
cat >"$config" <<EOF
{
  "root": "$spool_root",
  "source_host": "mac",
  "source_handle": "codex",
  "allowed_dest_aliases": ["grok/claude"]
}
EOF
chmod 0600 "$config"

body_marker="do-not-print-this-body-$$"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

make_message() {
  # make_message always runs inside a $(...) subshell, where $$ still reports
  # the parent PID and $RANDOM/a shell counter would replay the same
  # inherited state on every call. mktemp is a real external command, so its
  # output is unique per invocation regardless of subshell state.
  id_file=$(mktemp "$tmp_dir/msgid.XXXXXXXX")
  message_id="msg-$(basename "$id_file")"
  rm -f "$id_file"
  cat <<EOF
---json
{
  "schema": 1,
  "id": "$message_id",
  "from": "codex",
  "to": ["claude"],
  "thread": "p2p/claude__codex",
  "created": "2026-08-20T00:00:00Z"
}
---
$body_marker
EOF
}

# Refusal cases run with the real built amq-bridge reachable on PATH, so a
# wrapper bug that let a bad call through would be observable as a written
# spool file rather than masked by a "binary not found" error.
run_refused() {
  local desc=$1
  shift
  local out status
  out=$(AMQ_BRIDGE_ENQUEUE_CONFIG="${ENQUEUE_CONFIG-$config}" PATH="$tmp_dir:$PATH" \
    "$wrapper" "$@" <<<"$(make_message)" 2>&1) && status=0 || status=$?
  if [[ "$status" -eq 0 ]]; then
    fail "$desc: expected non-zero exit, got success (output: $out)"
  fi
  if [[ "$out" == *"$body_marker"* ]]; then
    fail "$desc: refusal printed the message body"
  fi
}

before_count=0
count_spool() {
  if [[ -d "$spool_dir" ]]; then
    find "$spool_dir" -type f | wc -l | tr -d ' '
  else
    printf '0\n'
  fi
}

env -u AMQ_BRIDGE_ENQUEUE_CONFIG PATH="$tmp_dir:$PATH" "$wrapper" --dest-alias grok/claude \
  <<<"$(make_message)" >"$tmp_dir/unset-config.out" 2>&1 && status=0 || status=$?
[[ "$status" -ne 0 ]] || fail "unset AMQ_BRIDGE_ENQUEUE_CONFIG: expected refusal"
grep -Fq "$body_marker" "$tmp_dir/unset-config.out" && fail "unset config refusal printed the message body"

ENQUEUE_CONFIG="$tmp_dir/does-not-exist.json" run_refused "missing config file" --dest-alias grok/claude

symlink_config="$tmp_dir/symlink-config.json"
ln -s "$config" "$symlink_config"
ENQUEUE_CONFIG="$symlink_config" run_refused "symlink config" --dest-alias grok/claude

loose_config="$tmp_dir/loose-config.json"
cp "$config" "$loose_config"
chmod 0644 "$loose_config"
loose_out=$(AMQ_BRIDGE_ENQUEUE_CONFIG="$loose_config" PATH="$tmp_dir:$PATH" \
  "$wrapper" --dest-alias grok/claude <<<"$(make_message)" 2>&1) && status=0 || status=$?
[[ "$status" -ne 0 ]] || fail "mode-0644 config: expected refusal"
if [[ "$loose_out" == *"$body_marker"* ]]; then
  fail "mode-0644 config: refusal printed the message body"
fi
# Named check: reject with a numeric mode, not a GNU `stat -f` filesystem dump.
printf '%s\n' "$loose_out" | grep -Fq 'AMQ_BRIDGE_ENQUEUE_CONFIG mode is 644, want 600' \
  || fail "mode-0644 config: expected numeric mode error, got: $loose_out"
printf 'ok: mode-0644 config rejected\n'

run_refused "extra positional argument" --dest-alias grok/claude extra
run_refused "wrong flag name" --to grok/claude
run_refused "smuggled --root as extra arg" --dest-alias grok/claude --root "$tmp_dir/evil-root"
run_refused "smuggled --rendezvous as extra arg" --rendezvous https://evil.example --dest-alias grok/claude
run_refused "smuggled --me as extra arg" --dest-alias grok/claude --me codex
run_refused "smuggled --spool as extra arg" --dest-alias grok/claude --spool "$tmp_dir/evil-spool"
run_refused "smuggled -root as extra arg" --dest-alias grok/claude -root "$tmp_dir/evil-root"
run_refused "dest-alias value is --root" --dest-alias --root
run_refused "dest-alias value is --rendezvous" --dest-alias --rendezvous
run_refused "dest-alias missing value" --dest-alias
run_refused "no arguments"
run_refused "dest-alias without slash" --dest-alias grokclaude
run_refused "dest-alias with leading dash host" --dest-alias -root/claude

if [[ "$(count_spool)" -ne "$before_count" ]]; then
  fail "a refused call wrote a spool file"
fi

# Named check: a real mode-600 config must pass (GNU `stat -c '%a'`, not a
# BSD-first `stat -f` filesystem dump). PATH resolution: amq-bridge via PATH.
out=$(AMQ_BRIDGE_ENQUEUE_CONFIG="$config" PATH="$tmp_dir:$PATH" \
  "$wrapper" --dest-alias grok/claude <<<"$(make_message)" 2>&1) && status=0 || status=$?
if [[ "$status" -ne 0 ]]; then
  fail "mode-600 config: expected accept, got exit $status (output: $out)"
fi
[[ "$out" == "$spool_dir"/*.md ]] || fail "PATH resolution: unexpected output: $out"
[[ -f "$out" ]] || fail "PATH resolution: spool file missing: $out"
grep -Fq "$body_marker" "$out" || fail "PATH resolution: spool file missing body"
printf 'ok: mode-600 config accepted\n'
dest_sidecar="${out%.md}.dest"
[[ -f "$dest_sidecar" ]] || fail "PATH resolution: missing .dest sidecar"
[[ "$(cat "$dest_sidecar")" == "grok/claude" ]] || fail "PATH resolution: wrong .dest sidecar content"

# Sibling-binary resolution: no amq-bridge on PATH, wrapper and binary share a directory.
sibling_dir="$tmp_dir/sibling"
mkdir -p "$sibling_dir"
cp "$wrapper" "$sibling_dir/amq-bridge-bot-enqueue.sh"
cp "$amq_bridge_bin" "$sibling_dir/amq-bridge"
chmod +x "$sibling_dir/amq-bridge-bot-enqueue.sh" "$sibling_dir/amq-bridge"
out=$(AMQ_BRIDGE_ENQUEUE_CONFIG="$config" PATH="/usr/bin:/bin" \
  "$sibling_dir/amq-bridge-bot-enqueue.sh" --dest-alias grok/claude <<<"$(make_message)")
[[ -f "$out" ]] || fail "sibling resolution: spool file missing: $out"

# AM_ROOT (and the rest of AM_* session env) must not override the config root.
other_root="$tmp_dir/am-root-decoy"
mkdir -p "$other_root"
out=$(AMQ_BRIDGE_ENQUEUE_CONFIG="$config" PATH="$tmp_dir:$PATH" \
  AM_ROOT="$other_root" AM_ME=someone-else AM_BASE_ROOT="$other_root" \
  "$wrapper" --dest-alias grok/claude <<<"$(make_message)")
[[ "$out" == "$spool_dir"/*.md ]] || fail "AM_ROOT override: message landed outside the configured root: $out"
if [[ -d "$other_root/bridge" ]]; then
  fail "AM_ROOT override: wrapper wrote into AM_ROOT instead of the config root"
fi

printf 'amq-bridge-bot-enqueue regression tests passed\n'
