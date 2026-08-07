#!/usr/bin/env bash
# Run the Linux-only wake self-upgrade E2E under a privileged container.  The
# raw wake transport is intentionally exercised, so this gate enables the
# kernel's legacy TIOCSTI opt-in and fails if the host/container cannot provide
# it.  A skipped E2E is not a passing gate.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
image="${AMQ_LINUX_GATE_IMAGE:-golang:1.25-bookworm}"

if ! command -v docker >/dev/null 2>&1; then
  echo "error: Docker is required for the Linux wake self-upgrade gate" >&2
  exit 1
fi

exec docker run --rm --privileged --init \
  --mount "type=bind,src=${repo_root},dst=/src,readonly" \
  --workdir /src \
  -e GOCACHE=/tmp/go-build \
  -e GOMODCACHE=/tmp/go-mod \
  "${image}" \
  bash -euo pipefail -c '
    sysctl=/proc/sys/dev/tty/legacy_tiocsti
    if [[ ! -r "$sysctl" || ! -w "$sysctl" ]]; then
      echo "error: privileged container cannot read and write $sysctl" >&2
      exit 1
    fi
    original="$(<"$sysctl")"
    case "$original" in
      0|1) ;;
      *)
        echo "error: unexpected $sysctl value: $original" >&2
        exit 1
        ;;
    esac
    restore_tiocsti() { printf "%s\\n" "$original" >"$sysctl"; }
    trap restore_tiocsti EXIT
    printf "1\\n" >"$sysctl"
    if [[ "$(<"$sysctl")" != 1 ]]; then
      echo "error: failed to enable $sysctl" >&2
      exit 1
    fi

    tests="^(TestLinuxWakeSelfUpgradeRealPTYStableSymlink|TestLinuxWakeRestartBindingSurvivesPublicPathSwap|TestProbeBoundWakeSelfUpgradeVersion)$"
    output=/tmp/wake-self-upgrade-linux-gate.log
    go test -count=1 -v ./internal/cli -run "$tests" 2>&1 | tee "$output"

    for test_name in \
      TestLinuxWakeSelfUpgradeRealPTYStableSymlink \
      TestLinuxWakeRestartBindingSurvivesPublicPathSwap \
      TestProbeBoundWakeSelfUpgradeVersion; do
      if ! grep -Fq -- "--- PASS: $test_name" "$output"; then
        echo "error: required test did not pass: $test_name" >&2
        exit 1
      fi
      if grep -Fq -- "--- SKIP: $test_name" "$output"; then
        echo "error: required test was skipped: $test_name" >&2
        exit 1
      fi
    done
  '
