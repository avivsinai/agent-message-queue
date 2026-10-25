#!/usr/bin/env python3
"""Pin the release-please migration's safety contract."""

from __future__ import annotations

import json
import pathlib


ROOT = pathlib.Path(__file__).resolve().parent.parent
APP_PIN = "actions/create-github-app-token@64ab729a6294000f5aa4a4bc0b73d1f32bc45510"


def test_release_please_config() -> None:
    config = json.loads((ROOT / "release-please-config.json").read_text())
    package = config["packages"]["."]

    assert config["release-type"] == "go"
    assert config["include-component-in-tag"] is False
    assert config["skip-github-release"] is True
    assert config["pull-request-title-pattern"] == "chore(release): v${version}"
    assert config["label"] == "autorelease: pending"
    assert config["release-label"] == "autorelease: tagged"
    assert "labels" not in config
    assert "release-labels" not in config

    assert package["extra-files"] == [
        {"type": "json", "path": ".claude-plugin/plugin.json", "jsonpath": "$.version"},
        {"type": "json", "path": ".codex-plugin/plugin.json", "jsonpath": "$.version"},
        {"type": "json", "path": "plugins/amq-board/.claude-plugin/plugin.json", "jsonpath": "$.version"},
        {"type": "generic", "path": "skills/amq-cli/SKILL.md"},
        {"type": "generic", "path": "skills/amq-spec/SKILL.md"},
        {"type": "generic", "path": "skills/amq-remote/SKILL.md"},
    ]

    manifest = json.loads((ROOT / ".release-please-manifest.json").read_text())
    assert list(manifest) == ["."]
    version = manifest["."]
    assert version

    for path in [".claude-plugin/plugin.json", ".codex-plugin/plugin.json", "plugins/amq-board/.claude-plugin/plugin.json"]:
        assert json.loads((ROOT / path).read_text())["version"] == version


def test_release_please_workflow_is_pr_only_and_staged() -> None:
    # The release workflows are a security boundary: only the repository App
    # creates release PRs and tags; publication consumes an existing tag.
    workflow = (ROOT / ".github/workflows/release-please.yml").read_text()
    assert APP_PIN in workflow
    assert "release-please@17.6.0 release-please release-pr" in workflow
    assert '--release-as "$RELEASE_VERSION"' in workflow
    assert '--config-file release-please-config.json' in workflow
    assert '--manifest-file .release-please-manifest.json' in workflow
    assert "RELEASE_PLEASE_TOKEN" not in workflow
    assert "secrets.RELEASE_APP_PRIVATE_KEY" in workflow
    assert "vars.RELEASE_APP_CLIENT_ID" in workflow
    assert "permission-pull-requests: write" in workflow
    # Run 40179756056: requesting Issues write rejected the App token. The
    # installed App uses Contents and Pull requests; PR write covers labels.
    assert "permission-issues:" not in workflow
    assert "cancel-in-progress: false" in workflow
    assert 'workflows: ["Release"]' in workflow
    assert "branches: [main]" not in workflow
    generation = workflow[workflow.index("      - name: Open or update release pull request\n"):]
    assert "if: github.event_name == 'workflow_dispatch'" in generation
    assert "steps.app.outputs.token" in generation
    assert "github.token" not in generation
    assert "./scripts/reconcile-release-please-labels.sh" in workflow

    tagger = (ROOT / ".github/workflows/release-tag.yml").read_text()
    assert APP_PIN in tagger
    assert "permission-contents: write" in tagger
    assert "./scripts/prepare-release-tag.sh" in tagger
    assert "./scripts/create-release-tag.sh" in tagger
    assert "needs: [prepare, verify]" in tagger
    assert "make test" in tagger and "make smoke" in tagger
    publisher = (ROOT / ".github/workflows/release.yml").read_text()
    assert 'tags:\n      - "v*"' in publisher
    assert "Create release tag" not in publisher
    assert "cancel-in-progress: false" in publisher
    assert "github.event.inputs.tag || github.ref_name" in publisher
    assert publisher.count("goreleaser/goreleaser-action@") == 1
    assert "predates scripts/release_changelog_section.py" in publisher


def test_release_state_scripts_are_injection_safe() -> None:
    state = (ROOT / "scripts/check-release-please-state.sh").read_text()
    assert "RELEASE_VERSION_PATTERN=" in state
    assert "gh api --include" in state
    assert '"repos/${github_repository}/releases/tags/${tag}"' in state
    assert 'jq -er --arg tag "$tag"' in state
    assert ".tag_name != $tag" in state
    assert "releases?per_page=" not in state
    assert "grep " not in state
    assert "printf 'version=%s\\n'" in state
    assert "printf 'released=%s\\n'" in state
    assert "printf 'main_sha=%s\\n'" in state
    assert 'echo "version=' not in state

    revalidate = (ROOT / "scripts/revalidate-release-please-main.sh").read_text()
    assert "git fetch --no-tags origin refs/heads/main" in revalidate
    assert "git rev-parse --verify 'FETCH_HEAD^{commit}'" in revalidate
    assert "printf 'current=%s\\n'" in revalidate

    reconcile = (ROOT / "scripts/reconcile-release-please-labels.sh").read_text()
    assert 'refs/tags/${tag}^{commit}' in reconcile
    assert 'git merge-base --is-ancestor "$tag_sha" "$main_sha"' in reconcile
    assert 'git show "${tag_sha}:${manifest_path}"' in reconcile
    assert 'commits/${tag_sha}/pulls?per_page=100' in reconcile
    assert '.base.ref == "main"' in reconcile
    assert ".title == $title" in reconcile
    assert ".merge_commit_sha == $tag_sha" in reconcile
    add_tagged = reconcile.index("--method POST")
    confirm_tagged = reconcile.index("Tagged label was not confirmed")
    remove_pending = reconcile.index("--method DELETE")
    assert add_tagged < confirm_tagged < remove_pending
    assert "RELEASE_PLEASE_TOKEN" not in reconcile


def test_release_workflow_marks_published_release_pr_as_tagged() -> None:
    workflow = (ROOT / ".github/workflows/release.yml").read_text()

    release_job = workflow[workflow.index("  release:\n") : workflow.index("  skill-publish:\n")]
    assert "pull-requests: write" in release_job

    publish = release_job.index("      - name: Release\n")
    mark_tagged = release_job.index(
        "      - name: Mark release pull request as tagged\n"
    )
    attest = release_job.index("      - name: Collect attestation subjects\n")
    assert publish < attest < mark_tagged

    mark_step = release_job[mark_tagged:]
    assert "if: always() && steps.publish_release.outcome == 'success'" in mark_step
    assert "if: github.event_name == 'push'" not in mark_step
    assert "RELEASE_SHA: ${{ needs.prepare.outputs.release_sha }}" in mark_step
    assert "VERSION: ${{ needs.prepare.outputs.version }}" in mark_step
    assert 'commits/${RELEASE_SHA}/pulls' in mark_step
    assert '.title == ("chore(release): v" + $version)' in mark_step
    assert "EVENT_NAME: ${{ github.event_name }}" in mark_step
    assert '"$EVENT_NAME" == "workflow_dispatch"' in mark_step
    assert "merge_commit_sha == $release_sha" in mark_step
    assert '($number | type == "number" and . > 0' in mark_step
    assert "jq -e --arg version" in mark_step
    assert "Historical dispatch" in mark_step
    assert "autorelease: pending" in mark_step
    assert "autorelease: tagged" in mark_step


def test_version_files_and_replacement_gates() -> None:
    version = json.loads((ROOT / ".release-please-manifest.json").read_text())["."]
    for path in ["skills/amq-cli/SKILL.md", "skills/amq-spec/SKILL.md", "skills/amq-remote/SKILL.md"]:
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
    ]:
        assert not (ROOT / removed).exists(), removed


if __name__ == "__main__":
    test_release_please_config()
    test_release_please_workflow_is_pr_only_and_staged()
    test_release_state_scripts_are_injection_safe()
    test_release_workflow_marks_published_release_pr_as_tagged()
    test_version_files_and_replacement_gates()
    print("release-please config tests ok")
