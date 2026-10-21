#!/usr/bin/env bash
set -euo pipefail

binary_path="${1:?usage: ./scripts/codesign-macos.sh /path/to/binary identifier [target-os]}"
identifier="${2:?usage: ./scripts/codesign-macos.sh /path/to/binary identifier [target-os]}"
target_os="${3:-darwin}"

if [[ "$target_os" != "darwin" ]]; then
  exit 0
fi

if [[ ! -f "$binary_path" ]]; then
  echo "error: binary not found: $binary_path" >&2
  exit 1
fi

if [[ "$(uname -s)" != "Darwin" ]]; then
  exit 0
fi

# Release builds: sign with the Developer ID that release.yml imports, then
# notarize.
#
# macOS keys a privacy grant such as Automation of Ghostty (and a Keychain
# partition) to the code identity. For ad-hoc code that identity is the cdhash,
# which changes on every build, so each upgrade asks the user again. For
# Developer ID code it is anchored to our team, so one grant survives upgrades.
# Keep the default designated requirement: it anchors to the Apple-issued
# certificate of our team, not only to the identifier.
if [[ -n "${AMQ_MACOS_TEAM_ID:-}" ]]; then
  : "${AMQ_MACOS_KEYCHAIN:?AMQ_MACOS_KEYCHAIN must be set with AMQ_MACOS_TEAM_ID}"
  : "${AMQ_MACOS_SIGN_IDENTITY:?AMQ_MACOS_SIGN_IDENTITY must be set with AMQ_MACOS_TEAM_ID}"
  : "${AMQ_NOTARY_KEY_PATH:?AMQ_NOTARY_KEY_PATH must be set with AMQ_MACOS_TEAM_ID}"
  : "${AMQ_NOTARY_KEY_ID:?AMQ_NOTARY_KEY_ID must be set with AMQ_MACOS_TEAM_ID}"
  : "${AMQ_NOTARY_ISSUER_ID:?AMQ_NOTARY_ISSUER_ID must be set with AMQ_MACOS_TEAM_ID}"

  work_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/amq-notary.XXXXXX")"
  trap 'rm -rf "$work_dir"' EXIT

  # Notarization requires the hardened runtime, and the hardened runtime
  # refuses Apple Events unless this entitlement is present. AMQ sends them
  # through osascript (Ghostty launch and wake adapters). Under launchd the
  # supervisor binary itself is the responsible process that TCC checks, so
  # every AMQ binary carries the entitlement, not only the one that runs
  # osascript.
  cat >"$work_dir/entitlements.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>com.apple.security.automation.apple-events</key>
	<true/>
</dict>
</plist>
PLIST

  codesign \
    --force \
    --options runtime \
    --timestamp \
    --entitlements "$work_dir/entitlements.plist" \
    --keychain "$AMQ_MACOS_KEYCHAIN" \
    --sign "$AMQ_MACOS_SIGN_IDENTITY" \
    --identifier "$identifier" \
    "$binary_path"

  team_id="$(codesign -dv "$binary_path" 2>&1 | sed -n 's/^TeamIdentifier=//p')"
  if [[ "$team_id" != "$AMQ_MACOS_TEAM_ID" ]]; then
    echo "error: $binary_path has TeamIdentifier '$team_id', expected '$AMQ_MACOS_TEAM_ID'" >&2
    exit 1
  fi

  # notarytool takes a zip; a bare Mach-O cannot be stapled, so Gatekeeper
  # checks the ticket online.
  ditto -c -k --keepParent "$binary_path" "$work_dir/binary.zip"
  submit_rc=0
  xcrun notarytool submit "$work_dir/binary.zip" \
    --key "$AMQ_NOTARY_KEY_PATH" \
    --key-id "$AMQ_NOTARY_KEY_ID" \
    --issuer "$AMQ_NOTARY_ISSUER_ID" \
    --wait \
    --timeout 20m \
    --output-format json >"$work_dir/result.json" || submit_rc=$?

  status="$(plutil -extract status raw -o - "$work_dir/result.json" 2>/dev/null || echo "unknown (notarytool exit $submit_rc)")"
  if [[ "$status" != "Accepted" ]]; then
    submission_id="$(plutil -extract id raw -o - "$work_dir/result.json" 2>/dev/null || true)"
    echo "error: notarization of $binary_path ended with status '$status' (submission $submission_id)" >&2
    if [[ -n "$submission_id" ]]; then
      xcrun notarytool log "$submission_id" \
        --key "$AMQ_NOTARY_KEY_PATH" \
        --key-id "$AMQ_NOTARY_KEY_ID" \
        --issuer "$AMQ_NOTARY_ISSUER_ID" >&2 || true
    fi
    exit 1
  fi
  exit 0
fi

# Local, fork and snapshot builds, and releases without the signing secrets:
# sign ad-hoc with the stable identifier.
codesign --force --sign - --identifier "$identifier" "$binary_path"
