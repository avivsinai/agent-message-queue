#!/usr/bin/env bash
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
TEST_TMPDIR="$(mktemp -d)"
cleanup() {
  rm -rf "$TEST_TMPDIR"
}
trap cleanup EXIT

MARK_SCRIPT="$TEST_TMPDIR/mark-release-pr.sh"
python3 - "$ROOT/.github/workflows/release.yml" "$MARK_SCRIPT" <<'PY'
import pathlib
import sys

workflow = pathlib.Path(sys.argv[1]).read_text()
step = workflow.split("      - name: Mark release pull request as tagged\n", 1)[1]
body = step.split("        run: |\n", 1)[1].split("\n  skill-publish:\n", 1)[0]
lines = []
for line in body.splitlines():
    if line and not line.startswith("          "):
        raise SystemExit(f"unexpected workflow indentation: {line!r}")
    lines.append(line[10:] if line else "")
pathlib.Path(sys.argv[2]).write_text(
    "#!/usr/bin/env bash\n" + "\n".join(lines) + "\n"
)
PY
chmod +x "$MARK_SCRIPT"

FAKE_BIN="$TEST_TMPDIR/bin"
mkdir -p "$FAKE_BIN"
cat >"$FAKE_BIN/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >>"${GH_CALL_LOG:?}"
[[ "$1" == "api" ]]

if [[ "$*" == *"/commits/"*"/pulls?"* ]]; then
  case "${FAKE_PR_MODE:?}" in
    zero)
      printf '[[]]\n'
      ;;
    exact)
      jq -cn --arg sha "${RELEASE_SHA:?}" \
        '[[{number: 23, merged_at: "2026-07-27", base: {ref: "main"},
            title: "chore(release): v1.2.3", merge_commit_sha: $sha}]]'
      ;;
    multiple)
      jq -cn --arg sha "${RELEASE_SHA:?}" \
        '[[{number: 23, merged_at: "2026-07-27", base: {ref: "main"},
            title: "chore(release): v1.2.3", merge_commit_sha: $sha},
           {number: 24, merged_at: "2026-07-27", base: {ref: "main"},
            title: "chore(release): v1.2.3", merge_commit_sha: $sha}]]'
      ;;
    wrong)
      jq -cn --arg sha "${RELEASE_SHA:?}" \
        '[[{number: 23, merged_at: "2026-07-27", base: {ref: "other"},
            title: "chore(release): v1.2.3", merge_commit_sha: $sha}]]'
      ;;
  esac
  exit
fi

if [[ "$*" == *"/issues/23/labels"* && " $* " == *" --method POST "* ]]; then
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

if [[ "$*" == *"/issues/23/labels/autorelease%3A%20pending"* ]]; then
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

if [[ "$*" == *"/issues/23"* ]]; then
  reads="$(cat "${FAKE_LABEL_READS:?}")"
  reads="$((reads + 1))"
  printf '%s\n' "$reads" >"$FAKE_LABEL_READS"
  if [[ "${FAKE_FAIL_LABEL_READ_AT:-0}" -eq "$reads" ]]; then
    exit 1
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

CALL_LOG="$TEST_TMPDIR/calls"
LABEL_STATE="$TEST_TMPDIR/labels"
LABEL_READS="$TEST_TMPDIR/label-reads"
RELEASE_SHA="1111111111111111111111111111111111111111"

run_mark() {
  local mode="$1"
  local event="$2"
  PATH="$FAKE_BIN:$PATH" \
    GH_CALL_LOG="$CALL_LOG" \
    FAKE_PR_MODE="$mode" \
    FAKE_LABEL_STATE="$LABEL_STATE" \
    FAKE_LABEL_READS="$LABEL_READS" \
    FAKE_POST_BEHAVIOR="${FAKE_POST_BEHAVIOR:-success}" \
    FAKE_DELETE_BEHAVIOR="${FAKE_DELETE_BEHAVIOR:-success}" \
    FAKE_FAIL_LABEL_READ_AT="${FAKE_FAIL_LABEL_READ_AT:-0}" \
    FAKE_NONZERO_LABEL_READ_AT="${FAKE_NONZERO_LABEL_READ_AT:-0}" \
    GITHUB_REPOSITORY="example/amq" \
    RELEASE_SHA="$RELEASE_SHA" \
    VERSION="1.2.3" \
    EVENT_NAME="$event" \
    "$MARK_SCRIPT"
}

reset_case() {
  : >"$CALL_LOG"
  printf '0\n' >"$LABEL_READS"
  unset FAKE_POST_BEHAVIOR
  unset FAKE_DELETE_BEHAVIOR
  unset FAKE_FAIL_LABEL_READ_AT
  unset FAKE_NONZERO_LABEL_READ_AT
}

# Historical manual re-release with no associated PR is a successful no-op.
reset_case
: >"$LABEL_STATE"
run_mark zero workflow_dispatch
if grep -q '/issues/' "$CALL_LOG"; then
  echo "historical zero-PR dispatch mutated labels" >&2
  exit 1
fi

# Canonical push still requires a bound PR.
reset_case
if run_mark zero push; then
  echo "canonical release without a PR unexpectedly succeeded" >&2
  exit 1
fi
if grep -q '/issues/' "$CALL_LOG"; then
  echo "canonical zero-PR release reached label API" >&2
  exit 1
fi

# One exact canonical PR converges in safe label order.
reset_case
printf 'autorelease: pending\n' >"$LABEL_STATE"
run_mark exact push
[[ "$(cat "$LABEL_STATE")" == "autorelease: tagged" ]]
post_line="$(grep -n -- '--method POST' "$CALL_LOG" | cut -d: -f1)"
delete_line="$(grep -n -- '--method DELETE' "$CALL_LOG" | cut -d: -f1)"
[[ "$post_line" -lt "$delete_line" ]]

# Duplicate tagged state must fail before DELETE; exact tagged convergence is
# a precondition for removing pending, not merely a final-state check.
reset_case
printf 'autorelease: pending\nautorelease: tagged\nautorelease: tagged\n' >"$LABEL_STATE"
if run_mark exact push; then
  echo "duplicate inline tagged labels unexpectedly accepted" >&2
  exit 1
fi
if grep -Eq -- '--method (POST|DELETE)' "$CALL_LOG"; then
  echo "duplicate inline tagged labels caused mutation" >&2
  exit 1
fi

# A peer DELETE/404 and lost successful POST/DELETE responses stay successful
# when the authoritative reread proves tagged-only convergence. This is the
# inline release step's success result that keeps skill-publish eligible.
reset_case
printf 'autorelease: pending\nautorelease: tagged\n' >"$LABEL_STATE"
export FAKE_DELETE_BEHAVIOR=peer-404
run_mark exact push
[[ "$(cat "$LABEL_STATE")" == "autorelease: tagged" ]]

reset_case
printf 'autorelease: pending\n' >"$LABEL_STATE"
export FAKE_POST_BEHAVIOR=apply-fail
run_mark exact push
[[ "$(cat "$LABEL_STATE")" == "autorelease: tagged" ]]

reset_case
printf 'autorelease: pending\nautorelease: tagged\n' >"$LABEL_STATE"
export FAKE_DELETE_BEHAVIOR=apply-fail
run_mark exact push
[[ "$(cat "$LABEL_STATE")" == "autorelease: tagged" ]]

# Nonzero mutations still fail closed without readable, exact convergence.
reset_case
printf 'autorelease: pending\n' >"$LABEL_STATE"
export FAKE_POST_BEHAVIOR=fail
if run_mark exact push; then
  echo "failed inline POST with nonconverged reread unexpectedly succeeded" >&2
  exit 1
fi

reset_case
printf 'autorelease: pending\nautorelease: tagged\n' >"$LABEL_STATE"
export FAKE_DELETE_BEHAVIOR=fail
if run_mark exact push; then
  echo "failed inline DELETE with nonconverged reread unexpectedly succeeded" >&2
  exit 1
fi

reset_case
printf 'autorelease: pending\nautorelease: tagged\n' >"$LABEL_STATE"
export FAKE_DELETE_BEHAVIOR=apply-fail
export FAKE_FAIL_LABEL_READ_AT=2
if run_mark exact push; then
  echo "failed inline DELETE with unreadable reread unexpectedly succeeded" >&2
  exit 1
fi

reset_case
printf 'autorelease: pending\n' >"$LABEL_STATE"
export FAKE_POST_BEHAVIOR=apply-fail
export FAKE_NONZERO_LABEL_READ_AT=2
if run_mark exact push; then
  echo "nonzero inline tagged reread with valid-looking JSON unexpectedly succeeded" >&2
  exit 1
fi
if grep -q -- '--method DELETE' "$CALL_LOG"; then
  echo "inline pending label was removed after a failed authoritative tagged reread" >&2
  exit 1
fi

# Associated ambiguity or a non-exact sole PR fails closed, even on dispatch.
for mode in multiple wrong; do
  reset_case
  printf 'autorelease: pending\n' >"$LABEL_STATE"
  if run_mark "$mode" workflow_dispatch; then
    echo "$mode dispatch binding unexpectedly succeeded" >&2
    exit 1
  fi
  if grep -q '/issues/' "$CALL_LOG"; then
    echo "$mode dispatch binding reached label API" >&2
    exit 1
  fi
  [[ "$(cat "$LABEL_STATE")" == "autorelease: pending" ]]
done

printf 'release workflow label tests ok\n'
