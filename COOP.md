# Co-op Mode: Inter-Agent Collaboration Protocol

Co-op mode enables Claude Code and Codex CLI to collaborate as **review partners**, sharing context, requesting reviews, and coordinating work through the Agent Message Queue.

## Autonomous Operation

**Critical**: In co-op mode, agents operate autonomously until task completion. Do not ask the user for direction—that's what your partner agent is for.

### Core Principles

1. **Work autonomously** - Complete your assigned task without user prompts. If blocked, message your partner agent, not the user.

2. **Use extended thinking** - For complex decisions, use thinking tiers:
   - `think` → routine tasks
   - `think hard` → multi-step problems
   - `think harder` → architectural decisions
   - `ultrathink` → critical design choices, security-sensitive code

3. **Multi-agent verification** - Instead of asking "is this right?", send a review request to your partner. This is the co-op advantage: parallel verification without blocking on human input.

4. **Completion signals** - When your task is done, signal completion clearly. Don't wait for user confirmation.

5. **Iteration over perfection** - Ship working code, request review, iterate based on feedback. Initial quality matters less than refinement cycles.

### When to Message Partner vs User

| Situation | Action |
|-----------|--------|
| Need code review | → Message partner |
| Blocked on design decision | → Message partner (kind: `decision`) |
| Found bug in partner's code | → Message partner (kind: `review_response`) |
| Task complete, ready for next | → Message partner (kind: `status`) |
| Need external credentials/access | → Ask user |
| Unclear on original requirements | → Ask user |

### Context Management

Long autonomous sessions accumulate context. Use `/clear` between major task boundaries to maintain decision quality. Your partner agent provides continuity—you don't need to hold everything in your own context.

## Quick Start

### Prerequisites (One-Time)

1. **Install amq CLI** ([releases](https://github.com/avivsinai/agent-message-queue/releases)):
   ```bash
   # macOS arm64
   curl -L https://github.com/avivsinai/agent-message-queue/releases/latest/download/amq_darwin_arm64.tar.gz | tar xz
   sudo mv amq /usr/local/bin/
   ```

2. **Install amq-cli skill** for your agents:

   **Claude Code** (via marketplace):
   ```
   /plugin marketplace add avivsinai/skills-marketplace
   /plugin install amq-cli@avivsinai-marketplace
   ```

   **Codex CLI** (via skill-installer):
   ```
   $skill-installer install https://github.com/avivsinai/agent-message-queue/tree/main/skills/amq-cli
   ```

### Per-Project Setup

Run the setup script in your project:

```bash
curl -sL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/setup-coop.sh | bash
```

This creates:
- `.agent-mail/` - Agent mailboxes (gitignored)
- `.claude/settings.json` - Stop hook (prevents stopping with pending messages)
- `scripts/amq-stop-hook.sh` - The stop hook script

### Running Co-op Mode

**Terminal 1 - Claude Code:**
```bash
export AM_ME=claude AM_ROOT=.agent-mail
claude

# In Claude Code, say:
# "Run amq-coop-watcher in background while I work on [task]"
```

**Terminal 2 - Codex CLI:**
```bash
export AM_ME=codex AM_ROOT=.agent-mail
codex

# First, enable Background terminal (one-time):
# Type: /experimental
# Toggle "Background terminal" to enabled

# Then tell Codex:
# "Run this in a background terminal: while true; do amq monitor --timeout 0 --json; sleep 0.2; done"

# Verify with: /ps
```

**For full autonomy:**
- Claude Code: The stop hook prevents stopping until inbox is empty
- Codex CLI: Use `/approvals` to set autonomous mode (command may vary by version; check `codex --help`)

### How It Works

1. Both agents run background watchers that block until messages arrive
2. When Agent A sends a message to Agent B, B's watcher wakes up
3. Agent B processes the message, responds, continues working
4. The stop hook ensures agents don't quit while messages are pending
5. Agents work autonomously—messaging each other, not the user

## Message Format

Co-op mode extends the standard AMQ message format with optional fields:

```json
{
  "schema": 1,
  "id": "2025-12-27T12-00-00.000Z_pid1234_abcd",
  "from": "codex",
  "to": ["claude"],
  "thread": "p2p/claude__codex",
  "subject": "Code review needed",
  "created": "2025-12-27T12:00:00Z",
  "ack_required": true,
  "refs": ["msg_prev_123"],

  "priority": "urgent",
  "kind": "review_request",
  "labels": ["parser", "edge-cases"],
  "context": {
    "paths": ["internal/cli/drain.go"],
    "focus": "sorting + ack behavior"
  }
}
```

### Priority Levels

| Priority | Behavior | Use When |
|----------|----------|----------|
| `urgent` | Interrupt current work | Blocking issues, critical bugs, time-sensitive decisions |
| `normal` | Add to TODO list | Code reviews, questions, standard requests |
| `low` | Batch/digest later | Status updates, FYIs, non-blocking info |

### Message Kinds

| Kind | Default Priority | Description |
|------|------------------|-------------|
| `review_request` | normal | Request code review |
| `review_response` | normal | Code review feedback |
| `question` | normal | Question needing answer |
| `answer` | normal | Response to a question |
| `decision` | normal (urgent if blocking) | Decision request |
| `brainstorm` | low | Open-ended discussion |
| `status` | low | Status update/FYI |
| `todo` | normal | Task assignment |

## CLI Commands

### Send with Co-op Fields

```bash
# Request a code review
amq send --me claude --to codex \
  --subject "Review: New parser" \
  --priority normal \
  --kind review_request \
  --labels "parser,refactor" \
  --context '{"paths": ["internal/format/message.go"], "focus": "error handling"}' \
  --body "Please review the new message parser..."

# Urgent blocking question
amq send --me codex --to claude \
  --subject "Blocked: API design question" \
  --priority urgent \
  --kind question \
  --body "I need to decide on the API shape before continuing..."
```

### Monitor (Combined Watch + Drain)

```bash
# Block until message arrives, drain, output JSON
amq monitor --me claude --timeout 0 --include-body --json

# With timeout (60s default)
amq monitor --me codex --timeout 30s --json

# For scripts/hooks
amq monitor --me claude --json
```

### Reply (Auto Thread/Refs)

```bash
# Reply to a message (auto-sets thread, refs, recipient)
amq reply --me codex --id "msg_123" \
  --kind review_response \
  --body "LGTM with minor suggestions..."

# Reply with urgency
amq reply --me claude --id "msg_456" \
  --priority urgent \
  --body "Found a critical issue..."
```

## Claude Code Integration

### Background Watcher Agent

The `amq-coop-watcher` agent (`.claude/agents/amq-coop-watcher.md`) runs in the background with a `while true` loop that auto-respawns after each message.

**In your session:**
```
"Run amq-coop-watcher in the background while I work on this feature"
```

**When watcher returns:**
1. Parse the message summary
2. Handle by priority:
   - `urgent` → Stop current work, address immediately
   - `normal` → Add to TodoWrite, continue current work
   - `low` → Note for later, continue

The watcher auto-respawns after each message. Only re-launch if the 10-minute background task timeout expires.

### CLAUDE.md Co-op Section

Add to your project's CLAUDE.md:

```markdown
## Co-op Mode (Claude <-> Codex)

On session start:
1. Set AM_ME=claude, AM_ROOT=.agent-mail
2. Spawn watcher: "Run amq-coop-watcher in background"

When watcher returns with messages:
- urgent → interrupt, respond now
- normal → add to TodoWrite, respond when current task done
- low → batch, respond at end of session

The watcher auto-respawns. Only re-launch after 10-min timeout.
```

## Codex CLI Integration

### Background Monitor

1. Enable the "Background terminal" feature (requires Codex 0.77.0+):
   ```
   /experimental
   ```
   Toggle **Background terminal** to enabled. Or add to `~/.codex/config.toml`:
   ```toml
   [features]
   unified_exec = true
   ```

2. Start the monitor in a background terminal:
   ```
   Run this in a background terminal: while true; do amq monitor --timeout 0 --include-body --json; sleep 0.2; done
   ```

3. Verify with `/ps` - you should see the monitor in the list.

### Notify Hook (Fallback)

For guaranteed message checking on each turn:

1. Add to `~/.codex/config.toml`:
   ```toml
   notify = ["python3", "/path/to/repo/scripts/codex-amq-notify.py"]
   ```

2. The script will:
   - Check for pending messages after each Codex turn
   - Send desktop notifications for new messages
   - Write to `.agent-mail/meta/amq-bulletin.json`

## Review Partner Workflow

### Requesting a Review

```bash
# Claude requests review from Codex
amq send --me claude --to codex \
  --subject "Review: Authentication refactor" \
  --kind review_request \
  --priority normal \
  --labels "auth,security" \
  --context '{"paths": ["internal/auth/"], "focus": "JWT handling"}' \
  --body @review-request.md \
  --ack
```

**review-request.md:**
```markdown
## Changes
- Refactored JWT token validation
- Added refresh token support
- Updated error handling

## What to Review
1. Security implications of refresh tokens
2. Error message exposure
3. Token expiration logic

## Context
See git diff in `internal/auth/` directory.
```

### Responding to a Review

```bash
# Codex responds
amq reply --me codex --id "msg_review_123" \
  --kind review_response \
  --body @review-response.md
```

## Context Object Schema

The `context` field accepts any JSON object. Recommended structure:

```json
{
  "paths": ["internal/cli/send.go", "internal/format/message.go"],
  "symbols": ["Header", "runSend"],
  "focus": "error handling in validation",
  "commands": ["go test ./internal/cli/..."],
  "hunks": [
    {"file": "send.go", "lines": "45-60"}
  ]
}
```

## Advanced: Intelligent Stop Hook

This repo includes a Stop hook that prevents the agent from stopping while messages are pending:

**Enabled by default** via `.claude/settings.json`:
```json
{
  "hooks": {
    "Stop": [
      {
        "type": "command",
        "command": "./scripts/amq-stop-hook.sh"
      }
    ]
  }
}
```

The hook (`scripts/amq-stop-hook.sh`) checks `amq list --new` and returns:
- `{"decision": "approve"}` → No pending messages, allow stop
- `{"decision": "block", "reason": "You have N pending message(s)..."}` → Force agent to drain inbox

This creates a self-sustaining loop where the agent can't stop until all messages are processed.

See [Claude Code hooks documentation](https://code.claude.com/docs/en/hooks) for more hook options.

## Best Practices

1. **Always set priority** - Helps receiving agent triage
2. **Use appropriate kind** - Enables smart auto-responses
3. **Include context.paths** - Helps focus the review
4. **Keep bodies concise** - Agent context is precious
5. **Use --ack for important messages** - Ensures delivery confirmation
6. **Re-launch watcher after timeout** - The 10-min limit requires periodic re-launch
7. **Thread replies** - Use `amq reply` for conversation continuity

## Troubleshooting

### Watcher not starting
```bash
# Check if inbox exists
ls -la .agent-mail/agents/claude/inbox/

# Initialize if needed
amq init --root .agent-mail --agents claude,codex
```

### Messages not appearing
```bash
# Check inbox directly
amq list --me claude --new --json

# Force poll mode if fsnotify issues
amq monitor --me claude --poll --json
```

### Codex notify hook not working
```bash
# Test the script directly
python3 scripts/codex-amq-notify.py '{"type": "agent-turn-complete"}'

# Check bulletin file
cat .agent-mail/meta/amq-bulletin.json
```
