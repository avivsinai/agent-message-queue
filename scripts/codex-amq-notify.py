#!/usr/bin/env python3
"""
Codex CLI AMQ notification hook.

This script is called by Codex's notify hook on events like agent-turn-complete.
It checks for pending AMQ messages and surfaces them via desktop notification.

Setup:
1. Add to ~/.codex/config.toml:
   notify = ["python3", "/path/to/scripts/codex-amq-notify.py"]

2. Ensure amq binary is in PATH or set AMQ_BIN environment variable.

Environment variables:
- AM_ROOT: Root directory for AMQ (default: .agent-mail)
- AM_ME: Agent handle (default: codex)
- AMQ_BIN: Path to amq binary (default: searches PATH)
- AMQ_BULLETIN: Path to bulletin file (default: .agent-mail/meta/amq-bulletin.json)
"""

import json
import os
import subprocess
import sys
from pathlib import Path


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


def send_notification(title: str, message: str) -> None:
    """Send a desktop notification (macOS/Linux)."""
    if sys.platform == "darwin":
        # macOS
        try:
            subprocess.run(
                [
                    "osascript",
                    "-e",
                    f'display notification "{message}" with title "{title}"',
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
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired, json.JSONDecodeError) as e:
        return {"count": 0, "messages": [], "error": str(e)}


def write_bulletin(bulletin_path: str, data: dict) -> None:
    """Write message summary to bulletin file."""
    Path(bulletin_path).parent.mkdir(parents=True, exist_ok=True)
    with open(bulletin_path, "w") as f:
        json.dump(data, f, indent=2)


def main():
    # Parse Codex notification event (passed as JSON in argv[1] or stdin)
    event = {}
    if len(sys.argv) > 1:
        try:
            event = json.loads(sys.argv[1])
        except json.JSONDecodeError:
            pass

    # Only process on agent-turn-complete events (or all events if not specified)
    event_type = event.get("type", "")
    if event_type and event_type != "agent-turn-complete":
        return

    # Configuration
    root = os.environ.get("AM_ROOT", ".agent-mail")
    me = os.environ.get("AM_ME", "codex")
    bulletin_path = os.environ.get(
        "AMQ_BULLETIN", os.path.join(root, "meta", "amq-bulletin.json")
    )

    # Find amq binary
    amq_bin = find_amq_binary()
    if not amq_bin:
        return  # Silently exit if amq not found

    # Check for messages
    result = check_amq_messages(amq_bin, root, me)

    if result["count"] > 0:
        # Write bulletin
        write_bulletin(bulletin_path, result)

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
