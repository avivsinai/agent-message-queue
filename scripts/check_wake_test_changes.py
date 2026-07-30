#!/usr/bin/env python3
"""Require PR-body justification before removing or renaming wake tests."""

from __future__ import annotations

import os
import re
import subprocess
import sys
from collections.abc import Mapping


BEGIN_MARKER = "BEGIN_WAKE_TEST_CHANGE_JUSTIFICATION"
END_MARKER = "END_WAKE_TEST_CHANGE_JUSTIFICATION"
TEST_FUNCTION = re.compile(
    r"(?m)^func\s+(?:\([^\n)]*\)\s+)?(Test[A-Za-z0-9_]+)\s*\("
)
COMMIT_SHA = re.compile(r"[0-9a-f]{40}(?:[0-9a-f]{24})?")
WAKE_TEST_PATH = re.compile(r"internal/cli/[^/]*wake[^/]*_test\.go")


def marker_matches(body: str, marker: str) -> list[re.Match[str]]:
    marker_line = rf"(?:{marker}|<!--[ \t]*{marker}[ \t]*-->)"
    return list(re.finditer(rf"(?m)^[ \t]*{marker_line}[ \t]*$", body))


def wake_test_names(files: Mapping[str, str]) -> set[str]:
    return {
        match.group(1)
        for path, source in files.items()
        if WAKE_TEST_PATH.fullmatch(path)
        for match in TEST_FUNCTION.finditer(source)
    }


def parse_justifications(body: str) -> tuple[dict[str, str], list[str]]:
    begin_count = body.count(BEGIN_MARKER)
    end_count = body.count(END_MARKER)
    if begin_count == 0 and end_count == 0:
        return {}, []
    if begin_count != 1 or end_count != 1:
        return {}, [
            "wake test justification markers must appear exactly once as a pair"
        ]

    begins = marker_matches(body, BEGIN_MARKER)
    ends = marker_matches(body, END_MARKER)
    if len(begins) != 1 or len(ends) != 1:
        return {}, [
            "wake test justification markers must occupy their own plain or "
            "HTML-comment lines"
        ]
    begin, end = begins[0], ends[0]
    if begin.start() >= end.start():
        return {}, ["wake test justification end marker must follow its begin marker"]

    entries: dict[str, str] = {}
    errors: list[str] = []
    for line in body[begin.end() : end.start()].strip().splitlines():
        if not line.strip():
            continue
        name, separator, reason = line.partition(":")
        name, reason = name.strip(), reason.strip()
        if not separator or not re.fullmatch(r"Test[A-Za-z0-9_]+", name):
            errors.append(
                "wake test justification entries must be 'TestName: reason' lines"
            )
            continue
        if not reason:
            errors.append(f"wake test justification for {name} must include a reason")
            continue
        if name in entries:
            errors.append(f"wake test justification for {name} is duplicated")
            continue
        entries[name] = reason
    if not entries and not errors:
        errors.append("wake test justification block must contain at least one entry")
    return entries, errors


def validate_change(
    before: Mapping[str, str], after: Mapping[str, str], body: str
) -> list[str]:
    removed = sorted(wake_test_names(before) - wake_test_names(after))
    if not removed:
        return []

    justifications, errors = parse_justifications(body)
    if not justifications and not errors:
        return [
            "removed or renamed wake tests require a WAKE_TEST_CHANGE_JUSTIFICATION "
            f"block: {', '.join(removed)}"
        ]
    errors.extend(
        f"wake test justification is missing for removed or renamed test {name}"
        for name in removed
        if name not in justifications
    )
    errors.extend(
        f"wake test justification names unchanged test {name}"
        for name in sorted(justifications)
        if name not in removed
    )
    return errors


def git_output(*args: str) -> str:
    return subprocess.run(
        ["git", *args], check=True, text=True, capture_output=True
    ).stdout


def files_at(revision: str) -> dict[str, str]:
    paths = git_output("ls-tree", "-r", "--name-only", revision).splitlines()
    return {
        path: git_output("show", f"{revision}:{path}")
        for path in paths
        if WAKE_TEST_PATH.fullmatch(path)
    }


def main() -> int:
    base = os.environ.get("PR_BASE_SHA", "")
    head = os.environ.get("PR_HEAD_SHA", "HEAD")
    if not COMMIT_SHA.fullmatch(base):
        print("error: PR_BASE_SHA must be a commit SHA", file=sys.stderr)
        return 1
    if head != "HEAD" and not COMMIT_SHA.fullmatch(head):
        print("error: PR_HEAD_SHA must be a commit SHA", file=sys.stderr)
        return 1
    try:
        git_output("rev-parse", "--verify", f"{base}^{{commit}}")
        git_output("rev-parse", "--verify", f"{head}^{{commit}}")
        comparison_base = git_output("merge-base", base, head).strip()
        errors = validate_change(
            files_at(comparison_base),
            files_at(head),
            os.environ.get("PR_BODY", ""),
        )
    except subprocess.CalledProcessError as error:
        print(error.stderr.strip() or "error: unable to inspect wake test changes", file=sys.stderr)
        return 1
    if errors:
        for error in errors:
            print(f"error: {error}", file=sys.stderr)
        return 1
    print("wake test removals and renames are justified")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
