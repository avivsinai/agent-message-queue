#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
bootstrap="$script_dir/amq-host-bootstrap.sh"
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/amq-host-bootstrap-test.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT

amq_bin="$tmp_dir/amq"
queue_root="$tmp_dir/amq-root"
body_text="host-bootstrap-round-trip-$(date +%s)"

(
  cd "$repo_root"
  go build -o "$amq_bin" ./cmd/amq
)

chmod +x "$bootstrap"

bootstrap_out=$("$bootstrap" \
  --root "$queue_root" \
  --host-id testhost \
  --agents claude,codex \
  --amq "$amq_bin")

if [[ ! -f "$queue_root/bridge/host-id" ]]; then
  printf 'missing host-id file\n' >&2
  exit 1
fi

host_id_mode=$(stat -f '%OLp' "$queue_root/bridge/host-id" 2>/dev/null || stat -c '%a' "$queue_root/bridge/host-id")
if [[ "$host_id_mode" != "600" ]]; then
  printf 'host-id mode = %s, want 600\n' "$host_id_mode" >&2
  exit 1
fi

if [[ "$(tr -d '\n' <"$queue_root/bridge/host-id")" != "testhost" ]]; then
  printf 'host-id contents mismatch\n' >&2
  exit 1
fi

if ! printf '%s\n' "$bootstrap_out" | grep -Fq "export AM_ROOT="; then
  printf 'bootstrap did not print AM_ROOT export:\n%s\n' "$bootstrap_out" >&2
  exit 1
fi

if ! printf '%s\n' "$bootstrap_out" | grep -Fq "export AM_ME=claude"; then
  printf 'bootstrap did not print AM_ME export:\n%s\n' "$bootstrap_out" >&2
  exit 1
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

amq_command send --root "$queue_root" --me claude --to codex --body "$body_text" >/dev/null

drain_out=$(amq_command drain --root "$queue_root" --me codex --include-body)
if ! printf '%s\n' "$drain_out" | grep -Fq "$body_text"; then
  printf 'drain output missing body %q:\n%s\n' "$body_text" "$drain_out" >&2
  exit 1
fi

printf 'amq-host-bootstrap integration test passed\n'
