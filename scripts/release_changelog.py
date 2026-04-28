#!/usr/bin/env python3
"""Prepare CHANGELOG.md for a release.

This script intentionally owns only the changelog rewrite. The shell release
flow owns git/GitHub operations and version metadata updates.
"""

from __future__ import annotations

import pathlib
import re
import subprocess
import sys


REPO_URL = "https://github.com/avivsinai/agent-message-queue"
VERSION_PATTERN = r"\d+\.\d+\.\d+(?:[-.][0-9A-Za-z.]+)?"
LINK_RE = re.compile(rf"^\[(?P<label>Unreleased|{VERSION_PATTERN})\]:\s+\S+", re.MULTILINE)
VERSION_LINK_RE = re.compile(rf"^\[(?P<version>{VERSION_PATTERN})\]:\s+\S+", re.MULTILINE)


def release_compare_url(previous: str, current: str) -> str:
    return f"{REPO_URL}/compare/v{previous}...v{current}"


def unreleased_compare_url(current: str) -> str:
    return f"{REPO_URL}/compare/v{current}...HEAD"


def previous_version_from_links(text: str, current: str) -> str:
    for match in VERSION_LINK_RE.finditer(text):
        version = match.group("version")
        if version != current:
            return version
    return ""


def previous_version_from_git() -> str:
    for args in (
        ["git", "describe", "--tags", "--abbrev=0", "HEAD~"],
        ["git", "describe", "--tags", "--abbrev=0", "HEAD"],
    ):
        try:
            out = subprocess.check_output(args, text=True, stderr=subprocess.DEVNULL).strip()
        except (OSError, subprocess.CalledProcessError):
            continue
        if out:
            return out.removeprefix("v")
    return ""


def update_compare_links(text: str, version: str, previous: str) -> str:
    if not previous:
        raise SystemExit("error: cannot determine previous version for CHANGELOG compare link")

    lines = text.splitlines()
    filtered = [
        line
        for line in lines
        if not line.startswith("[Unreleased]:") and not line.startswith(f"[{version}]:")
    ]

    insert_at = next((i for i, line in enumerate(filtered) if LINK_RE.match(line)), None)
    block = [
        f"[Unreleased]: {unreleased_compare_url(version)}",
        f"[{version}]: {release_compare_url(previous, version)}",
    ]

    if insert_at is None:
        while filtered and filtered[-1] == "":
            filtered.pop()
        filtered.extend(["", *block])
    else:
        filtered[insert_at:insert_at] = block

    return "\n".join(filtered) + "\n"


def prepare_changelog(text: str, version: str, release_date: str, allow_empty: bool) -> str:
    marker = "## [Unreleased]"
    if marker not in text:
        raise SystemExit("error: CHANGELOG.md is missing the Unreleased section")

    start = text.index(marker)
    after_marker = start + len(marker)
    rest = text[after_marker:]
    match = re.search(r"(?m)^## \[", rest)
    if match:
        unreleased_body = rest[: match.start()]
        suffix = rest[match.start() :]
    else:
        unreleased_body = rest
        suffix = ""

    if not unreleased_body.strip() and not allow_empty:
        raise SystemExit(
            "error: CHANGELOG.md Unreleased section is empty; add release notes first or pass --allow-empty"
        )

    release_header = f"\n\n## [{version}] - {release_date}\n"
    new_text = text[:start] + marker + release_header + unreleased_body.lstrip("\n")
    if suffix:
        new_text += suffix if suffix.startswith("\n") else "\n" + suffix

    previous = previous_version_from_links(text, version) or previous_version_from_git()
    return update_compare_links(new_text, version, previous)


def main(argv: list[str]) -> int:
    if len(argv) != 4:
        print("usage: release_changelog.py VERSION YYYY-MM-DD ALLOW_EMPTY", file=sys.stderr)
        return 2
    version, release_date, allow_empty_raw = argv[1], argv[2], argv[3]
    allow_empty = allow_empty_raw == "1"

    changelog = pathlib.Path("CHANGELOG.md")
    text = changelog.read_text()
    changelog.write_text(prepare_changelog(text, version, release_date, allow_empty))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
