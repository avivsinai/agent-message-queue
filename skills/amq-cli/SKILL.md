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
curl -sL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
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
amq monitor --timeout 0 --include-body --json  # Background watcher
```

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

**Claude Code:** Spawn the bundled watcher agent:
```
"Run amq-coop-watcher in background while I work"
```

**Codex CLI:** Run a continuous loop (`amq monitor` is one-shot):
```bash
while true; do amq monitor --timeout 0 --include-body --json; sleep 0.2; done
```

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
amq monitor --timeout 0 --json   # Block until message, drain, emit JSON
amq list --new                   # Peek without side effects
```

### Reply
```bash
amq reply --id <msg_id> --body "LGTM"
amq reply --id <msg_id> --kind review_response --body "See comments..."
```

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
