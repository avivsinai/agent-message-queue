#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
probe="$script_dir/check-keepalive-amq-contract.sh"
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/amq-keepalive-contract-test.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT

make_fake_amq() {
  local path=$1
  local missing=${2:-}
  cat >"$path" <<EOF
#!/usr/bin/env bash
set -euo pipefail
missing='$missing'
ready_file=''
for arg in "\$@"; do
  case "\$arg" in
    --ready-file|--accept-existing-wake|--baseline-existing)
      if [[ "\$arg" == "\$missing" ]]; then
        printf 'unknown flag: %s\\n' "\$arg" >&2
        exit 2
      fi
      ;;
  esac
done
for ((i = 1; i <= \$#; i++)); do
  if [[ "\${!i}" == --ready-file ]]; then
    next=\$((i + 1))
    ready_file="\${!next}"
  fi
done
if [[ -z "\$ready_file" ]]; then
  printf 'missing ready file\n' >&2
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

for missing in --ready-file --accept-existing-wake --baseline-existing; do
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
