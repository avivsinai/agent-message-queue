#!/usr/bin/env python3
from __future__ import annotations

import importlib.util
import pathlib


SCRIPT = pathlib.Path(__file__).with_name("release_changelog.py")
spec = importlib.util.spec_from_file_location("release_changelog", SCRIPT)
assert spec is not None and spec.loader is not None
release_changelog = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release_changelog)


def test_prepare_changelog_updates_release_and_compare_links() -> None:
    text = """# Changelog

## [Unreleased]
### Added

- New thing.

## [0.33.0] - 2026-04-28
### Added

- Previous thing.

[Unreleased]: https://github.com/avivsinai/agent-message-queue/compare/v0.32.2...HEAD
[0.33.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.32.2...v0.33.0
[0.32.2]: https://github.com/avivsinai/agent-message-queue/compare/v0.32.1...v0.32.2
"""

    got = release_changelog.prepare_changelog(text, "0.34.0", "2026-05-01", False)

    assert "## [0.34.0] - 2026-05-01" in got
    assert "- New thing." in got
    assert (
        "[Unreleased]: https://github.com/avivsinai/agent-message-queue/compare/v0.34.0...HEAD"
        in got
    )
    assert (
        "[0.34.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.33.0...v0.34.0"
        in got
    )
    assert got.count("[Unreleased]:") == 1
    assert got.count("[0.34.0]:") == 1


def test_update_compare_links_appends_when_footer_missing() -> None:
    text = "# Changelog\n\n## [Unreleased]\n\n## [0.1.0] - 2026-01-01\n"

    got = release_changelog.update_compare_links(text, "0.2.0", "0.1.0")

    assert got.endswith(
        "\n[Unreleased]: https://github.com/avivsinai/agent-message-queue/compare/v0.2.0...HEAD\n"
        "[0.2.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.1.0...v0.2.0\n"
    )


def main() -> int:
    test_prepare_changelog_updates_release_and_compare_links()
    test_update_compare_links_appends_when_footer_missing()
    print("release changelog tests ok")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
