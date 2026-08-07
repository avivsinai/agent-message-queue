#!/usr/bin/env bash

set -euo pipefail

die() {
  echo "ERROR: $*" >&2
  exit 1
}

VERSION="${1:-}"
ALIAS_FROM_SKILL="${2:-}"
SKILLS_DIR="${SKILD_SKILLS_DIR:-skills}"
PUBLISHER="${SKILD_PUBLISHER:-avivsinai}"
REGISTRY_URL="${SKILD_REGISTRY_URL:-}"
AUTH_FILE="${SKILD_AUTH_FILE:-${HOME}/.skild/registry-auth.json}"
CURL_BIN="${CURL_BIN:-curl}"
NPX_BIN="${NPX_BIN:-npx}"

[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?$ ]] ||
  die "version '$VERSION' is not semver"
[[ "$PUBLISHER" =~ ^[a-z0-9][a-z0-9-]{1,31}$ ]] ||
  die "publisher '$PUBLISHER' is invalid"
if [[ -n "$ALIAS_FROM_SKILL" && ! "$ALIAS_FROM_SKILL" =~ ^[a-z0-9][a-z0-9-]{1,63}$ ]]; then
  die "alias source skill '$ALIAS_FROM_SKILL' is invalid"
fi
[[ -d "$SKILLS_DIR" ]] || die "skills directory '$SKILLS_DIR' does not exist"
command -v jq >/dev/null 2>&1 || die "jq is required"
command -v "$CURL_BIN" >/dev/null 2>&1 || die "$CURL_BIN is required"
command -v "$NPX_BIN" >/dev/null 2>&1 || die "$NPX_BIN is required"

TOKEN="${SKILD_TOKEN:-}"
if [[ -z "$TOKEN" || -z "$REGISTRY_URL" ]]; then
  [[ -r "$AUTH_FILE" ]] || die "Skild auth file '$AUTH_FILE' is not readable"
  if [[ -z "$TOKEN" ]]; then
    TOKEN="$(jq -er '.token | select(type == "string" and length > 0)' "$AUTH_FILE")" ||
      die "Skild auth file does not contain a token"
  fi
  if [[ -z "$REGISTRY_URL" ]]; then
    REGISTRY_URL="$(jq -er '.registryUrl | select(type == "string" and length > 0)' "$AUTH_FILE")" ||
      die "Skild auth file does not contain a registry URL"
  fi
fi
REGISTRY_URL="${REGISTRY_URL%/}"
[[ "$REGISTRY_URL" =~ ^https://[^/[:space:]]+(/[^[:space:]]*)?$ ]] ||
  die "Skild registry URL must use HTTPS"

HTTP_STATUS=""
HTTP_BODY=""
request_json() {
  local method="$1"
  local url="$2"
  local body="${3:-}"
  local response_file
  local status
  local -a args

  response_file="$(mktemp "${TMPDIR:-/tmp}/amq-skild-response.XXXXXX")"
  args=(
    --silent
    --show-error
    --max-time 15
    --request "$method"
    --header "accept: application/json"
    --header "authorization: Bearer $TOKEN"
    --output "$response_file"
    --write-out '%{http_code}'
  )
  if [[ -n "$body" ]]; then
    args+=(--header "content-type: application/json" --data "$body")
  fi

  if ! status="$($CURL_BIN "${args[@]}" "$url")"; then
    rm -f "$response_file"
    die "Skild registry request failed"
  fi
  [[ "$status" =~ ^[0-9]{3}$ ]] || {
    rm -f "$response_file"
    die "Skild registry returned a malformed HTTP status"
  }
  HTTP_STATUS="$status"
  HTTP_BODY="$(<"$response_file")"
  rm -f "$response_file"
}

valid_error_response() {
  local expected="$1"
  jq -e --arg expected "$expected" '
    type == "object" and
    keys == ["error", "ok"] and
    .ok == false and
    .error == $expected
  ' >/dev/null 2>&1 <<<"$HTTP_BODY"
}

verify_version() {
  local skill="$1"
  local canonical="@$PUBLISHER/$skill"

  request_json GET "$REGISTRY_URL/skills/$PUBLISHER/$skill/versions/$VERSION"
  [[ "$HTTP_STATUS" == "200" ]] ||
    die "canonical version verification failed for $canonical@$VERSION (HTTP $HTTP_STATUS)"
  jq -e --arg canonical "$canonical" --arg version "$VERSION" '
    type == "object" and
    .ok == true and
    .name == $canonical and
    .version == $version and
    (.integrity | type == "string" and length > 0) and
    (.tarballUrl | type == "string" and length > 0)
  ' >/dev/null 2>&1 <<<"$HTTP_BODY" ||
    die "canonical version verification returned malformed or mismatched data for $canonical@$VERSION"
}

alias_success_response() {
  local canonical="$1"
  local alias="$2"
  jq -e --arg canonical "$canonical" --arg alias "$alias" '
    type == "object" and
    .ok == true and
    .name == $canonical and
    .alias == $alias
  ' >/dev/null 2>&1 <<<"$HTTP_BODY"
}

alias_clear_success_response() {
  local canonical="$1"
  jq -e --arg canonical "$canonical" '
    type == "object" and
    .ok == true and
    .name == $canonical and
    .alias == null
  ' >/dev/null 2>&1 <<<"$HTTP_BODY"
}

post_alias() {
  local skill="$1"
  local alias="$2"
  local payload

  if [[ -n "$alias" ]]; then
    payload="$(jq -cn --arg alias "$alias" '{alias: $alias}')"
  else
    payload='{"alias":null}'
  fi
  request_json POST "$REGISTRY_URL/publisher/skills/$PUBLISHER/$skill/alias" "$payload"
}

verify_alias() {
  local skill="$1"
  local canonical="@$PUBLISHER/$skill"

  request_json GET "$REGISTRY_URL/resolve?alias=$skill"
  [[ "$HTTP_STATUS" == "200" ]] ||
    die "alias verification failed for $skill (HTTP $HTTP_STATUS)"
  jq -e --arg alias "$skill" --arg canonical "$canonical" '
    type == "object" and
    .ok == true and
    .alias == $alias and
    .type == "registry" and
    .spec == $canonical
  ' >/dev/null 2>&1 <<<"$HTTP_BODY" ||
    die "alias '$skill' does not resolve exactly to $canonical"
}

verify_target_alias_empty() {
  local skill="$1"
  local canonical="@$PUBLISHER/$skill"

  request_json GET "$REGISTRY_URL/skills/$PUBLISHER/$skill"
  [[ "$HTTP_STATUS" == "200" ]] ||
    die "cannot verify target alias state for $canonical (HTTP $HTTP_STATUS)"
  jq -e --arg canonical "$canonical" '
    type == "object" and
    .ok == true and
    (.skill | type == "object") and
    .skill.name == $canonical and
    .skill.alias == null
  ' >/dev/null 2>&1 <<<"$HTTP_BODY" ||
    die "target $canonical already owns a different alias; refusing migration"
}

rollback_alias() {
  local old_skill="$1"
  local alias="$2"
  local old_canonical="@$PUBLISHER/$old_skill"

  if post_alias "$old_skill" "$alias" &&
      [[ "$HTTP_STATUS" == "200" ]] &&
      alias_success_response "$old_canonical" "$alias"; then
    echo "Restored alias '$alias' to $old_canonical after target assignment failed" >&2
  else
    echo "WARNING: failed to restore alias '$alias' to $old_canonical" >&2
  fi
}

reconcile_alias() {
  local skill="$1"
  local canonical="@$PUBLISHER/$skill"
  local old_canonical
  local failed_status

  post_alias "$skill" "$skill"
  if [[ "$HTTP_STATUS" == "200" ]]; then
    alias_success_response "$canonical" "$skill" ||
      die "alias assignment returned malformed or mismatched data for $canonical"
    verify_alias "$skill"
    echo "Alias '$skill' resolves to $canonical"
    return
  fi

  if [[ "$HTTP_STATUS" != "409" ]] || ! valid_error_response "Alias already in use."; then
    die "alias assignment failed for $canonical (HTTP $HTTP_STATUS)"
  fi
  [[ -n "$ALIAS_FROM_SKILL" ]] ||
    die "alias '$skill' is already in use; no explicit migration source was supplied"
  [[ "$ALIAS_FROM_SKILL" != "$skill" ]] ||
    die "alias migration source and target are both '$skill'"

  old_canonical="@$PUBLISHER/$ALIAS_FROM_SKILL"
  request_json GET "$REGISTRY_URL/resolve?alias=$skill"
  [[ "$HTTP_STATUS" == "200" ]] ||
    die "cannot verify migration source for alias '$skill' (HTTP $HTTP_STATUS)"
  jq -e --arg alias "$skill" --arg canonical "$old_canonical" '
    type == "object" and
    .ok == true and
    .alias == $alias and
    .type == "registry" and
    .spec == $canonical
  ' >/dev/null 2>&1 <<<"$HTTP_BODY" ||
    die "alias '$skill' does not resolve exactly to the approved migration source $old_canonical"

  verify_target_alias_empty "$skill"

  post_alias "$ALIAS_FROM_SKILL" ""
  if [[ "$HTTP_STATUS" != "200" ]] || ! alias_clear_success_response "$old_canonical"; then
    die "failed to clear alias '$skill' from $old_canonical (HTTP $HTTP_STATUS)"
  fi

  post_alias "$skill" "$skill"
  if [[ "$HTTP_STATUS" != "200" ]] || ! alias_success_response "$canonical" "$skill"; then
    failed_status="$HTTP_STATUS"
    rollback_alias "$ALIAS_FROM_SKILL" "$skill"
    die "failed to assign migrated alias '$skill' to $canonical (HTTP $failed_status)"
  fi

  verify_alias "$skill"
  echo "Migrated alias '$skill' from $old_canonical to $canonical"
}

published=0
for dir in "$SKILLS_DIR"/*/; do
  [[ -f "$dir/SKILL.md" ]] || continue
  skill="$(basename "$dir")"
  [[ "$skill" =~ ^[a-z0-9][a-z0-9-]{2,63}$ ]] ||
    die "skill '$skill' cannot be used as a Skild alias"
  canonical="@$PUBLISHER/$skill"
  output_file="$(mktemp "${TMPDIR:-/tmp}/amq-skild-publish.XXXXXX")"

  echo "Publishing $canonical@$VERSION"
  if "$NPX_BIN" skild publish --dir "$dir" --skill-version "$VERSION" >"$output_file" 2>&1; then
    echo "Published $canonical@$VERSION"
  elif grep -Fqx '{"ok":false,"error":"Version already exists."}' "$output_file"; then
    echo "$canonical@$VERSION already exists"
  else
    echo "Publish failed for $canonical@$VERSION" >&2
    sed -n '1,80p' "$output_file" >&2
    rm -f "$output_file"
    exit 1
  fi
  rm -f "$output_file"

  verify_version "$skill"
  reconcile_alias "$skill"
  published=$((published + 1))
done

[[ "$published" -gt 0 ]] || die "no publishable skills found in '$SKILLS_DIR'"
echo "Published and verified $published Skild skill(s)"
