#!/usr/bin/env bash
set -euo pipefail

event_name="${EVENT_NAME:-}"
input_tag="${INPUT_TAG:-}"
commit_message="${COMMIT_MESSAGE:-}"
github_sha="${GITHUB_SHA:-}"
github_output="${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

validate_tag() {
  local tag="$1"
  if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z]+)*$ ]]; then
    echo "::error::Release identifiers must use vX.Y.Z style tags."
    exit 1
  fi
}

tag_commit_sha() {
  git rev-parse "refs/tags/${1}^{commit}" 2>/dev/null || git rev-parse "refs/tags/${1}"
}

push_release_subject() {
  local commit_subject="$1"

  if [[ "$commit_subject" =~ ^chore\(release\):\ v ]]; then
    printf '%s\n' "$commit_subject"
    return 0
  fi

  if [[ "$commit_subject" == Merge\ pull\ request* ]] &&
    git rev-parse -q --verify "${github_sha}^1" >/dev/null 2>&1; then
    git log --format='%s' --no-merges "${github_sha}^1..${github_sha}" |
      awk 'found == 0 && /^chore\(release\): v/ { print; found = 1 }'
  fi
}

case "$event_name" in
  workflow_dispatch)
    tag="$input_tag"
    validate_tag "$tag"
    git fetch --force --tags origin
    if ! git rev-parse -q --verify "refs/tags/${tag}" >/dev/null 2>&1; then
      echo "::error::Tag ${tag} does not exist. workflow_dispatch is only for rerunning an existing release."
      exit 1
    fi
    version="${tag#v}"
    git checkout --detach "$tag"
    ./scripts/check-release-version.sh "$version"
    release_ref="$tag"
    release_sha="$(tag_commit_sha "$tag")"
    ;;
  push)
    if [[ -z "$github_sha" ]]; then
      echo "::error::GITHUB_SHA is required for push release resolution."
      exit 1
    fi

    commit_subject="${commit_message%%$'\n'*}"
    release_subject="$(push_release_subject "$commit_subject")"
    if [[ -z "$release_subject" ]]; then
      echo "::notice::No release commit found for push ${github_sha}; skipping release jobs."
      exit 0
    fi

    tag="${release_subject#chore(release): }"
    tag="${tag%% \(#*}"
    validate_tag "$tag"

    version="${tag#v}"
    ./scripts/check-release-version.sh "$version"

    release_sha="${github_sha}"
    if git rev-parse -q --verify "refs/tags/${tag}" >/dev/null 2>&1; then
      existing_sha="$(tag_commit_sha "$tag")"
      if [[ "$existing_sha" != "$release_sha" ]]; then
        echo "::error::Tag ${tag} already exists but points to ${existing_sha}, not ${release_sha}."
        exit 1
      fi
    fi
    release_ref="${release_sha}"
    ;;
  *)
    echo "::error::Unsupported event: ${event_name}"
    exit 1
    ;;
esac

prerelease=false
if [[ "$tag" == *-* ]]; then
  prerelease=true
fi

{
  echo "tag=${tag}"
  echo "version=${version}"
  echo "release_ref=${release_ref}"
  echo "release_sha=${release_sha}"
  echo "prerelease=${prerelease}"
} >>"$github_output"
