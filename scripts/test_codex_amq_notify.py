#!/usr/bin/env python3
"""Regression tests for the Codex notify fallback (hook audit S6).

Each row reproduces an observed defect:
- the script watched a root of its own discovery and notified after every
  turn while the same mail stayed unread (S6, field);
- one bulletin per root: a broadcast's second handle never notified, and an
  empty inbox of one handle cleared the other's state (#1198 review);
- an unlocked compare-and-write with a fixed temp name: two turns at once
  both notified, or one failed its rename (#1198 review).
"""

from __future__ import annotations

import importlib.util
import json
import os
import pathlib
import sys
import tempfile
import threading


SCRIPT = pathlib.Path(__file__).with_name("codex-amq-notify.py")
spec = importlib.util.spec_from_file_location("codex_amq_notify", SCRIPT)
if spec is None or spec.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
notify = importlib.util.module_from_spec(spec)
spec.loader.exec_module(notify)


class Fixture:
    """A scratch root whose handles each list the messages in unread[handle];
    notifications land in sent as (handle, title)."""

    def __init__(self, tmp: str) -> None:
        self.root = pathlib.Path(tmp) / "root"
        self.unread: dict[str, list[str]] = {}
        self.sent: list[tuple[str, str]] = []
        notify.find_amq_binary = lambda: "amq"
        notify.check_amq_messages = self.listing
        notify.send_notification = lambda title, message: self.sent.append((os.environ["AM_ME"], title))

    def listing(self, amq_bin: str, root: str, me: str) -> dict:
        if root != str(self.root):
            return {"count": 0, "messages": [], "error": "wrong root"}
        ids = self.unread.get(me, [])
        return {"count": len(ids), "messages": [{"id": i, "priority": "normal"} for i in ids]}

    def deliver(self, me: str, *ids: str) -> None:
        inbox = self.root / "agents" / me / "inbox" / "new"
        inbox.mkdir(parents=True, exist_ok=True)
        for i in ids:
            (inbox / f"{i}.md").write_text("x", encoding="utf-8")
        self.unread[me] = self.unread.get(me, []) + list(ids)

    def drain(self, me: str) -> None:
        for f in (self.root / "agents" / me / "inbox" / "new").iterdir():
            f.unlink()
        self.unread[me] = []

    def turn(self, me: str) -> None:
        os.environ.update({"AM_ROOT": str(self.root), "AM_ME": me})
        notify.main()


def run(test) -> None:
    saved_env, saved_argv = dict(os.environ), sys.argv
    read = notify.read_bulletin_ids
    try:
        for key in ("AMQ_BULLETIN", "AMQ_NOTIFY_LOG", "AM_SESSION"):
            os.environ.pop(key, None)
        sys.argv = ["codex-amq-notify.py", json.dumps({"type": "agent-turn-complete"})]
        with tempfile.TemporaryDirectory() as tmp:
            test(Fixture(tmp))
    finally:
        notify.read_bulletin_ids = read
        os.environ.clear()
        os.environ.update(saved_env)
        sys.argv = saved_argv


def test_notifies_once_per_set_of_unread_messages(f: Fixture) -> None:
    f.deliver("codex", "m1")
    f.turn("codex")
    f.turn("codex")
    assert f.sent == [("codex", "AMQ: 1 message(s)")], f.sent


def test_each_handle_keeps_its_own_state(f: Fixture) -> None:
    f.deliver("codex", "b1")
    f.deliver("claude", "b1")  # one broadcast, one id, two handles
    f.turn("codex")
    f.turn("claude")
    assert [me for me, _ in f.sent] == ["codex", "claude"], f.sent
    f.drain("claude")
    f.turn("claude")  # an empty inbox clears only its own state
    f.turn("codex")
    assert len(f.sent) == 2, f.sent


def test_two_turns_at_once_notify_once(f: Fixture) -> None:
    f.deliver("codex", "m1")
    # Both turns try to read the bulletin before either writes it; under the
    # lock the second waits, the meeting times out, and it reads the first's.
    meet = threading.Barrier(2, timeout=0.5)
    read = notify.read_bulletin_ids

    def racing_read(path: str):
        try:
            meet.wait()
        except threading.BrokenBarrierError:
            pass
        return read(path)

    errors: list[BaseException] = []

    def turn() -> None:
        try:
            notify.main()
        except BaseException as e:  # a shared temp file name failed the rename
            errors.append(e)

    notify.read_bulletin_ids = racing_read
    os.environ.update({"AM_ROOT": str(f.root), "AM_ME": "codex"})
    turns = [threading.Thread(target=turn) for _ in range(2)]
    for t in turns:
        t.start()
    for t in turns:
        t.join()
    assert not errors and len(f.sent) == 1, (errors, f.sent)


if __name__ == "__main__":
    for test in (test_notifies_once_per_set_of_unread_messages, test_each_handle_keeps_its_own_state, test_two_turns_at_once_notify_once):
        run(test)
    print("codex notify tests ok")
