#!/usr/bin/env bash

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/publish-skild-skills.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

run_case() {
  local name="$1"
  local scenario="$2"
  local publish_mode="$3"
  local expect="$4"
  local alias_from="${5:-}"
  local expected_curl_calls="${6:-}"
  local case_dir="$TEST_ROOT/$name"
  local status=0

  mkdir -p "$case_dir/bin" "$case_dir/home/.skild" "$case_dir/skills/amq-spec"
  printf '%s\n' '---' 'name: amq-spec' 'description: test' '---' >"$case_dir/skills/amq-spec/SKILL.md"
  printf '%s\n' '{"registryUrl":"https://registry.test","token":"test-secret"}' >"$case_dir/home/.skild/registry-auth.json"
  cp "$FAKE_NPX" "$case_dir/bin/npx"
  cp "$FAKE_CURL" "$case_dir/bin/curl"
  chmod +x "$case_dir/bin/npx" "$case_dir/bin/curl"

  if env \
    HOME="$case_dir/home" \
    PATH="$case_dir/bin:$PATH" \
    SKILD_SKILLS_DIR="$case_dir/skills" \
    FAKE_SCENARIO="$scenario" \
    FAKE_PUBLISH_MODE="$publish_mode" \
    FAKE_STATE_DIR="$case_dir/state" \
    "$SCRIPT" 0.57.0 "$alias_from" >"$case_dir/stdout" 2>"$case_dir/stderr"; then
    status=0
  else
    status=$?
  fi

  if [[ "$expect" == success && "$status" -ne 0 ]]; then
    sed -n '1,160p' "$case_dir/stderr" >&2
    fail "$name returned $status, expected success"
  fi
  if [[ "$expect" == failure && "$status" -eq 0 ]]; then
    fail "$name succeeded, expected failure"
  fi
  if grep -Fq 'test-secret' "$case_dir/stdout" "$case_dir/stderr"; then
    fail "$name leaked the auth token"
  fi
  if grep -Fq -- '--alias' "$case_dir/state/npx.log"; then
    fail "$name passed an alias to skild publish"
  fi
  [[ "$(wc -l <"$case_dir/state/npx.log" | tr -d ' ')" == "1" ]] ||
    fail "$name did not call skild publish exactly once"
  if [[ -n "$expected_curl_calls" ]]; then
    [[ "$(wc -l <"$case_dir/state/curl.log" | tr -d ' ')" == "$expected_curl_calls" ]] ||
      fail "$name made an unexpected number of registry calls"
  fi
}

TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/amq-skild-test.XXXXXX")"
trap 'rm -rf "$TEST_ROOT"' EXIT
FAKE_NPX="$TEST_ROOT/fake-npx"
FAKE_CURL="$TEST_ROOT/fake-curl"

cat >"$FAKE_NPX" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$FAKE_STATE_DIR"
printf '%q ' "$@" >>"$FAKE_STATE_DIR/npx.log"
printf '\n' >>"$FAKE_STATE_DIR/npx.log"
case "$FAKE_PUBLISH_MODE" in
  fresh) echo "published" ;;
  exists)
    echo "Publish failed (409)"
    echo '{"ok":false,"error":"Version already exists."}'
    exit 1
    ;;
  *) echo "unexpected publish failure" >&2; exit 1 ;;
esac
FAKE

cat >"$FAKE_CURL" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$FAKE_STATE_DIR"
method=GET
output=""
data=""
url=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --request) method="$2"; shift 2 ;;
    --output) output="$2"; shift 2 ;;
    --data) data="$2"; shift 2 ;;
    --header|--max-time|--write-out) shift 2 ;;
    --silent|--show-error) shift ;;
    *) url="$1"; shift ;;
  esac
done
printf '%s\t%s\t%s\n' "$method" "$url" "$data" >>"$FAKE_STATE_DIR/curl.log"
count="$(wc -l <"$FAKE_STATE_DIR/curl.log" | tr -d ' ')"
status=200
body='{}'

case "$FAKE_SCENARIO:$count" in
  fresh:1|exists:1|wrong-alias:1|wrong-conflict:1|target-alias:1|malformed:1|migration:1|rollback:1)
    body='{"ok":true,"name":"@avivsinai/amq-spec","version":"0.57.0","integrity":"sha256","tarballUrl":"/tarball"}'
    ;;
  network:1)
    exit 7
    ;;
  not-found:1)
    status=404
    body='{"ok":false,"error":"Skill not found."}'
    ;;
  fresh:2|exists:2)
    body='{"ok":true,"name":"@avivsinai/amq-spec","alias":"amq-spec"}'
    ;;
  fresh:3|exists:3|migration:7)
    body='{"ok":true,"alias":"amq-spec","type":"registry","spec":"@avivsinai/amq-spec"}'
    ;;
  wrong-alias:2|target-alias:2|migration:2|rollback:2)
    status=409
    body='{"ok":false,"error":"Alias already in use."}'
    ;;
  wrong-conflict:2)
    status=409
    body='{"ok":false,"error":"Different conflict."}'
    ;;
  malformed:2)
    body='{"ok":true,"name":42,"alias":"amq-spec"}'
    ;;
  target-alias:3|migration:3|rollback:3)
    body='{"ok":true,"alias":"amq-spec","type":"registry","spec":"@avivsinai/spec"}'
    ;;
  migration:4|rollback:4)
    body='{"ok":true,"skill":{"name":"@avivsinai/amq-spec","alias":null}}'
    ;;
  target-alias:4)
    body='{"ok":true,"skill":{"name":"@avivsinai/amq-spec","alias":"other-alias"}}'
    ;;
  migration:5|rollback:5)
    body='{"ok":true,"name":"@avivsinai/spec","alias":null}'
    ;;
  migration:6)
    body='{"ok":true,"name":"@avivsinai/amq-spec","alias":"amq-spec"}'
    ;;
  rollback:6)
    status=500
    body='{"ok":false,"error":"temporary failure"}'
    ;;
  rollback:7)
    body='{"ok":true,"name":"@avivsinai/spec","alias":"amq-spec"}'
    ;;
  *)
    status=500
    body='{"ok":false,"error":"unexpected request"}'
    ;;
esac

printf '%s' "$body" >"$output"
printf '%s' "$status"
FAKE

run_case fresh fresh fresh success "" 3
run_case version_exists exists exists success "" 3
run_case wrong_alias_409 wrong-alias exists failure "" 2
run_case wrong_conflict_409 wrong-conflict exists failure spec 2
run_case target_has_other_alias target-alias exists failure spec 4
run_case not_found not-found fresh failure "" 1
run_case malformed malformed fresh failure "" 2
run_case network network fresh failure "" 1
run_case migration migration exists success spec 7
run_case rollback rollback exists failure spec 7

assert_curl_line() {
  local case_name="$1"
  local line_number="$2"
  local expected="$3"
  local actual

  actual="$(sed -n "${line_number}p" "$TEST_ROOT/$case_name/state/curl.log")"
  [[ "$actual" == "$expected" ]] ||
    fail "$case_name registry call $line_number was '$actual', want '$expected'"
}

assert_curl_line fresh 1 $'GET\thttps://registry.test/skills/avivsinai/amq-spec/versions/0.57.0\t'
assert_curl_line fresh 2 $'POST\thttps://registry.test/publisher/skills/avivsinai/amq-spec/alias\t{"alias":"amq-spec"}'
assert_curl_line fresh 3 $'GET\thttps://registry.test/resolve?alias=amq-spec\t'

assert_curl_line migration 3 $'GET\thttps://registry.test/resolve?alias=amq-spec\t'
assert_curl_line migration 4 $'GET\thttps://registry.test/skills/avivsinai/amq-spec\t'
assert_curl_line migration 5 $'POST\thttps://registry.test/publisher/skills/avivsinai/spec/alias\t{"alias":null}'
assert_curl_line migration 6 $'POST\thttps://registry.test/publisher/skills/avivsinai/amq-spec/alias\t{"alias":"amq-spec"}'
assert_curl_line migration 7 $'GET\thttps://registry.test/resolve?alias=amq-spec\t'

grep -Fq $'POST\thttps://registry.test/publisher/skills/avivsinai/spec/alias\t{"alias":"amq-spec"}' \
  "$TEST_ROOT/rollback/state/curl.log" || fail "rollback did not attempt to restore the old alias"
if grep -Fq '/publisher/skills/avivsinai/spec/alias' "$TEST_ROOT/target_has_other_alias/state/curl.log"; then
  fail "migration cleared the source before proving the target alias was empty"
fi
if grep -Fq '/publisher/skills/avivsinai/spec/alias' "$TEST_ROOT/wrong_conflict_409/state/curl.log"; then
  fail "a non-alias 409 triggered migration"
fi

RELEASE_WORKFLOW="$REPO_ROOT/.github/workflows/release.yml"
MANUAL_WORKFLOW="$REPO_ROOT/.github/workflows/publish-skill.yml"
RELEASE_COMMAND="run: ./scripts/publish-skild-skills.sh \"\$TAG_VERSION\""
WORKFLOW_SHA_REF="ref: \${{ github.workflow_sha }}"
MANUAL_COMMAND="run: ./.skild-publisher/scripts/publish-skild-skills.sh \"\$TAG_VERSION\" \"\$ALIAS_FROM_SKILL\""

grep -Fq "$RELEASE_COMMAND" "$RELEASE_WORKFLOW" ||
  fail "release workflow does not use the hardened publisher"
grep -Fq 'alias-from-skill:' "$MANUAL_WORKFLOW" ||
  fail "manual workflow does not expose explicit alias migration"
grep -Fq "$WORKFLOW_SHA_REF" "$MANUAL_WORKFLOW" ||
  fail "manual workflow does not check out current publisher tooling"
grep -Fq "$MANUAL_COMMAND" "$MANUAL_WORKFLOW" ||
  fail "manual workflow does not use the hardened publisher"
if grep -Fq 'retrying without alias' "$RELEASE_WORKFLOW" "$MANUAL_WORKFLOW"; then
  fail "a workflow still masks alias conflicts with an aliasless retry"
fi

echo "PASS: Skild publish and alias reconciliation"
