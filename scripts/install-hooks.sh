#!/bin/sh
# Install git hooks for this repository

HOOK_DIR="$(git rev-parse --git-dir)/hooks"

cat > "$HOOK_DIR/pre-push" << 'EOF'
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

chmod +x "$HOOK_DIR/pre-push"
echo "✓ Installed pre-push hook"
