#!/usr/bin/env bash
set -euo pipefail

unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_COMMON_DIR \
  GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_NAMESPACE 2>/dev/null || true

ROOT="$(git rev-parse --show-toplevel)"
TMPDIR="$(mktemp -d)"
cleanup() {
  rm -rf "$TMPDIR"
}
trap cleanup EXIT

new_victim_repo() {
  local repo="$1"
  mkdir -p "$repo"
  git -C "$repo" init -q -b main
  git -C "$repo" config user.email test@example.com
  git -C "$repo" config user.name "Git Env Test"
  printf 'victim\n' >"$repo/README.md"
  git -C "$repo" add README.md
  git -C "$repo" commit -qm "victim base"
}

assert_victim_unchanged() {
  local repo="$1"
  local before_sha="$2"
  local label="$3"
  local after_sha
  after_sha="$(git -C "$repo" rev-parse HEAD)"
  if [[ "$after_sha" != "$before_sha" ]]; then
    echo "$label changed victim HEAD: $before_sha -> $after_sha"
    exit 1
  fi
  if git -C "$repo" show-ref --verify --quiet refs/heads/docs-change; then
    echo "$label created docs-change in victim repo"
    exit 1
  fi
  if git -C "$repo" show-ref --verify --quiet refs/heads/release/v9.9.9; then
    echo "$label created release/v9.9.9 in victim repo"
    exit 1
  fi
  if [[ -n "$(git -C "$repo" status --porcelain)" ]]; then
    echo "$label dirtied victim repo"
    git -C "$repo" status --short
    exit 1
  fi
}

release_victim="$TMPDIR/release-victim"
new_victim_repo "$release_victim"
release_before="$(git -C "$release_victim" rev-parse HEAD)"
GIT_DIR="$release_victim/.git" bash "$ROOT/scripts/test_release_metadata.sh" >/dev/null
assert_victim_unchanged "$release_victim" "$release_before" "test_release_metadata.sh"

changelog_victim="$TMPDIR/changelog-victim"
new_victim_repo "$changelog_victim"
changelog_before="$(git -C "$changelog_victim" rev-parse HEAD)"
GIT_DIR="$changelog_victim/.git" python3 "$ROOT/scripts/test_check_pr_changelog.py" >/dev/null
assert_victim_unchanged "$changelog_victim" "$changelog_before" "test_check_pr_changelog.py"

printf 'git env sanitization tests ok\n'
