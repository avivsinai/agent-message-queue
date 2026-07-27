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
SOURCE_REPO="$TEST_TMPDIR/source"
REMOTE_REPO="$TEST_TMPDIR/remote.git"
RUNNER_REPO="$TEST_TMPDIR/runner"
mkdir -p "$FAKE_BIN"

cat >"$FAKE_BIN/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >>"${GH_CALL_LOG:?}"
[[ "$1" == "api" ]]

if [[ "$*" == *"/commits/"*"/pulls?"* ]]; then
  case "${FAKE_PR_MODE:?}" in
    api-failure)
      exit 1
      ;;
    malformed)
      printf 'not json\n'
      ;;
    zero)
      printf '[[]]\n'
      ;;
    exact)
      jq -cn --arg sha "${FAKE_TAG_SHA:?}" \
        '[[{number: 17, merged_at: "2026-07-27", base: {ref: "main"},
            title: "chore(release): v1.2.3", merge_commit_sha: $sha}]]'
      ;;
    multiple)
      jq -cn --arg sha "${FAKE_TAG_SHA:?}" \
        '[[{number: 17, merged_at: "2026-07-27", base: {ref: "main"},
            title: "chore(release): v1.2.3", merge_commit_sha: $sha},
           {number: 18, merged_at: "2026-07-27", base: {ref: "main"},
            title: "chore(release): v1.2.3", merge_commit_sha: $sha}]]'
      ;;
    wrong-base)
      jq -cn --arg sha "${FAKE_TAG_SHA:?}" \
        '[[{number: 17, merged_at: "2026-07-27", base: {ref: "release"},
            title: "chore(release): v1.2.3", merge_commit_sha: $sha}]]'
      ;;
    wrong-title)
      jq -cn --arg sha "${FAKE_TAG_SHA:?}" \
        '[[{number: 17, merged_at: "2026-07-27", base: {ref: "main"},
            title: "release 1.2.3", merge_commit_sha: $sha}]]'
      ;;
    wrong-sha)
      jq -cn \
        '[[{number: 17, merged_at: "2026-07-27", base: {ref: "main"},
            title: "chore(release): v1.2.3",
            merge_commit_sha: "0000000000000000000000000000000000000000"}]]'
      ;;
  esac
  exit
fi

if [[ "$*" == *"/issues/17/labels"* && " $* " == *" --method POST "* ]]; then
  jq -e '.labels == ["autorelease: tagged"]' >/dev/null
  case "${FAKE_POST_BEHAVIOR:-success}" in
    success|apply-fail)
      if ! grep -Fxq 'autorelease: tagged' "${FAKE_LABEL_STATE:?}"; then
        printf 'autorelease: tagged\n' >>"$FAKE_LABEL_STATE"
      fi
      ;;
    fail)
      ;;
  esac
  printf '[]\n'
  [[ "${FAKE_POST_BEHAVIOR:-success}" != "apply-fail" &&
     "${FAKE_POST_BEHAVIOR:-success}" != "fail" ]]
  exit
fi

if [[ "$*" == *"/issues/17/labels/autorelease%3A%20pending"* ]]; then
  case "${FAKE_DELETE_BEHAVIOR:-success}" in
    success|apply-fail|peer-404)
      grep -Fxv 'autorelease: pending' "$FAKE_LABEL_STATE" >"${FAKE_LABEL_STATE}.next" || true
      mv "${FAKE_LABEL_STATE}.next" "$FAKE_LABEL_STATE"
      ;;
    fail)
      ;;
  esac
  [[ "${FAKE_DELETE_BEHAVIOR:-success}" != "apply-fail" &&
     "${FAKE_DELETE_BEHAVIOR:-success}" != "peer-404" &&
     "${FAKE_DELETE_BEHAVIOR:-success}" != "fail" ]]
  exit
fi

if [[ "$*" == *"/issues/17"* ]]; then
  reads="$(cat "${FAKE_LABEL_READS:?}")"
  reads="$((reads + 1))"
  printf '%s\n' "$reads" >"$FAKE_LABEL_READS"
  if [[ "${FAKE_FAIL_LABEL_READ_AT:-0}" -eq "$reads" ]]; then
    exit 1
  fi
  if [[ "${FAKE_MALFORMED_LABEL_READ_AT:-0}" -eq "$reads" ]]; then
    printf 'not json\n'
    exit
  fi
  jq -Rsc '{labels: (split("\n") | map(select(length > 0) | {name: .}))}' \
    <"${FAKE_LABEL_STATE:?}"
  if [[ "${FAKE_NONZERO_LABEL_READ_AT:-0}" -eq "$reads" ]]; then
    exit 1
  fi
  exit
fi

echo "unexpected gh invocation: $*" >&2
exit 1
EOF
chmod +x "$FAKE_BIN/gh"

git init -q -b main "$SOURCE_REPO"
git -C "$SOURCE_REPO" config user.email test@example.com
git -C "$SOURCE_REPO" config user.name "Release Reconcile Test"
printf '{".":"1.2.2"}\n' >"$SOURCE_REPO/.release-please-manifest.json"
git -C "$SOURCE_REPO" add .release-please-manifest.json
git -C "$SOURCE_REPO" commit -qm "initial"
OLD_SHA="$(git -C "$SOURCE_REPO" rev-parse HEAD)"
printf '{".":"1.2.3"}\n' >"$SOURCE_REPO/.release-please-manifest.json"
git -C "$SOURCE_REPO" add .release-please-manifest.json
git -C "$SOURCE_REPO" commit -qm "chore(release): v1.2.3"
TAG_SHA="$(git -C "$SOURCE_REPO" rev-parse HEAD)"
git -C "$SOURCE_REPO" tag v1.2.3
printf 'after\n' >"$SOURCE_REPO/after"
git -C "$SOURCE_REPO" add after
git -C "$SOURCE_REPO" commit -qm "after release"
git init -q --bare "$REMOTE_REPO"
git -C "$SOURCE_REPO" remote add origin "$REMOTE_REPO"
git -C "$SOURCE_REPO" push -q -u origin main --tags
git clone -q --depth=1 --branch main "file://${REMOTE_REPO}" "$RUNNER_REPO"
MAIN_SHA="$(git -C "$RUNNER_REPO" rev-parse HEAD)"

# actions/checkout defaults to a grafted depth-1 HEAD. Merely fetching the old
# tag cannot prove ancestry; fetch-depth: 0 must restore the connecting chain.
[[ "$(git -C "$RUNNER_REPO" rev-parse --is-shallow-repository)" == "true" ]]
git -C "$RUNNER_REPO" fetch -q --no-tags origin \
  refs/tags/v1.2.3:refs/tags/v1.2.3
if git -C "$RUNNER_REPO" merge-base --is-ancestor "$TAG_SHA" "$MAIN_SHA"; then
  echo "depth-1 checkout unexpectedly proved old tag ancestry" >&2
  exit 1
fi
git -C "$RUNNER_REPO" fetch -q --unshallow origin
[[ "$(git -C "$RUNNER_REPO" rev-parse --is-shallow-repository)" == "false" ]]
git -C "$RUNNER_REPO" merge-base --is-ancestor "$TAG_SHA" "$MAIN_SHA"

LABEL_STATE="$TEST_TMPDIR/labels"
CALL_LOG="$TEST_TMPDIR/calls"
LABEL_READS="$TEST_TMPDIR/label-reads"

reset_case() {
  : >"$CALL_LOG"
  printf '0\n' >"$LABEL_READS"
  unset FAKE_FAIL_LABEL_READ_AT
  unset FAKE_MALFORMED_LABEL_READ_AT
  unset FAKE_NONZERO_LABEL_READ_AT
  unset FAKE_POST_BEHAVIOR
  unset FAKE_DELETE_BEHAVIOR
}

set_labels() {
  : >"$LABEL_STATE"
  for label in "$@"; do
    printf '%s\n' "$label" >>"$LABEL_STATE"
  done
}

run_reconcile() {
  local mode="$1"
  (
    cd "$RUNNER_REPO"
    PATH="$FAKE_BIN:$PATH" \
      GH_CALL_LOG="$CALL_LOG" \
      FAKE_PR_MODE="$mode" \
      FAKE_TAG_SHA="$TAG_SHA" \
      FAKE_LABEL_STATE="$LABEL_STATE" \
      FAKE_LABEL_READS="$LABEL_READS" \
      FAKE_POST_BEHAVIOR="${FAKE_POST_BEHAVIOR:-success}" \
      FAKE_DELETE_BEHAVIOR="${FAKE_DELETE_BEHAVIOR:-success}" \
      FAKE_FAIL_LABEL_READ_AT="${FAKE_FAIL_LABEL_READ_AT:-0}" \
      FAKE_MALFORMED_LABEL_READ_AT="${FAKE_MALFORMED_LABEL_READ_AT:-0}" \
      FAKE_NONZERO_LABEL_READ_AT="${FAKE_NONZERO_LABEL_READ_AT:-0}" \
      GITHUB_REPOSITORY="example/amq" \
      RELEASE_VERSION="${RUN_VERSION:-1.2.3}" \
      EXPECTED_MAIN_SHA="${RUN_MAIN_SHA:-$MAIN_SHA}" \
      "$ROOT/scripts/reconcile-release-please-labels.sh"
  )
}

# Hostile version/output payloads and stale main bindings fail before any API.
for RUN_VERSION in $'1.2.3\npending=false' '1.2.3+build' '01.2.3'; do
  reset_case
  set_labels 'autorelease: pending'
  if run_reconcile exact; then
    echo "hostile reconcile version unexpectedly accepted" >&2
    exit 1
  fi
  [[ ! -s "$CALL_LOG" ]]
done
unset RUN_VERSION
RUN_MAIN_SHA="$OLD_SHA"
reset_case
if run_reconcile exact; then
  echo "stale bound main unexpectedly accepted" >&2
  exit 1
fi
[[ ! -s "$CALL_LOG" ]]
unset RUN_MAIN_SHA

# 1. Canonical pending-only state adds tagged, confirms it, then removes pending.
reset_case
set_labels 'autorelease: pending'
run_reconcile exact
[[ "$(cat "$LABEL_STATE")" == "autorelease: tagged" ]]
post_line="$(grep -n -- '--method POST' "$CALL_LOG" | cut -d: -f1)"
delete_line="$(grep -n -- '--method DELETE' "$CALL_LOG" | cut -d: -f1)"
[[ "$post_line" -lt "$delete_line" ]]

# 2. Failure after add and before delete leaves both; retry converges.
reset_case
set_labels 'autorelease: pending'
export FAKE_FAIL_LABEL_READ_AT=2
if run_reconcile exact; then
  echo "post-add confirmation failure unexpectedly succeeded" >&2
  exit 1
fi
grep -Fxq 'autorelease: pending' "$LABEL_STATE"
grep -Fxq 'autorelease: tagged' "$LABEL_STATE"
if grep -q -- '--method DELETE' "$CALL_LOG"; then
  echo "pending label was removed before tagged confirmation" >&2
  exit 1
fi
reset_case
run_reconcile exact
[[ "$(cat "$LABEL_STATE")" == "autorelease: tagged" ]]

# 3. A peer may remove pending before our DELETE returns 404. The authoritative
# final read proves tagged-only convergence, so the run succeeds.
reset_case
set_labels 'autorelease: pending' 'autorelease: tagged'
export FAKE_DELETE_BEHAVIOR=peer-404
run_reconcile exact
[[ "$(cat "$LABEL_STATE")" == "autorelease: tagged" ]]

# 4. The server may apply POST or DELETE while the client loses the response.
# Authoritative rereads prove the mutations took effect.
reset_case
set_labels 'autorelease: pending'
export FAKE_POST_BEHAVIOR=apply-fail
run_reconcile exact
[[ "$(cat "$LABEL_STATE")" == "autorelease: tagged" ]]

reset_case
set_labels 'autorelease: pending' 'autorelease: tagged'
export FAKE_DELETE_BEHAVIOR=apply-fail
run_reconcile exact
[[ "$(cat "$LABEL_STATE")" == "autorelease: tagged" ]]

# 5. Nonzero mutations remain fail-closed when reread is unreadable or does
# not prove the expected state.
reset_case
set_labels 'autorelease: pending'
export FAKE_POST_BEHAVIOR=fail
if run_reconcile exact; then
  echo "failed POST with nonconverged reread unexpectedly succeeded" >&2
  exit 1
fi
if grep -q -- '--method DELETE' "$CALL_LOG"; then
  echo "pending label was removed without tagged convergence" >&2
  exit 1
fi

reset_case
set_labels 'autorelease: pending' 'autorelease: tagged'
export FAKE_DELETE_BEHAVIOR=fail
if run_reconcile exact; then
  echo "failed DELETE with nonconverged reread unexpectedly succeeded" >&2
  exit 1
fi

reset_case
set_labels 'autorelease: pending' 'autorelease: tagged'
export FAKE_DELETE_BEHAVIOR=apply-fail
export FAKE_FAIL_LABEL_READ_AT=2
if run_reconcile exact; then
  echo "failed DELETE with unreadable reread unexpectedly succeeded" >&2
  exit 1
fi

reset_case
set_labels 'autorelease: pending'
export FAKE_POST_BEHAVIOR=apply-fail
export FAKE_NONZERO_LABEL_READ_AT=2
if run_reconcile exact; then
  echo "nonzero tagged reread with valid-looking JSON unexpectedly succeeded" >&2
  exit 1
fi
if grep -q -- '--method DELETE' "$CALL_LOG"; then
  echo "pending label was removed after a failed authoritative tagged reread" >&2
  exit 1
fi

# 6. Tagged-only is already converged and performs zero mutation.
reset_case
set_labels 'autorelease: tagged'
run_reconcile exact
if grep -Eq -- '--method (POST|DELETE)' "$CALL_LOG"; then
  echo "already-converged labels were mutated" >&2
  exit 1
fi

# 7. Both labels removes pending only.
reset_case
set_labels 'autorelease: pending' 'autorelease: tagged'
run_reconcile exact
[[ "$(cat "$LABEL_STATE")" == "autorelease: tagged" ]]
if grep -q -- '--method POST' "$CALL_LOG"; then
  echo "existing tagged label was redundantly added" >&2
  exit 1
fi
grep -q -- '--method DELETE' "$CALL_LOG"

# Malformed initial label state fails before either label mutation.
reset_case
set_labels 'autorelease: pending'
export FAKE_MALFORMED_LABEL_READ_AT=1
if run_reconcile exact; then
  echo "malformed labels response unexpectedly accepted" >&2
  exit 1
fi
if grep -Eq -- '--method (POST|DELETE)' "$CALL_LOG"; then
  echo "malformed labels response caused mutation" >&2
  exit 1
fi

# Duplicate lifecycle labels fail before either label mutation.
reset_case
set_labels 'autorelease: pending' 'autorelease: tagged' 'autorelease: tagged'
if run_reconcile exact; then
  echo "duplicate tagged labels unexpectedly accepted" >&2
  exit 1
fi
if grep -Eq -- '--method (POST|DELETE)' "$CALL_LOG"; then
  echo "duplicate tagged labels caused mutation" >&2
  exit 1
fi

# 8. Missing or mismatched tag identity fails before label API calls.
reset_case
set_labels 'autorelease: pending'
git -C "$RUNNER_REPO" tag -d v1.2.3 >/dev/null
saved_origin="$(git -C "$RUNNER_REPO" remote get-url origin)"
empty_remote="$TEST_TMPDIR/empty.git"
git init -q --bare "$empty_remote"
git -C "$RUNNER_REPO" remote set-url origin "$empty_remote"
if run_reconcile exact; then
  echo "missing tag unexpectedly accepted" >&2
  exit 1
fi
if grep -q 'issues/' "$CALL_LOG"; then
  echo "missing tag reached label API" >&2
  exit 1
fi
git -C "$RUNNER_REPO" remote set-url origin "$saved_origin"
git -C "$RUNNER_REPO" fetch -q --no-tags origin \
  refs/tags/v1.2.3:refs/tags/v1.2.3

git -C "$SOURCE_REPO" tag -f v1.2.3 "$OLD_SHA" >/dev/null
git -C "$SOURCE_REPO" push -q --force origin refs/tags/v1.2.3
git -C "$RUNNER_REPO" tag -d v1.2.3 >/dev/null
if run_reconcile exact; then
  echo "tag manifest mismatch unexpectedly accepted" >&2
  exit 1
fi
if grep -q 'issues/' "$CALL_LOG"; then
  echo "tag manifest mismatch reached label API" >&2
  exit 1
fi

git -C "$SOURCE_REPO" switch -q --orphan side
printf '{".":"1.2.3"}\n' >"$SOURCE_REPO/.release-please-manifest.json"
git -C "$SOURCE_REPO" add .release-please-manifest.json
git -C "$SOURCE_REPO" commit -qm "side"
SIDE_SHA="$(git -C "$SOURCE_REPO" rev-parse HEAD)"
git -C "$SOURCE_REPO" tag -f v1.2.3 "$SIDE_SHA" >/dev/null
git -C "$SOURCE_REPO" push -q --force origin refs/tags/v1.2.3
git -C "$RUNNER_REPO" tag -d v1.2.3 >/dev/null
if run_reconcile exact; then
  echo "non-ancestor tag unexpectedly accepted" >&2
  exit 1
fi
if grep -q 'issues/' "$CALL_LOG"; then
  echo "non-ancestor tag reached label API" >&2
  exit 1
fi
git -C "$SOURCE_REPO" tag -f v1.2.3 "$TAG_SHA" >/dev/null
git -C "$SOURCE_REPO" push -q --force origin refs/tags/v1.2.3
git -C "$RUNNER_REPO" tag -d v1.2.3 >/dev/null

# 9-10. Ambiguous, mismatched, malformed, and failing PR APIs fail closed.
for mode in zero multiple wrong-base wrong-title wrong-sha malformed api-failure; do
  reset_case
  set_labels 'autorelease: pending'
  if run_reconcile "$mode"; then
    echo "$mode PR response unexpectedly accepted" >&2
    exit 1
  fi
  if grep -q 'issues/' "$CALL_LOG"; then
    echo "$mode PR response reached label API" >&2
    exit 1
  fi
  [[ "$(cat "$LABEL_STATE")" == "autorelease: pending" ]]
done

printf 'release-please reconciliation tests ok\n'
