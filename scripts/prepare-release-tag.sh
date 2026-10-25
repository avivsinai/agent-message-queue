#!/usr/bin/env bash
set -euo pipefail

# Resolve only the immutable main merge event, then bind it to its merged PR.
outputs="${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"
./scripts/resolve-release-metadata.sh
tag="$(sed -n 's/^tag=//p' "$outputs")"
[[ -n "$tag" ]] || exit 0
sha="${GITHUB_SHA:?GITHUB_SHA is required}"
[[ "$(git rev-parse 'HEAD^{commit}')" == "$sha" ]]
pulls="$(gh api --paginate --slurp \
  "repos/${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}/commits/${sha}/pulls?per_page=100")"
jq -e --arg sha "$sha" --arg title "chore(release): ${tag}" '
  if type == "array" and all(.[]; type == "array") then
    [.[][]] as $pulls |
    ($pulls | length) == 1 and
    ($pulls[0] | .merged_at != null and .base.ref == "main" and
      .title == $title and .merge_commit_sha == $sha and
      (.number | type == "number" and . > 0 and floor == .))
  else false end
' >/dev/null <<<"$pulls" || {
  echo "::error::Release tag requires exactly one matching merged main release PR."
  exit 1
}
