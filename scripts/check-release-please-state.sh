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
set +e
release_response="$(
  gh api --include \
    "repos/${github_repository}/releases/tags/${tag}" 2>&1
)"
gh_status=$?
set -e

release_response="${release_response//$'\r'/}"
status_line="${release_response%%$'\n'*}"
if [[ ! "$status_line" =~ ^HTTP/[^[:space:]]+[[:space:]]+([0-9]{3})([[:space:]]|$) ]]; then
  echo "::error::GitHub release lookup did not return an HTTP status."
  exit 1
fi
http_status="${BASH_REMATCH[1]}"

case "$http_status" in
  200)
    if [[ "$gh_status" != "0" || "$release_response" != *$'\n\n'* ]]; then
      echo "::error::GitHub release lookup returned an invalid HTTP 200 response."
      exit 1
    fi
    release_body="${release_response#*$'\n\n'}"
    if ! released="$(
      jq -er --arg tag "$tag" '
        if type != "object" or
          (.tag_name | type) != "string" or .tag_name != $tag or
          (.draft | type) != "boolean" or
          ((.published_at | type) != "string" and .published_at != null)
        then error("invalid release response fields")
        else ((.draft == false and .published_at != null) | tostring)
        end
      ' <<<"$release_body"
    )"; then
      echo "::error::GitHub release lookup returned invalid JSON or response fields."
      exit 1
    fi
    if [[ "$released" != "true" && "$released" != "false" ]]; then
      echo "::error::GitHub release lookup returned multiple JSON values."
      exit 1
    fi
    ;;
  404)
    released=false
    ;;
  *)
    echo "::error::GitHub release lookup failed with HTTP ${http_status}."
    exit 1
    ;;
esac

{
  printf 'version=%s\n' "$version"
  printf 'released=%s\n' "$released"
  printf 'main_sha=%s\n' "$main_sha"
} >>"$github_output"
