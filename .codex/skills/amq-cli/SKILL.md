---
name: amq-cli
description: Coordinate agents via the AMQ CLI for file-based inter-agent messaging. Use when you need to send messages to another agent (Claude/Codex), receive messages from partner agents, set up co-op mode between Claude Code and Codex CLI, or manage agent-to-agent communication in any multi-agent workflow.
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

For manual install or build from source, see [INSTALL.md](https://github.com/avivsinai/agent-message-queue/blob/main/INSTALL.md).

## Quick Reference

```bash
export AM_ROOT=.agent-mail AM_ME=claude   # or: AM_ME=codex

amq send --to codex --body "Message"           # Send
amq drain --include-body                       # Receive (recommended)
amq reply --id <msg_id> --body "Response"      # Reply
amq watch --timeout 60s                        # Wait for messages
amq monitor --peek --timeout 0 --include-body --json  # Background watcher (peek)
```

Codex notify hook (mandatory):
```toml
notify = ["python3", "/path/to/repo/scripts/codex-amq-notify.py"]
```
Uses notify payload `cwd` to locate `.agent-mail` unless `AM_ROOT` is set. Optional `AMQ_NOTIFY_LOG`
captures raw payloads for debugging.
Python is used to parse the JSON payload without requiring `jq`.
The hook exits quickly when `inbox/new` is empty or missing to avoid extra overhead.

## Co-op Mode (Autonomous Multi-Agent)

In co-op mode, agents work autonomously. **Message your partner, not the user.**

| Situation | Action |
|----------|--------|
| Blocked | Message partner |
| Need review | Send `kind: review_request` |
| Done | Signal completion |
| Ask user only for | credentials, unclear requirements |

### Setup

Run once per project:
```bash
curl -sL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/setup-coop.sh | bash
export AM_ROOT=.agent-mail AM_ME=claude   # or: codex
```

### Background Watcher

Start a background watcher to receive messages while you work.

**Claude Code:** Use a subagent (haiku) to run the watcher:

```
Run this command and wait for messages (blocks until one arrives):
  amq monitor --peek --timeout 0 --include-body --json

When output returns, format a summary by priority:
- URGENT: List with from/subject/kind + body preview → requires immediate attention
- NORMAL: List with from/subject/kind → add to TODOs
- LOW: Count and summarize → batch for later

Do NOT take actions yourself. Just report what arrived, then STOP so the main agent wakes up.
```

The main agent will respawn the watcher after processing each batch.

**Claude Code (recommended):** Add SessionStart + Stop hooks in `.claude/settings.local.json`:
```json
{
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "$CLAUDE_PROJECT_DIR/scripts/claude-session-start.sh"}]}
    ],
    "Stop": [
      {"hooks": [{"type": "command", "command": "$CLAUDE_PROJECT_DIR/scripts/amq-stop-hook.sh"}]}
    ]
  }
}
```
The stop hook short-circuits (no `amq` call) when `inbox/new` is empty.

**Codex CLI (mandatory):** Configure notify hook so Codex surfaces AMQ messages after each turn:
```toml
notify = ["python3", "/path/to/repo/scripts/codex-amq-notify.py"]
```
When notified, run `amq drain --include-body`.

**Codex CLI (optional):** Background monitor for /ps visibility or manual diagnostics only; it does not wake Codex:
```bash
while true; do amq monitor --peek --timeout 0 --include-body --json; sleep 0.2; done
```
Drain after handling to avoid repeated notifications in peek mode.
(`drain` moves messages from `new` to `cur` as the archive.)

### Priority Handling

| Priority | Action |
|----------|--------|
| `urgent` | Interrupt, respond now |
| `normal` | Add to TODOs, respond after current task |
| `low` | Batch for session end |

## Commands

### Send
```bash
amq send --to codex --body "Quick message"
amq send --to codex --subject "Review" --kind review_request --body @file.md
amq send --to claude --priority urgent --kind question --body "Blocked on API"
```

### Receive
```bash
amq drain --include-body         # One-shot, silent when empty
amq monitor --timeout 0 --json        # Block until message, drain, emit JSON
amq monitor --peek --timeout 0 --json # Block until message, peek only
amq list --new                   # Peek without side effects
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
amq dlq retry --all [--force]       # Retry all (--force ignores max retries)
amq dlq purge --older-than 24h      # Clean old DLQ entries
```
Corrupt/unparseable messages auto-move to DLQ during drain/monitor. Max 3 retries before permanent DLQ.

### Other
```bash
amq thread --id p2p/claude__codex --include-body   # View thread
amq presence set --status busy --note "reviewing"  # Set presence
amq cleanup --tmp-older-than 36h                   # Clean stale tmp
```

## Message Kinds

| Kind | Reply kind | Use |
|------|------------|-----|
| `review_request` | `review_response` | Code review |
| `question` | `answer` | Questions |
| `decision` | — | Design decisions |
| `status` | — | FYI updates |

## Conventions

- Handles: lowercase `[a-z0-9_-]+`
- Threads: `p2p/<agentA>__<agentB>` (lexicographic)
- Delivery: atomic Maildir (tmp -> new -> cur)
- Never edit message files directly

See [COOP.md](https://github.com/avivsinai/agent-message-queue/blob/main/COOP.md) for full protocol.
