#!/usr/bin/env python3
"""
Codex CLI AMQ notification hook.

This script is called by Codex's notify hook on events like agent-turn-complete.
It checks for pending AMQ messages and surfaces them via desktop notification.
Background terminals do not wake Codex; use this hook for reliable co-op alerts.

Setup:
1. Add to ~/.codex/config.toml:
   notify = ["python3", "/path/to/scripts/codex-amq-notify.py"]

2. Ensure amq binary is in PATH or set AMQ_BIN environment variable.

Environment variables:
- AM_ROOT: Root directory for AMQ (overrides discovery from notify payload `cwd`)
- AM_ME: Agent handle (default: codex)
- AMQ_BIN: Path to amq binary (default: searches PATH)
- AMQ_BULLETIN: Path to bulletin file (default: <AM_ROOT>/meta/amq-bulletin.json)
- AMQ_NOTIFY_LOG: Optional path to append raw notify JSON (one line per event)
"""

import json
import os
import subprocess
import sys
from pathlib import Path
from typing import List, Optional


def find_amq_binary() -> str:
    """Find the amq binary."""
    # Check environment variable first
    if amq_bin := os.environ.get("AMQ_BIN"):
        if Path(amq_bin).is_file():
            return amq_bin

    # Check common locations
    candidates = [
        "./amq",
        Path.home() / ".local" / "bin" / "amq",
        "/usr/local/bin/amq",
    ]
    for candidate in candidates:
        if Path(candidate).is_file():
            return str(candidate)

    # Try PATH
    try:
        result = subprocess.run(
            ["which", "amq"], capture_output=True, text=True, check=True
        )
        return result.stdout.strip()
    except subprocess.CalledProcessError:
        pass

    return ""


def read_amqrc(directory: Path) -> Optional[dict]:
    """Read .amqrc JSON from the given directory. Returns None on failure."""
    amqrc_path = directory / ".amqrc"
    try:
        data = json.loads(amqrc_path.read_text(encoding="utf-8"))
        if isinstance(data, dict):
            return data
    except (OSError, json.JSONDecodeError):
        pass
    return None


def find_amqrc(start: Path) -> Optional[tuple]:
    """Walk upwards from start to find .amqrc. Returns (dir, config) or None."""
    try:
        start = start.resolve()
    except OSError:
        return None
    for parent in [start] + list(start.parents):
        cfg = read_amqrc(parent)
        if cfg is not None:
            return parent, cfg
    return None


def resolve_root(event: dict) -> str:
    """Resolve AM_ROOT with env override, .amqrc, and .agent-mail discovery."""
    if env_root := os.environ.get("AM_ROOT"):
        return env_root

    candidates: List[Path] = []
    payload_cwd = event.get("cwd")
    if isinstance(payload_cwd, str) and payload_cwd:
        candidates.append(Path(payload_cwd))
    try:
        candidates.append(Path.cwd())
    except FileNotFoundError:
        pass

    # For each candidate, try .amqrc first then .agent-mail directory.
    # This ensures payload cwd is fully resolved before falling back to process cwd.
    for candidate in candidates:
        # Try .amqrc resolution (base + default_session)
        result = find_amqrc(candidate)
        if result is not None:
            rc_dir, cfg = result
            base = cfg.get("root", "")
            if base:
                base_path = Path(base) if Path(base).is_absolute() else rc_dir / base
                session = cfg.get("default_session", "team")
                return str(base_path / session)

        # Fallback: look for .agent-mail directory and append default session
        try:
            resolved = candidate.resolve()
        except OSError:
            continue
        for parent in [resolved] + list(resolved.parents):
            agent_mail = parent / ".agent-mail"
            if agent_mail.is_dir():
                return str(agent_mail / "team")

    if isinstance(payload_cwd, str) and payload_cwd:
        return os.path.join(payload_cwd, ".agent-mail", "team")
    return os.path.join(".agent-mail", "team")


def inbox_has_messages(root: str, me: str) -> bool:
    """Fast path: check for .md files in inbox/new without invoking amq."""
    inbox_new = Path(root) / "agents" / me / "inbox" / "new"
    try:
        for entry in inbox_new.iterdir():
            if entry.is_file() and entry.name.endswith(".md") and not entry.name.startswith("."):
                return True
    except FileNotFoundError:
        return False
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
    """Escape a string for safe use in AppleScript double-quoted strings.

    AppleScript uses backslash escapes within double-quoted strings.
    We escape backslashes first, then double quotes.
    """
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
    """Check for pending AMQ messages using drain (non-destructive peek via list)."""
    try:
        # Use list to peek at messages without moving them
        result = subprocess.run(
            [amq_bin, "list", "--me", me, "--root", root, "--new", "--json"],
            capture_output=True,
            text=True,
            check=True,
            timeout=5,
        )
        messages = json.loads(result.stdout) if result.stdout.strip() else []
        return {"count": len(messages), "messages": messages}
    except subprocess.CalledProcessError as e:
        stderr = (e.stderr or "").strip()
        stdout = (e.stdout or "").strip()
        detail = stderr or stdout or str(e)
        if "--me is required" in detail:
            detail = f"{detail}. Set AM_ME=codex (or your handle)."
        return {"count": 0, "messages": [], "error": detail}
    except (subprocess.TimeoutExpired, json.JSONDecodeError) as e:
        return {"count": 0, "messages": [], "error": str(e)}


def write_bulletin(bulletin_path: str, data: dict, am_root: str) -> None:
    """Write message summary to bulletin file with secure permissions."""
    # Ensure AM_ROOT and all parent directories have 0700 permissions
    parent = Path(bulletin_path).parent
    parent.mkdir(parents=True, exist_ok=True)

    # chmod AM_ROOT (e.g., .agent-mail) if it exists
    root_path = Path(am_root)
    if root_path.exists():
        os.chmod(root_path, 0o700)

    # chmod the meta directory
    os.chmod(parent, 0o700)

    # Write file atomically with 0600 permissions
    tmp_path = bulletin_path + ".tmp"
    fd = os.open(tmp_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(data, f, indent=2)
        os.rename(tmp_path, bulletin_path)
    except Exception:
        if os.path.exists(tmp_path):
            os.unlink(tmp_path)
        raise


def clear_bulletin(bulletin_path: str) -> None:
    """Remove bulletin file when there are no messages."""
    try:
        os.unlink(bulletin_path)
    except FileNotFoundError:
        pass


def main():
    # Parse Codex notification event (passed as JSON in argv[1] or stdin)
    event = {}
    if len(sys.argv) > 1:
        try:
            event = json.loads(sys.argv[1])
        except json.JSONDecodeError:
            pass
    elif not sys.stdin.isatty():
        # Read from stdin if available
        try:
            stdin_data = sys.stdin.read().strip()
            if stdin_data:
                event = json.loads(stdin_data)
        except (json.JSONDecodeError, IOError):
            pass

    # Only process on agent-turn-complete events (or all events if not specified)
    event_type = event.get("type", "")
    if event_type and event_type != "agent-turn-complete":
        return
    if event:
        log_event(event)

    # Configuration
    root = resolve_root(event)
    me = os.environ.get("AM_ME") or "codex"
    bulletin_path = os.environ.get(
        "AMQ_BULLETIN", os.path.join(root, "meta", "amq-bulletin.json")
    )

    # Fast path: if inbox/new is empty or missing, clear bulletin and exit quickly.
    if not inbox_has_messages(root, me):
        clear_bulletin(bulletin_path)
        return

    # Find amq binary
    amq_bin = find_amq_binary()
    if not amq_bin:
        return  # Silently exit if amq not found

    # Check for messages
    result = check_amq_messages(amq_bin, root, me)

    if result.get("error"):
        # Preserve bulletin on errors to avoid hiding prior messages/state.
        print(f"[AMQ] notify hook error: {result['error']}", file=sys.stderr)
        return

    if result["count"] == 0:
        # Clear stale bulletin when no messages
        clear_bulletin(bulletin_path)
        return

    # Write bulletin
    write_bulletin(bulletin_path, result, root)

    # Count by priority
    urgent = sum(1 for m in result["messages"] if m.get("priority") == "urgent")
    normal = sum(1 for m in result["messages"] if m.get("priority") == "normal")
    low = result["count"] - urgent - normal

    # Send notification
    title = f"AMQ: {result['count']} message(s)"
    parts = []
    if urgent:
        parts.append(f"{urgent} urgent")
    if normal:
        parts.append(f"{normal} normal")
    if low:
        parts.append(f"{low} low")
    message = ", ".join(parts) if parts else f"{result['count']} new"

    send_notification(title, message)

    # Also print to stdout for Codex to see
    print(f"[AMQ] {result['count']} pending message(s): {message}")
    for msg in result["messages"][:3]:  # Show first 3
        priority = msg.get("priority", "-")
        subject = msg.get("subject", "(no subject)")
        from_agent = msg.get("from", "?")
        print(f"  - [{priority}] {from_agent}: {subject}")
    if result["count"] > 3:
        print(f"  ... and {result['count'] - 3} more")


if __name__ == "__main__":
    main()
