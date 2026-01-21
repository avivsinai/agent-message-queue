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

# Respect .amqrc if it exists (don't override user's configured root)
if [ -z "${AM_ROOT:-}" ] && [ -f .amqrc ]; then
    AM_ROOT=$(python3 -c "import json,sys; print(json.load(sys.stdin).get('root',''))" < .amqrc 2>/dev/null || echo "")
fi
AM_ROOT="${AM_ROOT:-.agent-mail}"
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

# Step 1: Initialize amq mailboxes (honors existing config)
echo -e "${GREEN}[1/4]${NC} Initializing AMQ mailboxes at ${AM_ROOT} for agents: ${AGENTS}..."
if command -v amq &> /dev/null; then
    # Check if config exists and has agents
    CONFIG_EXISTS=false
    USE_FORCE=true
    if [ -f "$AM_ROOT/meta/config.json" ]; then
        CONFIG_EXISTS=true
        if command -v jq &> /dev/null; then
            EXISTING=$(jq -r '.agents // [] | join(",")' "$AM_ROOT/meta/config.json" 2>/dev/null || echo "")
            if [ -n "$EXISTING" ]; then
                echo -e "  ${YELLOW}Existing agents found: ${EXISTING}${NC}"
                # Merge: add new agents to existing list
                MERGED=$(echo "$EXISTING,$AGENTS" | tr ',' '\n' | sort -u | tr '\n' ',' | sed 's/,$//')
                AGENTS="$MERGED"
                echo "  Merged agent list: ${AGENTS}"
            fi
        else
            # jq not available but config exists - don't use --force to avoid data loss
            echo -e "  ${YELLOW}Config exists but jq not available for merge. Using existing config safely.${NC}"
            USE_FORCE=false
        fi
    fi

    if [ "$USE_FORCE" = true ]; then
        amq init --root "$AM_ROOT" --agents "$AGENTS" --force 2>/dev/null || true
    else
        # Try without force - will only update if compatible or fail safely
        amq init --root "$AM_ROOT" --agents "$AGENTS" 2>/dev/null || {
            echo -e "  ${YELLOW}Init skipped (config exists). Install jq to enable agent merging.${NC}"
        }
    fi
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
echo -e "${GREEN}[2/4]${NC} Creating scripts/claude-session-start.sh..."
mkdir -p scripts
if ! fetch_script "claude-session-start.sh" "$SCRIPTS_BASE_URL/claude-session-start.sh"; then
    echo -e "${YELLOW}Warning: failed to fetch claude-session-start.sh${NC}"
fi

# Step 3: Create stop hook script (available for opt-in use)
echo -e "${GREEN}[3/4]${NC} Creating scripts/amq-stop-hook.sh..."
if ! fetch_script "amq-stop-hook.sh" "$SCRIPTS_BASE_URL/amq-stop-hook.sh"; then
    echo -e "${YELLOW}Warning: failed to fetch amq-stop-hook.sh${NC}"
fi

# Step 4: Update .gitignore
echo -e "${GREEN}[4/4]${NC} Updating .gitignore..."
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
echo "  .agent-mail/              - Agent mailboxes (gitignored)"
echo ""
echo -e "${BLUE}Next steps:${NC}"
echo ""
echo "1. Claude Code session:"
echo "   eval \"\$(amq env --me claude)\""
echo "   amq wake &  # Optional: terminal notifications"
echo "   # After handling messages, drain: amq drain --include-body"
echo "   # Optional (recommended): add SessionStart + Stop hooks in .claude/settings.local.json"
echo "   #   cat > .claude/settings.local.json <<'JSON'"
echo "   #   {"
echo "   #     \"hooks\": {"
echo "   #       \"SessionStart\": ["
echo "   #         {\"hooks\": [{\"type\": \"command\", \"command\": \"\$CLAUDE_PROJECT_DIR/scripts/claude-session-start.sh\"}]}"
echo "   #       ],"
echo "   #       \"Stop\": ["
echo "   #         {\"hooks\": [{\"type\": \"command\", \"command\": \"\$CLAUDE_PROJECT_DIR/scripts/amq-stop-hook.sh\"}]}"
echo "   #       ]"
echo "   #     }"
echo "   #   }"
echo "   #   JSON"
echo ""
echo "2. Codex CLI session:"
echo "   eval \"\$(amq env --me codex)\""
echo "   amq wake &  # Optional: terminal notifications"
echo "   codex"
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
