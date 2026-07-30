#!/usr/bin/env python3
"""Tests for the wake-test deletion PR-body gate."""

from __future__ import annotations

import importlib.util
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile


SCRIPT = pathlib.Path(__file__).with_name("check_wake_test_changes.py")
spec = importlib.util.spec_from_file_location("check_wake_test_changes", SCRIPT)
if spec is None or spec.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
check_wake_test_changes = importlib.util.module_from_spec(spec)
spec.loader.exec_module(check_wake_test_changes)


def run_git(repo: pathlib.Path, *args: str) -> str:
    return subprocess.run(
        ["git", *args],
        cwd=repo,
        check=True,
        text=True,
        capture_output=True,
    ).stdout.strip()


def write_wake_test(repo: pathlib.Path, name: str | None) -> None:
    path = repo / "internal" / "cli" / "wake_test.go"
    path.parent.mkdir(parents=True, exist_ok=True)
    if name is None:
        path.unlink(missing_ok=True)
        return
    path.write_text(
        f"package cli\n\nfunc {name}(t *testing.T) {{}}\n",
        encoding="utf-8",
    )


def commit_all(repo: pathlib.Path, message: str) -> str:
    run_git(repo, "add", "-A")
    run_git(repo, "commit", "-m", message)
    return run_git(repo, "rev-parse", "HEAD")


def run_checker(
    repo: pathlib.Path,
    base: str,
    head: str,
    body: str = "",
) -> subprocess.CompletedProcess[str]:
    env = os.environ.copy()
    env.update(PR_BASE_SHA=base, PR_HEAD_SHA=head, PR_BODY=body)
    return subprocess.run(
        [sys.executable, str(SCRIPT)],
        cwd=repo,
        env=env,
        text=True,
        capture_output=True,
    )


def initialized_repo() -> pathlib.Path:
    repo = pathlib.Path(tempfile.mkdtemp(prefix="wake-test-change-gate-"))
    run_git(repo, "init", "-b", "main")
    run_git(repo, "config", "user.name", "AMQ Test")
    run_git(repo, "config", "user.email", "amq-test@example.invalid")
    write_wake_test(repo, "TestWakeOriginal")
    commit_all(repo, "base")
    return repo


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


def test_wake_paths_include_coop_and_doctor_tests() -> None:
    before = {
        "internal/cli/coop_wake_reclaim_unix_test.go": (
            "func TestCoopWakeConflict(t *testing.T) {}\n"
        ),
        "internal/cli/doctor_stale_wake_binary_test.go": (
            "func TestDoctorWakeBinary(t *testing.T) {}\n"
        ),
    }
    after = {path: "" for path in before}

    errors = check_wake_test_changes.validate_change(before, after, "")

    assert errors == [
        "removed or renamed wake tests require a WAKE_TEST_CHANGE_JUSTIFICATION "
        "block: TestCoopWakeConflict, TestDoctorWakeBinary"
    ], errors


def test_checker_compares_topic_to_merge_base() -> None:
    repo = initialized_repo()
    try:
        topic = run_git(repo, "rev-parse", "HEAD")
        write_wake_test(repo, "TestWakeBaseOnly")
        base_tip = commit_all(repo, "rename on base")

        result = run_checker(repo, base_tip, topic)

        assert result.returncode == 0, result.stderr
    finally:
        shutil.rmtree(repo)


def test_checker_detects_topic_deletion_also_made_on_base() -> None:
    repo = initialized_repo()
    try:
        fork = run_git(repo, "rev-parse", "HEAD")
        run_git(repo, "switch", "-c", "topic")
        write_wake_test(repo, None)
        topic = commit_all(repo, "delete on topic")
        run_git(repo, "switch", "main")
        run_git(repo, "reset", "--hard", fork)
        write_wake_test(repo, None)
        base_tip = commit_all(repo, "delete on base")

        result = run_checker(repo, base_tip, topic)

        assert result.returncode != 0, result.stdout
        assert "TestWakeOriginal" in result.stderr, result.stderr

        justified = run_checker(
            repo,
            base_tip,
            topic,
            """BEGIN_WAKE_TEST_CHANGE_JUSTIFICATION
TestWakeOriginal: removed from the topic branch
END_WAKE_TEST_CHANGE_JUSTIFICATION
""",
        )
        assert justified.returncode == 0, justified.stderr
    finally:
        shutil.rmtree(repo)


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
    test_wake_paths_include_coop_and_doctor_tests()
    test_checker_compares_topic_to_merge_base()
    test_checker_detects_topic_deletion_also_made_on_base()
    test_ci_runs_the_wake_test_change_gate_on_pull_requests()
    print("wake test change checks ok")
