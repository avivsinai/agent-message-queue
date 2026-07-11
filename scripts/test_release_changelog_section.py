#!/usr/bin/env python3
"""Tests for release-note extraction from release-please changelogs."""

from __future__ import annotations

import importlib.util
import pathlib


SCRIPT = pathlib.Path(__file__).with_name("release_changelog_section.py")
spec = importlib.util.spec_from_file_location("release_changelog_section", SCRIPT)
if spec is None or spec.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
release_changelog_section = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release_changelog_section)


def test_release_please_heading() -> None:
    text = """# Changelog

## [1.2.0](https://example.test/compare/v1.1.0...v1.2.0) (2026-07-11)

### Features

* Add a feature.

## [1.1.0] - 2026-07-01

### Fixed

- Old fix.
"""
    want = """## [1.2.0](https://example.test/compare/v1.1.0...v1.2.0) (2026-07-11)

### Features

* Add a feature.
"""
    assert release_changelog_section.extract_release_section(text, "1.2.0") == want


def test_legacy_heading_remains_readable() -> None:
    text = """# Changelog

## [1.1.0] - 2026-07-01

### Fixed

- Old fix.
"""
    want = """## [1.1.0] - 2026-07-01

### Fixed

- Old fix.
"""
    assert release_changelog_section.extract_release_section(text, "1.1.0") == want


def test_missing_version_fails_closed() -> None:
    try:
        release_changelog_section.extract_release_section("# Changelog\n", "9.9.9")
    except ValueError as err:
        assert "missing changelog section for 9.9.9" in str(err)
    else:
        raise AssertionError("missing version unexpectedly succeeded")


if __name__ == "__main__":
    test_release_please_heading()
    test_legacy_heading_remains_readable()
    test_missing_version_fails_closed()
    print("release changelog section tests ok")
