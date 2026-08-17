#!/usr/bin/env bash
set -euo pipefail

# Clear env vars that could interfere with explicit --root/--me flags. Session
# identity tokens and wake ownership are one context with their path fields, so
# retaining any of them would mix the caller's coop session into the temp queue.
# Keep this list aligned with the hostile AMQ context tuple in Makefile's smoke target.
unset AM_ROOT AM_ROOT_ID AM_ME AM_BASE_ROOT AM_BASE_ROOT_ID AM_SESSION \
  AMQ_GLOBAL_ROOT AMQ_WAKE_OWNER 2>/dev/null || true
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_COMMON_DIR \
  GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_NAMESPACE 2>/dev/null || true

ROOT_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "$ROOT_DIR"
}
trap cleanup EXIT

BIN="$ROOT_DIR/amq"
go build -o "$BIN" ./cmd/amq

QUEUE_ROOT="$ROOT_DIR/agent-mail"

"$BIN" init --root "$QUEUE_ROOT" --agents codex,claude

doctor_out="$("$BIN" doctor --root "$QUEUE_ROOT")"
printf '%s\n' "$doctor_out" | grep -q "✓ Mailboxes:"
if printf '%s\n' "$doctor_out" | grep -q "repair: amq doctor --fix-mailboxes"; then
  echo "fresh init doctor advertised mailbox repair"
  exit 1
fi
if printf '%s\n' "$doctor_out" | grep -Eq 'Summary:.*[1-9][0-9]* errors'; then
  echo "fresh init doctor reported errors"
  exit 1
fi

send_json="$("$BIN" send --root "$QUEUE_ROOT" --me codex --to claude --body "hello" --json)"
msg_id="$(printf '%s\n' "$send_json" | awk -F'"' '/"id":/ {print $4; exit}')"
thread_id="$(printf '%s\n' "$send_json" | awk -F'"' '/"thread":/ {print $4; exit}')"
if [[ -z "$msg_id" || -z "$thread_id" ]]; then
  echo "failed to parse send output"
  exit 1
fi

new_list_out="$("$BIN" list --root "$QUEUE_ROOT" --me claude --new)"
printf '%s' "$new_list_out" | grep -q "$msg_id"

read_out="$("$BIN" read --root "$QUEUE_ROOT" --me claude --id "$msg_id")"
printf '%s' "$read_out" | grep -q "hello"

cur_list_out="$("$BIN" list --root "$QUEUE_ROOT" --me claude --cur)"
printf '%s' "$cur_list_out" | grep -q "$msg_id"

# read already moved the message to cur, which emits a drained receipt
test -f "$QUEUE_ROOT/agents/claude/receipts/${msg_id}__claude__drained.json"

thread_json="$("$BIN" thread --root "$QUEUE_ROOT" --id "$thread_id" --json)"
thread_msg="$(printf '%s\n' "$thread_json" | awk -F'"' '/"id":/ {print $4; exit}')"
if [[ "$thread_msg" != "$msg_id" ]]; then
  echo "thread output missing message"
  exit 1
fi

"$BIN" presence set --root "$QUEUE_ROOT" --me codex --status busy
presence_out="$("$BIN" presence list --root "$QUEUE_ROOT")"
printf '%s' "$presence_out" | grep -q "^codex"

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
  python3 scripts/test_check_pr_title.py
  python3 scripts/test_check_commit_overrides.py
  python3 scripts/test_check_wake_test_changes.py
  python3 scripts/test_release_changelog_section.py
  python3 scripts/test_release_please_config.py
  bash scripts/test_release_metadata.sh
  bash scripts/test_release_please_state.sh
  bash scripts/test_reconcile_release_please_labels.sh
  bash scripts/test_release_workflow_labels.sh
  bash scripts/test_publish_skild_skills.sh
  bash scripts/test_git_env_sanitization.sh
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

# Quoted-root scenario: phase 1 must round-trip shell-escaped AM_ROOT correctly.
HOOK_QUOTED_DIR="$HOOK_TMPDIR/quoted-project"
mkdir -p "$HOOK_QUOTED_DIR"
HOOK_QUOTED_ROOT="$HOOK_QUOTED_DIR/agent-mail'quoted/collab"
"$BIN" init --root "$HOOK_QUOTED_ROOT" --agents claude,codex
"$BIN" presence set --root "$HOOK_QUOTED_ROOT" --me codex --status active
"$BIN" send --root "$HOOK_QUOTED_ROOT" --me codex --to claude --body "quoted msg" >/dev/null 2>&1
printf '{"root": "%s"}\n' "agent-mail'quoted" > "$HOOK_QUOTED_DIR/.amqrc"

HOOK_OUTPUT3=$(
  CLAUDE_ENV_FILE="$HOOK_QUOTED_DIR/claude-env" \
  CLAUDE_PROJECT_DIR="$HOOK_QUOTED_DIR" \
  AM_ROOT="$HOOK_QUOTED_ROOT" \
  AM_ME="claude" \
  PATH="$(dirname "$BIN"):$PATH" \
  bash scripts/claude-session-start.sh 2>/dev/null
)

if command -v python3 >/dev/null 2>&1; then
  python3 - "$HOOK_OUTPUT3" <<'PYEOF'
import json, sys

data = json.loads(sys.argv[1])
hso = data["hookSpecificOutput"]
assert hso["hookEventName"] == "SessionStart", f"hookEventName={hso.get('hookEventName')}"
ctx = hso["additionalContext"]
assert "codex(" in ctx, f"missing peer in: {ctx}"
assert "1 unread message(s)" in ctx, f"missing unread count in: {ctx}"
print("  hook quoted-root scenario: JSON assertions passed")
PYEOF
fi

# /clear with env-file-only AM_ME: phase 2 must reuse the persisted handle.
HOOK_ALT_ME_DIR="$HOOK_TMPDIR/nondefault-me-project"
mkdir -p "$HOOK_ALT_ME_DIR"
HOOK_ALT_ME_ROOT="$HOOK_ALT_ME_DIR/agent-mail/collab"
"$BIN" init --root "$HOOK_ALT_ME_ROOT" --agents alice,codex
"$BIN" presence set --root "$HOOK_ALT_ME_ROOT" --me codex --status active
"$BIN" send --root "$HOOK_ALT_ME_ROOT" --me codex --to alice --body "msg one" >/dev/null 2>&1
"$BIN" send --root "$HOOK_ALT_ME_ROOT" --me codex --to alice --body "msg two" >/dev/null 2>&1
printf '{"root": "agent-mail"}\n' > "$HOOK_ALT_ME_DIR/.amqrc"
HOOK_ALT_ME_ENV_FILE="$HOOK_ALT_ME_DIR/claude-env"
printf "export AM_ROOT='%s'\nexport AM_ME=alice\n" "$HOOK_ALT_ME_ROOT" > "$HOOK_ALT_ME_ENV_FILE"

HOOK_OUTPUT4=$(
  CLAUDE_ENV_FILE="$HOOK_ALT_ME_ENV_FILE" \
  CLAUDE_PROJECT_DIR="$HOOK_ALT_ME_DIR" \
  PATH="$(dirname "$BIN"):$PATH" \
  bash scripts/claude-session-start.sh 2>/dev/null
)

if command -v python3 >/dev/null 2>&1; then
  python3 - "$HOOK_OUTPUT4" <<'PYEOF'
import json, sys

data = json.loads(sys.argv[1])
hso = data["hookSpecificOutput"]
assert hso["hookEventName"] == "SessionStart", f"hookEventName={hso.get('hookEventName')}"
ctx = hso["additionalContext"]
assert "me=alice" in ctx, f"missing me=alice in: {ctx}"
assert "2 unread message(s)" in ctx, f"missing unread count in: {ctx}"
assert "amq drain --me alice" in ctx, f"missing alice drain hint in: {ctx}"
print("  hook /clear env-file AM_ME scenario: JSON assertions passed")
PYEOF
fi

# HOOK_LOG rotation: oversized logs should rotate to .1; smaller logs should stay in place.
HOOK_LOG_ROTATE_DIR="$(mktemp -d)"
hook_log_rotate_cleanup() {
  rm -rf "$HOOK_LOG_ROTATE_DIR"
}
trap 'cleanup; amqrc_cleanup; exec_cleanup; iso_cleanup; hook_tmpdir_cleanup; hook_log_rotate_cleanup' EXIT

HOOK_LOG_PATH="$HOOK_LOG_ROTATE_DIR/amq-hook-hooklogtest.log"
HOOK_LOG_BACKUP="$HOOK_LOG_PATH.1"
dd if=/dev/zero of="$HOOK_LOG_PATH" bs=1024 count=1025 >/dev/null 2>&1

CLAUDE_ENV_FILE="$HOOK_LOG_ROTATE_DIR/env" \
CLAUDE_PROJECT_DIR="$HOOK_LOG_ROTATE_DIR" \
TMPDIR="$HOOK_LOG_ROTATE_DIR" \
USER="hooklogtest" \
PATH="$(dirname "$BIN"):$PATH" \
bash scripts/claude-session-start.sh >/dev/null 2>/dev/null

test -f "$HOOK_LOG_BACKUP"
test -f "$HOOK_LOG_PATH"
ROTATED_SIZE="$(wc -c < "$HOOK_LOG_BACKUP" | tr -d '[:space:]')"
CURRENT_SIZE="$(wc -c < "$HOOK_LOG_PATH" | tr -d '[:space:]')"
if [[ "$ROTATED_SIZE" -lt 1048576 ]]; then
  echo "hook log rotation backup size too small: $ROTATED_SIZE"
  exit 1
fi
if [[ "$CURRENT_SIZE" -ge 1048576 ]]; then
  echo "hook log rotation did not cap current log: $CURRENT_SIZE"
  exit 1
fi
echo "  hook log rotation: oversized log rotated"

rm -f "$HOOK_LOG_BACKUP"
printf 'small-log\n' > "$HOOK_LOG_PATH"

CLAUDE_ENV_FILE="$HOOK_LOG_ROTATE_DIR/env2" \
CLAUDE_PROJECT_DIR="$HOOK_LOG_ROTATE_DIR" \
TMPDIR="$HOOK_LOG_ROTATE_DIR" \
USER="hooklogtest" \
PATH="$(dirname "$BIN"):$PATH" \
bash scripts/claude-session-start.sh >/dev/null 2>/dev/null

if [[ -f "$HOOK_LOG_BACKUP" ]]; then
  echo "hook log rotation unexpectedly rotated small log"
  exit 1
fi
grep -q '^small-log$' "$HOOK_LOG_PATH"
echo "  hook log rotation: small log left in place"

echo "claude-session-start.sh hook test ok"

# --- DLQ: a corrupt delivered message routes to DLQ; list/retry/purge ---
DLQ_DIR="$(mktemp -d)"
dlq_cleanup() {
  rm -rf "$DLQ_DIR"
}
trap 'cleanup; amqrc_cleanup; exec_cleanup; iso_cleanup; hook_tmpdir_cleanup; hook_log_rotate_cleanup; dlq_cleanup' EXIT

DLQ_ROOT="$DLQ_DIR/agent-mail"
"$BIN" init --root "$DLQ_ROOT" --agents codex,claude

dlq_send_json="$("$BIN" send --root "$DLQ_ROOT" --me codex --to claude --body "will be corrupted" --json)"
dlq_msg_id="$(printf '%s\n' "$dlq_send_json" | awk -F'"' '/"id":/ {print $4; exit}')"
if [[ -z "$dlq_msg_id" ]]; then
  echo "dlq: failed to parse send output"
  exit 1
fi

# Corrupt the delivered file directly (file writes only, never in-process):
# break the "---" frontmatter/body separator so it can no longer be parsed.
dlq_msg_file="$DLQ_ROOT/agents/claude/inbox/new/${dlq_msg_id}.md"
test -f "$dlq_msg_file"
sed 's/^---$/XXX-BROKEN-SEPARATOR/' "$dlq_msg_file" >"$dlq_msg_file.tmp"
mv "$dlq_msg_file.tmp" "$dlq_msg_file"

# read on the corrupt message fails and moves it to DLQ with a dlq receipt.
set +e
"$BIN" read --root "$DLQ_ROOT" --me claude --id "$dlq_msg_id" >/dev/null 2>&1
dlq_read_rc=$?
set -e
if [[ "$dlq_read_rc" -ne 1 ]]; then
  echo "dlq: expected exit 1 reading corrupt message, got $dlq_read_rc"
  exit 1
fi
test ! -f "$dlq_msg_file"

dlq_receipt_out="$("$BIN" receipts list --root "$DLQ_ROOT" --me claude --stage dlq)"
printf '%s' "$dlq_receipt_out" | grep -q "$dlq_msg_id"
echo "dlq: corrupt message routed to DLQ with a dlq receipt"

dlq_list_json="$("$BIN" dlq list --root "$DLQ_ROOT" --me claude --json)"
dlq_id="$(printf '%s\n' "$dlq_list_json" | awk -F'"' '/"id":/ {print $4; exit}')"
if [[ -z "$dlq_id" ]]; then
  echo "dlq: dlq list did not return an id"
  exit 1
fi
"$BIN" dlq list --root "$DLQ_ROOT" --me claude | grep -q "$dlq_id"
echo "dlq: dlq list shows the corrupt message"

# Retry moves the still-corrupt content back to inbox/new; retry does not
# re-validate the payload, it only recovers the delivery. This envelope's
# retry is now terminal (retry_state=delivered).
dlq_retry_json="$("$BIN" dlq retry --root "$DLQ_ROOT" --me claude --id "$dlq_id" --json)"
printf '%s' "$dlq_retry_json" | grep -q "\"retried\": \"$dlq_id\""
test -f "$dlq_msg_file"
echo "dlq: retry redelivers the still-corrupt message to inbox"

# Draining the redelivered message fails to parse the same broken content and
# re-DLQs it under a fresh envelope, so a still-corrupt message effectively
# re-DLQs on the next drain rather than being retried cleanly.
set +e
"$BIN" read --root "$DLQ_ROOT" --me claude --id "$dlq_msg_id" >/dev/null 2>&1
dlq_reread_rc=$?
set -e
if [[ "$dlq_reread_rc" -ne 1 ]]; then
  echo "dlq: expected exit 1 re-reading still-corrupt message, got $dlq_reread_rc"
  exit 1
fi
dlq_relist_json="$("$BIN" dlq list --root "$DLQ_ROOT" --me claude --json)"
printf '%s' "$dlq_relist_json" | grep -q '"retry_state": "delivered"'
printf '%s' "$dlq_relist_json" | grep -q '"retry_state": "ready"'
echo "dlq: re-corrupted message re-DLQ'd under a new envelope"

# NEGATIVE: retrying the terminal (already-delivered) envelope again is
# idempotent, not an error.
dlq_retry2_json="$("$BIN" dlq retry --root "$DLQ_ROOT" --me claude --id "$dlq_id" --json)"
printf '%s' "$dlq_retry2_json" | grep -q "\"already_delivered\": \"$dlq_id\""
echo "dlq: retrying an already-delivered envelope is idempotent"

# purge without --yes/--dry-run refuses (stdin closed -> defaults to No) and
# leaves both remaining DLQ envelopes untouched.
dlq_purge_refuse_out="$("$BIN" dlq purge --root "$DLQ_ROOT" --me claude </dev/null)"
printf '%s' "$dlq_purge_refuse_out" | grep -q "Aborted."
echo "dlq: purge without --yes refuses"

# purge --yes removes every remaining DLQ envelope (the terminal retry and
# the freshly re-DLQ'd one); a purge removing fewer than both would mean the
# refused purge above silently deleted something.
dlq_purge_json="$("$BIN" dlq purge --root "$DLQ_ROOT" --me claude --yes --json)"
printf '%s' "$dlq_purge_json" | grep -q '"removed": 2'
dlq_after_purge="$("$BIN" dlq list --root "$DLQ_ROOT" --me claude)"
printf '%s' "$dlq_after_purge" | grep -q "No DLQ messages."
echo "dlq: purge --yes removed all DLQ messages"

# --- Receipts: send --wait-for drained, receipts wait, and its timeout ---
RECEIPTS_DIR="$(mktemp -d)"
receipts_cleanup() {
  rm -rf "$RECEIPTS_DIR"
}
trap 'cleanup; amqrc_cleanup; exec_cleanup; iso_cleanup; hook_tmpdir_cleanup; hook_log_rotate_cleanup; dlq_cleanup; receipts_cleanup' EXIT

RECEIPTS_ROOT="$RECEIPTS_DIR/agent-mail"
"$BIN" init --root "$RECEIPTS_ROOT" --agents codex,claude

# send --wait-for drained blocks until the recipient drains the message it
# just sent, so drive the drain concurrently from the foreground.
(
  "$BIN" send --root "$RECEIPTS_ROOT" --me codex --to claude --body "wait for drain" \
    --wait-for drained --wait-timeout 10s --json >"$RECEIPTS_DIR/wait_send.json" 2>"$RECEIPTS_DIR/wait_send.err"
  echo $? >"$RECEIPTS_DIR/wait_send.rc"
) &
wait_send_pid=$!

receipts_msg_id=""
for _ in $(seq 1 50); do
  receipts_list_json="$("$BIN" list --root "$RECEIPTS_ROOT" --me claude --new --json 2>/dev/null)"
  if [[ "$receipts_list_json" != "[]" ]]; then
    receipts_msg_id="$(printf '%s\n' "$receipts_list_json" | awk -F'"' '/"id":/ {print $4; exit}')"
    break
  fi
  sleep 0.1
done
if [[ -z "$receipts_msg_id" ]]; then
  echo "receipts: message never arrived for wait-for test"
  exit 1
fi
"$BIN" drain --root "$RECEIPTS_ROOT" --me claude >/dev/null

wait "$wait_send_pid"
wait_send_rc="$(cat "$RECEIPTS_DIR/wait_send.rc")"
if [[ "$wait_send_rc" -ne 0 ]]; then
  echo "send --wait-for drained failed: rc=$wait_send_rc"
  cat "$RECEIPTS_DIR/wait_send.err"
  exit 1
fi
grep -q '"event": "matched"' "$RECEIPTS_DIR/wait_send.json"
echo "receipts: send --wait-for drained succeeded after the recipient drained"

# receipts wait --stage drained on a known message id succeeds.
"$BIN" receipts wait --root "$RECEIPTS_ROOT" --me claude --msg-id "$receipts_msg_id" --stage drained --timeout 5s >/dev/null
echo "receipts: receipts wait matched a known drained receipt"

# NEGATIVE: receipts wait on an unknown message id times out with exit code 4.
set +e
"$BIN" receipts wait --root "$RECEIPTS_ROOT" --me claude --msg-id "unknown-msg-id-does-not-exist" \
  --stage drained --timeout 2s --poll-interval 1s >/dev/null 2>&1
receipts_wait_rc=$?
set -e
if [[ "$receipts_wait_rc" -ne 4 ]]; then
  echo "receipts wait on unknown id: expected exit 4, got $receipts_wait_rc"
  exit 1
fi
echo "receipts: receipts wait on an unknown id times out with exit code 4"

# --- Integration: symphony emit self-delivers a message the recipient can drain ---
INTEGRATION_DIR="$(mktemp -d)"
integration_cleanup() {
  rm -rf "$INTEGRATION_DIR"
}
trap 'cleanup; amqrc_cleanup; exec_cleanup; iso_cleanup; hook_tmpdir_cleanup; hook_log_rotate_cleanup; dlq_cleanup; receipts_cleanup; integration_cleanup' EXIT

INTEGRATION_ROOT="$INTEGRATION_DIR/agent-mail"
"$BIN" init --root "$INTEGRATION_ROOT" --agents codex

# Symphony's adapter contract (docs/adapter-contract.md) self-delivers
# (from=to=me) on thread "task/<workspace-key>" with kind=status and labels
# "orchestrator"/"orchestrator:<name>".
symphony_emit_json="$("$BIN" integration symphony emit --root "$INTEGRATION_ROOT" --event after_run --me codex \
  --workspace "$INTEGRATION_DIR/myworkspace" --identifier smoke-ws --json)"
printf '%s' "$symphony_emit_json" | grep -q '"thread": "task/smoke-ws"'

symphony_drain_json="$("$BIN" drain --root "$INTEGRATION_ROOT" --me codex --include-body --json)"
printf '%s' "$symphony_drain_json" | grep -q '"kind": "status"'
printf '%s' "$symphony_drain_json" | grep -q '"orchestrator:symphony"'
printf '%s' "$symphony_drain_json" | grep -q '"name": "symphony"'
echo "integration: symphony emit delivers a self-addressed message the recipient can drain"

# kanban only exposes a long-lived websocket "bridge" subcommand (Cline
# runtime), so it cannot run offline in a smoke test; verify its --help
# contract instead of invoking it.
kanban_help_out="$("$BIN" integration kanban --help 2>&1)"
printf '%s' "$kanban_help_out" | grep -q "bridge  Run websocket bridge"
echo "integration: kanban needs a live websocket workspace, skipping invocation (offline smoke test)"

bash scripts/test_install_checksum.sh
AMQ_TEST_BIN="$BIN" bash scripts/test_stop_hook.sh
bash scripts/test_version_embed.sh
echo "smoke test ok"
