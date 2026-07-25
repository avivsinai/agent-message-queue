#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
INSTALL_SCRIPT="$SCRIPT_DIR/install.sh"
INSTALL_DOC="$SCRIPT_DIR/../INSTALL.md"
TEST_ROOT=$(mktemp -d)
trap 'rm -rf "$TEST_ROOT"' EXIT

VERSION=v1.2.3
ASSET=amq_1.2.3_linux_amd64.tar.gz
GOOD_HASH=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
BAD_HASH=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

link_command() {
  local bin_dir=$1
  local name=$2
  local path

  path=$(command -v "$name")
  ln -s "$path" "$bin_dir/$name"
}

write_common_tools() {
  local bin_dir=$1

  for command_name in awk chmod cp cut grep mkdir mktemp mv rm touch tr; do
    link_command "$bin_dir" "$command_name"
  done

  cat >"$bin_dir/uname" <<'EOF'
#!/bin/bash
case "${1:-}" in
  -s) printf 'Linux\n' ;;
  -m) printf 'x86_64\n' ;;
  *) exit 2 ;;
esac
EOF

  cat >"$bin_dir/curl" <<'EOF'
#!/bin/bash
set -euo pipefail

output=
url=
while (($#)); do
  case "$1" in
    -o)
      output=$2
      shift 2
      ;;
    -*)
      shift
      ;;
    *)
      url=$1
      shift
      ;;
  esac
done

case "$url" in
  */checksums.txt)
    case "$TEST_SCENARIO" in
      checksum-download-failure)
        exit 22
        ;;
      missing-checksums)
        :
        ;;
      unreadable)
        printf '%s  %s\n' "$GOOD_HASH" "$ASSET" >"$output"
        chmod 000 "$output"
        ;;
      missing-entry)
        printf '%s  other_asset.tar.gz\n' "$GOOD_HASH" >"$output"
        ;;
      duplicate)
        printf '%s  %s\n%s  %s\n' \
          "$GOOD_HASH" "$ASSET" "$GOOD_HASH" "$ASSET" >"$output"
        ;;
      malformed)
        printf 'not-a-sha256  %s\n' "$ASSET" >"$output"
        ;;
      *)
        printf '%s  %s\n' "$GOOD_HASH" "$ASSET" >"$output"
        ;;
    esac
    ;;
  *)
    printf 'archive-data\n' >"$output"
    ;;
esac
EOF

  cat >"$bin_dir/tar" <<'EOF'
#!/bin/bash
set -euo pipefail
touch "$TEST_STATE_DIR/tar.called"
case "$TEST_SCENARIO" in
  tar-failure)
    exit 1
    ;;
  missing-archive-binary)
    exit 0
    ;;
  *)
    printf '%s\n' 'new-binary' >amq
    chmod 0755 amq
    ;;
esac
EOF

  cat >"$bin_dir/install" <<'EOF'
#!/bin/bash
set -euo pipefail
touch "$TEST_STATE_DIR/install.called"
source_path=${@: -2:1}
target_path=${@: -1}
if [[ "$TEST_SCENARIO" == install-partial-failure ]]; then
  printf '%s\n' 'partial-write' >"$target_path"
  chmod 0600 "$target_path"
  exit 1
fi
cp "$source_path" "$target_path"
chmod 0755 "$target_path"
EOF

  chmod +x "$bin_dir/uname" "$bin_dir/curl" "$bin_dir/tar" "$bin_dir/install"
}

write_sha256sum() {
  local bin_dir=$1

  cat >"$bin_dir/sha256sum" <<'EOF'
#!/bin/bash
set -euo pipefail
[[ "$#" -eq 2 && "$1" == "-c" && "$2" == "-" ]]
record=
IFS= read -r record
case "$TEST_SCENARIO" in
  sha256sum-mismatch)
    exit 1
    ;;
  *)
    [[ "$record" == "$GOOD_HASH  $ASSET" ]]
    ;;
esac
EOF
  chmod +x "$bin_dir/sha256sum"
}

write_shasum() {
  local bin_dir=$1

  cat >"$bin_dir/shasum" <<'EOF'
#!/bin/bash
set -euo pipefail
[[ "$#" -eq 3 && "$1" == "-a" && "$2" == "256" && "$3" == */"$ASSET" ]]
case "$TEST_SCENARIO" in
  shasum-mismatch)
    printf '%s  %s\n' "$BAD_HASH" "${3:-}"
    ;;
  *)
    printf '%s  %s\n' "$GOOD_HASH" "${3:-}"
    ;;
esac
EOF
  chmod +x "$bin_dir/shasum"
}

file_mode() {
  local path=$1

  if stat -c '%a' "$path" >/dev/null 2>&1; then
    stat -c '%a' "$path"
  else
    stat -f '%Lp' "$path"
  fi
}

run_case() {
  local scenario=$1
  local expected=$2
  local hash_tool=${3:-none}
  local expected_tar=${4:-no}
  local expected_install=${5:-no}
  local case_root="$TEST_ROOT/$scenario"
  local bin_dir="$case_root/bin"
  local install_dir="$case_root/install"
  local state_dir="$case_root/state"
  local home_dir="$case_root/home"
  local output_file="$case_root/output"
  local status

  mkdir -p "$bin_dir" "$install_dir" "$state_dir" "$home_dir"
  printf '%s\n' 'existing-binary' >"$install_dir/amq"
  chmod 0711 "$install_dir/amq"
  write_common_tools "$bin_dir"

  case "$hash_tool" in
    sha256sum) write_sha256sum "$bin_dir" ;;
    shasum) write_shasum "$bin_dir" ;;
    none) ;;
    *) printf 'unknown hash tool: %s\n' "$hash_tool" >&2; exit 2 ;;
  esac

  set +e
  PATH="$bin_dir" \
    HOME="$home_dir" \
    VERSION="$VERSION" \
    INSTALL_DIR="$install_dir" \
    TEST_SCENARIO="$scenario" \
    TEST_STATE_DIR="$state_dir" \
    GOOD_HASH="$GOOD_HASH" \
    BAD_HASH="$BAD_HASH" \
    ASSET="$ASSET" \
    /bin/bash "$INSTALL_SCRIPT" >"$output_file" 2>&1
  status=$?
  set -e

  if [[ "$expected" == failure ]]; then
    if ((status == 0)); then
      printf 'FAIL %s: expected nonzero exit\n' "$scenario" >&2
      sed -n '1,120p' "$output_file" >&2
      return 1
    fi
    [[ $(<"$install_dir/amq") == existing-binary ]] || {
      printf 'FAIL %s: existing target changed\n' "$scenario" >&2
      return 1
    }
    [[ $(file_mode "$install_dir/amq") == 711 ]] || {
      printf 'FAIL %s: existing target mode changed\n' "$scenario" >&2
      return 1
    }
  else
    if ((status != 0)); then
      printf 'FAIL %s: expected zero exit, got %s\n' "$scenario" "$status" >&2
      sed -n '1,120p' "$output_file" >&2
      return 1
    fi
    [[ $(<"$install_dir/amq") == new-binary ]] || {
      printf 'FAIL %s: target was not replaced\n' "$scenario" >&2
      return 1
    }
    [[ $(file_mode "$install_dir/amq") == 755 ]] || {
      printf 'FAIL %s: installed target mode is not 0755\n' "$scenario" >&2
      return 1
    }
  fi

  if [[ "$expected_tar" == yes ]]; then
    [[ -e "$state_dir/tar.called" ]] || {
      printf 'FAIL %s: extraction did not occur\n' "$scenario" >&2
      return 1
    }
  else
    [[ ! -e "$state_dir/tar.called" ]] || {
      printf 'FAIL %s: extraction occurred\n' "$scenario" >&2
      return 1
    }
  fi

  if [[ "$expected_install" == yes ]]; then
    [[ -e "$state_dir/install.called" ]] || {
      printf 'FAIL %s: install did not occur\n' "$scenario" >&2
      return 1
    }
  else
    [[ ! -e "$state_dir/install.called" ]] || {
      printf 'FAIL %s: install occurred\n' "$scenario" >&2
      return 1
    }
  fi

  if compgen -G "$install_dir/.amq.install.*" >/dev/null; then
    printf 'FAIL %s: staged install file leaked\n' "$scenario" >&2
    return 1
  fi

  printf 'PASS %s\n' "$scenario"
}

manual_install_snippet() {
  awk '
    /^TAG=vX[.]Y[.]Z$/ { capture = 1 }
    capture {
      if ($0 == "```") exit
      print
    }
  ' "$INSTALL_DOC"
}

run_manual_verifier_failure_case() {
  local hash_tool=$1
  local case_root="$TEST_ROOT/manual-$hash_tool-mismatch"
  local bin_dir="$case_root/bin"
  local state_dir="$case_root/state"
  local home_dir="$case_root/home"
  local output_file="$case_root/output"
  local snippet
  local status

  mkdir -p "$bin_dir" "$state_dir" "$home_dir"
  for command_name in awk mkdir mv touch; do
    link_command "$bin_dir" "$command_name"
  done

  cat >"$bin_dir/curl" <<'EOF'
#!/bin/bash
set -euo pipefail
output=
while (($#)); do
  case "$1" in
    -o)
      output=$2
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
printf '%s  amq_X.Y.Z_darwin_arm64.tar.gz\n' \
  'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' >"$output"
EOF

  cat >"$bin_dir/$hash_tool" <<'EOF'
#!/bin/bash
exit 1
EOF

  cat >"$bin_dir/tar" <<'EOF'
#!/bin/bash
touch "$TEST_STATE_DIR/tar.called"
exit 0
EOF

  chmod +x "$bin_dir/curl" "$bin_dir/$hash_tool" "$bin_dir/tar"
  snippet=$(manual_install_snippet)
  [[ -n "$snippet" ]] || {
    printf 'FAIL manual-%s-mismatch: INSTALL.md snippet not found\n' "$hash_tool" >&2
    return 1
  }

  set +e
  (
    cd "$case_root"
    PATH="$bin_dir" \
      HOME="$home_dir" \
      TEST_STATE_DIR="$state_dir" \
      /bin/bash -c "$snippet"
  ) >"$output_file" 2>&1
  status=$?
  set -e

  if ((status == 0)); then
    printf 'FAIL manual-%s-mismatch: expected nonzero exit\n' "$hash_tool" >&2
    sed -n '1,120p' "$output_file" >&2
    return 1
  fi
  if [[ -e "$state_dir/tar.called" ]]; then
    printf 'FAIL manual-%s-mismatch: extraction occurred after verifier failure\n' "$hash_tool" >&2
    return 1
  fi

  printf 'PASS manual-%s-mismatch\n' "$hash_tool"
}

run_case checksum-download-failure failure sha256sum
run_case missing-checksums failure sha256sum
run_case unreadable failure sha256sum
run_case missing-entry failure sha256sum
run_case duplicate failure sha256sum
run_case malformed failure sha256sum
run_case no-tool failure
run_case sha256sum-mismatch failure sha256sum
run_case shasum-mismatch failure shasum
run_case tar-failure failure sha256sum yes no
run_case missing-archive-binary failure sha256sum yes no
run_case install-partial-failure failure sha256sum yes yes
run_case sha256sum-valid success sha256sum yes yes
run_case shasum-valid success shasum yes yes
run_manual_verifier_failure_case sha256sum
run_manual_verifier_failure_case shasum
