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

3. **Multi-agent verification** - Instead of asking "is this right?", send a review request to your partner.

4. **Completion signals** - When your task is done, signal completion clearly.

5. **Iteration over perfection** - Ship working code, request review, iterate based on feedback.

### When to Message Partner vs User

| Situation | Action |
|-----------|--------|
| Need code review | → Message partner |
| Blocked on design decision | → Message partner (kind: `decision`) |
| Found bug in partner's code | → Message partner (kind: `review_response`) |
| Task complete, ready for next | → Message partner (kind: `status`) |
| Need external credentials/access | → Ask user |
| Unclear on original requirements | → Ask user |

## Quick Start

### Prerequisites (One-Time)

1. **Install amq CLI** ([releases](https://github.com/avivsinai/agent-message-queue/releases)):
   ```bash
   curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
   ```

2. **Install amq-cli skill** for your agents:

   **Claude Code** (via marketplace):
   ```
   /plugin marketplace add avivsinai/skills-marketplace
   /plugin install amq-cli@avivsinai-marketplace
   ```

   **Codex CLI** (Codex chat command):
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

### Running Co-op Mode

**Terminal 1 - Claude Code:**
```bash
export AM_ME=claude AM_ROOT=.agent-mail
amq wake &
claude
```

**Terminal 2 - Codex CLI:**
```bash
export AM_ME=codex AM_ROOT=.agent-mail
amq wake &
codex
```

That's it. When messages arrive, `amq wake` attempts to inject a notification into your terminal (best-effort).

### How It Works

1. `amq wake` runs as a background job in the same terminal
2. When a message arrives in your inbox, it uses TIOCSTI to inject text into your terminal
3. The CLI reads the input at the next prompt (if busy, it queues)
4. You run `amq drain --include-body` to read messages
5. Agents work autonomously—messaging each other, not the user

### Fallback: Codex Notify Hook

If `amq wake` doesn't work (e.g., kernel hardening disables TIOCSTI), configure the notify hook:

```toml
# ~/.codex/config.toml
notify = ["python3", "/path/to/repo/scripts/codex-amq-notify.py"]
```

The hook surfaces pending messages after each Codex turn.

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
| `urgent` | Interrupt current work | Blocking issues, critical bugs, time-sensitive |
| `normal` | Add to TODO list | Code reviews, questions, standard requests |
| `low` | Batch/digest later | Status updates, FYIs, non-blocking info |

### Message Kinds

| Kind | Default Priority | Description |
|------|------------------|-------------|
| `review_request` | normal | Request code review |
| `review_response` | normal | Code review feedback |
| `question` | normal | Question needing answer |
| `answer` | normal | Response to a question |
| `decision` | normal | Decision request |
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
  --body "Please review the new message parser..."

# Urgent blocking question
amq send --me codex --to claude \
  --subject "Blocked: API design question" \
  --priority urgent \
  --kind question \
  --body "I need to decide on the API shape..."
```

### Receive Messages

```bash
# Recommended: one-shot drain
amq drain --include-body

# Wait for messages (blocking)
amq watch --timeout 60s

# Peek without side effects
amq list --new
```

### Reply (Auto Thread/Refs)

```bash
amq reply --me codex --id "msg_123" \
  --kind review_response \
  --body "LGTM with minor suggestions..."
```

## Wake Command (Experimental)

`amq wake` uses TIOCSTI to inject notifications into your terminal:

```bash
# Start waker before CLI
amq wake --me claude &
claude
```

**Options:**
- `--bell` - Ring terminal bell on new messages
- `--inject-cmd "..."` - Inject actual command instead of notification
- `--debounce 250ms` - Batch rapid messages
- `--preview-len 48` - Max subject preview length

**Notification format:**
- Single message: `[AMQ] Message from codex: Review complete`
- Multiple: `[AMQ] 3 messages: 2 from codex, 1 from claude. Run: amq drain --include-body`

**Platform support:**
- macOS: Works
- Linux: May be disabled by kernel hardening (CONFIG_LEGACY_TIOCSTI)
- Windows: Not supported (use WSL)

## Review Partner Workflow

### Requesting a Review

```bash
amq send --me claude --to codex \
  --subject "Review: Authentication refactor" \
  --kind review_request \
  --priority normal \
  --context '{"paths": ["internal/auth/"], "focus": "JWT handling"}' \
  --body @review-request.md \
  --ack
```

### Responding to a Review

```bash
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
  "hunks": [{"file": "send.go", "lines": "45-60"}]
}
```

## Best Practices

1. **Always set priority** - Helps receiving agent triage
2. **Use appropriate kind** - Enables smart auto-responses
3. **Include context.paths** - Helps focus the review
4. **Keep bodies concise** - Agent context is precious
5. **Use --ack for important messages** - Ensures delivery confirmation
6. **Thread replies** - Use `amq reply` for conversation continuity

## Troubleshooting

### Wake not working
```bash
# Test: run amq wake and watch for warnings
amq wake --me claude

# Fallback: use notify hook for Codex, or run amq drain manually
```

### Messages not appearing
```bash
# Check inbox directly
amq list --me claude --new --json

# Force poll mode if fsnotify issues
amq watch --me claude --poll
```

### Codex not responding
Configure the notify hook as fallback:
```toml
notify = ["python3", "/path/to/repo/scripts/codex-amq-notify.py"]
```
