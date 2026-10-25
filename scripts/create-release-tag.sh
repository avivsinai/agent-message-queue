#!/usr/bin/env bash
set -euo pipefail

tag="${TAG:?TAG is required}"
sha="${RELEASE_SHA:?RELEASE_SHA is required}"
repository="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GH_TOKEN:?An App token is required}"
[[ "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
[[ "$(git rev-parse 'HEAD^{commit}')" == "$sha" ]]
./scripts/check-release-version.sh "${tag#v}"

confirm_existing() {
  git fetch --no-tags origin "refs/tags/${tag}"
  local existing
  existing="$(git rev-parse 'FETCH_HEAD^{commit}')"
  if [[ "$existing" != "$sha" ]]; then
    echo "::error::Tag ${tag} already points to ${existing}; refusing to retag."
    return 1
  fi
  echo "Tag ${tag} already points to ${sha}."
}

if git ls-remote --exit-code origin "refs/tags/${tag}" >/dev/null; then
  confirm_existing
  exit 0
else
  status=$?
  # ls-remote distinguishes a missing ref (2) from a transport failure.
  [[ "$status" == 2 ]] || exit "$status"
fi

if ! gh api --method POST "repos/${repository}/git/refs" \
  -f "ref=refs/tags/${tag}" -f "sha=${sha}" >/dev/null; then
  # A concurrent same-commit creation is harmless; no ref is ever updated.
  confirm_existing
fi
