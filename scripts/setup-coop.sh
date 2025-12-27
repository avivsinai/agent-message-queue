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

AM_ROOT="${AM_ROOT:-.agent-mail}"
AGENTS="${AGENTS:-claude,codex}"

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
    if [ -f "$AM_ROOT/meta/config.json" ]; then
        EXISTING=$(jq -r '.agents // [] | join(",")' "$AM_ROOT/meta/config.json" 2>/dev/null || echo "")
        if [ -n "$EXISTING" ]; then
            echo -e "  ${YELLOW}Existing agents found: ${EXISTING}${NC}"
            # Merge: add new agents to existing list
            MERGED=$(echo "$EXISTING,$AGENTS" | tr ',' '\n' | sort -u | tr '\n' ',' | sed 's/,$//')
            AGENTS="$MERGED"
            echo "  Merged agent list: ${AGENTS}"
        fi
    fi
    amq init --root "$AM_ROOT" --agents "$AGENTS" --force 2>/dev/null || true
else
    echo "  Skipping (amq not installed)"
fi

# Step 2: Create/merge .claude/settings.json
echo -e "${GREEN}[2/4]${NC} Configuring .claude/settings.json..."
mkdir -p .claude

SETTINGS_FILE=".claude/settings.json"
HOOK_CMD="./scripts/amq-stop-hook.sh"

if [ -f "$SETTINGS_FILE" ]; then
    echo -e "  ${YELLOW}Existing settings found - backing up to ${SETTINGS_FILE}.bak${NC}"
    cp "$SETTINGS_FILE" "${SETTINGS_FILE}.bak"

    # Check if jq available for merging
    if command -v jq &> /dev/null; then
        # Check if Stop hook already exists
        EXISTING_STOP=$(jq -r '.hooks.Stop // empty' "$SETTINGS_FILE" 2>/dev/null)
        if [ -n "$EXISTING_STOP" ]; then
            # Check if our hook is already there
            if jq -e '.hooks.Stop[].hooks[] | select(.command == "'"$HOOK_CMD"'")' "$SETTINGS_FILE" &>/dev/null; then
                echo "  AMQ stop hook already configured"
            else
                echo "  Adding AMQ stop hook to existing Stop hooks"
                jq '.hooks.Stop[0].hooks += [{"type": "command", "command": "'"$HOOK_CMD"'"}]' "$SETTINGS_FILE" > "${SETTINGS_FILE}.tmp"
                mv "${SETTINGS_FILE}.tmp" "$SETTINGS_FILE"
            fi
        else
            # No Stop hooks yet, add our hook
            echo "  Adding Stop hook section"
            jq '.hooks.Stop = [{"hooks": [{"type": "command", "command": "'"$HOOK_CMD"'"}]}]' "$SETTINGS_FILE" > "${SETTINGS_FILE}.tmp"
            mv "${SETTINGS_FILE}.tmp" "$SETTINGS_FILE"
        fi
    else
        echo -e "  ${YELLOW}jq not available - please manually add the stop hook${NC}"
        echo "  Add to .claude/settings.json:"
        echo '  {"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "./scripts/amq-stop-hook.sh"}]}]}}'
    fi
else
    # Create new settings file
    cat > "$SETTINGS_FILE" << 'EOF'
{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "./scripts/amq-stop-hook.sh"
          }
        ]
      }
    ]
  }
}
EOF
    echo "  Created new settings file"
fi

# Step 3: Create stop hook script
echo -e "${GREEN}[3/4]${NC} Creating scripts/amq-stop-hook.sh..."
mkdir -p scripts

cat > scripts/amq-stop-hook.sh << 'HOOK'
#!/bin/bash
# AMQ Co-op Stop Hook
# Blocks stop if there are pending messages in inbox
# Safe fallback: approves if amq/jq unavailable or co-op not configured

ROOT="${AM_ROOT:-.agent-mail}"
ME="${AM_ME:-claude}"

# Fast path: approve immediately if co-op not set up
if [ ! -d "$ROOT/agents/$ME/inbox/new" ]; then
    echo '{"decision": "approve"}'
    exit 0
fi

# Safe fallback if dependencies missing
if ! command -v amq &> /dev/null || ! command -v jq &> /dev/null; then
    echo '{"decision": "approve"}'
    exit 0
fi

# Check for pending messages (safe fallback on any error)
COUNT=$(amq list --root "$ROOT" --me "$ME" --new --json 2>/dev/null | jq -r 'length // 0' 2>/dev/null || echo "0")

# Sanitize COUNT to ensure it's a number
if ! [[ "$COUNT" =~ ^[0-9]+$ ]]; then
    COUNT=0
fi

if [ "$COUNT" -gt 0 ]; then
    echo '{"decision": "block", "reason": "You have '"$COUNT"' pending message(s). Ask me to drain the inbox before stopping."}'
    exit 0
fi

echo '{"decision": "approve"}'
HOOK

chmod +x scripts/amq-stop-hook.sh

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
echo "  .claude/settings.json     - Stop hook configuration"
echo "  scripts/amq-stop-hook.sh  - Prevents stopping with pending messages"
echo "  .agent-mail/              - Agent mailboxes (gitignored)"
echo ""
echo -e "${BLUE}Next steps:${NC}"
echo ""
echo "1. Claude Code session:"
echo "   export AM_ME=claude AM_ROOT=.agent-mail"
echo "   # Start watcher: 'Run amq-coop-watcher in background'"
echo ""
echo "2. Codex CLI session:"
echo "   export AM_ME=codex AM_ROOT=.agent-mail"
echo "   # Enable background terminals in ~/.codex/config.toml:"
echo "   #   [features]"
echo "   #   unified_exec = true"
echo "   # Then run: while true; do amq monitor --timeout 0 --json; sleep 0.2; done"
echo ""
echo "See: https://github.com/avivsinai/agent-message-queue/blob/main/COOP.md"
