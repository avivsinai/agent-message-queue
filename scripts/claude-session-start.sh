#!/bin/sh
# Deprecated co-op SessionStart hook: remove this project hook entry and run
# `amq coop install-hooks`, which installs the same hook at user level.
[ -n "${AM_ME-}" ] || exit 0
command -v amq >/dev/null 2>&1 || exit 0
exec amq --no-update-check coop hook session-start
