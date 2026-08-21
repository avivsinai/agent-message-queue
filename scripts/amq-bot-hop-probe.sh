#!/bin/sh
# Probe whether Grok Bot can move one nonce file from host G to this Mac.
# This is evidence only. It is not an AMQ transport, mailbox, or secret channel.
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
mac_dir=${AMQ_BOT_HOP_MAC_DIR:-$repo_root/dist/amq-bot-hop-probe}
g_dir=${AMQ_BOT_HOP_G_DIR:-/workspace/amq/bridge/hop-probe}

usage() {
	cat >&2 <<'EOF'
usage:
  amq-bot-hop-probe.sh write
  amq-bot-hop-probe.sh check <nonce>

write runs on host G. Bot must move the reported file to the Mac.
check runs on the Mac and reads only the moved file; it never reads AMQ mailboxes.
EOF
}

unproven() {
	printf 'BOT_HOP_UNPROVEN: %s\n' "$*" >&2
	exit 1
}

valid_nonce() {
	nonce=$1
	[ "${#nonce}" -eq 32 ] || return 1
	case "$nonce" in
		*[!0123456789abcdef]*) return 1 ;;
		*) return 0 ;;
	esac
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

write_probe() {
	if ! mkdir -p "$g_dir" 2>/dev/null; then
		unproven "cannot create required G directory $g_dir"
	fi
	if ! chmod 700 "$g_dir" 2>/dev/null; then
		unproven "cannot set G directory mode on $g_dir"
	fi

	nonce=$(LC_ALL=C od -An -N16 -tx1 /dev/urandom | tr -d '[:space:]') ||
		unproven 'cannot generate nonce'
	valid_nonce "$nonce" || unproven 'generated invalid nonce'
	target=$g_dir/hop-$nonce.nonce
	tmp=$g_dir/.hop-$nonce.tmp.$$
	trap 'rm -f "$tmp"' 0 HUP INT TERM
	if ! (umask 077 && printf 'amq-bot-hop-probe-v1\nnonce=%s\n' "$nonce" >"$tmp"); then
		unproven "cannot write G probe file $target"
	fi
	if ! chmod 600 "$tmp" || ! mv -f "$tmp" "$target"; then
		unproven "cannot commit G probe file $target"
	fi
	mode=$(file_mode "$target") || unproven "cannot inspect mode on $target"
	case "$mode" in
		600|0600) ;;
		*) rm -f "$target"; unproven "G probe file $target has mode $mode, want 0600" ;;
	esac
	trap - 0 HUP INT TERM
	printf 'BOT_HOP_NONCE=%s\n' "$nonce"
	printf 'BOT_HOP_SOURCE=%s\n' "$target"
	printf 'BOT_HOP_NEXT=move this file with Grok Bot; do not paste its contents\n'
}

check_probe() {
	nonce=$1
	valid_nonce "$nonce" || unproven 'nonce must be exactly 32 lowercase hexadecimal characters'
	target=$mac_dir/hop-$nonce.nonce
	if [ -L "$target" ] || [ ! -f "$target" ]; then
		unproven "nonce $nonce is missing at $target"
	fi
	mode=$(file_mode "$target") || unproven "cannot inspect mode on $target"
	case "$mode" in
		600|0600|644|0644) ;;
		*) unproven "Mac copy $target has mode $mode, want 0600 or 0644" ;;
	esac

	expected=$(printf 'amq-bot-hop-probe-v1\nnonce=%s' "$nonce")
	actual=$(cat "$target") || unproven "cannot read Mac copy $target"
	expected_bytes=$(printf '%s\n' "$expected" | wc -c | tr -d '[:space:]')
	actual_bytes=$(wc -c <"$target" | tr -d '[:space:]')
	if [ "$actual_bytes" != "$expected_bytes" ] || [ "$actual" != "$expected" ]; then
		unproven "nonce file $target has unexpected contents"
	fi
	printf 'BOT_HOP_PROVEN nonce=%s path=%s mode=%s\n' "$nonce" "$target" "$mode"
}

if [ "$#" -lt 1 ]; then
	usage
	exit 2
fi
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
