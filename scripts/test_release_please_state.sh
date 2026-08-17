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
[[ "$2" == "--include" ]]
[[ "$3" == "repos/example/amq/releases/tags/${FAKE_RELEASE_TAG:?}" ]]

python3 - "${FAKE_RELEASE_MODE:?}" "${FAKE_RELEASE_TAG:-}" <<'PY'
import json
import sys

mode, tag = sys.argv[1:]
status = "200 OK"
body = {"draft": False, "published_at": "2026-07-27", "tag_name": tag}
if mode == "not-found":
    status = "404 Not Found"
    body = {"message": "Not Found"}
elif mode == "server-error":
    status = "500 Internal Server Error"
    body = {"message": "server error"}
elif mode == "invalid-json":
    body = "not json"
elif mode == "draft":
    body = {"draft": True, "published_at": "2026-07-27", "tag_name": tag}
elif mode == "prerelease":
    body["prerelease"] = True
elif mode == "invalid-fields":
    body["draft"] = "false"

print(f"HTTP/2.0 {status}")
print("Content-Type: application/json; charset=utf-8\r")
print("\r")
print(body if isinstance(body, str) else json.dumps(body))
if status != "200 OK":
    sys.exit(1)
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
run_state_check not-found "v1.2.3"
grep -Fx "version=1.2.3" "$TEST_TMPDIR/state-output" >/dev/null
grep -Fx "released=false" "$TEST_TMPDIR/state-output" >/dev/null
grep -Fx "main_sha=$STATE_SHA" "$TEST_TMPDIR/state-output" >/dev/null
grep -Fx "api --include repos/example/amq/releases/tags/v1.2.3" \
  "$TEST_TMPDIR/gh-calls" >/dev/null

write_manifest "1.2.3-rc.1"
run_state_check prerelease "v1.2.3-rc.1"
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
if run_state_check server-error "v1.2.3"; then
  echo "release API server error unexpectedly accepted" >&2
  exit 1
fi
[[ ! -s "$TEST_TMPDIR/state-output" ]]

: >"$TEST_TMPDIR/gh-calls"
if run_state_check invalid-fields "v1.2.3"; then
  echo "invalid release response fields unexpectedly accepted" >&2
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
# Bare repositories inherit the host's default branch name. Bind the remote
# HEAD to the branch this fixture created so clones are deterministic even
# when the host default is not "main".
git -C "$REMOTE_REPO" symbolic-ref HEAD refs/heads/main
[[ "$(git -C "$REMOTE_REPO" symbolic-ref HEAD)" == "refs/heads/main" ]]
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
