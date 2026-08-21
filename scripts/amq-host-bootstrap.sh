#!/usr/bin/env bash
# Initialize a standalone AMQ host root with pinned host identity.
#
# Usage:
#   amq-host-bootstrap.sh --root DIR --host-id ID --agents claude,codex [--me AGENT] [--amq PATH] [--force]
#
# Creates the normal AMQ layout via `amq init`, writes bridge/host-id (mode 0600),
# and prints export lines for AM_ROOT and AM_ME. Does not copy .agent-mail or
# configure bridge rendezvous.

set -euo pipefail

usage() {
  cat <<'EOF' >&2
Usage: amq-host-bootstrap.sh --root DIR --host-id ID --agents a,b,c [--me AGENT] [--amq PATH] [--force]

Initialize a standalone AMQ host root with pinned host identity.
Does not copy .agent-mail or configure bridge rendezvous.
EOF
}

root=''
host_id=''
agents=''
me=''
amq_bin='amq'
force=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --root)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      root=$2
      shift 2
      ;;
    --host-id)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      host_id=$2
      shift 2
      ;;
    --agents)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      agents=$2
      shift 2
      ;;
    --me)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      me=$2
      shift 2
      ;;
    --amq)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      amq_bin=$2
      shift 2
      ;;
    --force)
      force=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --rendezvous|--rendezvous-url)
      printf 'amq-host-bootstrap: %s is not supported; configure rendezvous separately\n' "$1" >&2
      exit 2
      ;;
    *)
      printf 'amq-host-bootstrap: unknown argument: %s\n' "$1" >&2
      usage
      exit 2
      ;;
  esac
done

if [[ -z "$root" || -z "$host_id" || -z "$agents" ]]; then
  usage
  exit 2
fi

if [[ ! "$host_id" =~ ^[a-z0-9][a-z0-9_-]*$ ]]; then
  printf 'amq-host-bootstrap: invalid --host-id %q; want lowercase [a-z0-9_-]+\n' "$host_id" >&2
  exit 2
fi

if [[ -L "$root" ]]; then
  printf 'amq-host-bootstrap: --root must not be a symlink: %s\n' "$root" >&2
  exit 2
fi

if [[ ! -x "$amq_bin" ]] && ! command -v "$amq_bin" >/dev/null 2>&1; then
  printf 'amq-host-bootstrap: amq executable not found: %s\n' "$amq_bin" >&2
  exit 1
fi

if [[ -z "$me" ]]; then
  me=${agents%%,*}
fi

if [[ -z "$me" ]]; then
  printf 'amq-host-bootstrap: --agents must list at least one handle\n' >&2
  exit 2
fi

amq_command() {
  env \
    -u AM_ROOT \
    -u AM_ROOT_ID \
    -u AM_ME \
    -u AM_BASE_ROOT \
    -u AM_BASE_ROOT_ID \
    -u AM_SESSION \
    -u AMQ_GLOBAL_ROOT \
    -u AMQ_WAKE_OWNER \
    AMQ_NO_UPDATE_CHECK=1 \
    "$amq_bin" "$@"
}

init_args=(init --root "$root" --agents "$agents")
if [[ "$force" == true ]]; then
  init_args+=(--force)
fi

if ! amq_command "${init_args[@]}"; then
  printf 'amq-host-bootstrap: amq init failed\n' >&2
  exit 1
fi

umask 077
mkdir -p "$root/bridge"
host_id_file="$root/bridge/host-id"
printf '%s\n' "$host_id" >"$host_id_file"
chmod 0600 "$host_id_file"

printf 'export AM_ROOT=%q\n' "$root"
printf 'export AM_ME=%q\n' "$me"
