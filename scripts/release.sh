#!/usr/bin/env bash
set -euo pipefail

version="${1:-}"
if [[ "$#" != 1 || ! "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "usage: scripts/release.sh X.Y.Z" >&2
  exit 2
fi
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repository="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"
title="Release request ${version}"
previous="$(gh run list --repo "$repository" --workflow release-please.yml \
  --limit 1 --json databaseId --jq '.[0].databaseId // 0')"
gh workflow run release-please.yml --repo "$repository" --ref main -f "version=${version}"

# Dispatch does not return a run ID. Match the new dispatch by its version title.
deadline=$((SECONDS + 600))
run_id=""
while (( SECONDS < deadline )); do
  if [[ -z "$run_id" ]]; then
    runs="$(gh run list --repo "$repository" --workflow release-please.yml \
      --event workflow_dispatch --limit 100 --json databaseId,displayTitle)"
    run_id="$(jq -r --arg title "$title" --argjson previous "$previous" \
      '[.[] | select(.databaseId > $previous and .displayTitle == $title)] |
       sort_by(.databaseId) | .[0].databaseId // empty' <<<"$runs")"
  fi
  if [[ -n "$run_id" ]]; then
    run="$(gh run view "$run_id" --repo "$repository" --json status,conclusion,url)"
    if [[ "$(jq -r .status <<<"$run")" == completed ]]; then
      if [[ "$(jq -r .conclusion <<<"$run")" != success ]]; then
        echo "Release request failed: $(jq -r .url <<<"$run")" >&2
        exit 1
      fi
      pr="$(GITHUB_REPOSITORY="$repository" "$script_dir/find-release-pr.sh")"
      url="$(jq -er --arg title "chore(release): v${version}" \
        '.pull_request | if .title == $title then .html_url else error("Expected the requested release PR") end' <<<"$pr")"
      printf '%s\n' "$url"
      exit 0
    fi
  fi
  sleep 5
done
echo "Timed out waiting for the release request. Inspect Release Please runs in ${repository}." >&2
exit 1
