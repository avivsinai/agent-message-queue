#!/usr/bin/env python3
"""Tests for the commit-override PR-body gate."""

from __future__ import annotations

import importlib.util
import pathlib


SCRIPT = pathlib.Path(__file__).with_name("check_commit_overrides.py")
spec = importlib.util.spec_from_file_location("check_commit_overrides", SCRIPT)
if spec is None or spec.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
check_commit_overrides = importlib.util.module_from_spec(spec)
spec.loader.exec_module(check_commit_overrides)


def test_valid_bodies() -> None:
    bodies = [
        "",
        "No release-note overrides.",
        """## Release notes
BEGIN_COMMIT_OVERRIDE
fix(upgrade): report stale wakes after upgrade

fix(wake): preserve usable legacy raw wakes
END_COMMIT_OVERRIDE
""",
        """## Release notes
<!-- BEGIN_COMMIT_OVERRIDE -->
fix(upgrade): report stale wakes after upgrade

fix(wake): preserve usable legacy raw wakes
<!-- END_COMMIT_OVERRIDE -->
""",
    ]
    for body in bodies:
        assert check_commit_overrides.validate_body(body) == [], body


def test_invalid_marker_shapes() -> None:
    bodies = [
        "BEGIN_COMMIT_OVERRIDE\nfix: one\n",
        "fix: one\nEND_COMMIT_OVERRIDE",
        """BEGIN_COMMIT_OVERRIDE
fix: one
BEGIN_COMMIT_OVERRIDE
fix: two
END_COMMIT_OVERRIDE
""",
        """BEGIN_COMMIT_OVERRIDE
END_COMMIT_OVERRIDE
""",
    ]
    for body in bodies:
        assert check_commit_overrides.validate_body(body), body


def test_prose_marker_mention_has_actionable_remedy() -> None:
    bodies = [
        "This prose mentions BEGIN_COMMIT_OVERRIDE support.",
        """This prose mentions BEGIN_COMMIT_OVERRIDE.
It later mentions END_COMMIT_OVERRIDE too.
""",
    ]
    for body in bodies:
        errors = check_commit_overrides.validate_body(body)
        assert len(errors) == 1
        assert (
            'write "commit override block" instead of the literal marker text'
            in errors[0]
        )


def test_invalid_entries() -> None:
    bodies = [
        """BEGIN_COMMIT_OVERRIDE
not a conventional header
END_COMMIT_OVERRIDE
""",
        """BEGIN_COMMIT_OVERRIDE
fix(upgrade): report stale wakes across the active AMQ base tree after
upgrade
END_COMMIT_OVERRIDE
""",
        """BEGIN_COMMIT_OVERRIDE
fix(upgrade): report stale wakes across the active AMQ base tree after upgrade
END_COMMIT_OVERRIDE
""",
    ]
    for body in bodies:
        assert check_commit_overrides.validate_body(body), body


if __name__ == "__main__":
    test_valid_bodies()
    test_invalid_marker_shapes()
    test_prose_marker_mention_has_actionable_remedy()
    test_invalid_entries()
    print("commit override tests ok")
