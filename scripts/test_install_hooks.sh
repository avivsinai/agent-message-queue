#!/bin/sh
# agent-message-queue-7hr: install-hooks.sh used to truncate an existing
# pre-push hook. A foreign hook must stay, and --force may replace it.
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/amq-install-hooks.XXXXXX")"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

REPO="$WORK/repo"
mkdir -p "$REPO"
git -C "$REPO" init -q
HOOK="$REPO/.git/hooks/pre-push"
printf 'foreign hook\n' > "$HOOK"

set +e
err="$(cd "$REPO" && sh "$ROOT/scripts/install-hooks.sh" 2>&1)"
code=$?
set -e
named="$(git -C "$REPO" rev-parse --git-dir)/hooks/pre-push"
if [ "$code" -eq 0 ] || ! printf '%s\n' "$err" | grep -F -q "$named"; then
  echo "FAIL: foreign hook was not refused (exit=$code)"
  printf '%s\n' "$err"
  exit 1
fi
if [ "$(cat "$HOOK")" != "foreign hook" ]; then
  echo "FAIL: foreign hook was modified"
  exit 1
fi

(cd "$REPO" && sh "$ROOT/scripts/install-hooks.sh" --force >/dev/null)
if ! grep -q "Running pre-push checks" "$HOOK"; then
  echo "FAIL: --force did not install the repository hook"
  exit 1
fi

(cd "$REPO" && sh "$ROOT/scripts/install-hooks.sh" >/dev/null)
if ! grep -q "Running pre-push checks" "$HOOK"; then
  echo "FAIL: reinstalling the repository hook changed it"
  exit 1
fi

echo "install-hooks keeps a foreign pre-push hook"
