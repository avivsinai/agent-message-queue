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
cp "$ROOT/scripts/prepare-release-tag.sh" "$REPO/scripts/"
cp "$ROOT/scripts/create-release-tag.sh" "$REPO/scripts/"
cp "$ROOT/scripts/release_changelog_section.py" "$REPO/scripts/"
chmod +x "$REPO/scripts/resolve-release-metadata.sh" "$REPO/scripts/check-release-version.sh" \
  "$REPO/scripts/release_changelog_section.py"

cat >"$REPO/CHANGELOG.md" <<'EOF'
# Changelog

## [9.9.9](https://example.test/compare/v9.9.8...v9.9.9) (2026-06-22)

### Fixed

- Test release.
EOF

for skill in amq-cli amq-spec; do
  cat >"$REPO/skills/$skill/SKILL.md" <<'EOF'
---
name: test
version: 9.9.9 # x-release-please-version
---

# Test
EOF
done

printf '{".":"9.9.9"}\n' >"$REPO/.release-please-manifest.json"
printf '{"version":"9.9.9"}\n' >"$REPO/.claude-plugin/plugin.json"
printf '{"version":"9.9.9"}\n' >"$REPO/.codex-plugin/plugin.json"

(
  cd "$REPO"
  python3 scripts/release_changelog_section.py 9.9.9 "$TMPDIR/release-notes.md"
)
grep -F "## [9.9.9](https://example.test/compare/v9.9.8...v9.9.9) (2026-06-22)" \
  "$TMPDIR/release-notes.md" >/dev/null

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
      GITHUB_REF=refs/heads/main \
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

# Happy path: an exact merged release PR passes pre-tag preparation, then the
# App-authenticated API request creates that commit's immutable release ref.
git init -q --bare "$TMPDIR/origin.git"
git -C "$REPO" remote add origin "$TMPDIR/origin.git"
git -C "$REPO" push -q origin main
mkdir -p "$TMPDIR/bin"
cat > "$TMPDIR/bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  "api --paginate --slurp repos/example/amq/commits/${TEST_RELEASE_SHA}/pulls?per_page=100")
    test "$GH_TOKEN" = reader-token
    printf '[[{"number":7,"merged_at":"2026-06-22T00:00:00Z","base":{"ref":"main"},"title":"chore(release): v9.9.9","merge_commit_sha":"%s"}]]\n' "$TEST_RELEASE_SHA"
    ;;
  "api --method POST repos/example/amq/git/refs -f ref=refs/tags/v9.9.9 -f sha=${TEST_RELEASE_SHA}")
    test "$GH_TOKEN" = app-token
    printf '%s\n' "$*" > "$TAG_REQUEST_LOG"
    git --git-dir "$TEST_ORIGIN" update-ref refs/tags/v9.9.9 "$TEST_RELEASE_SHA"
    ;;
  *) echo "Unexpected gh request: $*" >&2; exit 1 ;;
esac
GH
chmod +x "$TMPDIR/bin/gh"
export PATH="$TMPDIR/bin:$PATH"
export TEST_RELEASE_SHA="$release_squash" TEST_ORIGIN="$TMPDIR/origin.git"
export TAG_REQUEST_LOG="$TMPDIR/tag-request"
: > "$TMPDIR/prepared"
(
  cd "$REPO"
  EVENT_NAME=push GITHUB_REF=refs/heads/main GITHUB_SHA="$release_squash" \
    COMMIT_MESSAGE="$(git log -1 --format=%B)" GITHUB_REPOSITORY=example/amq \
    GITHUB_OUTPUT="$TMPDIR/prepared" GH_TOKEN=reader-token \
    ./scripts/prepare-release-tag.sh
  TAG=v9.9.9 RELEASE_SHA="$release_squash" GITHUB_REPOSITORY=example/amq \
    GH_TOKEN=app-token ./scripts/create-release-tag.sh
)
grep -Fx 'tag=v9.9.9' "$TMPDIR/prepared"
grep -Fx "release_sha=$release_squash" "$TMPDIR/prepared"
grep -Fx "api --method POST repos/example/amq/git/refs -f ref=refs/tags/v9.9.9 -f sha=$release_squash" "$TAG_REQUEST_LOG"
test "$(git --git-dir "$TEST_ORIGIN" rev-parse 'refs/tags/v9.9.9^{commit}')" = "$release_squash"

# Tag publication resolves the immutable tag rather than the merge subject.
git -C "$REPO" tag v9.9.9 "$release_squash"
: > "$TMPDIR/tag-output"
(
  cd "$REPO"
  EVENT_NAME=push GITHUB_REF=refs/tags/v9.9.9 GITHUB_SHA="$release_squash" \
    GITHUB_OUTPUT="$TMPDIR/tag-output" ./scripts/resolve-release-metadata.sh
)
grep -Fx 'release_ref=v9.9.9' "$TMPDIR/tag-output"
grep -Fx "release_sha=$release_squash" "$TMPDIR/tag-output"
printf 'release metadata resolver tests ok\n'
