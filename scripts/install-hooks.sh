#!/bin/sh
# Install git hooks for this repository

HOOK_DIR="$(git rev-parse --git-dir)/hooks"

cat > "$HOOK_DIR/pre-push" << 'EOF'
#!/bin/sh
# Pre-push hook: runs lint and tests before allowing push

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
