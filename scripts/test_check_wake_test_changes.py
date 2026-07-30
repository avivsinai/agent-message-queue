#!/usr/bin/env python3
"""Tests for the wake-test deletion PR-body gate."""

from __future__ import annotations

import importlib.util
import pathlib


SCRIPT = pathlib.Path(__file__).with_name("check_wake_test_changes.py")
spec = importlib.util.spec_from_file_location("check_wake_test_changes", SCRIPT)
if spec is None or spec.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
check_wake_test_changes = importlib.util.module_from_spec(spec)
spec.loader.exec_module(check_wake_test_changes)


def test_no_removed_wake_tests_needs_no_justification() -> None:
    errors = check_wake_test_changes.validate_change(
        {"internal/cli/wake_test.go": "func TestWakeStillHere(t *testing.T) {}\n"},
        {"internal/cli/wake_test.go": "func TestWakeStillHere(t *testing.T) {}\n"},
        "",
    )
    assert errors == [], errors


def test_ci_revisions_must_be_commit_shas() -> None:
    assert check_wake_test_changes.COMMIT_SHA.fullmatch("a" * 40)
    assert check_wake_test_changes.COMMIT_SHA.fullmatch("a" * 64)
    assert not check_wake_test_changes.COMMIT_SHA.fullmatch("main")


def test_removed_wake_test_requires_named_justification() -> None:
    before = {"internal/cli/wake_test.go": "func TestWakeRetired(t *testing.T) {}\n"}
    after = {"internal/cli/wake_test.go": ""}

    errors = check_wake_test_changes.validate_change(before, after, "")

    assert errors == [
        "removed or renamed wake tests require a WAKE_TEST_CHANGE_JUSTIFICATION block: TestWakeRetired"
    ], errors


def test_rename_requires_the_old_test_name_to_be_justified() -> None:
    before = {"internal/cli/wake_test.go": "func TestWakeOldName(t *testing.T) {}\n"}
    after = {"internal/cli/wake_test.go": "func TestWakeNewName(t *testing.T) {}\n"}
    body = """BEGIN_WAKE_TEST_CHANGE_JUSTIFICATION
TestWakeOldName: renamed to describe the retained behavior
END_WAKE_TEST_CHANGE_JUSTIFICATION
"""

    assert check_wake_test_changes.validate_change(before, after, body) == []


def test_justification_must_cover_every_removed_test_and_have_a_reason() -> None:
    before = {
        "internal/cli/wake_test.go": (
            "func TestWakeFirst(t *testing.T) {}\n"
            "func (suite *wakeSuite) TestWakeSecond() {}\n"
        )
    }
    after = {"internal/cli/wake_test.go": ""}
    body = """<!-- BEGIN_WAKE_TEST_CHANGE_JUSTIFICATION -->
TestWakeFirst:
<!-- END_WAKE_TEST_CHANGE_JUSTIFICATION -->
"""

    errors = check_wake_test_changes.validate_change(before, after, body)

    assert any("TestWakeFirst" in error and "reason" in error for error in errors), errors
    assert any("TestWakeSecond" in error for error in errors), errors


def test_ci_runs_the_wake_test_change_gate_on_pull_requests() -> None:
    ci = (SCRIPT.parent.parent / ".github" / "workflows" / "ci.yml").read_text()

    assert "Check wake test changes" in ci
    assert "python3 scripts/check_wake_test_changes.py" in ci
    assert "PR_BASE_SHA: ${{ github.event.pull_request.base.sha }}" in ci
    assert "PR_HEAD_SHA: ${{ github.event.pull_request.head.sha }}" in ci
    assert "PR_BODY: ${{ github.event.pull_request.body }}" in ci

    smoke = (SCRIPT.parent / "smoke-test.sh").read_text()
    assert "python3 scripts/test_check_wake_test_changes.py" in smoke


if __name__ == "__main__":
    test_no_removed_wake_tests_needs_no_justification()
    test_ci_revisions_must_be_commit_shas()
    test_removed_wake_test_requires_named_justification()
    test_rename_requires_the_old_test_name_to_be_justified()
    test_justification_must_cover_every_removed_test_and_have_a_reason()
    test_ci_runs_the_wake_test_change_gate_on_pull_requests()
    print("wake test change checks ok")
