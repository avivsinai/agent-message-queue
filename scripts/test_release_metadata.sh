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

REPO="$TMPDIR/repo"
mkdir -p "$REPO/scripts" "$REPO/skills/amq-cli" "$REPO/skills/amq-spec" \
  "$REPO/.claude-plugin" "$REPO/.codex-plugin"

cp "$ROOT/scripts/resolve-release-metadata.sh" "$REPO/scripts/"
cp "$ROOT/scripts/check-release-version.sh" "$REPO/scripts/"
chmod +x "$REPO/scripts/resolve-release-metadata.sh" "$REPO/scripts/check-release-version.sh"

cat >"$REPO/CHANGELOG.md" <<'EOF'
# Changelog

## [Unreleased]

## [9.9.9] - 2026-06-22

### Fixed

- Test release.
EOF

for skill in amq-cli amq-spec; do
  cat >"$REPO/skills/$skill/SKILL.md" <<'EOF'
---
name: test
version: 9.9.9
---

# Test
EOF
done

printf '{"version":"9.9.9"}\n' >"$REPO/.claude-plugin/plugin.json"
printf '{"version":"9.9.9"}\n' >"$REPO/.codex-plugin/plugin.json"

git -C "$REPO" init -q -b main
git -C "$REPO" config user.email test@example.com
git -C "$REPO" config user.name "Release Test"
git -C "$REPO" add .
git -C "$REPO" commit -qm "base"

git -C "$REPO" checkout -qb release/v9.9.9
git -C "$REPO" commit --allow-empty -qm "chore(release): v9.9.9"
release_commit="$(git -C "$REPO" rev-parse HEAD)"

git -C "$REPO" checkout -q main
git -C "$REPO" merge --no-ff -m $'Merge pull request #1 from test/release/v9.9.9\n\nchore(release): v9.9.9' release/v9.9.9
release_merge="$(git -C "$REPO" rev-parse HEAD)"

git -C "$REPO" checkout -qb docs-change
git -C "$REPO" commit --allow-empty -qm "docs: cleanup"
git -C "$REPO" checkout -q main
git -C "$REPO" merge --no-ff -m $'Merge pull request #2 from test/docs-change\n\nchore(release): v9.9.9' docs-change
feature_merge="$(git -C "$REPO" rev-parse HEAD)"

git -C "$REPO" commit --allow-empty -qm "chore(release): v9.9.9 (#3)"
release_squash="$(git -C "$REPO" rev-parse HEAD)"

run_resolver() {
  local sha="$1"
  local output="$TMPDIR/output"
  local stdout="$TMPDIR/stdout"
  rm -f "$output" "$stdout"
  (
    cd "$REPO"
    EVENT_NAME=push \
      INPUT_TAG= \
      COMMIT_MESSAGE="$(git log -1 --format=%B "$sha")" \
      GITHUB_SHA="$sha" \
      GITHUB_OUTPUT="$output" \
      ./scripts/resolve-release-metadata.sh
  ) >"$stdout"
}

assert_release_output() {
  local sha="$1"
  local output="$TMPDIR/output"
  grep -Fx "tag=v9.9.9" "$output"
  grep -Fx "version=9.9.9" "$output"
  grep -Fx "release_ref=$sha" "$output"
  grep -Fx "release_sha=$sha" "$output"
  grep -Fx "prerelease=false" "$output"
}

run_resolver "$release_squash"
assert_release_output "$release_squash" >/dev/null

run_resolver "$release_merge"
assert_release_output "$release_merge" >/dev/null

run_resolver "$feature_merge"
if [[ -s "$TMPDIR/output" ]]; then
  echo "feature merge unexpectedly produced release outputs"
  cat "$TMPDIR/output"
  exit 1
fi
grep -F "No release commit found" "$TMPDIR/stdout" >/dev/null

printf 'release metadata resolver tests ok\n'
