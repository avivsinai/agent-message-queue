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
# Required setup (run once per terminal session)
eval "$(amq env --me claude)"    # For Claude Code
eval "$(amq env --me codex)"     # For Codex CLI

# Send and receive messages
amq send --to codex --body "Message"           # Send
amq drain --include-body                       # Receive (recommended)
amq reply --id <msg_id> --body "Response"      # Reply
amq watch --timeout 60s                        # Wait for messages
```

**Note**: After setup, all commands work from any subdirectory.

> **Important**: Don't hardcode `AM_ROOT=.agent-mail`. Use `amq env` which auto-detects the configured root from `.amqrc` or existing directories. Only set `AM_ROOT` explicitly when intentionally overriding (e.g., multi-pair isolation with `--root`).

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

Run once per project:
```bash
curl -sL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/setup-coop.sh | bash
eval "$(amq env --me claude)"   # or: --me codex
```

### Multiple Pairs (Isolated Sessions)

Run multiple agent pairs on different features using separate root paths (`AM_ROOT` or `--root`):

```bash
# Pair A (auth feature): AM_ROOT=.agent-mail/auth
# Pair B (api refactor): AM_ROOT=.agent-mail/api
```

Each root has isolated inboxes and wake processes. Initialize each once:
```bash
amq init --root .agent-mail/auth --agents claude,codex
amq init --root .agent-mail/api --agents claude,codex
```

### Wake Notifications (Optional, Interactive Terminals Only)

> **AI agents**: Skip this section. Wake requires an interactive terminal with TTY access (TIOCSTI/ioctl). Non-interactive sessions (scripts, CI, headless) cannot use wake. Just use `amq drain` or `amq watch` to check for messages.

For **human operators** running Claude Code or Codex CLI in an interactive terminal, wake provides background notifications when messages arrive:

```bash
amq wake &
claude
```

When messages arrive, you'll see a notification injected into your terminal (best-effort):
```
AMQ: message from codex - Review complete. Run: amq drain --include-body
```

Then run `amq drain --include-body` to read messages.

**Inject Modes**: The wake command auto-detects your CLI type:
- `--inject-mode=auto` (default): Uses `raw` for Claude Code/Codex, `paste` for others
- `--inject-mode=raw`: Plain text + CR (best for Ink-based CLIs like Claude Code)
- `--inject-mode=paste`: Bracketed paste with delayed CR (best for crossterm CLIs)

If notifications require manual Enter, try `--inject-mode=raw`.

### Priority Handling

| Priority | Action |
|----------|--------|
| `urgent` | Interrupt, respond now |
| `normal` | Add to TODOs, respond after current task |
| `low` | Batch for session end |

### Progress Updates

When starting long work, send a status message so the sender knows you're working:

```bash
# Signal you've started (with optional ETA)
amq reply --id <msg_id> --kind status --body "Started processing, eta ~20m"

# If blocked, update status
amq reply --id <msg_id> --kind status --body "Blocked: waiting for API clarification"

# When done, send the final response (use appropriate kind: answer, review_response, etc.)
amq reply --id <msg_id> --kind answer --body "Here's my response..."
```

The sender can check your progress via:
```bash
amq thread --id <thread_id> --include-body --limit 5
```

## Commands

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

### Environment Setup
```bash
amq env                      # Output shell exports (auto-detects .amqrc or .agent-mail/)
amq env --me codex           # Override agent handle
amq env --shell fish         # Fish shell syntax
amq env --json               # Machine-readable output
amq env --wake               # Include 'amq wake &' (interactive terminals only)
```

### Other
```bash
amq thread --id p2p/claude__codex --include-body   # View thread
amq presence set --status busy --note "reviewing"  # Set presence
amq cleanup --tmp-older-than 36h                   # Clean stale tmp
```

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
