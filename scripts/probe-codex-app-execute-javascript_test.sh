#!/bin/sh
# Static and fake-command tests for the non-live task-0 evidence harness.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
PROBE=$ROOT/scripts/probe-codex-app-execute-javascript.sh
WORK=$(mktemp -d "${TMPDIR:-/tmp}/codex-app-task0-test.XXXXXX")
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

[ -x "$PROBE" ] || fail "probe is not executable"
sh -n "$PROBE" || fail "probe has invalid shell syntax"
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck "$PROBE" || fail "probe failed shellcheck"
fi

for forbidden in 'System Events' 'keystroke' 'the clipboard' 'AXRaise' 'activate'; do
  if grep -F -q -- "$forbidden" "$PROBE"; then
    fail "probe contains forbidden automation API: $forbidden"
  fi
done
grep -F -q -- 'execute javascript' "$PROBE" || fail 'probe does not call execute javascript'
grep -F -q -- "AMQ_CODEX_APP_EXECUTE_JS_TASK_0" "$PROBE" || fail 'probe marker is not fixed'
if grep -E -q '\$[0-9]|\$@|\$\*' "$PROBE"; then
  fail 'probe accepts message-derived shell arguments'
fi

BIN=$WORK/bin
mkdir -p "$BIN"
cat >"$BIN/uname" <<'EOF'
#!/bin/sh
printf '%s\n' Darwin
EOF
cat >"$BIN/osascript" <<'EOF'
#!/bin/sh
set -eu
[ "${1:-}" = '-e' ] || exit 2
printf '%s' "${2:-}" >"${CODEX_APP_TASK0_SCRIPT_CAPTURE:?}"
printf '%s\n' 'AMQ_CODEX_APP_EXECUTE_JS_TASK_0'
EOF
chmod 0755 "$BIN/uname" "$BIN/osascript"

CODEX_APP_TASK0_SCRIPT_CAPTURE=$WORK/apple-script.txt \
  PATH="$BIN:/bin:/usr/bin" sh "$PROBE" >"$WORK/pass.out"
grep -F -q -- 'task-0 probe: PASS' "$WORK/pass.out" || fail 'fake successful probe did not pass'
grep -F -q -- 'execute javascript' "$WORK/apple-script.txt" || fail 'fixed AppleScript was not passed to osascript'
grep -F -q -- "AMQ_CODEX_APP_EXECUTE_JS_TASK_0" "$WORK/apple-script.txt" || fail 'fixed JavaScript marker was not passed'

cat >"$BIN/osascript" <<'EOF'
#!/bin/sh
printf '%s\n' 'unexpected result'
EOF
chmod 0755 "$BIN/osascript"
status=0
PATH="$BIN:/bin:/usr/bin" sh "$PROBE" >"$WORK/fail.out" 2>"$WORK/fail.err" || status=$?
[ "$status" -ne 0 ] || fail 'wrong fixed result was accepted'

printf '%s\n' 'Codex app task-0 probe tests passed'
