#!/bin/bash
# AMQ Co-op Mode Setup Script
# Run this in your project to enable Claude Code <-> Codex collaboration
#
# Usage:
#   curl -sL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/setup-coop.sh | bash
#
# Environment variables:
#   AM_ROOT  - Root directory for mailboxes (default: .agent-mail)
#   AGENTS   - Comma-separated agent list (default: claude,codex)

set -e

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "${BLUE}=== AMQ Co-op Mode Setup ===${NC}"
echo ""

AGENTS="${AGENTS:-claude,codex}"
REPO="avivsinai/agent-message-queue"
SCRIPTS_BASE_URL="https://raw.githubusercontent.com/${REPO}/main/scripts"

# Check dependencies
if ! command -v amq &> /dev/null; then
    echo -e "${YELLOW}Warning: 'amq' CLI not found in PATH${NC}"
    echo "Install from: https://github.com/avivsinai/agent-message-queue/releases"
    echo ""
fi

if ! command -v jq &> /dev/null; then
    echo -e "${YELLOW}Warning: 'jq' not found - stop hook will use safe fallback${NC}"
    echo ""
fi

if ! command -v python3 &> /dev/null; then
    echo -e "${YELLOW}Warning: 'python3' not found - UserPromptSubmit AMQ hook will be unavailable${NC}"
    echo ""
fi

# Step 1: Initialize amq mailboxes using coop init (creates base+session layout)
echo -e "${GREEN}[1/5]${NC} Initializing AMQ co-op mode for agents: ${AGENTS}..."
if command -v amq &> /dev/null; then
    amq coop init --agents "$AGENTS" --force 2>/dev/null || true
else
    echo "  Skipping (amq not installed)"
fi

fetch_script() {
    local name="$1"
    local url="$2"
    local dest="scripts/$name"
    if [ -f "$dest" ]; then
        chmod +x "$dest"
        return 0
    fi
    if curl -fsSL "$url" -o "$dest"; then
        chmod +x "$dest"
        return 0
    fi
    return 1
}

# Step 2: Create Claude SessionStart hook (available for opt-in use)
echo -e "${GREEN}[2/5]${NC} Creating scripts/claude-session-start.sh..."
mkdir -p scripts
if ! fetch_script "claude-session-start.sh" "$SCRIPTS_BASE_URL/claude-session-start.sh"; then
    echo -e "${YELLOW}Warning: failed to fetch claude-session-start.sh${NC}"
fi

# Step 3: Create stop hook script (available for opt-in use)
echo -e "${GREEN}[3/5]${NC} Creating scripts/amq-stop-hook.sh..."
if ! fetch_script "amq-stop-hook.sh" "$SCRIPTS_BASE_URL/amq-stop-hook.sh"; then
    echo -e "${YELLOW}Warning: failed to fetch amq-stop-hook.sh${NC}"
fi

# Step 4: Create UserPromptSubmit hook script (plan-mode friendly)
echo -e "${GREEN}[4/5]${NC} Creating scripts/claude-amq-user-prompt-submit.py..."
if ! fetch_script "claude-amq-user-prompt-submit.py" "$SCRIPTS_BASE_URL/claude-amq-user-prompt-submit.py"; then
    echo -e "${YELLOW}Warning: failed to fetch claude-amq-user-prompt-submit.py${NC}"
fi

# Step 5: Update .gitignore
echo -e "${GREEN}[5/5]${NC} Updating .gitignore..."
if [ -f .gitignore ]; then
    if ! grep -q ".agent-mail" .gitignore 2>/dev/null; then
        echo "" >> .gitignore
        echo "# AMQ co-op mode" >> .gitignore
        echo ".agent-mail/" >> .gitignore
    fi
else
    echo "# AMQ co-op mode" > .gitignore
    echo ".agent-mail/" >> .gitignore
fi

echo ""
echo -e "${GREEN}=== Setup Complete ===${NC}"
echo ""
echo "Created/updated:"
echo "  scripts/claude-session-start.sh  - SessionStart hook (opt-in, see below)"
echo "  scripts/amq-stop-hook.sh  - Stop hook (opt-in, see below)"
echo "  scripts/claude-amq-user-prompt-submit.py  - UserPromptSubmit hook for plan mode"
echo "  .agent-mail/              - Agent mailboxes (gitignored)"
echo ""
echo -e "${BLUE}Next steps:${NC}"
echo ""
echo "1. Claude Code session:"
echo "   amq coop exec claude"
echo "   # Or with flags: amq coop exec claude -- --dangerously-skip-permissions"
echo "   # Optional (recommended): add SessionStart + UserPromptSubmit + Stop hooks in .claude/settings.local.json"
echo "   #   cat > .claude/settings.local.json <<'JSON'"
echo "   #   {"
echo "   #     \"hooks\": {"
echo "   #       \"SessionStart\": ["
echo "   #         {\"hooks\": [{\"type\": \"command\", \"command\": \"\$CLAUDE_PROJECT_DIR/scripts/claude-session-start.sh\"}]}"
echo "   #       ],"
echo "   #       \"UserPromptSubmit\": ["
echo "   #         {\"hooks\": [{\"type\": \"command\", \"command\": \"AMQ_PROMPT_HOOK_MODE=plan AMQ_PROMPT_HOOK_ACTION=list python3 \$CLAUDE_PROJECT_DIR/scripts/claude-amq-user-prompt-submit.py\"}]}"
echo "   #       ],"
echo "   #       \"Stop\": ["
echo "   #         {\"hooks\": [{\"type\": \"command\", \"command\": \"\$CLAUDE_PROJECT_DIR/scripts/amq-stop-hook.sh\"}]}"
echo "   #       ]"
echo "   #     }"
echo "   #   }"
echo "   #   JSON"
echo ""
echo "2. Codex CLI session:"
echo "   amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox"
echo "   # Fallback (if TIOCSTI unavailable): add to ~/.codex/config.toml:"
echo "   #   notify = [\"python3\", \"/path/to/repo/scripts/codex-amq-notify.py\"]"
echo ""
echo -e "${YELLOW}Optional: Enable stop hook${NC}"
echo "To prevent stopping with pending messages:"
echo "  mkdir -p .claude"
echo "  # Add to .claude/settings.local.json:"
echo '  {"hooks":{"Stop":[{"hooks":[{"type":"command","command":"$CLAUDE_PROJECT_DIR/scripts/amq-stop-hook.sh"}]}]}}'
echo ""
echo "See: https://github.com/avivsinai/agent-message-queue/blob/main/COOP.md"
