#!/usr/bin/env python3
"""Tests for session name resolution in AMQ hook scripts.

Covers the duplicated Python session-detection logic in both
codex-amq-notify.py and claude-amq-user-prompt-submit.py, ensuring
parity with Go's classifyRoot / resolveSessionName.
"""

import importlib.util
import json
import os
import sys
import tempfile
from pathlib import Path

# Import the helpers from both scripts via importlib (they aren't packages)
_SCRIPTS_DIR = Path(__file__).resolve().parent


def _load_module(name: str, filename: str):
    spec = importlib.util.spec_from_file_location(name, _SCRIPTS_DIR / filename)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


codex_notify = _load_module("codex_notify", "codex-amq-notify.py")
claude_hook = _load_module("claude_hook", "claude-amq-user-prompt-submit.py")


class TestResolveSessionName:
    """Tests for resolve_session_name / _resolve_session_name."""

    def test_non_session_root_returns_empty(self, tmp_path: Path):
        """Plain root (no .agent-mail parent, no siblings) returns ''."""
        root = tmp_path / "plain-root"
        root.mkdir()
        (root / "agents").mkdir()

        # Clear env to avoid interference
        env_backup = os.environ.pop("AM_BASE_ROOT", None)
        try:
            assert codex_notify.resolve_session_name(str(root)) == ""
            assert claude_hook._resolve_session_name(str(root)) == ""
        finally:
            if env_backup is not None:
                os.environ["AM_BASE_ROOT"] = env_backup

    def test_agent_mail_session_detected(self, tmp_path: Path):
        """.agent-mail/<session> layout is detected by parent name."""
        agent_mail = tmp_path / ".agent-mail"
        session = agent_mail / "collab"
        session.mkdir(parents=True)
        (session / "agents").mkdir()

        env_backup = os.environ.pop("AM_BASE_ROOT", None)
        try:
            assert codex_notify.resolve_session_name(str(session)) == "collab"
            assert claude_hook._resolve_session_name(str(session)) == "collab"
        finally:
            if env_backup is not None:
                os.environ["AM_BASE_ROOT"] = env_backup

    def test_sibling_session_detected(self, tmp_path: Path):
        """Session detected via sibling dirs with agents/ subdirectory."""
        base = tmp_path / "custom-root"
        session_a = base / "stream1"
        session_b = base / "stream2"
        session_a.mkdir(parents=True)
        session_b.mkdir(parents=True)
        (session_a / "agents").mkdir()
        (session_b / "agents").mkdir()

        env_backup = os.environ.pop("AM_BASE_ROOT", None)
        try:
            assert codex_notify.resolve_session_name(str(session_a)) == "stream1"
            assert claude_hook._resolve_session_name(str(session_a)) == "stream1"
        finally:
            if env_backup is not None:
                os.environ["AM_BASE_ROOT"] = env_backup

    def test_am_base_root_detection(self, tmp_path: Path):
        """AM_BASE_ROOT env var makes a direct child a session."""
        base = tmp_path / "myroot"
        session = base / "mysession"
        session.mkdir(parents=True)
        (session / "agents").mkdir()

        old_val = os.environ.get("AM_BASE_ROOT")
        os.environ["AM_BASE_ROOT"] = str(base)
        try:
            assert codex_notify.resolve_session_name(str(session)) == "mysession"
            assert claude_hook._resolve_session_name(str(session)) == "mysession"
        finally:
            if old_val is None:
                os.environ.pop("AM_BASE_ROOT", None)
            else:
                os.environ["AM_BASE_ROOT"] = old_val

    def test_amqrc_derived_base_root(self, tmp_path: Path):
        """Session detected via .amqrc in parent directory."""
        project = tmp_path / "project"
        project.mkdir()

        # Write .amqrc pointing to custom-mail as root
        amqrc = project / ".amqrc"
        amqrc.write_text(json.dumps({"root": "custom-mail"}))

        # Create session layout under the .amqrc-configured root
        base = project / "custom-mail"
        session = base / "auth"
        session.mkdir(parents=True)
        (session / "agents").mkdir()

        env_backup = os.environ.pop("AM_BASE_ROOT", None)
        try:
            assert codex_notify.resolve_session_name(str(session)) == "auth"
            assert claude_hook._resolve_session_name(str(session)) == "auth"
        finally:
            if env_backup is not None:
                os.environ["AM_BASE_ROOT"] = env_backup


if __name__ == "__main__":
    # Simple runner for environments without pytest
    import traceback

    passed = 0
    failed = 0
    errors = []

    suite = TestResolveSessionName()
    for name in dir(suite):
        if not name.startswith("test_"):
            continue
        method = getattr(suite, name)
        with tempfile.TemporaryDirectory() as tmp:
            try:
                method(Path(tmp))
                passed += 1
                print(f"  PASS  {name}")
            except Exception:
                failed += 1
                errors.append((name, traceback.format_exc()))
                print(f"  FAIL  {name}")

    print(f"\n{passed} passed, {failed} failed")
    for name, tb in errors:
        print(f"\n--- {name} ---\n{tb}")

    sys.exit(1 if failed else 0)
