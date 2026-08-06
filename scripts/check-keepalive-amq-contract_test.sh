#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
probe="$script_dir/check-keepalive-amq-contract.sh"
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/amq-keepalive-contract-test.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT
export AMQ_KEEPALIVE_CONTRACT_TMPDIR="$tmp_dir/contracts"
export AMQ_KEEPALIVE_FAKE_LOG="$tmp_dir/amq-calls.log"

make_fake_amq() {
  local path=$1
  local missing=${2:-}
  cat >"$path" <<EOF
#!/usr/bin/env bash
set -euo pipefail
missing='$missing'
printf '%s\n' "\$*" >>"\${AMQ_KEEPALIVE_FAKE_LOG:?}"
if [[ "\${1:-}" == init ]]; then
  exit 0
fi
ready_file=''
retry_until=''
previous=''
for arg in "\$@"; do
  case "\$arg" in
    --ready-file|--accept-existing-wake|--baseline-existing|--retry-until)
      if [[ "\$arg" == "\$missing" ]]; then
        printf 'unknown flag: %s\\n' "\$arg" >&2
        exit 2
      fi
      ;;
  esac
  if [[ "\$previous" == --ready-file ]]; then
    ready_file="\$arg"
  elif [[ "\$previous" == --retry-until ]]; then
    retry_until="\$arg"
  fi
  previous="\$arg"
done
if [[ -z "\$ready_file" ]]; then
  printf 'missing ready file\n' >&2
  exit 2
fi
if [[ "\$retry_until" != injected ]]; then
  printf 'missing or invalid retry-until policy: %s\n' "\$retry_until" >&2
  exit 2
fi
touch "\$ready_file"
trap 'exit 0' TERM INT
while :; do sleep 1; done
EOF
  chmod +x "$path"
}

passing="$tmp_dir/amq-pass"
make_fake_amq "$passing"
"$probe" "$passing" >/dev/null
if ! grep -Eq '^init --root .+ --agents probe$' "$AMQ_KEEPALIVE_FAKE_LOG"; then
  printf 'contract did not initialize its isolated root with the candidate binary\n' >&2
  exit 1
fi
if ! grep -Eq '^wake .*--retry-until injected( |$)' "$AMQ_KEEPALIVE_FAKE_LOG"; then
  printf 'contract did not exercise --retry-until injected\n' >&2
  exit 1
fi

for missing in --ready-file --accept-existing-wake --baseline-existing --retry-until; do
  failing="$tmp_dir/amq-missing-${missing#--}"
  make_fake_amq "$failing" "$missing"
  if output=$("$probe" "$failing" 2>&1); then
    printf 'expected missing flag to fail: %s\n' "$missing" >&2
    exit 1
  fi
  if [[ "$output" != *"$missing"* ]]; then
    printf 'failure did not identify required flags for %s:\n%s\n' "$missing" "$output" >&2
    exit 1
  fi
done

printf 'keepalive AMQ contract regression tests passed\n'
