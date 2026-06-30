#!/usr/bin/env python3
from __future__ import annotations

import importlib.util
import json
import pathlib
import subprocess
import tempfile


SCRIPT = pathlib.Path(__file__).with_name("check_pr_changelog.py")
spec = importlib.util.spec_from_file_location("check_pr_changelog", SCRIPT)
assert spec is not None and spec.loader is not None
check_pr_changelog = importlib.util.module_from_spec(spec)
spec.loader.exec_module(check_pr_changelog)


BASE_CHANGELOG = """# Changelog

## [Unreleased]
### Added

- Existing entry.

## [0.1.0] - 2026-01-01
"""


def run_git(repo: pathlib.Path, *args: str) -> str:
    return subprocess.check_output(["git", *args], cwd=repo, text=True).strip()


def commit(repo: pathlib.Path, message: str) -> str:
    run_git(repo, "add", "CHANGELOG.md")
    run_git(repo, "commit", "--allow-empty", "-m", message)
    return run_git(repo, "rev-parse", "HEAD")


def with_git_repo(head_changelog: str) -> tuple[pathlib.Path, str, str, tempfile.TemporaryDirectory[str]]:
    temp = tempfile.TemporaryDirectory()
    repo = pathlib.Path(temp.name)
    run_git(repo, "init")
    run_git(repo, "config", "user.email", "test@example.com")
    run_git(repo, "config", "user.name", "Test User")
    (repo / "CHANGELOG.md").write_text(BASE_CHANGELOG)
    base = commit(repo, "base")
    (repo / "CHANGELOG.md").write_text(head_changelog)
    head = commit(repo, "head")
    return repo, base, head, temp


def test_added_unreleased_bullet_passes() -> None:
    repo, base, head, temp = with_git_repo(
        """# Changelog

## [Unreleased]
### Added

- Existing entry.
- Enforce per-PR changelog entries in CI.

## [0.1.0] - 2026-01-01
"""
    )
    with temp:
        assert check_pr_changelog.has_added_unreleased_entry(repo, base, head)


def test_missing_unreleased_bullet_fails() -> None:
    repo, base, head, temp = with_git_repo(BASE_CHANGELOG)
    with temp:
        assert not check_pr_changelog.has_added_unreleased_entry(repo, base, head)


def test_added_heading_without_entry_fails() -> None:
    repo, base, head, temp = with_git_repo(
        """# Changelog

## [Unreleased]
### Added

- Existing entry.

### Fixed

## [0.1.0] - 2026-01-01
"""
    )
    with temp:
        assert not check_pr_changelog.has_added_unreleased_entry(repo, base, head)


def test_explicit_skip_reasons() -> None:
    assert check_pr_changelog.skip_reason(actor="dependabot[bot]", head_ref="deps/x", labels=[]) == (
        "dependabot actor"
    )
    assert check_pr_changelog.skip_reason(actor="avivsinai", head_ref="release/v0.40.0", labels=[]) == (
        "release branch"
    )
    assert check_pr_changelog.skip_reason(actor="avivsinai", head_ref="docs/update", labels=["docs"]) == (
        "label docs"
    )
    assert check_pr_changelog.skip_reason(actor="avivsinai", head_ref="chore/tooling", labels=["chore"]) == (
        "label chore"
    )
    assert check_pr_changelog.skip_reason(
        actor="avivsinai", head_ref="feature/x", labels=["no-changelog"]
    ) == "label no-changelog"


def test_title_like_branch_does_not_skip_without_label() -> None:
    assert check_pr_changelog.skip_reason(actor="avivsinai", head_ref="docs/update", labels=[]) is None
    assert check_pr_changelog.skip_reason(actor="avivsinai", head_ref="chore/tooling", labels=[]) is None


def test_event_metadata_prefers_pull_request_fields() -> None:
    event = {
        "sender": {"login": "ignored"},
        "pull_request": {
            "head": {"ref": "feature/changelog", "sha": "head-sha"},
            "base": {"sha": "base-sha"},
            "labels": [{"name": "no-changelog"}],
        },
    }
    metadata = check_pr_changelog.metadata_from_event(event, actor="avivsinai")
    assert metadata.actor == "avivsinai"
    assert metadata.head_ref == "feature/changelog"
    assert metadata.base_sha == "base-sha"
    assert metadata.head_sha == "head-sha"
    assert metadata.labels == ["no-changelog"]


def test_event_file_loader() -> None:
    with tempfile.TemporaryDirectory() as temp:
        path = pathlib.Path(temp) / "event.json"
        path.write_text(json.dumps({"pull_request": {"labels": [{"name": "docs"}]}}))
        assert check_pr_changelog.load_event(path)["pull_request"]["labels"][0]["name"] == "docs"


def main() -> int:
    test_added_unreleased_bullet_passes()
    test_missing_unreleased_bullet_fails()
    test_added_heading_without_entry_fails()
    test_explicit_skip_reasons()
    test_title_like_branch_does_not_skip_without_label()
    test_event_metadata_prefers_pull_request_fields()
    test_event_file_loader()
    print("check PR changelog tests ok")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
