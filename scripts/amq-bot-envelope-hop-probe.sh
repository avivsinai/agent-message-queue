#!/bin/sh
# Probe whether Grok Bot can move one signed bridge envelope from host G to
# this Mac. This is evidence only. It is not an AMQ transport, mailbox, or
# public locker.
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
g_root=${AMQ_BOT_ENVELOPE_HOP_G_ROOT:-$repo_root}
g_dir=${AMQ_BOT_ENVELOPE_HOP_G_DIR:-/workspace/amq/bridge/hop-probe}
mac_dir=${AMQ_BOT_ENVELOPE_HOP_MAC_DIR:-$repo_root/dist/amq-bot-envelope-hop-probe}
mac_root=${AMQ_BOT_ENVELOPE_HOP_MAC_ROOT:-}
source_host=${AMQ_BOT_ENVELOPE_HOP_SOURCE_HOST:-grok}
source_handle=${AMQ_BOT_ENVELOPE_HOP_SOURCE_HANDLE:-codex}
dest_alias=${AMQ_BOT_ENVELOPE_HOP_DEST_ALIAS:-mac/claude}

usage() {
	cat >&2 <<'EOF'
usage:
  amq-bot-envelope-hop-probe.sh write
  amq-bot-envelope-hop-probe.sh check <transfer-id>

write runs on host G. It signs one complete internal/bridge.Envelope with
G's bridge identity and writes it under the hop-probe drop. Bot must move the
reported envelope file to the Mac without pasting its contents.

check runs on the Mac. Set AMQ_BOT_ENVELOPE_HOP_MAC_ROOT to a throwaway AMQ
root whose bridge/host-id matches the destination alias host and whose
bridge/trusted/<source-host> contains G's public identity record. The command
copies the moved envelope into bridge/drop, runs amq-bridge apply-file, and
checks the destination receipt and inbox/new payload.
EOF
}

unproven() {
	printf 'BOT_ENVELOPE_HOP_UNPROVEN: %s\n' "$*"
	exit 1
}

valid_transfer_id() {
	id=$1
	[ -n "$id" ] || return 1
	case "$id" in
		-*) return 1 ;;
		*[!a-z0-9_-]*) return 1 ;;
		.*|*..*) return 1 ;;
		*) return 0 ;;
	esac
}

validate_config() {
	valid_transfer_id "$source_host" || unproven "invalid source host: $source_host"
	case "$dest_alias" in
		*/*) ;;
		*) unproven "invalid destination alias: $dest_alias" ;;
	esac
	case "$dest_alias" in
		*/|*/*/*|/*) unproven "invalid destination alias: $dest_alias" ;;
	esac
	dest_host_config=${dest_alias%%/*}
	dest_agent_config=${dest_alias#*/}
	valid_transfer_id "$dest_host_config" || unproven "invalid destination host: $dest_host_config"
	valid_transfer_id "$dest_agent_config" || unproven "invalid destination agent: $dest_agent_config"
}

file_mode() {
	path=$1
	if mode=$(stat -c '%a' "$path" 2>/dev/null); then
		printf '%s\n' "$mode"
		return 0
	fi
	if mode=$(stat -f '%Lp' "$path" 2>/dev/null); then
		printf '%s\n' "$mode"
		return 0
	fi
	return 1
}

run_writer() {
	(cd "$repo_root" && go run ./scripts/amq-bot-envelope-hop-probe-tool write "$@")
}

run_bridge() {
	if [ -n "${AMQ_BOT_ENVELOPE_HOP_BRIDGE_BIN:-}" ]; then
		"$AMQ_BOT_ENVELOPE_HOP_BRIDGE_BIN" "$@"
		return
	fi
	if command -v amq-bridge >/dev/null 2>&1; then
		amq-bridge "$@"
		return
	fi
	if [ -x "$repo_root/amq-bridge" ]; then
		"$repo_root/amq-bridge" "$@"
		return
	fi
	(cd "$repo_root" && go run ./cmd/amq-bridge "$@")
}

write_probe() {
	[ -d "$g_root" ] || unproven "G root is missing: $g_root"
	[ ! -L "$g_root" ] || unproven "G root must not be a symlink: $g_root"
	if ! mkdir -p "$g_dir" 2>/dev/null; then
		unproven "cannot create required G directory $g_dir"
	fi
	if ! chmod 700 "$g_dir" 2>/dev/null; then
		unproven "cannot set G directory mode on $g_dir"
	fi

	nonce=$(LC_ALL=C od -An -N16 -tx1 /dev/urandom | tr -d '[:space:]') ||
		unproven 'cannot generate transfer nonce'
	transfer_id=xfer-probe-$nonce
	valid_transfer_id "$transfer_id" || unproven 'generated invalid transfer id'
	target=$g_dir/envelope-$transfer_id.json
	output=$(run_writer \
		--root "$g_root" \
		--output "$target" \
		--transfer-id "$transfer_id" \
		--source-host "$source_host" \
		--source-handle "$source_handle" \
		--dest-alias "$dest_alias") ||
		unproven "cannot create signed envelope $target"
	mode=$(file_mode "$target") || unproven "cannot inspect mode on $target"
	case "$mode" in
		600|0600) ;;
		*) unproven "G envelope $target has mode $mode, want 0600" ;;
	esac
	[ -s "$target" ] || unproven "G envelope $target is empty"
	printf '%s\n' "$output"
	printf 'BOT_ENVELOPE_HOP_G_ROOT=%s\n' "$g_root"
	printf 'BOT_ENVELOPE_HOP_G_DIR=%s\n' "$g_dir"
}

check_host_files() {
	expected_host=$1
	[ -n "$mac_root" ] || unproven 'AMQ_BOT_ENVELOPE_HOP_MAC_ROOT must name a throwaway root'
	[ -d "$mac_root" ] || unproven "Mac root is missing: $mac_root"
	[ ! -L "$mac_root" ] || unproven "Mac root must not be a symlink: $mac_root"
	mac_root=$(CDPATH='' cd -- "$mac_root" && pwd) || unproven "cannot resolve Mac root: $mac_root"
	host_file=$mac_root/bridge/host-id
	trusted_file=$mac_root/bridge/trusted/$source_host
	[ -f "$host_file" ] || unproven "Mac host-id is missing: $host_file"
	[ ! -L "$host_file" ] || unproven "Mac host-id must not be a symlink: $host_file"
	[ -f "$trusted_file" ] || unproven "trusted source record is missing: $trusted_file"
	[ ! -L "$trusted_file" ] || unproven "trusted source record must not be a symlink: $trusted_file"
	actual_host=$(cat "$host_file") || unproven "cannot read Mac host-id: $host_file"
	actual_host=$(printf '%s' "$actual_host" | tr -d '\r\n')
	[ "$actual_host" = "$expected_host" ] ||
		unproven "Mac host-id is $actual_host, want $expected_host"
}

digest_file() {
	path=$1
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$path" | awk '{print $1}'
		return
	fi
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$path" | awk '{print $1}'
		return
	fi
	return 1
}

check_receipt() {
	receipt=$1
	transfer_id=$2
	payload_path=$3
	printf '%s\n' "$receipt" | grep -Fq '"stage":"destination_maildir_committed"' ||
		unproven 'apply-file did not emit destination_maildir_committed'
	printf '%s\n' "$receipt" | grep -Fq "\"transfer_id\":\"$transfer_id\"" ||
		unproven 'destination receipt has the wrong transfer id'
	printf '%s\n' "$receipt" | grep -Fq "\"committed_path\":\"$payload_path\"" ||
		unproven 'destination receipt has the wrong committed path'
	[ -f "$payload_path" ] || unproven "payload is missing from inbox/new: $payload_path"
	[ ! -L "$payload_path" ] || unproven "payload in inbox/new is a symlink: $payload_path"
	[ -s "$payload_path" ] || unproven "payload in inbox/new is empty: $payload_path"
	payload_digest=$(digest_file "$payload_path") ||
		unproven 'no SHA-256 utility is available to verify the committed payload'
	printf '%s\n' "$receipt" | grep -Fq "\"payload_sha256\":\"$payload_digest\"" ||
		unproven 'destination receipt digest does not match the inbox payload'
}

check_probe() {
	transfer_id=$1
	valid_transfer_id "$transfer_id" ||
		unproven 'transfer id must use lowercase [a-z0-9_-] without path components'
	check_host_files "${dest_alias%%/*}"
	source=$mac_dir/envelope-$transfer_id.json
	[ -f "$source" ] || unproven "moved envelope is missing: $source"
	[ ! -L "$source" ] || unproven "moved envelope must not be a symlink: $source"
	mode=$(file_mode "$source") || unproven "cannot inspect mode on $source"
	case "$mode" in
		600|0600|644|0644) ;;
		*) unproven "Mac envelope $source has mode $mode, want 0600 or 0644" ;;
	esac
	[ -s "$source" ] || unproven "Mac envelope is empty: $source"

	drop_dir=$mac_root/bridge/drop
	apply_file=$drop_dir/envelope-$transfer_id.json
	if [ "$source" != "$apply_file" ]; then
		if [ -e "$apply_file" ] || [ -L "$apply_file" ]; then
			unproven "apply-file drop already exists: $apply_file"
		fi
		if ! mkdir -p "$drop_dir" 2>/dev/null; then
			unproven "cannot create apply-file drop: $drop_dir"
		fi
		if ! (umask 077 && cp "$source" "$apply_file"); then
			unproven "cannot copy moved envelope into apply-file drop"
		fi
		if ! chmod 600 "$apply_file"; then
			unproven "cannot set apply-file drop mode on $apply_file"
		fi
	fi

	dest_host=${dest_alias%%/*}
	dest_agent=${dest_alias#*/}
	payload_path=$mac_root/agents/$dest_agent/inbox/new/xfer-$source_host-$transfer_id.md
	err_file=$(mktemp "${TMPDIR:-/tmp}/amq-envelope-hop-error.XXXXXX") ||
		unproven 'cannot create apply-file error capture'
	trap 'rm -f "$err_file"' 0 HUP INT TERM
	if ! receipt=$(run_bridge apply-file --root "$mac_root" --file "$apply_file" 2>"$err_file"); then
		cat "$err_file" >&2
		unproven "apply-file refused envelope $transfer_id"
	fi
	check_receipt "$receipt" "$transfer_id" "$payload_path"
	printf 'BOT_ENVELOPE_HOP_PROVEN transfer_id=%s receipt=destination_maildir_committed payload=%s\n' \
		"$transfer_id" "$payload_path"
}

[ "$#" -ge 1 ] || { usage; exit 2; }
validate_config
case "$1" in
	write)
		[ "$#" -eq 1 ] || { usage; exit 2; }
		write_probe
		;;
	check)
		[ "$#" -eq 2 ] || { usage; exit 2; }
		check_probe "$2"
		;;
	*)
		usage
		exit 2
		;;
esac
