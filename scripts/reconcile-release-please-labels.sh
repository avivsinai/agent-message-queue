#!/usr/bin/env bash
set -euo pipefail

github_repository="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
release_version="${RELEASE_VERSION:?RELEASE_VERSION is required}"
expected_main_sha="${EXPECTED_MAIN_SHA:?EXPECTED_MAIN_SHA is required}"
manifest_path="${RELEASE_PLEASE_MANIFEST:-.release-please-manifest.json}"

RELEASE_VERSION_PATTERN='(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?'

if [[ ! "$release_version" =~ ^${RELEASE_VERSION_PATTERN}$ ]]; then
  echo "::error::Release version is not canonical."
  exit 1
fi
if [[ ! "$expected_main_sha" =~ ^[0-9a-f]{40}([0-9a-f]{24})?$ ]]; then
  echo "::error::Expected main commit has an invalid object ID."
  exit 1
fi

manifest_version="$(
  jq -er \
    --arg pattern "\\A${RELEASE_VERSION_PATTERN}\\z" \
    '."." | select(type == "string" and test($pattern))' \
    "$manifest_path"
)" || {
  echo "::error::Checked-out main manifest is malformed."
  exit 1
}
if [[ "$manifest_version" != "$release_version" ]]; then
  echo "::error::Release version does not match the checked-out main manifest."
  exit 1
fi

main_sha="$(git rev-parse --verify 'HEAD^{commit}')"
if [[ "$main_sha" != "$expected_main_sha" ]]; then
  echo "::error::Checked-out main does not match the bound main commit."
  exit 1
fi

tag="v${release_version}"
if ! git fetch --no-tags origin "refs/tags/${tag}:refs/tags/${tag}"; then
  echo "::error::Canonical release tag ${tag} is missing or conflicts with local state."
  exit 1
fi
tag_sha="$(git rev-parse --verify "refs/tags/${tag}^{commit}")" || {
  echo "::error::Canonical release tag ${tag} does not peel to a commit."
  exit 1
}
if [[ ! "$tag_sha" =~ ^[0-9a-f]{40}([0-9a-f]{24})?$ ]]; then
  echo "::error::Canonical release tag has an invalid commit object ID."
  exit 1
fi
if ! git merge-base --is-ancestor "$tag_sha" "$main_sha"; then
  echo "::error::Canonical release tag is not an ancestor of checked-out main."
  exit 1
fi

tag_manifest="$(
  git show "${tag_sha}:${manifest_path}"
)" || {
  echo "::error::Canonical release tag commit has no release manifest."
  exit 1
}
tag_manifest_version="$(
  jq -er \
    --arg pattern "\\A${RELEASE_VERSION_PATTERN}\\z" \
    '."." | select(type == "string" and test($pattern))' \
    <<<"$tag_manifest"
)" || {
  echo "::error::Canonical release tag commit manifest is malformed."
  exit 1
}
if [[ "$tag_manifest_version" != "$release_version" ]]; then
  echo "::error::Canonical release tag commit manifest does not match ${release_version}."
  exit 1
fi

release_title="chore(release): v${release_version}"
pull_pages="$(
  gh api --paginate --slurp \
    --header "Accept: application/vnd.github+json" \
    "repos/${github_repository}/commits/${tag_sha}/pulls?per_page=100"
)"
release_prs="$(
  jq -ce \
    --arg title "$release_title" \
    --arg tag_sha "$tag_sha" \
    '
      if type == "array" and all(.[]; type == "array") and
          all(.[][]; type == "object")
      then [
        .[][] |
        select(
          .merged_at != null and
          .base.ref == "main" and
          .title == $title and
          .merge_commit_sha == $tag_sha and
          (.number | type == "number" and . > 0 and floor == .)
        ) |
        .number
      ]
      else error("malformed commit pulls response")
      end
    ' \
    <<<"$pull_pages"
)" || {
  echo "::error::Commit pull request response was malformed."
  exit 1
}
release_pr_count="$(jq -r 'length' <<<"$release_prs")"
if [[ "$release_pr_count" != "1" ]]; then
  echo "::error::Expected one exact merged main release PR for ${tag}; found ${release_pr_count}."
  exit 1
fi
release_pr="$(jq -er '.[0] | select(type == "number" and . > 0 and floor == .)' <<<"$release_prs")"

read_labels() {
  local response
  if ! response="$(
    gh api \
      --header "Accept: application/vnd.github+json" \
      "repos/${github_repository}/issues/${release_pr}"
  )"; then
    return 1
  fi
  jq -ce \
    '
      if type == "object" and (.labels | type == "array") and
          all(.labels[]; type == "object" and (.name | type == "string"))
      then [.labels[].name]
      else error("malformed labels response")
      end
    ' \
    <<<"$response"
}

validate_target_labels() {
  local labels="$1"
  local pending_count tagged_count
  pending_count="$(jq -r '[.[] | select(. == "autorelease: pending")] | length' <<<"$labels")"
  tagged_count="$(jq -r '[.[] | select(. == "autorelease: tagged")] | length' <<<"$labels")"
  if [[ "$pending_count" -gt 1 || "$tagged_count" -gt 1 ]]; then
    echo "::error::Release PR has duplicate lifecycle labels."
    exit 1
  fi
  printf '%s %s\n' "$pending_count" "$tagged_count"
}

labels="$(read_labels)" || {
  echo "::error::Unable to read release PR labels safely."
  exit 1
}
read -r pending_count tagged_count < <(validate_target_labels "$labels")

if [[ "$pending_count" == "0" && "$tagged_count" == "1" ]]; then
  echo "Release PR #${release_pr} is already reconciled."
  exit 0
fi
if [[ "$pending_count" == "0" && "$tagged_count" == "0" ]]; then
  echo "::error::Release PR has neither pending nor tagged lifecycle state."
  exit 1
fi

if [[ "$tagged_count" == "0" ]]; then
  if ! printf '%s\n' '{"labels":["autorelease: tagged"]}' |
    gh api \
        --method POST \
        --header "Accept: application/vnd.github+json" \
        --input - \
        "repos/${github_repository}/issues/${release_pr}/labels" \
        >/dev/null; then
    echo "::warning::Tagged label mutation returned nonzero; checking authoritative state."
  fi

  labels="$(read_labels)" || {
    echo "::error::Unable to confirm tagged label after adding it."
    exit 1
  }
  read -r pending_count tagged_count < <(validate_target_labels "$labels")
  if [[ "$tagged_count" != "1" ]]; then
    echo "::error::Tagged label was not confirmed; pending label remains untouched."
    exit 1
  fi
fi

if [[ "$pending_count" == "1" ]]; then
  if ! gh api \
    --method DELETE \
    --header "Accept: application/vnd.github+json" \
    "repos/${github_repository}/issues/${release_pr}/labels/autorelease%3A%20pending" \
    >/dev/null; then
    echo "::warning::Pending label mutation returned nonzero; checking authoritative state."
  fi
fi

labels="$(read_labels)" || {
  echo "::error::Unable to confirm reconciled release PR labels."
  exit 1
}
read -r pending_count tagged_count < <(validate_target_labels "$labels")
if [[ "$pending_count" != "0" || "$tagged_count" != "1" ]]; then
  echo "::error::Release PR lifecycle labels did not converge."
  exit 1
fi

echo "Reconciled published release PR #${release_pr} for ${tag}."
