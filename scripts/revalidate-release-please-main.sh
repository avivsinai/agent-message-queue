#!/usr/bin/env bash
set -euo pipefail

expected_main_sha="${EXPECTED_MAIN_SHA:?EXPECTED_MAIN_SHA is required}"
github_output="${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

if [[ ! "$expected_main_sha" =~ ^[0-9a-f]{40}([0-9a-f]{24})?$ ]]; then
  echo "::error::Expected main commit has an invalid object ID."
  exit 1
fi

git fetch --no-tags origin refs/heads/main
current_main_sha="$(git rev-parse --verify 'FETCH_HEAD^{commit}')"
if [[ ! "$current_main_sha" =~ ^[0-9a-f]{40}([0-9a-f]{24})?$ ]]; then
  echo "::error::Current main commit has an invalid object ID."
  exit 1
fi

current=false
if [[ "$current_main_sha" == "$expected_main_sha" ]]; then
  current=true
else
  echo "::notice::Main advanced from ${expected_main_sha} to ${current_main_sha}; this run is deferred to the newer trigger."
fi
printf 'current=%s\n' "$current" >>"$github_output"
