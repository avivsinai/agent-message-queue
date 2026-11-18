#!/usr/bin/env bash
# The .claude, .agents, and .grok skills must be symlinks to the canonical skills/.
set -euo pipefail

fail() {
	echo "❌ $*" >&2
	exit 1
}

echo "Checking skill symlinks..."
for dest in .claude .agents .grok; do
	for skill in amq-cli amq-spec amq-remote amq-board amq-lead; do
		[[ -L "$dest/skills/$skill" ]] || fail "$dest/skills/$skill is not a symlink"
		[[ "$(readlink "$dest/skills/$skill")" == "../../skills/$skill" ]] || fail "$dest/skills/$skill target wrong"
		file="$dest/skills/$skill/SKILL.md"
		[[ -f "$file" && -r "$file" ]] || fail "$file is not a readable regular file"
	done
done
echo "✓ Skill symlinks valid"

# Regression check: an unquoted colon in a description made a skill unloadable.
# Ruby is the platform's YAML parser; no package install is needed.
for skill in skills/*/SKILL.md; do
	[[ -f "$skill" ]] || fail "$skill is missing"
	ruby -ryaml -e '
		text = File.read(ARGV.fetch(0), encoding: "UTF-8")
		match = text.match(/\A---\r?\n(.+?)\r?\n---\r?\n/m)
		abort "invalid front matter boundaries" unless match
		data = YAML.safe_load(match[1], permitted_classes: [], aliases: false)
		unless data.is_a?(Hash) && data["name"].is_a?(String) && data["description"].is_a?(String)
			abort "front matter must contain name and description"
		end
	' "$skill" || fail "$skill has unparsable or incomplete YAML front matter"
done
echo "✓ Skill YAML front matter parses"
