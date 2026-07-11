#!/usr/bin/env python3
"""Require project-approved Conventional Commit syntax in PR titles."""

from __future__ import annotations

import re
import sys


ALLOWED_TYPES = (
    "build",
    "chore",
    "ci",
    "deps",
    "docs",
    "feat",
    "fix",
    "perf",
    "refactor",
    "revert",
    "style",
    "test",
)
TYPE_PATTERN = "|".join(ALLOWED_TYPES)
TITLE_PATTERN = re.compile(
    rf"(?:{TYPE_PATTERN})(?:\([a-z0-9][a-z0-9._/-]*\))?!?: \S(?:.*\S)?"
)


def is_valid_title(title: str) -> bool:
    return TITLE_PATTERN.fullmatch(title) is not None


def main(argv: list[str]) -> int:
    if len(argv) != 1:
        print("usage: check_pr_title.py 'type(scope): description'", file=sys.stderr)
        return 2

    title = argv[0]
    if is_valid_title(title):
        print(f"PR title is conventional: {title}")
        return 0

    allowed = ", ".join(ALLOWED_TYPES)
    print(
        "error: PR title must be 'type(scope): description' "
        f"with a lowercase type ({allowed}); got {title!r}",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
