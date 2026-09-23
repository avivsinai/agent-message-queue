#!/bin/sh
# Install git hooks for this repository.
# An existing pre-push hook is left in place unless it is already this
# script's hook, or the caller passes --force.

HOOK_DIR="$(git rev-parse --git-dir)/hooks"
HOOK="$HOOK_DIR/pre-push"
force=0
if [ "${1:-}" = "--force" ]; then
  force=1
elif [ -n "${1:-}" ]; then
  echo "usage: scripts/install-hooks.sh [--force]" >&2
  exit 2
fi

hook_body() {
  cat << 'EOF'
#!/bin/sh
# Pre-push hook: runs lint and tests before allowing push

# git exports repo-selection variables into hooks; make ci runs tests that
# create git fixtures, and an inherited GIT_DIR would point their git
# commands at this repository itself. Config-injection variables are
# scrubbed too so hook-launched git sees only on-disk configuration.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_COMMON_DIR \
  GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_NAMESPACE \
  GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS GIT_CONFIG_NOSYSTEM 2>/dev/null || true

echo "Running pre-push checks..."

# Run the CI checks (fmt-check, vet, lint, test)
if ! make ci; then
    echo ""
    echo "❌ Pre-push checks failed. Fix the issues above before pushing."
    exit 1
fi

echo "✓ Pre-push checks passed"
EOF
}

if [ -e "$HOOK" ] || [ -L "$HOOK" ]; then
  if [ "$force" -eq 0 ]; then
    if [ -L "$HOOK" ] || ! hook_body | cmp -s - "$HOOK"; then
      echo "refusing to replace existing pre-push hook: $HOOK" >&2
      exit 1
    fi
  elif [ -L "$HOOK" ]; then
    rm "$HOOK"
  fi
fi

hook_body > "$HOOK"
chmod +x "$HOOK"
echo "✓ Installed pre-push hook"
