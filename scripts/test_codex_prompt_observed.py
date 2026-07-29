#!/usr/bin/env python3

import json
import os
import shutil
import socket
import subprocess
import tempfile
import threading
from pathlib import Path


REPO = Path(__file__).resolve().parent.parent
HOOK = REPO / "hooks" / "codex-prompt-observed.py"
TOKEN = "0123456789abcdef0123456789abcdef"
PROMPT = (
    f": AMQ doorbell v1 token={TOKEN} "
    "run amq drain --include-body then act on it"
)


def invoke(root, prompt):
    event = {
        "hook_event_name": "UserPromptSubmit",
        "session_id": "test-session",
        "turn_id": "test-turn",
        "prompt": prompt,
    }
    env = dict(os.environ, AM_ROOT=str(root), AM_ME="codex")
    return subprocess.run(
        ["/usr/bin/python3", str(HOOK)],
        input=json.dumps(event),
        text=True,
        capture_output=True,
        env=env,
        timeout=3,
        check=False,
    )


def fixture():
    temp = tempfile.TemporaryDirectory()
    root = Path(temp.name) / "queue"
    agent_dir = root / "agents" / "codex"
    agent_dir.mkdir(parents=True, mode=0o700)
    os.chmod(agent_dir, 0o700)
    socket_path = agent_dir / ".w.test"
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(str(socket_path))
    os.chmod(socket_path, 0o600)
    server.listen(1)
    server.settimeout(1)
    lock = {
        "pid": os.getpid(),
        "tty": "/dev/ttys999",
        "root": str(root.resolve()),
        "agent": "codex",
        "started": "2026-07-29T00:00:00Z",
        "wake_mode": "raw",
        "generation": "11111111111111111111111111111111",
        "control_socket": str(socket_path),
    }
    lock_path = agent_dir / ".wake.lock"
    lock_path.write_text(json.dumps(lock), encoding="utf-8")
    os.chmod(lock_path, 0o600)
    return temp, root, server


def test_valid_prompt():
    temp, root, server = fixture()
    received = []

    def accept():
        connection, _ = server.accept()
        with connection:
            received.append(connection.recv(4096))
            connection.sendall(b"ACK\n")

    thread = threading.Thread(target=accept)
    thread.start()
    result = invoke(root, PROMPT)
    thread.join(timeout=2)
    server.close()
    temp.cleanup()

    assert result.returncode == 0, result
    assert result.stdout == "" and result.stderr == "", result
    assert len(received) == 1, received
    request = json.loads(received[0])
    assert request == {
        "generation": "11111111111111111111111111111111",
        "operation": "prompt_observed",
        "token": TOKEN,
    }, request


def test_nonmatching_prompts_do_nothing():
    variants = [
        PROMPT + " ",
        " " + PROMPT,
        PROMPT.replace("token=", "TOKEN="),
        PROMPT.replace(TOKEN, TOKEN.upper()),
        PROMPT.replace(" then act on it", "; rm -rf /"),
        "ordinary user prompt",
    ]
    for prompt in variants:
        temp, root, server = fixture()
        server.settimeout(0.1)
        result = invoke(root, prompt)
        try:
            server.accept()
            raise AssertionError(f"nonmatching prompt connected: {prompt!r}")
        except TimeoutError:
            pass
        finally:
            server.close()
            temp.cleanup()
        assert result.returncode == 0, result
        assert result.stdout == "" and result.stderr == "", result


def test_real_codex_hook():
    if os.environ.get("AMQ_RUN_CODEX_HOOK_E2E") != "1":
        return
    temp, root, server = fixture()
    checkout = tempfile.TemporaryDirectory()
    checkout_root = Path(checkout.name)
    (checkout_root / ".codex").mkdir()
    (checkout_root / "hooks").mkdir()
    shutil.copy2(REPO / "hooks" / "codex-hooks.json", checkout_root / "hooks" / "codex-hooks.json")
    os.symlink("../hooks/codex-hooks.json", checkout_root / ".codex" / "hooks.json")
    shutil.copy2(HOOK, checkout_root / "hooks" / HOOK.name)
    subprocess.run(
        ["git", "init", "--quiet", str(checkout_root)],
        check=True,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    env = dict(os.environ, AM_ROOT=str(root), AM_ME="codex")
    process = subprocess.Popen(
        [
            "codex",
            "exec",
            "--ephemeral",
            "--dangerously-bypass-hook-trust",
            "--json",
            "-C",
            str(checkout_root),
            PROMPT,
        ],
        cwd=checkout_root,
        env=env,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        server.settimeout(30)
        connection, _ = server.accept()
        with connection:
            request = json.loads(connection.recv(4096))
            connection.sendall(b"ACK\n")
        assert request["operation"] == "prompt_observed", request
        assert request["token"] == TOKEN, request
    finally:
        process.terminate()
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=3)
        server.close()
        temp.cleanup()
        checkout.cleanup()


if __name__ == "__main__":
    test_valid_prompt()
    test_nonmatching_prompts_do_nothing()
    test_real_codex_hook()
    print("codex prompt-observed hook tests ok")
