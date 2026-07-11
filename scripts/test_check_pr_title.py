#!/usr/bin/env python3
"""Tests for the conventional PR-title gate."""

from __future__ import annotations

import importlib.util
import pathlib


SCRIPT = pathlib.Path(__file__).with_name("check_pr_title.py")
spec = importlib.util.spec_from_file_location("check_pr_title", SCRIPT)
if spec is None or spec.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
check_pr_title = importlib.util.module_from_spec(spec)
spec.loader.exec_module(check_pr_title)


def test_valid_titles() -> None:
    titles = [
        "feat: add zero-input wake mode",
        "fix(wake): reject unsafe transport",
        "feat(api)!: remove legacy contract",
        "deps: bump golang.org/x/sys to 0.48.0",
        "chore(release): v0.42.0",
        "ci(release): migrate to release-please (#220)",
    ]
    for title in titles:
        assert check_pr_title.is_valid_title(title), title


def test_invalid_titles() -> None:
    titles = [
        "Add zero-input wake mode",
        "Feat: uppercase type",
        "feet: misspelled type",
        "fix(scope) missing colon",
        "fix: ",
        "fix: trailing space ",
        "fix(BadScope): uppercase scope",
    ]
    for title in titles:
        assert not check_pr_title.is_valid_title(title), title


if __name__ == "__main__":
    test_valid_titles()
    test_invalid_titles()
    print("PR title tests ok")
