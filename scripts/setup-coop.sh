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

# Step 2: Create Claude SessionStart hook (available for opt-in use)
echo -e "${GREEN}[2/4]${NC} Creating scripts/claude-session-start.sh..."
mkdir -p scripts

cat > scripts/claude-session-start.sh << 'HOOK'
#!/bin/bash
# AMQ Claude SessionStart hook
# Sets AM_ROOT/AM_ME for Claude Code by appending to CLAUDE_ENV_FILE.

set -euo pipefail

if [ -z "${CLAUDE_ENV_FILE:-}" ]; then
    exit 0
fi

DEFAULT_ROOT=".agent-mail"
if [ -n "${CLAUDE_PROJECT_DIR:-}" ]; then
    DEFAULT_ROOT="${CLAUDE_PROJECT_DIR}/.agent-mail"
fi

ROOT="${AM_ROOT:-$DEFAULT_ROOT}"
ME="${AM_ME:-claude}"

touch "$CLAUDE_ENV_FILE"

if ! grep -q '^export AM_ROOT=' "$CLAUDE_ENV_FILE"; then
    printf 'export AM_ROOT=%q\n' "$ROOT" >> "$CLAUDE_ENV_FILE"
fi
if ! grep -q '^export AM_ME=' "$CLAUDE_ENV_FILE"; then
    printf 'export AM_ME=%q\n' "$ME" >> "$CLAUDE_ENV_FILE"
fi
HOOK

chmod +x scripts/claude-session-start.sh

# Step 3: Create stop hook script (available for opt-in use)
echo -e "${GREEN}[3/4]${NC} Creating scripts/amq-stop-hook.sh..."

cat > scripts/amq-stop-hook.sh << 'HOOK'
#!/bin/bash
# AMQ Co-op Stop Hook
# Blocks stop if there are pending messages in inbox
# Safe fallback: approves if amq unavailable or co-op not configured

DEFAULT_ROOT=".agent-mail"
if [ -n "${CLAUDE_PROJECT_DIR:-}" ]; then
    DEFAULT_ROOT="${CLAUDE_PROJECT_DIR}/.agent-mail"
fi
ROOT="${AM_ROOT:-$DEFAULT_ROOT}"
ME="${AM_ME:-claude}"

# Fast path: approve immediately if co-op not set up
if [ ! -d "$ROOT/agents/$ME/inbox/new" ]; then
    echo '{"decision": "approve"}'
    exit 0
fi

# Fast path: if inbox/new has no message files, approve without invoking amq
inbox_new="$ROOT/agents/$ME/inbox/new"
shopt -s nullglob
files=("$inbox_new"/*.md)
shopt -u nullglob
if [ ${#files[@]} -eq 0 ]; then
    echo '{"decision": "approve"}'
    exit 0
fi

# Safe fallback if dependencies missing
if ! command -v amq &> /dev/null; then
    echo '{"decision": "approve"}'
    exit 0
fi

if command -v jq &> /dev/null; then
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
else
    OUT=$(amq list --root "$ROOT" --me "$ME" --new 2>/dev/null || true)
    if echo "$OUT" | grep -q "^No messages\\.$"; then
        echo '{"decision": "approve"}'
        exit 0
    fi
    if [ -n "$OUT" ]; then
        echo '{"decision": "block", "reason": "You have pending message(s). Ask me to drain the inbox before stopping."}'
        exit 0
    fi
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
echo "  scripts/claude-session-start.sh  - SessionStart hook (opt-in, see below)"
echo "  scripts/amq-stop-hook.sh  - Stop hook (opt-in, see below)"
echo "  .agent-mail/              - Agent mailboxes (gitignored)"
echo ""
echo -e "${BLUE}Next steps:${NC}"
echo ""
echo "1. Claude Code session:"
echo "   export AM_ME=claude AM_ROOT=.agent-mail"
echo "   # Start watcher (Task tool, haiku model): 'Run amq monitor --peek --timeout 0 --include-body --json'"
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
echo "   export AM_ME=codex AM_ROOT=.agent-mail"
echo "   amq wake &  # Primary: TTY injection"
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
