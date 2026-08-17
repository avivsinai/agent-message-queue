#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

fail() {
  printf '%s\n' "$*" >&2
  exit 1
}

assert_make_embeds() {
  local input="$1"
  local want="$2"
  local dry
  dry="$(make -n build VERSION="$input")"
  printf '%s\n' "$dry" | grep -F -- "-X main.version=${want}" >/dev/null ||
    fail "make build VERSION=${input} ldflags missing -X main.version=${want}: ${dry}"
}

assert_make_embeds v9.9.9 9.9.9
assert_make_embeds 9.9.9 9.9.9
assert_make_embeds vv9.9.9 v9.9.9
assert_make_embeds dev dev

if make -n build VERSION=v9.9.9 | grep -F -- "-X main.version=v9.9.9" >/dev/null; then
  fail "make build VERSION=v9.9.9 still embeds a leading v"
fi

make build VERSION=v9.9.9
got="$(./amq --version)"
[[ "$got" == "9.9.9" ]] || fail "amq --version = ${got}, want 9.9.9"
got="$(./amq version)"
[[ "$got" == "9.9.9" ]] || fail "amq version = ${got}, want 9.9.9"
got="$(./amq-keepalive --version)"
[[ "$got" == "9.9.9" ]] || fail "amq-keepalive --version = ${got}, want 9.9.9"
echo "version embed ok"
make build
