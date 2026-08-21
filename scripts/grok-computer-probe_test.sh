#!/bin/sh
# Run grok-computer-probe.sh in a temp dir. Proves missing tools still
# exit 0, sentinels are removed, and the script never claims live G.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
PROBE=$ROOT/scripts/grok-computer-probe.sh
WORK=$(mktemp -d "${TMPDIR:-/tmp}/grok-computer-probe-test.XXXXXX")
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

[ -f "$PROBE" ] || {
  printf 'FAIL: missing %s\n' "$PROBE" >&2
  exit 1
}

fail() {
  printf '%s\n' "FAIL: $1" >&2
  if [ -n "${OUT:-}" ]; then
    printf '%s\n' "----- probe stdout -----" >&2
    printf '%s\n' "$OUT" >&2
  fi
  exit 1
}

contain() {
  printf '%s\n' "$OUT" | grep -F -q -- "$1" || fail "expected output to contain: $1"
}

absent() {
  printf '%s\n' "$OUT" | grep -E -q -- "$1" && fail "output must not match: $1"
  return 0
}

# Isolated layout: writable, missing, and not-writable candidates.
writable=$WORK/writable
missing=$WORK/missing-dir
readonly=$WORK/readonly
mkdir -p "$writable" "$readonly"
chmod a-w "$readonly"

BIN=$WORK/bin
mkdir -p "$BIN"
cat >"$BIN/uname" <<'EOF'
#!/bin/sh
printf 'Darwin-probe-test\n'
EOF
cat >"$BIN/whoami" <<'EOF'
#!/bin/sh
printf 'probe\n'
EOF
cat >"$BIN/id" <<'EOF'
#!/bin/sh
printf 'uid=0(probe)\n'
EOF
cat >"$BIN/curl" <<'EOF'
#!/bin/sh
printf '200'
EOF
cat >"$BIN/crontab" <<'EOF'
#!/bin/sh
printf '%s\n' '* * * * * echo grok-computer-probe'
EOF
chmod +x "$BIN/uname" "$BIN/whoami" "$BIN/id" "$BIN/curl" "$BIN/crontab"

# date and rm come from /bin. Keep /usr/bin off PATH so the host curl/crontab
# cannot hide the missing-tool path in the second case.
PATH_ISOLATED=$BIN:/bin
export GROK_COMPUTER_PROBE_DIRS="$writable $missing $readonly"

cd "$WORK"

st=0
OUT=$(PATH=$PATH_ISOLATED HOME=$WORK sh "$PROBE") || st=$?
[ "$st" -eq 0 ] || fail "probe exited $st with curl present"

contain "A Mac is not G"
contain "does **not** close bead \`amq-hws\`"
contain "Live G is still required"
contain "does **not** prove the host is G"
contain "Darwin-probe-test"
contain "uid=0(probe)"
contain "$WORK"
contain "| \`curl\` |"
contain "| \`$writable\` | yes | yes |"
contain "| \`$missing\` | no | n/a |"
contain "| \`$readonly\` | yes | no |"
contain "HTTP 200"
contain "* * * * * echo grok-computer-probe"

[ ! -e "$writable/grok-computer-probe.sentinel" ] || fail "sentinel left behind in writable dir"
[ ! -e "$readonly/grok-computer-probe.sentinel" ] || fail "sentinel left behind in readonly dir"

absent "this host is G"
absent "this machine is G"
absent "live G confirmed"
absent "^printenv$"
absent "^export "
absent "AWS_|GITHUB_TOKEN|COOKIE|cookie:|Authorization:"

# Negative: a probe that treated missing curl as a hard failure would fail here.
rm -f "$BIN/curl" "$BIN/crontab"
st=0
OUT=$(PATH=$PATH_ISOLATED HOME=$WORK sh "$PROBE") || st=$?
[ "$st" -eq 0 ] || fail "probe exited $st when tools were missing (must be evidence, not a gate)"
contain "no curl"
contain "Missing tools:"
contain "amq"
contain "curl"
# crontab lives in /usr/bin, so a /bin-only PATH must list it missing rather
# than running the host crontab.
contain "crontab: missing"

printf 'grok-computer-probe tests passed\n'
