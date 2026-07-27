#!/usr/bin/env bash
set -euo pipefail

github_output="${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"
github_repository="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
manifest_path="${RELEASE_PLEASE_MANIFEST:-.release-please-manifest.json}"

RELEASE_VERSION_PATTERN='(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?'

version="$(
  jq -er \
    --arg pattern "\\A${RELEASE_VERSION_PATTERN}\\z" \
    '."." | select(type == "string" and test($pattern))' \
    "$manifest_path"
)" || {
  echo "::error::Release manifest version must be one canonical X.Y.Z or X.Y.Z-prerelease line."
  exit 1
}
if [[ ! "$version" =~ ^${RELEASE_VERSION_PATTERN}$ ]]; then
  echo "::error::Release manifest version must be one canonical X.Y.Z or X.Y.Z-prerelease line."
  exit 1
fi

main_sha="$(git rev-parse --verify 'HEAD^{commit}')"
if [[ ! "$main_sha" =~ ^[0-9a-f]{40}([0-9a-f]{24})?$ ]]; then
  echo "::error::Checked-out main commit has an invalid object ID."
  exit 1
fi

tag="v${version}"
release_pages="$(
  gh api --paginate --slurp \
    "repos/${github_repository}/releases?per_page=100"
)"
released=false
if jq -e --arg tag "$tag" \
  'any(.[][]; .draft == false and .published_at != null and
    (.tag_name | type == "string") and .tag_name == $tag)' \
  >/dev/null <<<"$release_pages"; then
  released=true
else
  jq_status=$?
  if [[ "$jq_status" != "1" ]]; then
    echo "::error::GitHub releases response was not valid paginated JSON."
    exit "$jq_status"
  fi
fi

{
  printf 'version=%s\n' "$version"
  printf 'released=%s\n' "$released"
  printf 'main_sha=%s\n' "$main_sha"
} >>"$github_output"
