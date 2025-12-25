#!/bin/sh
# Install git hooks for this repository

HOOK_DIR="$(git rev-parse --git-dir)/hooks"

cat > "$HOOK_DIR/pre-commit" << 'EOF'
#!/bin/sh
# Pre-commit hook: runs lint and tests before allowing commit

echo "Running pre-commit checks..."

# Run the CI checks (fmt-check, vet, lint, test)
if ! make ci; then
    echo ""
    echo "❌ Pre-commit checks failed. Fix the issues above before committing."
    exit 1
fi

echo "✓ Pre-commit checks passed"
EOF

chmod +x "$HOOK_DIR/pre-commit"
echo "✓ Installed pre-commit hook"
