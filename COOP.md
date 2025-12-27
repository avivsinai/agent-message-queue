# Co-op Mode: Inter-Agent Collaboration Protocol

Co-op mode enables Claude Code and Codex CLI to collaborate as **review partners**, sharing context, requesting reviews, and coordinating work through the Agent Message Queue.

## Quick Start

### Claude Code Session

```bash
export AM_ME=claude
export AM_ROOT=.agent-mail

# Start the co-op watcher in background
# "Run amq-coop-watcher in the background while I work"

# When messages arrive, handle by priority:
# - urgent → interrupt and respond
# - normal → add to TODOs
# - low → batch for later
```

### Codex CLI Session

```bash
export AM_ME=codex
export AM_ROOT=.agent-mail

# Enable background terminals in ~/.codex/config.toml:
# [features]
# unified_exec = true

# Run monitor in background terminal:
./scripts/codex-coop-monitor.sh

# Or use the notify hook for turn-based checking
```

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
amq monitor --me claude --once --timeout 0 --include-body --json

# With timeout (60s default)
amq monitor --me codex --timeout 30s --json

# For scripts/hooks
amq monitor --me claude --once --json
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

The `amq-coop-watcher` agent (`.claude/agents/amq-coop-watcher.md`) runs in the background and wakes the main agent when messages arrive.

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
3. Respawn the watcher

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

After handling, respawn watcher immediately.
```

## Codex CLI Integration

### Background Monitor

1. Enable in `~/.codex/config.toml`:
   ```toml
   [features]
   unified_exec = true
   ```

2. Run the monitor script:
   ```bash
   ./scripts/codex-coop-monitor.sh
   ```

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

## Best Practices

1. **Always set priority** - Helps receiving agent triage
2. **Use appropriate kind** - Enables smart auto-responses
3. **Include context.paths** - Helps focus the review
4. **Keep bodies concise** - Agent context is precious
5. **Use --ack for important messages** - Ensures delivery confirmation
6. **Respawn watcher immediately** - Don't miss messages
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
amq monitor --me claude --poll --once --json
```

### Codex notify hook not working
```bash
# Test the script directly
python3 scripts/codex-amq-notify.py '{"type": "agent-turn-complete"}'

# Check bulletin file
cat .agent-mail/meta/amq-bulletin.json
```
