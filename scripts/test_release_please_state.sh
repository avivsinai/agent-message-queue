#!/usr/bin/env bash
set -euo pipefail

unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_COMMON_DIR \
  GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_NAMESPACE 2>/dev/null || true

ROOT="$(git rev-parse --show-toplevel)"
TEST_TMPDIR="$(mktemp -d)"
cleanup() {
  rm -rf "$TEST_TMPDIR"
}
trap cleanup EXIT

FAKE_BIN="$TEST_TMPDIR/bin"
STATE_REPO="$TEST_TMPDIR/state-repo"
mkdir -p "$FAKE_BIN" "$STATE_REPO"

cat >"$FAKE_BIN/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >>"${GH_CALL_LOG:?}"
[[ "$1" == "api" ]]
[[ " $* " == *" --paginate "* ]]
[[ " $* " == *" --slurp "* ]]

if [[ "${FAKE_RELEASE_MODE:?}" == "api-failure" ]]; then
  exit 1
fi

python3 - "${FAKE_RELEASE_MODE:?}" "${FAKE_RELEASE_TAG:-}" <<'PY'
import json
import sys

mode, tag = sys.argv[1:]
if mode == "invalid-json":
    print("not json")
elif mode == "exact":
    print(json.dumps([[{"draft": False, "published_at": "2026-07-27", "tag_name": tag}]]))
elif mode == "draft":
    print(json.dumps([[{"draft": True, "published_at": None, "tag_name": tag}]]))
elif mode == "similar":
    print(json.dumps([[{"draft": False, "published_at": "2026-07-27", "tag_name": tag + "0"}]]))
else:
    print(json.dumps([[]]))
PY
EOF
chmod +x "$FAKE_BIN/gh"

git -C "$STATE_REPO" init -q -b main
git -C "$STATE_REPO" config user.email test@example.com
git -C "$STATE_REPO" config user.name "Release State Test"
printf 'state\n' >"$STATE_REPO/state.txt"
git -C "$STATE_REPO" add state.txt
git -C "$STATE_REPO" commit -qm "state"
STATE_SHA="$(git -C "$STATE_REPO" rev-parse HEAD)"

write_manifest() {
  local version="$1"
  python3 - "$version" >"$STATE_REPO/.release-please-manifest.json" <<'PY'
import json
import sys

json.dump({".": sys.argv[1]}, sys.stdout)
sys.stdout.write("\n")
PY
}

run_state_check() {
  local mode="$1"
  local tag="$2"
  local output="$TEST_TMPDIR/state-output"
  : >"$output"
  (
    cd "$STATE_REPO"
    PATH="$FAKE_BIN:$PATH" \
      FAKE_RELEASE_MODE="$mode" \
      FAKE_RELEASE_TAG="$tag" \
      GH_CALL_LOG="$TEST_TMPDIR/gh-calls" \
      GITHUB_OUTPUT="$output" \
      GITHUB_REPOSITORY="example/amq" \
      "$ROOT/scripts/check-release-please-state.sh"
  )
}

write_manifest "1.2.3"
: >"$TEST_TMPDIR/gh-calls"
run_state_check similar "v1.2.3"
grep -Fx "version=1.2.3" "$TEST_TMPDIR/state-output" >/dev/null
grep -Fx "released=false" "$TEST_TMPDIR/state-output" >/dev/null
grep -Fx "main_sha=$STATE_SHA" "$TEST_TMPDIR/state-output" >/dev/null

write_manifest "1.2.3-rc.1"
run_state_check exact "v1.2.3-rc.1"
grep -Fx "released=true" "$TEST_TMPDIR/state-output" >/dev/null

write_manifest "1.2.3"
run_state_check draft "v1.2.3"
grep -Fx "released=false" "$TEST_TMPDIR/state-output" >/dev/null

write_manifest "1.2.3"
: >"$TEST_TMPDIR/gh-calls"
if run_state_check invalid-json "v1.2.3"; then
  echo "invalid releases JSON unexpectedly accepted" >&2
  exit 1
fi
[[ ! -s "$TEST_TMPDIR/state-output" ]]

: >"$TEST_TMPDIR/gh-calls"
if run_state_check api-failure "v1.2.3"; then
  echo "releases API failure unexpectedly accepted" >&2
  exit 1
fi
[[ ! -s "$TEST_TMPDIR/state-output" ]]

for hostile_version in \
  $'1.2.3\nreleased=true' \
  '1.2.3.*' \
  '01.2.3' \
  '1.2.3+build'; do
  write_manifest "$hostile_version"
  : >"$TEST_TMPDIR/gh-calls"
  if run_state_check exact "v1.2.3"; then
    printf 'hostile version unexpectedly accepted: %q\n' "$hostile_version" >&2
    exit 1
  fi
  [[ ! -s "$TEST_TMPDIR/state-output" ]]
  [[ ! -s "$TEST_TMPDIR/gh-calls" ]]
done

SOURCE_REPO="$TEST_TMPDIR/source"
REMOTE_REPO="$TEST_TMPDIR/remote.git"
RUNNER_REPO="$TEST_TMPDIR/runner"
git init -q -b main "$SOURCE_REPO"
git -C "$SOURCE_REPO" config user.email test@example.com
git -C "$SOURCE_REPO" config user.name "Release State Test"
printf 'one\n' >"$SOURCE_REPO/value"
git -C "$SOURCE_REPO" add value
git -C "$SOURCE_REPO" commit -qm "one"
git init -q --bare "$REMOTE_REPO"
git -C "$SOURCE_REPO" remote add origin "$REMOTE_REPO"
git -C "$SOURCE_REPO" push -q -u origin main
git clone -q "$REMOTE_REPO" "$RUNNER_REPO"

BOUND_SHA="$(git -C "$RUNNER_REPO" rev-parse HEAD)"
CURRENT_OUTPUT="$TEST_TMPDIR/current-output"
(
  cd "$RUNNER_REPO"
  EXPECTED_MAIN_SHA="$BOUND_SHA" \
    GITHUB_OUTPUT="$CURRENT_OUTPUT" \
    "$ROOT/scripts/revalidate-release-please-main.sh"
)
grep -Fx "current=true" "$CURRENT_OUTPUT" >/dev/null

printf 'two\n' >>"$SOURCE_REPO/value"
git -C "$SOURCE_REPO" add value
git -C "$SOURCE_REPO" commit -qm "two"
git -C "$SOURCE_REPO" push -q origin main
: >"$CURRENT_OUTPUT"
(
  cd "$RUNNER_REPO"
  EXPECTED_MAIN_SHA="$BOUND_SHA" \
    GITHUB_OUTPUT="$CURRENT_OUTPUT" \
    "$ROOT/scripts/revalidate-release-please-main.sh"
)
grep -Fx "current=false" "$CURRENT_OUTPUT" >/dev/null

: >"$CURRENT_OUTPUT"
if (
  cd "$RUNNER_REPO"
  EXPECTED_MAIN_SHA=$'bad\ncurrent=true' \
    GITHUB_OUTPUT="$CURRENT_OUTPUT" \
    "$ROOT/scripts/revalidate-release-please-main.sh"
); then
  echo "hostile expected main SHA unexpectedly accepted" >&2
  exit 1
fi
[[ ! -s "$CURRENT_OUTPUT" ]]

printf 'release-please state tests ok\n'
