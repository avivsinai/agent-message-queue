#!/usr/bin/env bash
set -euo pipefail

amq_bin=${1:-${AMQ_BIN:-amq}}
required_flags=(--ready-file --accept-existing-wake --baseline-existing --retry-until)

if [[ ! -x "$amq_bin" ]] && ! command -v "$amq_bin" >/dev/null 2>&1; then
  printf 'AMQ keepalive contract failed: executable not found: %s\n' "$amq_bin" >&2
  exit 1
fi

private_base=${AMQ_KEEPALIVE_CONTRACT_TMPDIR:-${XDG_CACHE_HOME:-${HOME:?HOME is required}/.cache}/amq-keepalive-contract}
umask 077
mkdir -p "$private_base"
tmp_dir=$(mktemp -d "$private_base/probe.XXXXXX")
amq_pid=''

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

cleanup() {
  if [[ -n "$amq_pid" ]] && kill -0 "$amq_pid" 2>/dev/null; then
    kill "$amq_pid" 2>/dev/null || true
    wait "$amq_pid" 2>/dev/null || true
  fi
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

root="$tmp_dir/amq-root"
ready_file="$tmp_dir/ready"
if ! amq_command init --root "$root" --agents probe \
  >"$tmp_dir/init.stdout" 2>"$tmp_dir/init.stderr"; then
  printf 'AMQ keepalive contract failed: candidate could not initialize isolated root\n' >&2
  sed -n '1,5p' "$tmp_dir/init.stderr" >&2
  exit 1
fi

# The inject-via process is deliberately a no-op. The isolated root ensures
# this probe cannot inspect or mutate the user's live AMQ registry.
inject_via="$tmp_dir/inject-via"
cat >"$inject_via" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$inject_via"

set +e
amq_command wake \
  --root "$root" \
  --me probe \
  --baseline-existing \
  --accept-existing-wake \
  --retry-until injected \
  --ready-file "$ready_file" \
  -inject-via "$inject_via" \
  -inject-arg inject \
  -inject-arg probe \
  >"$tmp_dir/stdout" 2>"$tmp_dir/stderr" &
amq_pid=$!
set -e

startup_deadline=$((SECONDS + 5))
while [[ ! -e "$ready_file" ]] && kill -0 "$amq_pid" 2>/dev/null; do
  process_state=$(ps -o stat= -p "$amq_pid" 2>/dev/null || true)
  if [[ "$process_state" == *Z* ]]; then
    break
  fi
  if (( SECONDS >= startup_deadline )); then
    printf 'AMQ keepalive contract failed: ready file was not observed within 5 seconds\n' >&2
    sed -n '1,5p' "$tmp_dir/stderr" >&2
    exit 1
  fi
  sleep 0.05
done

if [[ ! -e "$ready_file" ]]; then
  wait "$amq_pid" || status=$?
  status=${status:-0}
else
  status=0
fi

if grep -Eiq 'unknown flag|flag provided but not defined|undefined flag' "$tmp_dir/stderr"; then
  printf 'AMQ keepalive contract failed: executable rejected one or more required flags (%s)\n' \
    "${required_flags[*]}" >&2
  sed -n '1,5p' "$tmp_dir/stderr" >&2
  exit 1
fi

if (( status != 0 )); then
  printf 'AMQ keepalive contract failed: wake exited before readiness (status %d)\n' "$status" >&2
  sed -n '1,5p' "$tmp_dir/stderr" >&2
  exit 1
fi

kill "$amq_pid" 2>/dev/null || true
wait "$amq_pid" 2>/dev/null || true
amq_pid=''
printf 'AMQ keepalive contract passed: %s\n' "${required_flags[*]}"
