#!/bin/sh
# Synthetic hop-probe test. Does not claim a live G↔Mac Bot transfer.
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
probe=$script_dir/amq-bot-hop-probe.sh
work=$(mktemp -d "${TMPDIR:-/tmp}/amq-bot-hop-probe-test.XXXXXX")
cleanup() { rm -rf "$work"; }
trap cleanup EXIT INT TERM

g_dir=$work/g
mac_dir=$work/mac
mkdir -p "$mac_dir"

fail() {
	printf 'FAIL: %s\n' "$1" >&2
	exit 1
}

out=$(AMQ_BOT_HOP_G_DIR=$g_dir sh "$probe" write) || fail "write exited nonzero"
nonce=$(printf '%s\n' "$out" | sed -n 's/^BOT_HOP_NONCE=//p')
source=$(printf '%s\n' "$out" | sed -n 's/^BOT_HOP_SOURCE=//p')
[ "${#nonce}" -eq 32 ] || fail "write did not print a 32-char nonce"
[ -f "$source" ] || fail "write did not create $source"

if AMQ_BOT_HOP_MAC_DIR=$mac_dir sh "$probe" check "$nonce" >/dev/null 2>&1; then
	fail "check succeeded before the file was copied"
fi

cp "$source" "$mac_dir/hop-$nonce.nonce"
chmod 600 "$mac_dir/hop-$nonce.nonce"
AMQ_BOT_HOP_MAC_DIR=$mac_dir sh "$probe" check "$nonce" >/dev/null || fail "check failed after a faithful copy"

chmod 644 "$mac_dir/hop-$nonce.nonce"
mode_output=$(AMQ_BOT_HOP_MAC_DIR=$mac_dir sh "$probe" check "$nonce") || fail "check failed for a mode-644 faithful copy"
case "$mode_output" in
	*'mode=644') ;;
	*) fail "mode-644 check did not report mode=644" ;;
esac

chmod 640 "$mac_dir/hop-$nonce.nonce"
if AMQ_BOT_HOP_MAC_DIR=$mac_dir sh "$probe" check "$nonce" >/dev/null 2>&1; then
	fail "check succeeded for an unsupported mode"
fi

chmod 644 "$mac_dir/hop-$nonce.nonce"
printf 'wrong\n' >"$mac_dir/hop-$nonce.nonce"
if AMQ_BOT_HOP_MAC_DIR=$mac_dir sh "$probe" check "$nonce" >/dev/null 2>&1; then
	fail "check succeeded for wrong contents"
fi

cp "$source" "$mac_dir/real-$nonce.nonce"
chmod 644 "$mac_dir/real-$nonce.nonce"
rm -f "$mac_dir/hop-$nonce.nonce"
ln -s "real-$nonce.nonce" "$mac_dir/hop-$nonce.nonce"
if AMQ_BOT_HOP_MAC_DIR=$mac_dir sh "$probe" check "$nonce" >/dev/null 2>&1; then
	fail "check succeeded for a symlink"
fi

if AMQ_BOT_HOP_MAC_DIR=$mac_dir sh "$probe" check 00000000000000000000000000000000 >/dev/null 2>&1; then
	fail "check succeeded for a missing nonce"
fi

printf 'amq-bot-hop-probe synthetic tests passed\n'
