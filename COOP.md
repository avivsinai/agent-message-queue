# Co-op Mode: Phased Parallel Work

Co-op mode enables Claude Code and Codex CLI to work **in parallel where safe, coordinate where risky**, leveraging cognitive diversity (different models = different training = different blind spots) to catch errors that same-model review would miss.

## Roles

- **Claude Code** = Leader + Worker (coordinates phases, merges, prepares commits, gets user approval)
- **Codex** = Worker (executes phases, reports to leader, awaits next assignment)

## Phased Flow

| Phase | Mode | Description |
|-------|------|-------------|
| **Research** | Parallel | Both explore codebase, read docs, search. No conflicts. |
| **Design** | Parallel → Merge | Both propose approaches. Leader merges/decides. |
| **Code** | Split | Divide by file/module. Never edit same file. |
| **Review** | Parallel | Both review each other's code. Leader decides disputes. |
| **Test** | Parallel | Both run tests, report results to leader. |

```
Research (parallel) → sync findings
    ↓
Design (parallel) → leader merges approach
    ↓
Code (split: e.g., Claude=files A,B; Codex=files C,D)
    ↓
Review (parallel: each reviews other's code)
    ↓
Test (parallel: both run tests)
    ↓
Leader prepares commit → user approves → push
```

## Core Principles

1. **Parallel where safe** - Research, design, review, and test phases run in parallel.

2. **Split where risky** - Code phase divides files/modules to avoid conflicts. If same file unavoidable, assign one owner; other reviews/proposes via message.

3. **Never branch** - Always work on same branch (joined work). No feature branches.

4. **Cognitive diversity** - Different models catch different bugs. Cross-model work > same-model self-review.

5. **Leader coordinates** - Claude Code handles phase transitions, merges, and final decisions.

6. **Coordinate between phases** - Sync findings/decisions before moving to next phase.

## When to Act

| Agent | Action |
|-------|--------|
| Codex | Complete phase → report to leader → await next assignment |
| Claude Code | Receive reports → merge/decide → assign next phase work |
| Either | Ask user only for: credentials, unclear requirements |

**While waiting**: Safe to do light work — review partner's code, run tests, read docs. If no assignment comes, ask leader (not user) for next task.

## Quick Start

### Prerequisites (One-Time)

1. **Install amq CLI** ([releases](https://github.com/avivsinai/agent-message-queue/releases)):
   ```bash
   curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
   ```

2. **Install amq-cli skill** for your agents:

   **Via skills** (recommended):
   ```bash
   npx skills add avivsinai/agent-message-queue -g -y
   ```

   **Or via skild:**
   ```bash
   npx skild install @avivsinai/amq-cli -t claude -y
   ```

   See [INSTALL.md](INSTALL.md) for manual installation or troubleshooting.

### Per-Project Setup

Initialize your project for co-op mode:

```bash
amq coop init
```

This creates:
- `.amqrc` - Configuration file
- `.agent-mail/` - Agent mailboxes
- Updates `.gitignore` (if present)

### Running Co-op Mode

**Terminal 1 - Claude Code:**
```bash
amq coop start claude
```

**Terminal 2 - Codex CLI:**
```bash
amq coop start codex
```

Pass flags to the agent after `--`:
```bash
amq coop start claude -- --dangerously-skip-permissions
```

For terminal notifications, run `amq wake &` before starting the agent:
```bash
amq wake --me claude &
amq coop start claude
```

### Multiple Pairs (Isolated Sessions)

Need multiple agent pairs working on different features simultaneously? Use separate `--root` paths:

```bash
# Initialize isolated roots
amq coop init --root .agent-mail/auth
amq coop init --root .agent-mail/api

# Pair A: auth feature
amq coop start --root .agent-mail/auth claude   # Terminal 1
amq coop start --root .agent-mail/auth codex    # Terminal 2

# Pair B: api refactor
amq coop start --root .agent-mail/api claude    # Terminal 3
amq coop start --root .agent-mail/api codex     # Terminal 4
```

Each pair has isolated inboxes and threads. Messages stay within their root—auth-Claude talks to auth-Codex, never api-Codex.

### How It Works

1. `amq coop start` sets environment variables (AM_ME, AM_ROOT) and execs the agent
2. Run `amq drain --include-body` periodically to check for messages
3. Optionally run `amq wake --me <agent> &` before starting for terminal notifications
4. Agents work autonomously—messaging each other, not the user

### Fallback: Notify Hook (if wake unavailable)

`amq wake` uses TIOCSTI which may be unavailable on:
- Hardened Linux (CONFIG_LEGACY_TIOCSTI=n)
- Windows (use WSL)

If wake fails, configure the notify hook for desktop notifications:

```toml
# ~/.codex/config.toml
notify = ["python3", "/path/to/repo/scripts/codex-amq-notify.py"]
```

The hook surfaces pending messages after each agent turn + sends desktop notification.

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

## Wake Command (Optional)

> Co-op works without wake. This is an optional enhancement for interactive terminals.

`amq wake` uses TIOCSTI to inject notifications into your terminal:

```bash
# Start waker before CLI
amq wake --me claude &
claude
```

**Options:**
- `--inject-mode auto|raw|paste` - Injection strategy (see below)
- `--bell` - Ring terminal bell on new messages
- `--inject-cmd "..."` - Inject actual command instead of notification
- `--debounce 250ms` - Batch rapid messages
- `--preview-len 48` - Max subject preview length

**Inject Modes:**
- `auto` (default) - Detects CLI type: uses `raw` for Claude Code/Codex, `paste` for others
- `raw` - Plain text + carriage return, no bracketed paste (best for Ink-based CLIs)
- `paste` - Bracketed paste with delayed CR (best for crossterm-based CLIs)

If notifications appear but require manual Enter, use `--inject-mode=raw`.

**Notification format:**
- Single message: `AMQ: message from codex - Review complete. Drain with: amq drain --include-body — then act on it`
- Multiple: `AMQ: 3 messages - 2 from codex, 1 from claude. Drain with: amq drain --include-body — then act on it`

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

## Progress Updates for Long-Running Work

When starting work that may take a while, send a status message so the sender knows you're working on it:

```bash
# Signal you've started (optional ETA)
amq reply --me codex --id "msg_review_123" \
  --kind status \
  --body "Started processing, eta ~20m"

# If blocked, send another status
amq reply --me codex --id "msg_review_123" \
  --kind status \
  --body "Blocked: waiting for API clarification"

# When done, send the final response (use appropriate kind: answer, review_response, etc.)
amq reply --me codex --id "msg_review_123" \
  --kind answer \
  --body "Here's my response..."
```

The sender can check progress via thread view:

```bash
amq thread --id <thread_id> --include-body --limit 5
```

This pattern helps when:
- Claude is faster than Codex (or vice versa)
- Long tasks where the sender might think you're not responding
- You're blocked and need to communicate why

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

# If TIOCSTI unavailable, use notify hook (see Fallback section above)
# Or poll manually: amq drain --include-body
```

### Messages not appearing
```bash
# Check inbox directly
amq list --me claude --new --json

# Force poll mode if fsnotify issues
amq watch --me claude --poll
```
