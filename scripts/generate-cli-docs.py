#!/usr/bin/env python3
"""Generate the checked-in AMQ CLI reference from one built binary."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import re
import subprocess
import tempfile


ENTRY = re.compile(r"^  ([A-Za-z0-9_-]+)\s{2,}(.+?)\s*$")

# `wake` dispatches these children, but its platform-specific help is a
# long option reference and does not print a Subcommands section. Keep this
# small source-backed supplement so the public operator commands are covered.
SOURCE_SUBCOMMANDS = {
    "wake": ("check", "repair", "restart", "recover-owner", "retire"),
}


def entries(help_text: str, heading: str) -> list[tuple[str, str]]:
    """Read the command rows immediately under a Commands/Subcommands heading."""
    lines = help_text.splitlines()
    try:
        start = next(i for i, line in enumerate(lines) if line == heading) + 1
    except StopIteration:
        return []

    found: list[tuple[str, str]] = []
    for line in lines[start:]:
        if not line.strip():
            break
        match = ENTRY.match(line)
        if match is None:
            break
        found.append((match.group(1), match.group(2)))
    return found


def command_help(binary: Path, command: tuple[str, ...], home: Path) -> str:
    """Run only an explicit --help invocation under a hermetic environment."""
    env = {
        "PATH": "/usr/bin:/bin",
        "HOME": str(home),
        "TMPDIR": str(home),
        "LANG": "C",
        "LC_ALL": "C",
        "TZ": "UTC",
        "AMQ_NO_UPDATE_CHECK": "1",
        "AM_ROOT": ".agent-mail",
        "AM_BASE_ROOT": ".agent-mail",
        "AMQ_GLOBAL_ROOT": ".agent-mail",
    }
    argv = [str(binary), *command, "--help"]
    result = subprocess.run(
        argv,
        cwd=home,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=False,
        timeout=15,
    )
    if result.returncode != 0:
        rendered = " ".join(argv)
        raise RuntimeError(f"{rendered} failed with exit {result.returncode}:\n{result.stdout}")
    if str(home) in result.stdout:
        rendered = " ".join(argv)
        raise RuntimeError(f"{rendered} leaked the generator temp path into help output")
    return result.stdout.rstrip()


def collect(binary: Path) -> list[tuple[tuple[str, ...], str]]:
    with tempfile.TemporaryDirectory(prefix="amq-cli-docs-") as temp:
        home = Path(temp)
        root = command_help(binary, (), home)
        result: list[tuple[tuple[str, ...], str]] = [((), root)]
        commands = entries(root, "Commands:")
        if not commands:
            raise RuntimeError("root help has no Commands section")

        pending = [(name,) for name, _ in commands]
        seen: set[tuple[str, ...]] = set()
        while pending:
            command = pending.pop(0)
            if command in seen:
                continue
            seen.add(command)
            text = command_help(binary, command, home)
            result.append((command, text))
            child_names = [name for name, _ in entries(text, "Subcommands:")]
            child_names.extend(SOURCE_SUBCOMMANDS.get(" ".join(command), ()))
            pending.extend((*command, name) for name in child_names)
        return result


def slug(command: tuple[str, ...]) -> str:
    return "amq-" + "-".join(command) if command else "amq"


def render(commands: list[tuple[tuple[str, ...], str]]) -> str:
    lines = [
        "<!-- GENERATED FILE. DO NOT EDIT. Run `make docs-cli` to regenerate. -->",
        "# AMQ CLI reference",
        "",
        "This reference is generated from the built `amq` binary. It includes the",
        "root command, every command listed by root help, and every listed",
        "subcommand. The binary's `--help` output is authoritative for aliases and",
        "options not repeated here.",
        "",
        "## Command index",
        "",
    ]
    for command, _ in commands:
        label = "amq" if not command else "amq " + " ".join(command)
        lines.append(f"- [{label}](#{slug(command)})")
    lines.append("")
    for command, text in commands:
        label = "amq" if not command else "amq " + " ".join(command)
        lines.extend([f"## {label}", "", "```text", f"$ {label} --help", text, "```", ""])
    return "\n".join(lines)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()

    binary = args.binary.resolve()
    if not binary.is_file() or not os.access(binary, os.X_OK):
        parser.error(f"binary is not executable: {binary}")
    commands = collect(binary)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(render(commands), encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
