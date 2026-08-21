#!/usr/bin/env bash
# Negative proof: a wrong Grok skill symlink must fail the checker.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$ROOT/scripts/check-skills.sh"
FIX="$(mktemp -d "${TMPDIR:-/tmp}/amq-check-skills.XXXXXX")"
cleanup() { rm -rf "$FIX"; }
trap cleanup EXIT INT TERM

mkdir -p "$FIX/skills/amq-cli" "$FIX/skills/amq-spec" \
	"$FIX/.claude/skills" "$FIX/.agents/skills" "$FIX/.grok/skills"
printf 'cli\n' >"$FIX/skills/amq-cli/SKILL.md"
printf 'spec\n' >"$FIX/skills/amq-spec/SKILL.md"
for dest in .claude .agents .grok; do
	ln -s ../../skills/amq-cli "$FIX/$dest/skills/amq-cli"
	ln -s ../../skills/amq-spec "$FIX/$dest/skills/amq-spec"
done

if ! (cd "$FIX" && bash "$SCRIPT" >/dev/null); then
	echo "FAIL: valid skill tree was rejected" >&2
	exit 1
fi

rm -f "$FIX/.grok/skills/amq-cli"
ln -s ../../skills/not-amq-cli "$FIX/.grok/skills/amq-cli"
if (cd "$FIX" && bash "$SCRIPT" >/dev/null 2>&1); then
	echo "FAIL: bad grok symlink passed; checker fails open" >&2
	exit 1
fi

echo "✓ check-skills fails closed on a bad grok symlink"
