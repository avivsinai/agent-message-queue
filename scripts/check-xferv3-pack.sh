#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
fail() { echo "$1" >&2; exit 1; }

grep -q 'bin.install "amq-bridge"' "$root/.goreleaser.yaml" || fail "Homebrew formula missing amq-bridge"
grep -q 'amq-bridge' "$root/.goreleaser.yaml" || fail "goreleaser missing amq-bridge"
test -f "$root/integrations/grokbot-plugin/skills/amq-mail/SKILL.md" || fail "missing amq-mail skill"
test -x "$root/integrations/grokbot-plugin/scripts/install.sh" || fail "install.sh is not executable"
test -f "$root/docs/dot-onboarding.md" || fail "missing dot onboarding"

dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT
printf 'not-a-sum amq\n' >"$dir/manifest"
if AMQ_RELEASE_MANIFEST="$dir/manifest" AMQ_RELEASE_DIR="$dir" AMQ_BIN="$dir/bin" \
  "$root/integrations/grokbot-plugin/scripts/install.sh"; then
  fail "install.sh accepted a missing file"
fi

printf 'hello' >"$dir/amq"
sum="$(sha256sum "$dir/amq" | awk '{print $1}')"
printf '%s amq\n' "$sum" >"$dir/manifest"
AMQ_RELEASE_MANIFEST="$dir/manifest" AMQ_RELEASE_DIR="$dir" AMQ_BIN="$dir/bin" \
  "$root/integrations/grokbot-plugin/scripts/install.sh" >/dev/null
test -x "$dir/bin/amq" || fail "install.sh did not install amq"

CGO_ENABLED=0 go build -o "$dir/amq-bridge" "$root/cmd/amq-bridge"
if command -v ldd >/dev/null; then
  if ldd "$dir/amq-bridge" 2>&1 | grep -q 'not a dynamic'; then
    :
  elif ldd "$dir/amq-bridge" >/dev/null 2>&1; then
    fail "amq-bridge is dynamically linked"
  fi
fi
echo "check-xferv3-pack ok"
