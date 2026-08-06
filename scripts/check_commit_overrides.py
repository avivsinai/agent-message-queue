#!/usr/bin/env python3
"""Validate release-note commit overrides embedded in a PR body."""

from __future__ import annotations

import os
import re
import sys

import check_pr_title


BEGIN_MARKER = "BEGIN_COMMIT_OVERRIDE"
END_MARKER = "END_COMMIT_OVERRIDE"
MAX_HEADER_LENGTH = 72


def marker_matches(body: str, marker: str) -> list[re.Match[str]]:
    marker_line = rf"(?:{marker}|<!--[ \t]*{marker}[ \t]*-->)"
    return list(re.finditer(rf"(?m)^[ \t]*{marker_line}[ \t]*$", body))


def validate_body(body: str) -> list[str]:
    begin_count = body.count(BEGIN_MARKER)
    end_count = body.count(END_MARKER)
    if begin_count == 0 and end_count == 0:
        return []
    if begin_count != 1 or end_count != 1:
        return [
            "commit override markers must appear exactly once as a pair; if you "
            'are only referencing a marker in prose, write "commit override block" '
            "instead of the literal marker text"
        ]

    begin_matches = marker_matches(body, BEGIN_MARKER)
    end_matches = marker_matches(body, END_MARKER)
    if len(begin_matches) != 1 or len(end_matches) != 1:
        return [
            "commit override markers must occupy their own plain or HTML-comment "
            'lines; if you are only referencing a marker in prose, write "commit '
            'override block" instead of the literal marker text'
        ]

    begin = begin_matches[0]
    end = end_matches[0]
    if begin.start() >= end.start():
        return ["commit override end marker must follow its begin marker"]

    content = body[begin.end() : end.start()].strip()
    if not content:
        return ["commit override block must contain at least one entry"]

    errors: list[str] = []
    entries = re.split(r"\n[ \t]*\n", content)
    for index, entry in enumerate(entries, start=1):
        lines = entry.splitlines()
        if len(lines) != 1:
            errors.append(f"commit override entry {index} must be a single line")
            continue

        header = lines[0]
        if len(header) > MAX_HEADER_LENGTH:
            errors.append(
                f"commit override entry {index} exceeds {MAX_HEADER_LENGTH} characters"
            )
        if not check_pr_title.is_valid_title(header):
            errors.append(
                f"commit override entry {index} is not a conventional header: {header!r}"
            )
    return errors


def main() -> int:
    body = os.environ.get("PR_BODY", "")
    errors = validate_body(body)
    if errors:
        for error in errors:
            print(f"error: {error}", file=sys.stderr)
        return 1

    print("PR commit overrides are valid")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
