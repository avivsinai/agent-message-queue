#!/usr/bin/env python3
"""Require PRs to add a CHANGELOG.md Unreleased entry unless explicitly skipped."""

from __future__ import annotations

import argparse
import difflib
import json
import os
import pathlib
import subprocess
import sys


CHANGELOG_PATH = "CHANGELOG.md"
SKIP_LABELS = {"no-changelog", "docs", "chore"}
DEPENDABOT_ACTORS = {"dependabot", "dependabot[bot]"}
GIT_REPOSITORY_ENV_VARS = (
    "GIT_DIR",
    "GIT_WORK_TREE",
    "GIT_INDEX_FILE",
    "GIT_OBJECT_DIRECTORY",
    "GIT_COMMON_DIR",
    "GIT_ALTERNATE_OBJECT_DIRECTORIES",
    "GIT_NAMESPACE",
)


class PRMetadata:
    def __init__(
        self,
        *,
        actor: str,
        author: str,
        head_ref: str,
        base_sha: str,
        head_sha: str,
        labels: list[str],
    ) -> None:
        self.actor = actor
        self.author = author
        self.head_ref = head_ref
        self.base_sha = base_sha
        self.head_sha = head_sha
        self.labels = labels


def load_event(path: pathlib.Path) -> dict:
    if not path:
        return {}
    with path.open() as f:
        return json.load(f)


def git_env() -> dict[str, str]:
    env = os.environ.copy()
    for name in GIT_REPOSITORY_ENV_VARS:
        env.pop(name, None)
    return env


def metadata_from_event(event: dict, actor: str = "") -> PRMetadata:
    pr = event.get("pull_request") or {}
    sender = event.get("sender") or {}
    author = pr.get("user") or {}
    head = pr.get("head") or {}
    base = pr.get("base") or {}
    labels = pr.get("labels") or []

    label_names = []
    for label in labels:
        if isinstance(label, dict):
            name = str(label.get("name") or "").strip()
        else:
            name = str(label).strip()
        if name:
            label_names.append(name)

    return PRMetadata(
        actor=(actor or str(sender.get("login") or "")).strip(),
        author=str(author.get("login") or "").strip(),
        head_ref=str(head.get("ref") or "").strip(),
        base_sha=str(base.get("sha") or "").strip(),
        head_sha=str(head.get("sha") or "").strip(),
        labels=label_names,
    )


def skip_reason(*, actor: str, head_ref: str, labels: list[str], author: str = "") -> str | None:
    if actor.strip().lower() in DEPENDABOT_ACTORS:
        return "dependabot actor"
    # A maintainer updating the branch becomes the synchronize-event actor;
    # the PR author stays dependabot[bot].
    if author.strip().lower() in DEPENDABOT_ACTORS:
        return "dependabot author"
    if head_ref.strip().startswith("release/"):
        return "release branch"
    normalized_labels = {label.strip().lower() for label in labels}
    for label in sorted(SKIP_LABELS):
        if label in normalized_labels:
            return f"label {label}"
    return None


def git_show(repo: pathlib.Path, ref: str, path: str) -> str:
    return subprocess.check_output(
        ["git", "show", f"{ref}:{path}"],
        cwd=repo,
        env=git_env(),
        stderr=subprocess.STDOUT,
        text=True,
    )


def unreleased_lines(changelog: str) -> list[str]:
    lines = changelog.splitlines()
    start = None
    for i, line in enumerate(lines):
        if line.strip() == "## [Unreleased]":
            start = i + 1
            break
    if start is None:
        raise ValueError("CHANGELOG.md is missing ## [Unreleased]")

    end = len(lines)
    for i in range(start, len(lines)):
        if lines[i].startswith("## ["):
            end = i
            break
    return lines[start:end]


def added_unreleased_entry(base_changelog: str, head_changelog: str) -> bool:
    base_lines = unreleased_lines(base_changelog)
    head_lines = unreleased_lines(head_changelog)
    for diff_line in difflib.ndiff(base_lines, head_lines):
        if not diff_line.startswith("+ "):
            continue
        line = diff_line[2:].strip()
        if line.startswith("- "):
            return True
    return False


def has_added_unreleased_entry(repo: pathlib.Path, base_ref: str, head_ref: str) -> bool:
    base_changelog = git_show(repo, base_ref, CHANGELOG_PATH)
    head_changelog = git_show(repo, head_ref, CHANGELOG_PATH)
    return added_unreleased_entry(base_changelog, head_changelog)


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo-root", default=".", help="Repository root to inspect")
    parser.add_argument("--event-path", default=os.environ.get("GITHUB_EVENT_PATH", ""))
    parser.add_argument("--actor", default=os.environ.get("GITHUB_ACTOR", ""))
    parser.add_argument("--base-ref", default="", help="Base git ref/SHA for the PR diff")
    parser.add_argument("--head-ref", default="HEAD", help="Head git ref/SHA for the PR diff")
    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    event = load_event(pathlib.Path(args.event_path)) if args.event_path else {}
    metadata = metadata_from_event(event, actor=args.actor)

    reason = skip_reason(
        actor=metadata.actor,
        author=metadata.author,
        head_ref=metadata.head_ref,
        labels=metadata.labels,
    )
    if reason:
        print(f"CHANGELOG.md check skipped: {reason}")
        return 0

    base_ref = args.base_ref or metadata.base_sha
    if not base_ref:
        print("error: cannot determine PR base ref; pass --base-ref", file=sys.stderr)
        return 2

    repo = pathlib.Path(args.repo_root)
    try:
        ok = has_added_unreleased_entry(repo, base_ref, args.head_ref)
    except (subprocess.CalledProcessError, ValueError) as err:
        print(f"error: failed to inspect CHANGELOG.md: {err}", file=sys.stderr)
        return 2

    if ok:
        print("CHANGELOG.md Unreleased entry found")
        return 0

    print(
        "error: PRs must add a bullet entry to CHANGELOG.md under ## [Unreleased]. "
        "Use an explicit no-changelog, docs, or chore label only for PRs that should skip this gate.",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
