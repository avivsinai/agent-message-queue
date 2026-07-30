#!/usr/bin/python3
"""Observe one exact AMQ doorbell prompt without touching mailbox contents."""

import json
import os
import re
import socket
import stat
import sys


MAX_HOOK_INPUT = 64 * 1024
MAX_LOCK_INPUT = 16 * 1024
PROMPT_RE = re.compile(
    r": AMQ doorbell v1 token=([0-9a-f]{32}) "
    r"run amq drain --include-body then act on it"
)
AGENT_RE = re.compile(r"[A-Za-z0-9._-]+")


def _read_bounded(stream, limit):
    data = stream.read(limit + 1)
    if len(data) > limit:
        raise ValueError("input too large")
    return data


def _owned_private_directory(path):
    info = os.lstat(path)
    return (
        stat.S_ISDIR(info.st_mode)
        and not stat.S_ISLNK(info.st_mode)
        and info.st_uid == os.geteuid()
        and info.st_mode & 0o077 == 0
    )


def _read_lock(path):
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    fd = os.open(path, flags)
    try:
        info = os.fstat(fd)
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_uid != os.geteuid()
            or info.st_mode & 0o077 != 0
        ):
            raise ValueError("unsafe wake lock")
        with os.fdopen(fd, "rb", closefd=False) as stream:
            data = _read_bounded(stream, MAX_LOCK_INPUT)
    finally:
        os.close(fd)
    value = json.loads(data)
    if not isinstance(value, dict):
        raise ValueError("invalid wake lock")
    return value


def _safe_socket(agent_dir, raw_path):
    if not isinstance(raw_path, str) or not raw_path:
        raise ValueError("missing control socket")
    path = os.path.realpath(raw_path)
    if os.path.dirname(path) != agent_dir:
        raise ValueError("control socket outside agent directory")
    info = os.lstat(path)
    if (
        not stat.S_ISSOCK(info.st_mode)
        or info.st_uid != os.geteuid()
        or info.st_mode & 0o077 != 0
    ):
        raise ValueError("unsafe control socket")
    return path


def main():
    event = json.loads(_read_bounded(sys.stdin.buffer, MAX_HOOK_INPUT))
    if not isinstance(event, dict) or event.get("hook_event_name") != "UserPromptSubmit":
        return
    if event.get("agent_id") is not None or event.get("agent_type") is not None:
        return
    prompt = event.get("prompt")
    if not isinstance(prompt, str):
        return
    match = PROMPT_RE.fullmatch(prompt)
    if match is None:
        return
    token = match.group(1)

    raw_root = os.environ.get("AM_ROOT", "")
    agent = os.environ.get("AM_ME", "")
    if not os.path.isabs(raw_root) or AGENT_RE.fullmatch(agent) is None:
        return
    root = os.path.realpath(raw_root)
    agent_dir = os.path.join(root, "agents", agent)
    if not _owned_private_directory(agent_dir):
        return

    lock = _read_lock(os.path.join(agent_dir, ".wake.lock"))
    generation = lock.get("generation")
    if (
        lock.get("root") != root
        or lock.get("agent") != agent
        or lock.get("wake_mode") not in ("raw", "paste", "owner-inject-via-v1")
        or not isinstance(generation, str)
        or re.fullmatch(r"[0-9a-f]{32}", generation) is None
    ):
        return
    control_socket = _safe_socket(agent_dir, lock.get("control_socket"))

    request = {
        "generation": generation,
        "operation": "prompt_observed",
        "token": token,
    }
    owner = lock.get("owner")
    if isinstance(owner, dict):
        request["owner"] = owner

    payload = json.dumps(request, separators=(",", ":")).encode("ascii") + b"\n"
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
        client.settimeout(0.5)
        client.connect(control_socket)
        client.sendall(payload)
        if client.recv(16).strip() != b"ACK":
            return


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, TypeError, json.JSONDecodeError):
        pass
