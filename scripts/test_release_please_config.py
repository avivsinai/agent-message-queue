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
    assert config["label"] == "autorelease: pending"
    assert config["release-label"] == "autorelease: tagged"
    assert "labels" not in config
    assert "release-labels" not in config

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
    assert "if: ${{ env.RELEASE_PLEASE_TOKEN == '' }}" in workflow
    assert "github.event_name == 'workflow_dispatch' &&" in workflow
    assert "github.ref != 'refs/heads/main' &&" in workflow
    assert "format('rejected-{0}', github.run_id) ||" in workflow
    assert "github.repository" in workflow
    assert "cancel-in-progress: true" in workflow

    release_job = workflow[workflow.index("  release-please:\n") :]
    assert "github.event_name != 'workflow_dispatch' ||" in release_job
    assert "github.ref == 'refs/heads/main'" in release_job
    assert "github.event.head_commit.message" not in release_job
    assert 'workflows: ["Release"]' in workflow
    assert "types: [completed]" in workflow
    assert "branches: [main]" in workflow
    assert "github.event.workflow_run.conclusion" not in workflow
    assert "github.event.workflow_run.head_sha" not in workflow

    checkout_step = release_job[
        release_job.index("      - uses: actions/checkout@") :
        release_job.index("      - name: Check published release state\n")
    ]
    assert "ref: refs/heads/main" in checkout_step
    assert "fetch-depth: 0" in checkout_step

    state_step = release_job[
        release_job.index("      - name: Check published release state\n") :
        release_job.index("      - name: Revalidate current main\n")
    ]
    assert "run: ./scripts/check-release-please-state.sh" in state_step

    revalidate_step = release_job[
        release_job.index("      - name: Revalidate current main\n") :
        release_job.index("      - name: Reconcile published release pull request\n")
    ]
    assert "if: ${{ steps.release_state.outputs.released == 'true' }}" in revalidate_step
    assert "EXPECTED_MAIN_SHA: ${{ steps.release_state.outputs.main_sha }}" in revalidate_step
    assert "run: ./scripts/revalidate-release-please-main.sh" in revalidate_step

    reconcile_step = release_job[
        release_job.index("      - name: Reconcile published release pull request\n") :
        release_job.index("      - name: Await release-please credential\n")
    ]
    assert "GH_TOKEN: ${{ github.token }}" in reconcile_step
    assert "RELEASE_VERSION: ${{ steps.release_state.outputs.version }}" in reconcile_step
    assert "EXPECTED_MAIN_SHA: ${{ steps.release_state.outputs.main_sha }}" in reconcile_step
    assert "steps.release_state.outputs.released == 'true'" in reconcile_step
    assert "steps.main_state.outputs.current == 'true'" in reconcile_step
    assert "RELEASE_PLEASE_TOKEN" not in reconcile_step
    assert "run: ./scripts/reconcile-release-please-labels.sh" in reconcile_step

    action_step = release_job[
        release_job.index("      - name: Open or update release pull request\n") :
    ]
    assert (
        "if: ${{ env.RELEASE_PLEASE_TOKEN != '' && "
        "steps.release_state.outputs.released == 'true' && "
        "steps.main_state.outputs.current == 'true' }}"
    ) in action_step
    assert "target-branch: main" in action_step
    assert "aborts before PR mutation while a merged release PR is pending" in action_step
    assert "release-please 17.6.0" in action_step

    release_workflow = (ROOT / ".github/workflows/release.yml").read_text()
    assert "predates scripts/release_changelog_section.py" in release_workflow
    assert "python3 scripts/release_changelog_section.py" not in release_workflow


def test_release_state_scripts_are_injection_safe() -> None:
    state = (ROOT / "scripts/check-release-please-state.sh").read_text()
    assert "RELEASE_VERSION_PATTERN=" in state
    assert 'jq -e --arg tag "$tag"' in state
    assert ".tag_name == $tag" in state
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
    test_release_state_scripts_are_injection_safe()
    test_release_workflow_marks_published_release_pr_as_tagged()
    test_version_files_and_replacement_gates()
    print("release-please config tests ok")
