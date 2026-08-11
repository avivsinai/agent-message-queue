#!/bin/sh
# Integration test for the generated pre-push hook's git-env scrub.
# Installs the hook into a temp repo, invokes it under hostile git
# environment variables, and proves the hook's `make ci` sees none of them.
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/amq-hook-env.XXXXXX")"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

REPO="$WORK/repo"
mkdir -p "$REPO"
git -C "$REPO" init -q

# Fake `make` earlier in PATH: records which scrubbed variables leaked
# through, so the assertion proves the postcondition instead of trusting
# the unset line.
BIN="$WORK/bin"
mkdir -p "$BIN"
cat > "$BIN/make" << 'EOF'
#!/bin/sh
leaked=""
for name in GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
    GIT_COMMON_DIR GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_NAMESPACE \
    GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS GIT_CONFIG_NOSYSTEM; do
  if eval "[ \"\${$name+set}\" = set ]"; then
    leaked="$leaked $name"
  fi
done
printf '%s\n' "$leaked" > "$MAKE_ENV_REPORT"
[ -z "$leaked" ]
EOF
chmod +x "$BIN/make"

(cd "$REPO" && sh "$ROOT/scripts/install-hooks.sh" > /dev/null)
HOOK="$REPO/.git/hooks/pre-push"
[ -x "$HOOK" ] || { echo "FAIL: hook not installed"; exit 1; }

REPORT="$WORK/report"
if ! env \
    GIT_DIR="$REPO/.git" \
    GIT_WORK_TREE="$REPO" \
    GIT_INDEX_FILE="$REPO/.git/index" \
    GIT_OBJECT_DIRECTORY="$REPO/.git/objects" \
    GIT_COMMON_DIR="$REPO/.git" \
    GIT_ALTERNATE_OBJECT_DIRECTORIES="$WORK/alt" \
    GIT_NAMESPACE=hostile \
    GIT_CONFIG_COUNT=1 \
    GIT_CONFIG_PARAMETERS="'core.bare=true'" \
    GIT_CONFIG_NOSYSTEM= \
    MAKE_ENV_REPORT="$REPORT" \
    PATH="$BIN:$PATH" \
    sh "$HOOK" origin file:///dev/null < /dev/null > "$WORK/hook.out" 2>&1; then
  echo "FAIL: hook exited nonzero under hostile git env"
  cat "$WORK/hook.out"
  cat "$REPORT" 2>/dev/null || true
  exit 1
fi

[ -f "$REPORT" ] || { echo "FAIL: hook never ran make ci"; exit 1; }
leaked="$(cat "$REPORT")"
if [ -n "$(printf '%s' "$leaked" | tr -d ' ')" ]; then
  echo "FAIL: hook leaked git env into make ci:$leaked"
  exit 1
fi

echo "pre-push hook git env scrub ok"
