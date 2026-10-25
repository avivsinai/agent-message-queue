#!/usr/bin/env bash
set -euo pipefail

repository="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
branch=release-please--branches--main
owner="${repository%%/*}"
pages="$(gh api --paginate --slurp \
  "repos/${repository}/pulls?state=open&base=main&head=${owner}%3A${branch}&per_page=100")"
jq -e --arg repository "$repository" --arg branch "$branch" '
  if type == "array" and all(.[]; type == "array") then
    [.[][]] as $pulls |
    if ($pulls | length) <= 1 and all($pulls[];
      .state == "open" and .base.ref == "main" and
      .head.ref == $branch and .head.repo.full_name == $repository and
      (.title | type == "string") and (.html_url | type == "string"))
    then {pull_request: ($pulls[0] // null)}
    else error("Expected at most one owned Release Please main PR") end
  else error("Malformed release PR response") end
' <<<"$pages"
