#!/usr/bin/env bash
# The .claude, .agents, and .grok skills must be symlinks to the canonical skills/.
set -euo pipefail

fail() {
	echo "❌ $*" >&2
	exit 1
}

echo "Checking skill symlinks..."
for dest in .claude .agents .grok; do
	for skill in amq-cli amq-spec amq-remote amq-board; do
		[[ -L "$dest/skills/$skill" ]] || fail "$dest/skills/$skill is not a symlink"
		[[ "$(readlink "$dest/skills/$skill")" == "../../skills/$skill" ]] || fail "$dest/skills/$skill target wrong"
	done
done
echo "✓ Skill symlinks valid"
