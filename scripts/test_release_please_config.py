#!/usr/bin/env python3
"""Pin the release-please migration's safety contract."""

from __future__ import annotations

import json
import pathlib


ROOT = pathlib.Path(__file__).resolve().parent.parent
ACTION_PIN = "googleapis/release-please-action@45996ed1f6d02564a971a2fa1b5860e934307cf7"


def test_release_please_config() -> None:
    config = json.loads((ROOT / "release-please-config.json").read_text())
    package = config["packages"]["."]

    assert config["release-type"] == "go"
    assert config["include-component-in-tag"] is False
    assert config["skip-github-release"] is True
    assert config["pull-request-title-pattern"] == "chore(release): v${version}"

    assert package["extra-files"] == [
        {"type": "json", "path": ".claude-plugin/plugin.json", "jsonpath": "$.version"},
        {"type": "json", "path": ".codex-plugin/plugin.json", "jsonpath": "$.version"},
        {"type": "generic", "path": "skills/amq-cli/SKILL.md"},
        {"type": "generic", "path": "skills/amq-spec/SKILL.md"},
    ]

    manifest = json.loads((ROOT / ".release-please-manifest.json").read_text())
    assert list(manifest) == ["."]
    version = manifest["."]
    assert version

    for path in [".claude-plugin/plugin.json", ".codex-plugin/plugin.json"]:
        assert json.loads((ROOT / path).read_text())["version"] == version


def test_release_please_workflow_is_pr_only_and_staged() -> None:
    workflow = (ROOT / ".github/workflows/release-please.yml").read_text()
    assert ACTION_PIN in workflow
    assert "token: ${{ secrets.RELEASE_PLEASE_TOKEN }}" in workflow
    assert "skip-github-release: true" in workflow
    assert "RELEASE_PLEASE_TOKEN: ${{ secrets.RELEASE_PLEASE_TOKEN }}" in workflow
    assert "if: ${{ env.RELEASE_PLEASE_TOKEN != '' }}" in workflow
    assert "if: ${{ env.RELEASE_PLEASE_TOKEN == '' }}" in workflow

    release_workflow = (ROOT / ".github/workflows/release.yml").read_text()
    assert "predates scripts/release_changelog_section.py" in release_workflow
    assert "python3 scripts/release_changelog_section.py" not in release_workflow


def test_version_files_and_replacement_gates() -> None:
    version = json.loads((ROOT / ".release-please-manifest.json").read_text())["."]
    for path in ["skills/amq-cli/SKILL.md", "skills/amq-spec/SKILL.md"]:
        version_line = (ROOT / path).read_text().splitlines()[2]
        assert version_line == f"version: {version} # x-release-please-version"

    ci = (ROOT / ".github/workflows/ci.yml").read_text()
    assert "scripts/check_pr_title.py" in ci
    assert "scripts/check_pr_changelog.py" not in ci

    dependabot = (ROOT / ".github/dependabot.yml").read_text()
    assert dependabot.count('prefix: "deps"') == 2

    for removed in [
        "scripts/check_pr_changelog.py",
        "scripts/test_check_pr_changelog.py",
        "scripts/release_changelog.py",
        "scripts/test_release_changelog.py",
        "scripts/release.sh",
    ]:
        assert not (ROOT / removed).exists(), removed


if __name__ == "__main__":
    test_release_please_config()
    test_release_please_workflow_is_pr_only_and_staged()
    test_version_files_and_replacement_gates()
    print("release-please config tests ok")
