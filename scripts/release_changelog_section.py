#!/usr/bin/env python3
"""Extract one version section from legacy or release-please changelogs."""

from __future__ import annotations

import pathlib
import re
import sys


def extract_release_section(text: str, version: str) -> str:
    date = r"\d{4}-\d{2}-\d{2}"
    heading = re.compile(
        rf"(?m)^## \[{re.escape(version)}\](?:\([^\n)]+\))? "
        rf"(?:\({date}\)|- {date})$"
    )
    match = heading.search(text)
    if not match:
        raise ValueError(f"missing changelog section for {version}")

    next_heading = re.search(r"(?m)^## \[", text[match.end() :])
    end = match.end() + next_heading.start() if next_heading else len(text)
    return text[match.start() : end].strip() + "\n"


def main(argv: list[str]) -> int:
    if len(argv) not in (1, 2):
        print(
            "usage: release_changelog_section.py VERSION [OUTPUT_PATH]",
            file=sys.stderr,
        )
        return 2

    version = argv[0].removeprefix("v")
    text = pathlib.Path("CHANGELOG.md").read_text(encoding="utf-8")
    try:
        section = extract_release_section(text, version)
    except ValueError as err:
        print(f"error: {err}", file=sys.stderr)
        return 1

    if len(argv) == 2:
        pathlib.Path(argv[1]).write_text(section, encoding="utf-8")
    else:
        sys.stdout.write(section)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
