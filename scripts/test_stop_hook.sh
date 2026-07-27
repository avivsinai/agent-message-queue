#!/usr/bin/env bash
set -euo pipefail

BIN="${AMQ_TEST_BIN:?AMQ_TEST_BIN is required}"
HOOK="${AMQ_STOP_HOOK:-scripts/amq-stop-hook.sh}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
BASE="$TMP/agent-mail"
ROOT="$BASE/session1"
"$BIN" init --root "$ROOT" --agents claude,codex >/dev/null

send() {
  "$BIN" send --root "$ROOT" --me codex --to claude --body "$1" --json |
    awk -F'"' '/"id":/ {print $4; exit}'
}
run_hook() {
  local active="$1"
  printf '{"stop_hook_active":%s}\n' "$active" |
    env -u AM_ROOT AM_BASE_ROOT="$BASE" AM_SESSION=session1 AM_ME=claude \
      AMQ_NO_UPDATE_CHECK=1 PATH="$(dirname "$BIN"):$PATH" \
      CLAUDE_PROJECT_DIR="$TMP" bash "$HOOK"
}

assert_eq() { # want got label
  if [ "$1" != "$2" ]; then
    printf 'FAIL %s: want %s got %s\n' "$3" "$1" "$2" >&2
    exit 1
  fi
}
assert_absent() { # path label
  if [ -e "$1" ]; then
    printf 'FAIL %s: unexpected path exists: %s\n' "$2" "$1" >&2
    exit 1
  fi
}

assert_eq "" "$(run_hook false)" "ordinary allow output"
id1="$(send one)"
first="$(run_hook false)"
python3 - "$first" "$ROOT" <<'PY'
import json, sys
d=json.loads(sys.argv[1]); assert d["decision"]=="block"
assert sys.argv[2] in d["reason"] and "session1" in d["reason"]
PY
assert_eq "" "$(run_hook true)" "active repeat output"
assert_eq 1 "$("$BIN" list --root "$ROOT" --me claude --new --json | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')" "message remains unread"

for n in 2 3 4 5; do
  send "$n" >/dev/null
  out="$(run_hook true)"
  python3 -c 'import json,sys; assert json.loads(sys.argv[1])["decision"]=="block"' "$out"
  assert_eq "" "$(run_hook true)" "active repeat output after message $n"
done
send six >/dev/null
exhausted="$(run_hook true)"
python3 - "$exhausted" <<'PY'
import json, sys
d=json.loads(sys.argv[1]); assert "decision" not in d
assert "budget" in d["systemMessage"].lower() and "reset" in d["systemMessage"].lower()
PY

# A new, non-active stop chain can block the still-fresh sixth message.
healed="$(run_hook false)"
python3 -c 'import json,sys; assert json.loads(sys.argv[1])["decision"]=="block"' "$healed"
assert_eq "" "$(run_hook true)" "active repeat output after budget reset"

state="$ROOT/agents/claude/.stop-hook-state.json"
mode="$(python3 -c 'import os,sys; print(oct(os.stat(sys.argv[1]).st_mode & 0o777)[2:])' "$state")"
assert_eq 600 "$mode" "state file mode"
assert_absent "$state.tmp" "atomic state temporary file"
"$BIN" read --root "$ROOT" --me claude --id "$id1" >/dev/null
assert_eq "" "$(run_hook true)" "read message output"
python3 - "$state" "$id1" <<'PY'
import json, sys
d=json.load(open(sys.argv[1])); assert sys.argv[2] not in d["blocked_ids"]
PY
echo "stop hook tests ok"
