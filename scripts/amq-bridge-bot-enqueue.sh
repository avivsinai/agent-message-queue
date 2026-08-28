#!/usr/bin/env bash
# Fixed Bot-chat submit wrapper around `amq-bridge enqueue`.
#
# Usage:
#   amq-bridge-bot-enqueue.sh --dest-alias host/agent
#
# stdin must be exactly one AMQ message. It is passed through unread by this
# script to:
#   amq-bridge enqueue --config "$AMQ_BRIDGE_ENQUEUE_CONFIG" --dest-alias ALIAS
#
# Required env:
#   AMQ_BRIDGE_ENQUEUE_CONFIG  Path to a private mode-0600 enqueue config JSON
#                              (root, source_host, source_handle,
#                              allowed_dest_aliases). See internal/bridge.
#
# argv is exactly `--dest-alias host/agent` and nothing else. Bot chat/prompt
# content cannot add --root, --rendezvous, --me, --spool, or any other flag:
# every argv and env check below runs before stdin is ever read, so a refusal
# never reads or prints the message body. AM_ROOT and other AM_* environment
# have no effect; the enqueue root comes only from the config file.

set -euo pipefail

refuse() {
  printf 'amq-bridge-bot-enqueue: %s\n' "$1" >&2
  exit 1
}

usage="usage: amq-bridge-bot-enqueue.sh --dest-alias host/agent"

[[ $# -eq 2 ]] || refuse "$usage"
[[ "$1" == "--dest-alias" ]] || refuse "$usage"

dest_alias=$2
[[ "$dest_alias" =~ ^[a-z0-9_][a-z0-9_-]*/[a-z0-9_][a-z0-9_-]*$ ]] \
  || refuse "--dest-alias must be host/agent: $dest_alias"

config=${AMQ_BRIDGE_ENQUEUE_CONFIG:-}
[[ -n "$config" ]] || refuse "AMQ_BRIDGE_ENQUEUE_CONFIG is required"
[[ ! -L "$config" ]] || refuse "AMQ_BRIDGE_ENQUEUE_CONFIG must not be a symlink: $config"
[[ -e "$config" ]] || refuse "AMQ_BRIDGE_ENQUEUE_CONFIG not found: $config"
[[ -f "$config" ]] || refuse "AMQ_BRIDGE_ENQUEUE_CONFIG must be a regular file: $config"

# Prefer GNU `stat -c '%a'`. BSD-first `stat -f` is `--file-system` on GNU
# coreutils and dumps filesystem info into the compared mode string.
config_mode=$(stat -c '%a' "$config" 2>/dev/null || stat -f '%OLp' "$config")
[[ "$config_mode" == "600" ]] \
  || refuse "AMQ_BRIDGE_ENQUEUE_CONFIG mode is $config_mode, want 600"

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

if command -v amq-bridge >/dev/null 2>&1; then
  amq_bridge_cmd=(amq-bridge)
elif [[ -x "$script_dir/amq-bridge" ]]; then
  amq_bridge_cmd=("$script_dir/amq-bridge")
elif [[ "${AMQ_BRIDGE_BOT_ENQUEUE_ALLOW_GO_RUN:-0}" == "1" ]]; then
  # Test-only fallback: never reached in a Bot-chat deployment, which must
  # install a real amq-bridge binary on PATH or beside this script.
  repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
  cd "$repo_root"
  amq_bridge_cmd=(go run ./cmd/amq-bridge)
else
  refuse "amq-bridge executable not found on PATH or beside $script_dir"
fi

exec env \
  -u AM_ROOT -u AM_ROOT_ID -u AM_ME -u AM_BASE_ROOT -u AM_BASE_ROOT_ID \
  -u AM_SESSION -u AMQ_GLOBAL_ROOT -u AMQ_WAKE_OWNER \
  "${amq_bridge_cmd[@]}" enqueue --config "$config" --dest-alias "$dest_alias"
