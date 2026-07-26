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

  for command_name in chmod cmp cp cut grep ln mkdir mktemp rm touch tr; do
    link_command "$bin_dir" "$command_name"
  done

  cat >"$bin_dir/awk" <<'EOF'
#!/bin/bash
set -euo pipefail
if [[ "$TEST_SCENARIO" == parser-failure ]]; then
  exit 2
fi
exec "$TEST_REAL_AWK" "$@"
EOF

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
        ln -s "$TEST_STATE_DIR/does-not-exist" "$output"
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
      checksum-crlf)
        printf '%s  %s\r\n' "$GOOD_HASH" "$ASSET" >"$output"
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
output_dir=$PWD
while (($#)); do
  case "$1" in
    -C)
      output_dir=$2
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
case "$TEST_SCENARIO" in
  tar-failure)
    exit 1
    ;;
  tar-partial-failure)
    printf '%s\n' 'partial-archive-binary' >"$output_dir/amq"
    chmod 0755 "$output_dir/amq"
    exit 1
    ;;
  missing-archive-binary)
    exit 0
    ;;
  version-check-failure)
    printf '%s\n' \
      '#!/bin/bash' \
      'touch "$TEST_STATE_DIR/prepublish-staged.called"' \
      'exit 42' >"$output_dir/amq"
    chmod 0755 "$output_dir/amq"
    ;;
  *)
    printf '%s\n' \
      '#!/bin/bash' \
      'if [[ "$0" == "$TEST_INSTALL_DIR/amq" ]]; then' \
      '  touch "$TEST_STATE_DIR/postinstall-final.called"' \
      'elif [[ "$0" == *"/.amq.install."*"/amq" ]]; then' \
      '  touch "$TEST_STATE_DIR/prepublish-staged.called"' \
      'else' \
      '  touch "$TEST_STATE_DIR/postinstall-wrong-path.called"' \
      'fi' \
      "printf '%s\\n' 'amq test-version'" >"$output_dir/amq"
    chmod 0755 "$output_dir/amq"
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
if [[ "$TEST_SCENARIO" == staged-binary-empty ]]; then
  : >"$target_path"
  chmod 0755 "$target_path"
  exit 0
fi
if [[ "$TEST_SCENARIO" == signal-int ]]; then
  kill -INT "$PPID"
  /bin/sleep 0.1
  exit 1
fi
if [[ "$TEST_SCENARIO" == signal-term ]]; then
  kill -TERM "$PPID"
  /bin/sleep 0.1
  exit 1
fi
cp "$source_path" "$target_path"
chmod 0755 "$target_path"
EOF

  cat >"$bin_dir/mv" <<'EOF'
#!/bin/bash
set -euo pipefail
case "$TEST_SCENARIO" in
  publish-failure)
    exit 1
    ;;
  target-shape-change)
    rm -f "$TEST_INSTALL_DIR/amq"
    mkdir "$TEST_INSTALL_DIR/amq"
    exec "$TEST_REAL_MV" "$@"
    ;;
  *)
    exec "$TEST_REAL_MV" "$@"
    ;;
esac
EOF

  cat >"$bin_dir/rmdir" <<'EOF'
#!/bin/bash
set -euo pipefail
if [[ "$TEST_SCENARIO" == postpublish-rmdir-failure ]]; then
  exit 1
fi
exec "$TEST_REAL_RMDIR" "$@"
EOF

  cat >"$bin_dir/amq" <<'EOF'
#!/bin/bash
touch "$TEST_STATE_DIR/path-amq.called"
printf '%s\n' 'wrong PATH amq'
EOF

  chmod +x \
    "$bin_dir/awk" \
    "$bin_dir/uname" \
    "$bin_dir/curl" \
    "$bin_dir/tar" \
    "$bin_dir/install" \
    "$bin_dir/mv" \
    "$bin_dir/rmdir" \
    "$bin_dir/amq"
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
  local target_shape=${6:-regular}
  local case_root="$TEST_ROOT/$scenario"
  local bin_dir="$case_root/bin"
  local caller_dir="$case_root/caller"
  local install_dir="$case_root/install"
  local install_arg="$install_dir"
  local state_dir="$case_root/state"
  local home_dir="$case_root/home"
  local temp_dir="$case_root/tmp"
  local referent_file="$case_root/referent-amq"
  local referent_dir="$case_root/referent-dir"
  local output_file="$case_root/output"
  local leaks
  local real_awk
  local real_mv
  local real_rmdir
  local status

  real_awk=$(command -v awk)
  real_mv=$(command -v mv)
  real_rmdir=$(command -v rmdir)
  if [[ "$scenario" == relative-install-dir ]]; then
    install_dir="$caller_dir/relative-bin"
    install_arg=relative-bin
  fi

  mkdir -p \
    "$bin_dir" \
    "$install_dir" \
    "$state_dir" \
    "$home_dir" \
    "$caller_dir" \
    "$temp_dir"
  case "$target_shape" in
    absent)
      ;;
    regular|target-shape-change)
      printf '%s\n' 'existing-binary' >"$install_dir/amq"
      chmod 0711 "$install_dir/amq"
      ;;
    symlink-file)
      printf '%s\n' 'referent-binary' >"$referent_file"
      chmod 0711 "$referent_file"
      ln -s "$referent_file" "$install_dir/amq"
      ;;
    directory)
      mkdir "$install_dir/amq"
      printf '%s\n' 'directory-sentinel' >"$install_dir/amq/sentinel"
      ;;
    symlink-dir)
      mkdir "$referent_dir"
      printf '%s\n' 'directory-sentinel' >"$referent_dir/sentinel"
      ln -s "$referent_dir" "$install_dir/amq"
      ;;
    *)
      printf 'unknown target shape: %s\n' "$target_shape" >&2
      exit 2
      ;;
  esac
  write_common_tools "$bin_dir"
  if [[ "$scenario" == path-same-symlink ]]; then
    rm -f "$bin_dir/amq"
    ln -s "$install_dir/amq" "$bin_dir/amq"
  fi

  case "$hash_tool" in
    sha256sum) write_sha256sum "$bin_dir" ;;
    shasum) write_shasum "$bin_dir" ;;
    none) ;;
    *) printf 'unknown hash tool: %s\n' "$hash_tool" >&2; exit 2 ;;
  esac

  set +e
  (
    cd "$caller_dir"
    PATH="$bin_dir" \
      HOME="$home_dir" \
      TMPDIR="$temp_dir" \
      VERSION="$VERSION" \
      INSTALL_DIR="$install_arg" \
      TEST_SCENARIO="$scenario" \
      TEST_STATE_DIR="$state_dir" \
      TEST_INSTALL_DIR="$install_dir" \
      TEST_REAL_AWK="$real_awk" \
      TEST_REAL_MV="$real_mv" \
      TEST_REAL_RMDIR="$real_rmdir" \
      GOOD_HASH="$GOOD_HASH" \
      BAD_HASH="$BAD_HASH" \
      ASSET="$ASSET" \
      /bin/bash "$INSTALL_SCRIPT"
  ) >"$output_file" 2>&1
  status=$?
  set -e

  if [[ "$expected" == failure ]]; then
    if ((status == 0)); then
      printf 'FAIL %s: expected nonzero exit\n' "$scenario" >&2
      sed -n '1,120p' "$output_file" >&2
      return 1
    fi
    case "$target_shape" in
      absent)
        [[ ! -e "$install_dir/amq" && ! -L "$install_dir/amq" ]] || {
          printf 'FAIL %s: absent target was created\n' "$scenario" >&2
          return 1
        }
        ;;
      regular)
        [[ $(<"$install_dir/amq") == existing-binary ]] || {
          printf 'FAIL %s: existing target changed\n' "$scenario" >&2
          return 1
        }
        [[ $(file_mode "$install_dir/amq") == 711 ]] || {
          printf 'FAIL %s: existing target mode changed\n' "$scenario" >&2
          return 1
        }
        ;;
      target-shape-change)
        [[ -d "$install_dir/amq" && ! -L "$install_dir/amq" ]] || {
          printf 'FAIL %s: target shape-change fixture was not retained\n' "$scenario" >&2
          return 1
        }
        ;;
      directory)
        [[ -d "$install_dir/amq" && ! -L "$install_dir/amq" ]] || {
          printf 'FAIL %s: target directory changed type\n' "$scenario" >&2
          return 1
        }
        [[ $(<"$install_dir/amq/sentinel") == directory-sentinel ]] || {
          printf 'FAIL %s: target directory sentinel changed\n' "$scenario" >&2
          return 1
        }
        ;;
      symlink-dir)
        [[ -L "$install_dir/amq" && $(readlink "$install_dir/amq") == "$referent_dir" ]] || {
          printf 'FAIL %s: target directory symlink changed\n' "$scenario" >&2
          return 1
        }
        [[ $(<"$referent_dir/sentinel") == directory-sentinel ]] || {
          printf 'FAIL %s: directory referent changed\n' "$scenario" >&2
          return 1
        }
        ;;
      symlink-file)
        [[ -L "$install_dir/amq" ]] || {
          printf 'FAIL %s: target file symlink changed\n' "$scenario" >&2
          return 1
        }
        ;;
    esac
  else
    if ((status != 0)); then
      printf 'FAIL %s: expected zero exit, got %s\n' "$scenario" "$status" >&2
      sed -n '1,120p' "$output_file" >&2
      return 1
    fi
    [[ ! -L "$install_dir/amq" && -f "$install_dir/amq" ]] || {
      printf 'FAIL %s: target is not a regular non-symlink file\n' "$scenario" >&2
      return 1
    }
    [[ $(file_mode "$install_dir/amq") == 755 ]] || {
      printf 'FAIL %s: installed target mode is not 0755\n' "$scenario" >&2
      return 1
    }
    [[ $(TEST_STATE_DIR="$state_dir" TEST_INSTALL_DIR="$install_dir" \
      "$install_dir/amq" --version) == "amq test-version" ]] || {
      printf 'FAIL %s: installed target did not execute\n' "$scenario" >&2
      return 1
    }
  fi

  if [[ "$scenario" == version-check-failure ]]; then
    if grep -F "Installation complete!" "$output_file" >/dev/null; then
      printf 'FAIL %s: success banner printed before version validation\n' "$scenario" >&2
      return 1
    fi
  fi

  if [[ "$target_shape" == symlink-file ]]; then
    [[ $(<"$referent_file") == referent-binary ]] || {
      printf 'FAIL %s: symlink referent bytes changed\n' "$scenario" >&2
      return 1
    }
    [[ $(file_mode "$referent_file") == 711 ]] || {
      printf 'FAIL %s: symlink referent mode changed\n' "$scenario" >&2
      return 1
    }
  fi
  if [[ "$target_shape" == symlink-dir ]]; then
    [[ $(<"$referent_dir/sentinel") == directory-sentinel ]] || {
      printf 'FAIL %s: directory symlink referent changed\n' "$scenario" >&2
      return 1
    }
    [[ $(find "$referent_dir" -mindepth 1 -maxdepth 1 | wc -l | tr -d '[:space:]') == 1 ]] || {
      printf 'FAIL %s: publication descended into directory symlink referent\n' "$scenario" >&2
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

  leaks=$(find "$case_root" -name '.amq.install.*' -print -quit)
  if [[ -n "$leaks" && "$scenario" != postpublish-rmdir-failure ]]; then
    printf 'FAIL %s: staged install file leaked\n' "$scenario" >&2
    return 1
  fi
  leaks=$(find "$temp_dir" -mindepth 1 -print -quit)
  if [[ -n "$leaks" ]]; then
    printf 'FAIL %s: temporary download state leaked\n' "$scenario" >&2
    return 1
  fi

  if [[ "$expected" == success ]]; then
    [[ -e "$state_dir/prepublish-staged.called" ]] || {
      printf 'FAIL %s: staged binary was not verified before publication\n' "$scenario" >&2
      return 1
    }
    [[ -e "$state_dir/postinstall-final.called" ]] || {
      printf 'FAIL %s: exact installed binary was not verified\n' "$scenario" >&2
      return 1
    }
  elif [[ "$scenario" == version-check-failure ]]; then
    [[ -e "$state_dir/prepublish-staged.called" ]] || {
      printf 'FAIL %s: failing staged binary was not executed before publication\n' "$scenario" >&2
      return 1
    }
  else
    [[ ! -e "$state_dir/postinstall-final.called" ]] || {
      printf 'FAIL %s: postinstall verification ran after failure\n' "$scenario" >&2
      return 1
    }
  fi
  [[ ! -e "$state_dir/postinstall-wrong-path.called" ]] || {
    printf 'FAIL %s: postinstall verification used a non-final path\n' "$scenario" >&2
    return 1
  }
  [[ ! -e "$state_dir/path-amq.called" ]] || {
    printf 'FAIL %s: postinstall verification resolved amq through PATH\n' "$scenario" >&2
    return 1
  }

  case "$scenario" in
    stale-path)
      grep -F "Warning: PATH resolves amq to a different executable" "$output_file" >/dev/null || {
        printf 'FAIL %s: stale PATH warning missing\n' "$scenario" >&2
        return 1
      }
      grep -F "  PATH-selected: $bin_dir/amq" "$output_file" >/dev/null || {
        printf 'FAIL %s: stale PATH warning omitted selected executable\n' "$scenario" >&2
        return 1
      }
      grep -F "  Verified install: $install_dir/amq" "$output_file" >/dev/null || {
        printf 'FAIL %s: stale PATH warning omitted verified executable\n' "$scenario" >&2
        return 1
      }
      grep -F "  export PATH=\"$install_dir:\$PATH\"" "$output_file" >/dev/null || {
        printf 'FAIL %s: stale PATH warning omitted concrete repair\n' "$scenario" >&2
        return 1
      }
      ;;
    path-same-symlink)
      if grep -F "Warning: PATH resolves amq to a different executable" "$output_file" >/dev/null; then
        printf 'FAIL %s: same-file PATH symlink was reported as stale\n' "$scenario" >&2
        return 1
      fi
      ;;
  esac
  case "$scenario" in
    stale-path|path-same-symlink)
      grep -F "  1. Start agent: \"$install_dir/amq\" coop exec claude" "$output_file" >/dev/null || {
        printf 'FAIL %s: immediate next step did not use verified binary\n' "$scenario" >&2
        return 1
      }
      if grep -F "  1. Start agent: amq coop exec claude" "$output_file" >/dev/null; then
        printf 'FAIL %s: immediate next step retained stale PATH lookup\n' "$scenario" >&2
        return 1
      fi
      ;;
  esac

  printf 'PASS %s\n' "$scenario"
}

manual_install_snippet() {
  awk '
    /^For manual installs, verify / { armed = 1 }
    armed && /^```bash$/ { capture = 1; next }
    capture && /^```$/ { exit }
    capture { print }
  ' "$INSTALL_DOC"
}

run_manual_case() {
  local scenario=$1
  local expected=$2
  local hash_tool=${3:-none}
  local expected_tar=${4:-no}
  local expected_publish=${5:-no}
  local archive_shape=${6:-regular}
  local case_root="$TEST_ROOT/$scenario"
  local bin_dir="$case_root/bin"
  local state_dir="$case_root/state"
  local home_dir="$case_root/home"
  local caller_dir="$case_root/caller"
  local temp_dir="$case_root/tmp"
  local target_dir="$home_dir/.local/bin"
  local archive_referent="$case_root/archive-referent"
  local sidecar_sentinel="$case_root/sidecar-sentinel"
  local output_file="$case_root/output"
  local leaks
  local real_awk
  local real_mkdir
  local real_mktemp
  local real_mv
  local real_rmdir
  local snippet
  local status

  real_awk=$(command -v awk)
  real_mkdir=$(command -v mkdir)
  real_mktemp=$(command -v mktemp)
  real_mv=$(command -v mv)
  real_rmdir=$(command -v rmdir)
  mkdir -p "$bin_dir" "$state_dir" "$target_dir" "$caller_dir" "$temp_dir"
  printf '%s\n' 'old-installed-binary' >"$target_dir/amq"
  chmod 0711 "$target_dir/amq"
  printf '%s\n' 'stale-caller-amq' >"$caller_dir/amq"
  chmod 0755 "$caller_dir/amq"
  if [[ "$archive_shape" == symlink ]]; then
    printf '%s\n' 'archive-data' >"$archive_referent"
    ln -s "$archive_referent" \
      "$caller_dir/amq_X.Y.Z_darwin_arm64.tar.gz"
  else
    printf '%s\n' 'archive-data' \
      >"$caller_dir/amq_X.Y.Z_darwin_arm64.tar.gz"
  fi
  printf '%s  amq_X.Y.Z_darwin_arm64.tar.gz\n' "$GOOD_HASH" \
    >"$caller_dir/checksums.txt"
  printf '%s\n' 'sidecar-sentinel' >"$sidecar_sentinel"
  if [[ "$scenario" == manual-record-write-failure ]]; then
    mkdir "$case_root/sidecar-directory"
    ln -s "$case_root/sidecar-directory" \
      "$caller_dir/amq_X.Y.Z_darwin_arm64.tar.gz.sha256"
  else
    ln -s "$sidecar_sentinel" \
      "$caller_dir/amq_X.Y.Z_darwin_arm64.tar.gz.sha256"
  fi

  for command_name in chmod cmp cp install ln rm touch tr; do
    link_command "$bin_dir" "$command_name"
  done

  cat >"$bin_dir/awk" <<'EOF'
#!/bin/bash
set -euo pipefail
if [[ "$TEST_SCENARIO" == manual-parser-failure ]]; then
  exit 2
fi
exec "$TEST_REAL_AWK" "$@"
EOF

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
if [[ "$TEST_SCENARIO" == manual-curl-stale-failure ]]; then
  exit 22
fi
if [[ "$TEST_SCENARIO" == manual-checksum-crlf ]]; then
  printf '%s  amq_X.Y.Z_darwin_arm64.tar.gz\r\n' "$GOOD_HASH" >"$output"
else
  printf '%s  amq_X.Y.Z_darwin_arm64.tar.gz\n' "$GOOD_HASH" >"$output"
fi
EOF

  if [[ "$hash_tool" != none ]]; then
    cat >"$bin_dir/$hash_tool" <<'EOF'
#!/bin/bash
set -euo pipefail
touch "$TEST_STATE_DIR/hash.called"
case "$TEST_SCENARIO" in
  manual-sha256sum-mismatch|manual-shasum-mismatch)
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
EOF
    chmod +x "$bin_dir/$hash_tool"
  fi

  cat >"$bin_dir/tar" <<'EOF'
#!/bin/bash
set -euo pipefail
touch "$TEST_STATE_DIR/tar.called"
output_dir=$PWD
while (($#)); do
  case "$1" in
    -C)
      output_dir=$2
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
case "$TEST_SCENARIO" in
  manual-tar-failure)
    exit 1
    ;;
  manual-tar-partial-failure)
    printf '%s\n' 'partial-extracted-amq' >"$output_dir/amq"
    chmod 0755 "$output_dir/amq"
    exit 1
    ;;
  manual-missing-extracted-amq)
    exit 0
    ;;
  manual-version-check-failure)
    printf '%s\n' '#!/bin/bash' 'exit 42' >"$output_dir/amq"
    chmod 0755 "$output_dir/amq"
    ;;
  *)
    printf '%s\n' \
      '#!/bin/bash' \
      "printf '%s\\n' 'amq manual-test-version'" >"$output_dir/amq"
    chmod 0755 "$output_dir/amq"
    ;;
esac
EOF

  cat >"$bin_dir/mkdir" <<'EOF'
#!/bin/bash
set -euo pipefail
if [[ "$TEST_SCENARIO" == manual-mkdir-failure &&
      "${*: -1}" == "$HOME/.local/bin" ]]; then
  exit 1
fi
exec "$TEST_REAL_MKDIR" "$@"
EOF

  cat >"$bin_dir/mktemp" <<'EOF'
#!/bin/bash
set -euo pipefail
result=$("$TEST_REAL_MKTEMP" "$@")
count=0
if [[ -f "$TEST_STATE_DIR/mktemp.count" ]]; then
  count=$(<"$TEST_STATE_DIR/mktemp.count")
fi
count=$((count + 1))
printf '%s\n' "$count" >"$TEST_STATE_DIR/mktemp.count"
if [[ "$TEST_SCENARIO" == manual-record-write-failure && "$count" -eq 1 ]]; then
  "$TEST_REAL_MKDIR" "$result/selected.sha256"
fi
printf '%s\n' "$result"
if [[ "$count" -eq 1 ]]; then
  case "$TEST_SCENARIO" in
    manual-signal-int)
      kill -INT "$PPID"
      ;;
    manual-signal-term)
      kill -TERM "$PPID"
      ;;
  esac
fi
EOF

  cat >"$bin_dir/mv" <<'EOF'
#!/bin/bash
set -euo pipefail
touch "$TEST_STATE_DIR/publish.called"
if [[ "$TEST_SCENARIO" == manual-publish-failure ]]; then
  exit 1
fi
exec "$TEST_REAL_MV" "$@"
EOF

  cat >"$bin_dir/rmdir" <<'EOF'
#!/bin/bash
set -euo pipefail
if [[ "$TEST_SCENARIO" == manual-postpublish-rmdir-failure ]]; then
  exit 1
fi
exec "$TEST_REAL_RMDIR" "$@"
EOF

  chmod +x \
    "$bin_dir/awk" \
    "$bin_dir/curl" \
    "$bin_dir/tar" \
    "$bin_dir/mkdir" \
    "$bin_dir/mktemp" \
    "$bin_dir/mv" \
    "$bin_dir/rmdir"
  snippet=$(manual_install_snippet)
  [[ -n "$snippet" ]] || {
    printf 'FAIL %s: INSTALL.md snippet not found\n' "$scenario" >&2
    return 1
  }

  set +e
  (
    cd "$caller_dir"
    PATH="$bin_dir" \
      HOME="$home_dir" \
      TMPDIR="$temp_dir" \
      TEST_SCENARIO="$scenario" \
      TEST_STATE_DIR="$state_dir" \
      TEST_REAL_AWK="$real_awk" \
      TEST_REAL_MKDIR="$real_mkdir" \
      TEST_REAL_MKTEMP="$real_mktemp" \
      TEST_REAL_MV="$real_mv" \
      TEST_REAL_RMDIR="$real_rmdir" \
      GOOD_HASH="$GOOD_HASH" \
      /bin/bash -c "
$snippet
snippet_status=\$?
printf '%s\n' survived >'$state_dir/shell-survived'
exit \"\$snippet_status\"
"
  ) >"$output_file" 2>&1
  status=$?
  set -e

  if [[ "$expected" == failure ]]; then
    if ((status == 0)); then
      printf 'FAIL %s: expected nonzero exit\n' "$scenario" >&2
      sed -n '1,120p' "$output_file" >&2
      return 1
    fi
  elif ((status != 0)); then
    printf 'FAIL %s: expected zero exit, got %s\n' "$scenario" "$status" >&2
    sed -n '1,120p' "$output_file" >&2
    return 1
  fi
  [[ -e "$state_dir/shell-survived" ]] || {
    printf 'FAIL %s: documentation failure exited the caller shell\n' "$scenario" >&2
    return 1
  }

  if [[ "$expected_tar" == yes ]]; then
    [[ -e "$state_dir/tar.called" ]] || {
      printf 'FAIL %s: extraction did not reach the intended failure\n' "$scenario" >&2
      return 1
    }
  else
    [[ ! -e "$state_dir/tar.called" ]] || {
      printf 'FAIL %s: extraction occurred after an earlier failure\n' "$scenario" >&2
      return 1
    }
  fi
  if [[ "$expected_publish" == yes ]]; then
    [[ -e "$state_dir/publish.called" ]] || {
      printf 'FAIL %s: publication did not reach the intended failure\n' "$scenario" >&2
      return 1
    }
  else
    [[ ! -e "$state_dir/publish.called" ]] || {
      printf 'FAIL %s: publication occurred after an earlier failure\n' "$scenario" >&2
      return 1
    }
  fi

  if [[ "$expected" == failure ]]; then
    [[ $(<"$target_dir/amq") == old-installed-binary ]] || {
      printf 'FAIL %s: existing installed binary changed\n' "$scenario" >&2
      return 1
    }
    [[ $(file_mode "$target_dir/amq") == 711 ]] || {
      printf 'FAIL %s: existing installed mode changed\n' "$scenario" >&2
      return 1
    }
  else
    [[ ! -L "$target_dir/amq" && -f "$target_dir/amq" ]] || {
      printf 'FAIL %s: installed target is not a regular file\n' "$scenario" >&2
      return 1
    }
    [[ $(file_mode "$target_dir/amq") == 755 ]] || {
      printf 'FAIL %s: installed target mode is not 0755\n' "$scenario" >&2
      return 1
    }
    [[ $("$target_dir/amq" --version) == "amq manual-test-version" ]] || {
      printf 'FAIL %s: installed target did not execute\n' "$scenario" >&2
      return 1
    }
  fi
  [[ $(<"$caller_dir/amq") == stale-caller-amq ]] || {
    printf 'FAIL %s: stale caller amq was consumed or changed\n' "$scenario" >&2
    return 1
  }
  if [[ "$scenario" != manual-record-write-failure ]]; then
    [[ -L "$caller_dir/amq_X.Y.Z_darwin_arm64.tar.gz.sha256" ]] || {
      printf 'FAIL %s: caller sidecar symlink was replaced\n' "$scenario" >&2
      return 1
    }
    [[ $(<"$sidecar_sentinel") == sidecar-sentinel ]] || {
      printf 'FAIL %s: caller sidecar symlink referent changed\n' "$scenario" >&2
      return 1
    }
  fi
  leaks=$(find "$temp_dir" "$target_dir" -name '.amq.*' -print -quit)
  [[ -z "$leaks" || "$scenario" == manual-postpublish-rmdir-failure ]] || {
    printf 'FAIL %s: private manual install state leaked\n' "$scenario" >&2
    return 1
  }

  printf 'PASS %s\n' "$scenario"
}

run_case checksum-download-failure failure sha256sum
run_case missing-checksums failure sha256sum
run_case unreadable failure sha256sum
run_case parser-failure failure sha256sum
run_case missing-entry failure sha256sum
run_case duplicate failure sha256sum
run_case malformed failure sha256sum
run_case checksum-crlf success sha256sum yes yes
run_case no-tool failure
run_case sha256sum-mismatch failure sha256sum
run_case shasum-mismatch failure shasum
run_case tar-failure failure sha256sum yes no
run_case tar-partial-failure failure sha256sum yes no
run_case missing-archive-binary failure sha256sum yes no
run_case install-partial-failure failure sha256sum yes yes
run_case staged-binary-empty failure sha256sum yes yes
run_case version-check-failure failure sha256sum yes yes
run_case signal-int failure sha256sum yes yes
run_case signal-term failure sha256sum yes yes
run_case publish-failure failure sha256sum yes yes
run_case postpublish-rmdir-failure success sha256sum yes yes
run_case target-directory failure sha256sum yes yes directory
run_case target-symlink-dir success sha256sum yes yes symlink-dir
run_case target-shape-change failure sha256sum yes yes target-shape-change
run_case target-absent success sha256sum yes yes absent
run_case target-regular success sha256sum yes yes regular
run_case target-symlink-file success sha256sum yes yes symlink-file
run_case relative-install-dir success sha256sum yes yes regular
run_case shasum-valid success shasum yes yes regular
run_case stale-path success sha256sum yes yes regular
run_case path-same-symlink success sha256sum yes yes regular
run_manual_case manual-curl-stale-failure failure sha256sum no no
run_manual_case manual-record-write-failure failure sha256sum no no
run_manual_case manual-parser-failure failure sha256sum no no
run_manual_case manual-no-hash failure none no no
run_manual_case manual-sha256sum-mismatch failure sha256sum no no
run_manual_case manual-shasum-mismatch failure shasum no no
run_manual_case manual-tar-failure failure sha256sum yes no
run_manual_case manual-tar-partial-failure failure sha256sum yes no
run_manual_case manual-missing-extracted-amq failure sha256sum yes no
run_manual_case manual-mkdir-failure failure sha256sum yes no
run_manual_case manual-publish-failure failure sha256sum yes yes
run_manual_case manual-version-check-failure failure sha256sum yes no
run_manual_case manual-signal-term failure sha256sum no no
run_manual_case manual-success success sha256sum yes yes
run_manual_case manual-checksum-crlf success sha256sum yes yes
run_manual_case manual-archive-symlink success sha256sum yes yes symlink
run_manual_case manual-postpublish-rmdir-failure success sha256sum yes yes
