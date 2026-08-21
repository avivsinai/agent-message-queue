#!/usr/bin/env bash
# Canonical skills/ must match the .claude, .agents, and .grok skill trees.
set -euo pipefail

fail() {
	echo "❌ $*" >&2
	exit 1
}

echo "Checking skill symlinks..."
for dest in .claude .agents .grok; do
	for skill in amq-cli amq-spec; do
		[[ -L "$dest/skills/$skill" ]] || fail "$dest/skills/$skill is not a symlink"
		[[ "$(readlink "$dest/skills/$skill")" == "../../skills/$skill" ]] || fail "$dest/skills/$skill target wrong"
	done
	diff -rq skills/amq-cli "$dest/skills/amq-cli" || fail "amq-cli content mismatch in $dest"
	diff -rq skills/amq-spec "$dest/skills/amq-spec" || fail "amq-spec content mismatch in $dest"
done
echo "✓ Skill symlinks valid"
