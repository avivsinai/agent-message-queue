#!/usr/bin/env bash
# Negative proof: a wrong Grok skill symlink must fail the checker.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$ROOT/scripts/check-skills.sh"
FIX="$(mktemp -d "${TMPDIR:-/tmp}/amq-check-skills.XXXXXX")"
cleanup() { rm -rf "$FIX"; }
trap cleanup EXIT INT TERM

mkdir -p "$FIX/skills/amq-cli" "$FIX/skills/amq-spec" "$FIX/skills/amq-remote" "$FIX/skills/amq-board" "$FIX/skills/amq-lead" \
	"$FIX/.claude/skills" "$FIX/.agents/skills" "$FIX/.grok/skills"
printf -- '---\nname: amq-cli\ndescription: cli\n---\n' >"$FIX/skills/amq-cli/SKILL.md"
printf -- '---\nname: amq-spec\ndescription: spec\n---\n' >"$FIX/skills/amq-spec/SKILL.md"
printf -- '---\nname: amq-remote\ndescription: remote\n---\n' >"$FIX/skills/amq-remote/SKILL.md"
printf -- '---\nname: amq-board\ndescription: board\n---\n' >"$FIX/skills/amq-board/SKILL.md"
printf -- '---\nname: amq-lead\ndescription: lead\n---\n' >"$FIX/skills/amq-lead/SKILL.md"
for dest in .claude .agents .grok; do
	ln -s ../../skills/amq-cli "$FIX/$dest/skills/amq-cli"
	ln -s ../../skills/amq-spec "$FIX/$dest/skills/amq-spec"
	ln -s ../../skills/amq-remote "$FIX/$dest/skills/amq-remote"
	ln -s ../../skills/amq-board "$FIX/$dest/skills/amq-board"
	ln -s ../../skills/amq-lead "$FIX/$dest/skills/amq-lead"
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
