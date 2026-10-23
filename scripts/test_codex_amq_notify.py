#!/usr/bin/env python3
"""Regression test for the Codex notify fallback (hook audit S6).

Observed defects: the script found its root through its own Python
discovery, which appended a "collab" session the Go CLI does not use, so it
watched a different queue; and it raised a desktop notification after every
turn while the same mail stayed unread. With AM_ROOT and AM_ME set and one
unread message, it notifies once; a second turn with the same unread
message notifies nobody.
"""

from __future__ import annotations

import importlib.util
import json
import os
import pathlib
import sys
import tempfile


SCRIPT = pathlib.Path(__file__).with_name("codex-amq-notify.py")
spec = importlib.util.spec_from_file_location("codex_amq_notify", SCRIPT)
if spec is None or spec.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
notify = importlib.util.module_from_spec(spec)
spec.loader.exec_module(notify)


def test_notifies_once_per_set_of_unread_messages() -> None:
    with tempfile.TemporaryDirectory() as tmp:
        root = pathlib.Path(tmp) / "root"
        inbox = root / "agents" / "codex" / "inbox" / "new"
        inbox.mkdir(parents=True)
        (inbox / "m1.md").write_text("x", encoding="utf-8")
        sent: list[tuple[str, str]] = []
        listing = {"count": 1, "messages": [{"id": "m1", "priority": "normal", "from": "claude", "subject": "hi"}]}
        notify.find_amq_binary = lambda: "amq"
        notify.check_amq_messages = lambda amq_bin, r, me: (
            listing if (r, me) == (str(root), "codex") else {"count": 0, "messages": [], "error": "wrong mailbox"}
        )
        notify.send_notification = lambda title, message: sent.append((title, message))
        saved_env, saved_argv = dict(os.environ), sys.argv
        try:
            os.environ.update({"AM_ROOT": str(root), "AM_ME": "codex"})
            for key in ("AMQ_BULLETIN", "AMQ_NOTIFY_LOG", "AM_SESSION"):
                os.environ.pop(key, None)
            sys.argv = ["codex-amq-notify.py", json.dumps({"type": "agent-turn-complete", "cwd": tmp})]
            notify.main()
            notify.main()
        finally:
            os.environ.clear()
            os.environ.update(saved_env)
            sys.argv = saved_argv
        assert len(sent) == 1, sent


if __name__ == "__main__":
    test_notifies_once_per_set_of_unread_messages()
    print("codex notify tests ok")
