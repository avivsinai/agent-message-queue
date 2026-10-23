#!/usr/bin/env python3
"""Regression tests for the Codex notify fallback (hook audit S6).

Each row reproduces an observed defect:
- the script watched a root of its own discovery and notified after every
  turn while the same mail stayed unread (S6, field);
- one bulletin per root: a broadcast's second handle never notified, and an
  empty inbox of one handle cleared the other's state (#1198 review);
- an unlocked compare-and-write with a fixed temp name: two turns at once
  both notified, or one failed its rename (#1198 review);
- the mailbox read outside the lock: a turn that saw an empty inbox cleared
  the state of a turn that notified meanwhile, so the next turn notified
  again (#1198 review).
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
    try:
        for key in ("AMQ_BULLETIN", "AMQ_NOTIFY_LOG", "AM_SESSION"):
            os.environ.pop(key, None)
        sys.argv = ["codex-amq-notify.py", json.dumps({"type": "agent-turn-complete"})]
        with tempfile.TemporaryDirectory() as tmp:
            test(Fixture(tmp))
    finally:
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


def held_race(f: Fixture, name: str, first_result=None) -> None:
    """Run turn A until it returns from notify.<name> and hold it there, then
    run turn B; release A once B finished its turn or blocked on the lock.
    first_result, when set, replaces A's result of <name>."""
    real = getattr(notify, name)
    a_in, b_in, release = threading.Event(), threading.Event(), threading.Event()
    calls = []

    def held(*args):
        calls.append(None)
        if len(calls) == 1:  # turn A
            out = real(*args) if first_result is None else first_result
            a_in.set()
            release.wait(5)
            return out
        b_in.set()
        return real(*args)

    errors: list[BaseException] = []

    def turn() -> None:
        try:
            notify.main()
        except BaseException as e:
            errors.append(e)

    setattr(notify, name, held)
    try:
        os.environ.update({"AM_ROOT": str(f.root), "AM_ME": "codex"})
        a = threading.Thread(target=turn)
        a.start()
        assert a_in.wait(5), "turn A never reached " + name
        b = threading.Thread(target=turn)
        b.start()
        # Under the lock B blocks before <name> and this times out; without
        # it B reaches <name> and finishes its turn before A goes on.
        if b_in.wait(0.5):
            b.join(5)
        release.set()
        a.join()
        b.join()
    finally:
        setattr(notify, name, real)
    assert not errors, errors


def test_two_turns_at_once_notify_once(f: Fixture) -> None:
    f.deliver("codex", "m1")
    held_race(f, "read_bulletin_ids")  # A holds after its read, before its write
    assert len(f.sent) == 1, f.sent


def test_an_empty_snapshot_does_not_clear_a_later_notice(f: Fixture) -> None:
    f.deliver("codex", "m1")
    # A saw an empty inbox and holds before it clears; B lists m1 and notifies.
    held_race(f, "inbox_has_messages", first_result=False)
    f.turn("codex")  # m1 is still the unread set: no second notice
    assert len(f.sent) == 1, f.sent


if __name__ == "__main__":
    for test in (
        test_notifies_once_per_set_of_unread_messages,
        test_each_handle_keeps_its_own_state,
        test_two_turns_at_once_notify_once,
        test_an_empty_snapshot_does_not_clear_a_later_notice,
    ):
        run(test)
    print("codex notify tests ok")
