---
name: amq-cli
description: Coordinate agents via the AMQ CLI for file-based inter-agent messaging. Use when you need to send messages to another agent (Claude/Codex), receive messages from partner agents, set up co-op mode between Claude Code and Codex CLI, or manage agent-to-agent communication in any multi-agent workflow. Triggers include "message codex", "talk to claude", "collaborate with partner agent", "AMQ", "inter-agent messaging", or "agent coordination".
metadata:
  short-description: Inter-agent messaging via AMQ CLI
---

# AMQ CLI Skill

File-based message queue for agent-to-agent coordination.

## Prerequisites

Requires `amq` binary in PATH. Install:
```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
```

Verify: `amq --version`

## Quick Reference

```bash
# Start a session (one command, sets up everything)
amq session start --me claude   # For Claude Code
amq session start --me codex    # For Codex CLI

# Send and receive messages
amq send --to codex --body "Message"           # Send
amq drain --include-body                       # Receive (recommended)
amq reply --id <msg_id> --body "Response"      # Reply
amq watch --timeout 60s                        # Wait for messages

# End session
amq session stop
```

**Note**: After `amq session start`, all commands work from any subdirectory.

## Co-op Mode (Autonomous Multi-Agent)

In co-op mode, agents work autonomously. **Message your partner, not the user.**

### Shared Workspace

**Both agents work in the same project folder.** Files are shared automatically:
- If partner says "done with X" → check the files directly, don't ask for code
- If partner says "see my changes" → read the files, they're already there
- Don't send code snippets in messages → just reference file paths

Only use messages for: coordination, questions, review requests, status updates.

| Situation | Action |
|----------|--------|
| Blocked | Message partner |
| Need review | Send `kind: review_request` with file paths |
| Partner done | Read files directly (they're local) |
| Done | Signal completion |
| Ask user only for | credentials, unclear requirements |

### Setup

Run once per project to initialize the queue:
```bash
curl -sL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/setup-coop.sh | bash
```

Optionally create `.amqrc` for project-level root config:
```bash
echo '{"root": ".agent-mail"}' > .amqrc
```

Then start a session in each terminal:
```bash
# Terminal 1 - Claude Code
amq session start --me claude

# Terminal 2 - Codex CLI
amq session start --me codex
```

### Priority Handling

| Priority | Action |
|----------|--------|
| `urgent` | Interrupt, respond now |
| `normal` | Add to TODOs, respond after current task |
| `low` | Batch for session end |

## Commands

### Session Management
```bash
amq session start --me claude   # Start session (also starts wake)
amq session start --me codex --no-wake  # Start without wake
amq session stop                # Stop session and wake process
amq session status              # Show current session
amq session status --json       # Machine-readable status
```

### Send
```bash
amq send --to codex --body "Quick message"
amq send --to codex --subject "Review" --kind review_request --body @file.md
amq send --to claude --priority urgent --kind question --body "Blocked on API"
amq send --to codex --labels "bug,parser" --body "Found issue in parser"
amq send --to codex --context '{"paths": ["internal/cli/"]}' --body "Review these"
```

### Receive
```bash
amq drain --include-body         # One-shot, silent when empty
amq watch --timeout 60s          # Block until message arrives
amq list --new                   # Peek without side effects
```

### Filter Messages
```bash
amq list --new --priority urgent              # By priority
amq list --new --from codex                   # By sender
amq list --new --kind review_request          # By kind
amq list --new --label bug --label critical   # By labels (can repeat)
amq list --new --from codex --priority urgent # Combine filters
```

### Reply
```bash
amq reply --id <msg_id> --body "LGTM"
amq reply --id <msg_id> --kind review_response --body "See comments..."
```

### Dead Letter Queue
```bash
amq dlq list                        # List failed messages
amq dlq read --id <dlq_id>          # Inspect failure details
amq dlq retry --id <dlq_id>         # Retry (move back to inbox)
amq dlq retry --all [--force]       # Retry all
amq dlq purge --older-than 24h      # Clean old DLQ entries
```

### Upgrade
```bash
amq upgrade                    # Self-update to latest release
amq --no-update-check ...      # Disable update hint for this command
export AMQ_NO_UPDATE_CHECK=1   # Disable update hints globally
```

### Other
```bash
amq thread --id p2p/claude__codex --include-body   # View thread
amq presence set --status busy --note "reviewing"  # Set presence
amq cleanup --tmp-older-than 36h                   # Clean stale tmp
```

## Configuration

### .amqrc (Project Config)

Create `.amqrc` in your project root for shared root configuration:
```json
{"root": ".agent-mail"}
```

Note: `.amqrc` only configures `root`. Agent identity (`me`) is set per-session via `amq session start --me <agent>`.

### Precedence

Configuration is resolved in this order (highest to lowest):
- **Root**: flags > env (AM_ROOT) > session > .amqrc > auto-detect (.agent-mail/)
- **Me**: flags > env (AM_ME) > session

## Message Kinds

| Kind | Reply Kind | Default Priority | Use |
|------|------------|------------------|-----|
| `review_request` | `review_response` | normal | Code review |
| `review_response` | — | normal | Review feedback |
| `question` | `answer` | normal | Questions |
| `answer` | — | normal | Answers |
| `decision` | — | normal | Design decisions |
| `brainstorm` | — | low | Open discussion |
| `status` | — | low | FYI updates |
| `todo` | — | normal | Task assignments |

## Labels and Context

**Labels** tag messages for filtering:
```bash
amq send --to codex --labels "bug,urgent" --body "Critical issue"
```

**Context** provides structured metadata:
```bash
amq send --to codex --kind review_request \
  --context '{"paths": ["internal/cli/send.go"], "focus": "error handling"}' \
  --body "Please review"
```

## Conventions

- Handles: lowercase `[a-z0-9_-]+`
- Threads: `p2p/<agentA>__<agentB>` (lexicographic)
- Delivery: atomic Maildir (tmp -> new -> cur)
- Never edit message files directly

## References

Read these when you need deeper context:

- `references/coop-mode.md` — Read when setting up or debugging co-op workflows between agents
- `references/message-format.md` — Read when you need the full frontmatter schema (all fields, types, defaults)
