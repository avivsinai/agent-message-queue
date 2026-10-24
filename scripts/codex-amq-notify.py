#!/usr/bin/env python3
"""
Codex CLI AMQ notification hook.

Codex runs this program from its notify setting after each turn. When the
agent has unread AMQ messages it raises one desktop notification, and again
only when the set of unread messages changes. Use it where `amq wake` cannot
type into the terminal.

Setup:
1. Add to ~/.codex/config.toml:
   notify = ["python3", "/path/to/scripts/codex-amq-notify.py"]

2. Start Codex with `amq coop exec codex`, which sets AM_ROOT and AM_ME.
   Without both the hook does nothing.

Environment variables:
- AM_ROOT, AM_ME: the agent's queue root and handle (required)
- AM_SESSION: shown in the notification title when set
- AMQ_BIN: Path to amq binary (default: searches PATH)
- AMQ_BULLETIN: Path to bulletin file (default: <AM_ROOT>/meta/amq-bulletin-<AM_ME>.json)
- AMQ_NOTIFY_LOG: Optional path to append raw notify JSON (one line per event)
"""

import contextlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Iterator, List, Optional

# HANDLE_RE is an AMQ handle; AM_ME becomes part of a file name.
HANDLE_RE = re.compile(r"[a-z0-9_][a-z0-9_-]*")

try:
    import fcntl
except ImportError:  # no advisory locks on this platform
    fcntl = None


def find_amq_binary() -> str:
    """Find the amq binary: AMQ_BIN, else PATH."""
    if amq_bin := os.environ.get("AMQ_BIN"):
        if Path(amq_bin).is_file():
            return amq_bin
    return shutil.which("amq") or ""


def inbox_has_messages(root: str, me: str) -> bool:
    """Fast path: check for .md files in inbox/new without invoking amq."""
    inbox_new = Path(root) / "agents" / me / "inbox" / "new"
    try:
        for entry in inbox_new.iterdir():
            if entry.is_file() and entry.name.endswith(".md") and not entry.name.startswith("."):
                return True
    except OSError:
        return False
    return False


def log_event(event: dict) -> None:
    """Append raw event payload to AMQ_NOTIFY_LOG if configured."""
    log_path = os.environ.get("AMQ_NOTIFY_LOG")
    if not log_path:
        return
    try:
        Path(log_path).parent.mkdir(parents=True, exist_ok=True)
        with open(log_path, "a", encoding="utf-8") as f:
            f.write(json.dumps(event, sort_keys=True) + "\n")
    except OSError:
        pass


def escape_applescript_string(s: str) -> str:
    """Escape a string for safe use in AppleScript double-quoted strings."""
    s = s.replace("\\", "\\\\")  # Escape backslashes first
    s = s.replace('"', '\\"')    # Escape double quotes
    return s


def send_notification(title: str, message: str) -> None:
    """Send a desktop notification (macOS/Linux)."""
    if sys.platform == "darwin":
        # macOS - escape strings to prevent AppleScript injection
        safe_title = escape_applescript_string(title)
        safe_message = escape_applescript_string(message)
        try:
            subprocess.run(
                [
                    "osascript",
                    "-e",
                    f'display notification "{safe_message}" with title "{safe_title}"',
                ],
                check=False,
            )
        except FileNotFoundError:
            pass
    else:
        # Linux (requires notify-send)
        try:
            subprocess.run(["notify-send", title, message], check=False)
        except FileNotFoundError:
            pass


def check_amq_messages(amq_bin: str, root: str, me: str) -> dict:
    """List unread messages without moving them."""
    try:
        result = subprocess.run(
            [amq_bin, "--no-update-check", "list", "--me", me, "--root", root, "--new", "--json"],
            capture_output=True,
            text=True,
            check=True,
            timeout=5,
        )
        messages = json.loads(result.stdout) if result.stdout.strip() else []
        return {"count": len(messages), "messages": messages}
    except subprocess.CalledProcessError as e:
        detail = (e.stderr or "").strip() or (e.stdout or "").strip() or str(e)
        return {"count": 0, "messages": [], "error": detail}
    except (subprocess.TimeoutExpired, json.JSONDecodeError) as e:
        return {"count": 0, "messages": [], "error": str(e)}


def message_ids(data: dict) -> List[str]:
    """The sorted ids of a message listing."""
    return sorted(str(m.get("id", "")) for m in data.get("messages", []))


def read_bulletin_ids(bulletin_path: str) -> Optional[List[str]]:
    """The ids the previous bulletin recorded, or None without one."""
    try:
        with open(bulletin_path, encoding="utf-8") as f:
            return message_ids(json.load(f))
    except (OSError, ValueError, AttributeError):
        return None


def write_bulletin(bulletin_path: str, data: dict) -> None:
    """Write the message summary atomically, mode 0600, through a temp file
    of its own name."""
    parent = Path(bulletin_path).parent
    parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd, tmp_path = tempfile.mkstemp(dir=parent, prefix=".amq-bulletin.", suffix=".tmp")
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(data, f, indent=2)
        os.replace(tmp_path, bulletin_path)
    except Exception:
        if os.path.exists(tmp_path):
            os.unlink(tmp_path)
        raise


@contextlib.contextmanager
def bulletin_lock(bulletin_path: str) -> Iterator[None]:
    """Hold <bulletin>.lock, so two turns' compare-and-write of one bulletin
    run one after the other. The lock file is never removed."""
    if fcntl is None:
        yield
        return
    Path(bulletin_path).parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd = os.open(bulletin_path + ".lock", os.O_RDWR | os.O_CREAT, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)
        yield
    finally:
        os.close(fd)


def clear_bulletin(bulletin_path: str) -> None:
    """Remove bulletin file when there are no messages."""
    try:
        os.unlink(bulletin_path)
    except FileNotFoundError:
        pass


def observe(amq_bin: str, root: str, me: str, bulletin_path: str) -> Optional[dict]:
    """Read the mailbox and update the bulletin. Returns the listing to notify
    about, or None: no unread mail, an error, or the set the last bulletin
    recorded. Run under bulletin_lock."""
    # Fast path: if inbox/new is empty or missing, clear bulletin.
    if not inbox_has_messages(root, me):
        clear_bulletin(bulletin_path)
        return None
    result = check_amq_messages(amq_bin, root, me)
    if result.get("error"):
        # Preserve bulletin on errors to avoid hiding prior messages/state.
        print(f"[AMQ] notify hook error: {result['error']}", file=sys.stderr)
        return None
    if result["count"] == 0:
        clear_bulletin(bulletin_path)
        return None
    # One notification per set of unread messages: the same set as the last
    # bulletin writes nothing and notifies nobody.
    if read_bulletin_ids(bulletin_path) == message_ids(result):
        return None
    write_bulletin(bulletin_path, result)
    return result


def main():
    # Parse Codex notification event (passed as JSON in argv[1] or stdin)
    event = {}
    if len(sys.argv) > 1:
        try:
            event = json.loads(sys.argv[1])
        except json.JSONDecodeError:
            pass
    elif not sys.stdin.isatty():
        try:
            stdin_data = sys.stdin.read().strip()
            if stdin_data:
                event = json.loads(stdin_data)
        except (json.JSONDecodeError, IOError):
            pass
    if not isinstance(event, dict):
        event = {}

    event_type = event.get("type", "")
    if event_type and event_type != "agent-turn-complete":
        return

    # The agent's identity comes from the Codex environment; without it the
    # hook has no mailbox and does nothing.
    root = os.environ.get("AM_ROOT", "").strip()
    me = os.environ.get("AM_ME", "").strip()
    if not root or not me or not HANDLE_RE.fullmatch(me):
        return
    if event:
        log_event(event)

    # One bulletin per handle: a broadcast reaches several handles of one
    # root with the same message id, and each must notify once.
    bulletin_path = os.environ.get(
        "AMQ_BULLETIN", os.path.join(root, "meta", f"amq-bulletin-{me}.json")
    )

    amq_bin = find_amq_binary()
    if not amq_bin:
        return
    # One critical section from the mailbox read to the bulletin update: a
    # turn that saw an empty inbox must not clear the state of a turn that
    # has since notified.
    with bulletin_lock(bulletin_path):
        result = observe(amq_bin, root, me, bulletin_path)
    if result is None:
        return
    urgent = sum(1 for m in result["messages"] if m.get("priority") == "urgent")
    normal = sum(1 for m in result["messages"] if m.get("priority") == "normal")
    low = result["count"] - urgent - normal

    session = os.environ.get("AM_SESSION", "").strip()
    session_tag = f" [{session}]" if session else ""
    title = f"AMQ{session_tag}: {result['count']} message(s)"
    parts = []
    if urgent:
        parts.append(f"{urgent} urgent")
    if normal:
        parts.append(f"{normal} normal")
    if low:
        parts.append(f"{low} low")
    message = ", ".join(parts) if parts else f"{result['count']} new"

    send_notification(title, message)

    print(f"[AMQ{session_tag}] {result['count']} pending message(s): {message}")
    for msg in result["messages"][:3]:
        priority = msg.get("priority", "-")
        subject = msg.get("subject", "(no subject)")
        from_agent = msg.get("from", "?")
        print(f"  - [{priority}] {from_agent}: {subject}")
    if result["count"] > 3:
        print(f"  ... and {result['count'] - 3} more")


if __name__ == "__main__":
    main()
