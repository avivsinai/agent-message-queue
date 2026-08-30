#!/usr/bin/env bash
# Synthetic envelope hop-probe test. Does not claim a live G↔Mac Bot transfer.
set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
probe="$script_dir/amq-bot-envelope-hop-probe.sh"
work="$(mktemp -d "${TMPDIR:-/tmp}/amq-bot-envelope-hop-probe-test.XXXXXX")"
cleanup() { rm -rf "$work"; }
trap cleanup EXIT INT TERM

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

amq_bin="$work/amq"
bridge_bin="$work/amq-bridge"
(
  cd "$repo_root"
  go build -o "$amq_bin" ./cmd/amq
  go build -o "$bridge_bin" ./cmd/amq-bridge
)

g_root="$work/g-root"
g_dir="$work/g-hop"
mac_dir="$work/mac-hop"
grok_public=''
grok_generation=''

"$amq_bin" init --root "$g_root" --agents codex,claude >/dev/null
mkdir -p "$g_root/bridge"
printf 'grok\n' >"$g_root/bridge/host-id"
chmod 600 "$g_root/bridge/host-id"
"$bridge_bin" identity init --root "$g_root" --generation 1 >/dev/null

write_probe() {
  AMQ_BOT_ENVELOPE_HOP_G_ROOT="$g_root" \
  AMQ_BOT_ENVELOPE_HOP_G_DIR="$g_dir" \
  AMQ_BOT_ENVELOPE_HOP_BRIDGE_BIN="$bridge_bin" \
  sh "$probe" write
}

first_write="$(write_probe)"
first_id="$(printf '%s\n' "$first_write" | sed -n 's/^BOT_ENVELOPE_HOP_TRANSFER_ID=//p')"
first_source="$(printf '%s\n' "$first_write" | sed -n 's/^BOT_ENVELOPE_HOP_SOURCE=//p')"
grok_public="$(printf '%s\n' "$first_write" | sed -n 's/^BOT_ENVELOPE_HOP_PUBLIC=//p')"
grok_generation="$(printf '%s\n' "$first_write" | sed -n 's/^BOT_ENVELOPE_HOP_KEY_GENERATION=//p')"
[[ ${#first_id} -eq 52 ]] || fail "write did not print a 52-char transfer id"
case "$first_id" in *[!a-z2-7]*) fail "write printed a non-base32 transfer id" ;; esac
[[ -f "$first_source" ]] || fail "write did not create $first_source"
[[ -n "$grok_public" && -n "$grok_generation" ]] || fail "write did not expose trusted public identity metadata"

setup_mac_root() {
  local root="$1"
  "$amq_bin" init --root "$root" --agents claude >/dev/null
  mkdir -p "$root/bridge/trusted"
  printf 'mac\n' >"$root/bridge/host-id"
  chmod 600 "$root/bridge/host-id"
  printf 'generation %s\npublic %s\n' "$grok_generation" "$grok_public" >"$root/bridge/trusted/grok"
  chmod 600 "$root/bridge/trusted/grok"
}

mac_root="$work/mac-root"
setup_mac_root "$mac_root"
mkdir -p "$mac_dir"
cp "$first_source" "$mac_dir/envelope-$first_id.json"
chmod 644 "$mac_dir/envelope-$first_id.json"

first_check="$(
  AMQ_BOT_ENVELOPE_HOP_MAC_ROOT="$mac_root" \
  AMQ_BOT_ENVELOPE_HOP_MAC_DIR="$mac_dir" \
  AMQ_BOT_ENVELOPE_HOP_BRIDGE_BIN="$bridge_bin" \
  sh "$probe" check "$first_id"
)"
printf '%s\n' "$first_check" | grep -Fq 'BOT_ENVELOPE_HOP_PROVEN transfer_id=' ||
  fail "faithful moved envelope did not prove"
first_payload="$mac_root/agents/claude/inbox/new/xfer-grok-$first_id.md"
[[ -s "$first_payload" ]] || fail "faithful envelope did not commit a payload"

reply_dir="$work/reply-hop"
reply_write="$(
  AMQ_BOT_ENVELOPE_HOP_G_ROOT="$g_root" \
  AMQ_BOT_ENVELOPE_HOP_G_DIR="$reply_dir" \
  AMQ_BOT_ENVELOPE_HOP_SOURCE_HOST=grok \
  AMQ_BOT_ENVELOPE_HOP_SOURCE_HANDLE=codex \
  AMQ_BOT_ENVELOPE_HOP_DEST_ALIAS=mac/claude \
  AMQ_BOT_ENVELOPE_HOP_THREAD="probe/$first_id" \
  sh "$probe" write
)"
reply_id="$(printf '%s\n' "$reply_write" | sed -n 's/^BOT_ENVELOPE_HOP_TRANSFER_ID=//p')"
reply_source="$(printf '%s\n' "$reply_write" | sed -n 's/^BOT_ENVELOPE_HOP_SOURCE=//p')"
[[ -f "$reply_source" ]] || fail "threaded write did not create $reply_source"
python3 - "$reply_source" "probe/$first_id" <<'PY' || fail "threaded write did not keep the inbound thread"
import json, sys
path, want = sys.argv[1], sys.argv[2]
envelope = json.load(open(path, encoding="utf-8"))
if envelope.get("thread_id") != want:
    raise SystemExit(f"thread_id={envelope.get('thread_id')!r} want {want!r}")
PY

second_write="$(write_probe)"
second_id="$(printf '%s\n' "$second_write" | sed -n 's/^BOT_ENVELOPE_HOP_TRANSFER_ID=//p')"
second_source="$(printf '%s\n' "$second_write" | sed -n 's/^BOT_ENVELOPE_HOP_SOURCE=//p')"
[[ ${#second_id} -eq 52 ]] || fail "second write did not print a 52-char transfer id"
case "$second_id" in *[!a-z2-7]*) fail "second write printed a non-base32 transfer id" ;; esac
[[ -f "$second_source" ]] || fail "second write did not create $second_source"

unsigned_root="$work/unsigned-root"
setup_mac_root "$unsigned_root"
unsigned_file="$unsigned_root/bridge/drop/envelope-$second_id.json"
mkdir -p "$(dirname "$unsigned_file")"
cp "$second_source" "$unsigned_file"
chmod 600 "$unsigned_file"
python3 - "$unsigned_file" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as handle:
    envelope = json.load(handle)
envelope["signature"] = ""
with open(path, "w", encoding="utf-8") as handle:
    json.dump(envelope, handle, separators=(",", ":"))
PY
if "$bridge_bin" apply-file --root "$unsigned_root" --file "$unsigned_file" >/dev/null 2>&1; then
  fail "unsigned envelope committed"
fi
[[ ! -e "$unsigned_root/agents/claude/inbox/new/xfer-grok-$second_id.md" ]] ||
  fail "unsigned envelope left a payload"

forged_root="$work/forged-root"
setup_mac_root "$forged_root"
printf 'generation %s\npublic %s\n' "$grok_generation" "$grok_public" >"$forged_root/bridge/trusted/attacker"
chmod 600 "$forged_root/bridge/trusted/attacker"
forged_file="$forged_root/bridge/drop/envelope-$second_id.json"
mkdir -p "$(dirname "$forged_file")"
cp "$second_source" "$forged_file"
chmod 600 "$forged_file"
python3 - "$forged_file" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as handle:
    envelope = json.load(handle)
envelope["source_host"] = "attacker"
with open(path, "w", encoding="utf-8") as handle:
    json.dump(envelope, handle, separators=(",", ":"))
PY
if "$bridge_bin" apply-file --root "$forged_root" --file "$forged_file" >/dev/null 2>&1; then
  fail "forged source_host committed"
fi
[[ ! -e "$forged_root/agents/claude/inbox/new/xfer-attacker-$second_id.md" ]] ||
  fail "forged source_host left a payload"

printf 'amq-bot-envelope-hop-probe synthetic tests passed\n'
