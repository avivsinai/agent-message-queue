#!/usr/bin/env python3
"""
Claude Code UserPromptSubmit hook for AMQ.

This hook can surface AMQ inbox context even when Claude is in plan mode
and cannot run shell tools directly.

Defaults:
- Runs only in plan mode (`AMQ_PROMPT_HOOK_MODE=plan`)
- Uses non-destructive inbox peek (`AMQ_PROMPT_HOOK_ACTION=list`)

Optional env vars:
- AMQ_PROMPT_HOOK_MODE: plan | all | nonplan
- AMQ_PROMPT_HOOK_ACTION: list | drain
- AMQ_PROMPT_HOOK_LIMIT: max messages to include (default: 5, max: 20)
- AMQ_PROMPT_HOOK_BODY_CHARS: body snippet length for drain mode (default: 220)
- AMQ_PROMPT_HOOK_MAX_CONTEXT_CHARS: cap injected context (default: 4000)
- AMQ_BIN: path to amq binary
- AM_ROOT / AM_ME: standard AMQ overrides
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Any

DEFAULT_MODE = "plan"
DEFAULT_ACTION = "list"
DEFAULT_LIMIT = 5
MAX_LIMIT = 20
DEFAULT_BODY_CHARS = 220
DEFAULT_MAX_CONTEXT_CHARS = 4000


def _read_event() -> dict[str, Any]:
    raw = sys.stdin.read().strip()
    if not raw:
        return {}
    try:
        parsed = json.loads(raw)
    except json.JSONDecodeError:
        return {}
    return parsed if isinstance(parsed, dict) else {}


def _env_int(name: str, default: int, min_value: int, max_value: int) -> int:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError:
        return default
    if value < min_value:
        return min_value
    if value > max_value:
        return max_value
    return value


def _should_run(event: dict[str, Any]) -> bool:
    mode = os.environ.get("AMQ_PROMPT_HOOK_MODE", DEFAULT_MODE).strip().lower()
    permission_mode = str(event.get("permission_mode", "")).strip().lower()

    if mode in ("", "all"):
        return True
    if mode == "plan":
        return permission_mode == "plan"
    if mode == "nonplan":
        return permission_mode != "plan"
    return False


def _read_amqrc(directory: Path) -> dict[str, str] | None:
    """Read .amqrc JSON from the given directory. Returns None on failure."""
    amqrc_path = directory / ".amqrc"
    try:
        data = json.loads(amqrc_path.read_text(encoding="utf-8"))
        if isinstance(data, dict):
            return data
    except (OSError, json.JSONDecodeError):
        pass
    return None


def _find_amqrc(start: Path) -> tuple[Path, dict[str, str]] | None:
    """Walk upwards from start to find .amqrc. Returns (dir, config) or None."""
    try:
        start = start.resolve()
    except OSError:
        return None
    for parent in [start, *start.parents]:
        cfg = _read_amqrc(parent)
        if cfg is not None:
            return parent, cfg
    return None


def _resolve_root(event: dict[str, Any]) -> str:
    env_root = os.environ.get("AM_ROOT", "").strip()
    if env_root:
        return env_root

    candidates: list[Path] = []
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
        result = _find_amqrc(candidate)
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
        for parent in [resolved, *resolved.parents]:
            agent_mail = parent / ".agent-mail"
            if agent_mail.is_dir():
                return str(agent_mail / "team")

    if isinstance(payload_cwd, str) and payload_cwd:
        return str(Path(payload_cwd) / ".agent-mail" / "team")
    return str(Path(".agent-mail") / "team")


def _inbox_has_new_messages(root: str, me: str) -> bool:
    inbox_new = Path(root) / "agents" / me / "inbox" / "new"
    try:
        for entry in inbox_new.iterdir():
            if entry.is_file() and entry.suffix == ".md" and not entry.name.startswith("."):
                return True
    except OSError:
        return False
    return False


def _find_amq_binary() -> str:
    env_bin = os.environ.get("AMQ_BIN", "").strip()
    if env_bin and Path(env_bin).is_file():
        return env_bin

    local_bin = Path.cwd() / "amq"
    if local_bin.is_file():
        return str(local_bin)

    path_bin = shutil.which("amq")
    return path_bin or ""


def _run_amq(cmd: list[str]) -> dict[str, Any] | list[dict[str, Any]] | None:
    try:
        result = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=6,
            check=True,
        )
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired, FileNotFoundError):
        return None

    out = result.stdout.strip()
    if not out:
        return None
    try:
        parsed = json.loads(out)
    except json.JSONDecodeError:
        return None
    if isinstance(parsed, dict):
        return parsed
    if isinstance(parsed, list):
        return [item for item in parsed if isinstance(item, dict)]
    return None


def _query_messages(amq_bin: str, root: str, me: str, action: str, limit: int) -> tuple[list[dict[str, Any]], bool]:
    query_limit = limit + 1
    if action == "drain":
        cmd = [
            amq_bin,
            "drain",
            "--me",
            me,
            "--root",
            root,
            "--json",
            "--include-body",
            "--limit",
            str(query_limit),
        ]
        parsed = _run_amq(cmd)
        if not isinstance(parsed, dict):
            return [], False
        drained = parsed.get("drained", [])
        if not isinstance(drained, list):
            return [], False
        rows = [item for item in drained if isinstance(item, dict)]
        has_more = len(rows) > limit
        return rows[:limit], has_more

    cmd = [
        amq_bin,
        "list",
        "--me",
        me,
        "--root",
        root,
        "--new",
        "--json",
        "--limit",
        str(query_limit),
    ]
    parsed = _run_amq(cmd)
    if not isinstance(parsed, list):
        return [], False
    rows = [item for item in parsed if isinstance(item, dict)]
    has_more = len(rows) > limit
    return rows[:limit], has_more


def _clean_snippet(raw: str, limit: int) -> str:
    collapsed = " ".join(raw.split())
    if len(collapsed) <= limit:
        return collapsed
    if limit <= 1:
        return collapsed[:limit]
    return collapsed[: limit - 1] + "..."


def _build_context(
    rows: list[dict[str, Any]],
    has_more: bool,
    root: str,
    me: str,
    action: str,
    body_chars: int,
    max_chars: int,
) -> str:
    mode_text = "drained" if action == "drain" else "found"
    lines: list[str] = [
        f"AMQ hook {mode_text} {len(rows)} message(s) for {me} in {root}.",
    ]
    if action == "list":
        lines.append("Run `amq drain --include-body` to ingest full message bodies.")

    for idx, row in enumerate(rows, start=1):
        sender = str(row.get("from", "-")).strip() or "-"
        subject = str(row.get("subject", "")).strip() or "(no subject)"
        msg_id = str(row.get("id", "-")).strip() or "-"
        priority = str(row.get("priority", "-")).strip() or "-"
        kind = str(row.get("kind", "-")).strip() or "-"
        lines.append(f"{idx}. {sender} [{priority}/{kind}] {subject} (id={msg_id})")

        if action == "drain":
            body = row.get("body")
            if isinstance(body, str) and body.strip():
                snippet = _clean_snippet(body, body_chars)
                if snippet:
                    lines.append(f"   body: {snippet}")

    if has_more:
        lines.append("...and more messages are pending (increase AMQ_PROMPT_HOOK_LIMIT to include more).")

    text = "\n".join(lines).strip()
    if len(text) <= max_chars:
        return text
    if max_chars <= 3:
        return text[:max_chars]
    return text[: max_chars - 3] + "..."


def _emit_context(context: str) -> None:
    payload = {
        "hookSpecificOutput": {
            "hookEventName": "UserPromptSubmit",
            "additionalContext": context,
        }
    }
    json.dump(payload, sys.stdout, ensure_ascii=True)
    sys.stdout.write("\n")


def main() -> None:
    event = _read_event()
    if not _should_run(event):
        return

    me = os.environ.get("AM_ME", "claude").strip() or "claude"
    root = _resolve_root(event)

    if not _inbox_has_new_messages(root, me):
        return

    amq_bin = _find_amq_binary()
    if not amq_bin:
        return

    action = os.environ.get("AMQ_PROMPT_HOOK_ACTION", DEFAULT_ACTION).strip().lower()
    if action not in {"list", "drain"}:
        action = DEFAULT_ACTION

    limit = _env_int("AMQ_PROMPT_HOOK_LIMIT", DEFAULT_LIMIT, 1, MAX_LIMIT)
    body_chars = _env_int("AMQ_PROMPT_HOOK_BODY_CHARS", DEFAULT_BODY_CHARS, 40, 1000)
    max_chars = _env_int("AMQ_PROMPT_HOOK_MAX_CONTEXT_CHARS", DEFAULT_MAX_CONTEXT_CHARS, 200, 20000)

    rows, has_more = _query_messages(amq_bin, root, me, action, limit)
    if not rows:
        return

    context = _build_context(rows, has_more, root, me, action, body_chars, max_chars)
    if context:
        _emit_context(context)


if __name__ == "__main__":
    main()
