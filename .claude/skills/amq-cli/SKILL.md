---
name: amq-cli
description: Coordinates agents via the AMQ CLI for atomic Maildir-style message delivery. Handles initializing mailboxes, sending/reading/acking messages, listing inboxes, viewing threads, setting presence, and cleaning stale tmp files. Covers tmp/new/cur semantics and safe, non-destructive usage conventions.
---

# AMQ CLI

## Quick start

- Build the binary if needed: `go build -o amq ./cmd/amq`.
- Prefer `AM_ROOT` and `AM_ME` to avoid repeating flags.

```bash
export AM_ROOT=.agent-mail
export AM_ME=codex
./amq init --root .agent-mail --agents codex,cloudcode
./amq send --to cloudcode --body "Quick ping"
```

## Core flows

### Drain (recommended for agents)

One-shot ingestion: reads all new messages, moves to cur, optionally acks. Designed for hooks and scripts.

```bash
# Drain all new messages with body, auto-ack
./amq drain --me cloudcode --include-body --ack

# Limit to 10 messages, JSON output
./amq drain --me cloudcode --limit 10 --json

# Silent when empty (perfect for hooks)
./amq drain --me $AM_ME --include-body
```

**Flags:**
- `--limit N` (default 20): Max messages to drain
- `--include-body`: Include message body in output
- `--ack` (default true): Ack messages that require acknowledgment

### Send

1. Use `--to` (required) and optionally `--thread`.
2. If `--thread` is omitted for a single recipient, use canonical p2p naming.

```bash
./amq send --me codex --to cloudcode --thread p2p/codex__cloudcode --body @notes.md
```

### List + Read + Ack (manual flow)

```bash
./amq list --me cloudcode --new
./amq read --me cloudcode --id <msg_id>
./amq ack  --me cloudcode --id <msg_id>
```

### Thread view

```bash
./amq thread --id p2p/codex__cloudcode --limit 50 --include-body
```

### Watch for messages

Wait for new messages with efficient OS-native notifications (uses fsnotify):

```bash
# Block until message arrives or timeout
./amq watch --me cloudcode --timeout 60s

# Use polling fallback for network filesystems
./amq watch --me cloudcode --timeout 60s --poll
```

### Presence (optional)

```bash
./amq presence set --me codex --status busy --note "reviewing"
./amq presence list
```

### Cleanup (explicit only)

```bash
./amq cleanup --tmp-older-than 36h
```

## Multi-Agent Coordination

### Preferred: Use drain for message ingestion

The `drain` command is designed for agent integration - it does list+read+ack in one atomic operation:

```bash
# Ingest all new messages (silent when empty - hook-friendly)
./amq drain --me $AM_ME --include-body

# With JSON output for programmatic use
./amq drain --me $AM_ME --include-body --json
```

### During active work: Quick inbox check

When doing multi-step work, use drain to check for coordination messages:

```bash
# One-shot: get messages, mark read, ack if needed
./amq drain --me $AM_ME --include-body
```

### Waiting for a reply: Use watch + drain

When you've sent a message and need to wait for a response:

```bash
# Send request
./amq send --to codex --subject "Review this" --body @file.go

# Wait for reply (blocks until message arrives)
./amq watch --me cloudcode --timeout 120s

# Ingest the reply
./amq drain --me cloudcode --include-body
```

### Workflow summary

Commands below assume `AM_ME` is set:

| Situation | Command | Behavior |
|-----------|---------|----------|
| Ingest messages | `amq drain --include-body` | One-shot: read+move+ack |
| Waiting for reply | `amq watch --timeout 60s` | Blocks until message |
| Quick peek only | `amq list --new` | Non-blocking, no side effects |

## Conventions and safety

- Do not edit message files in place; always use the CLI.
- Delivery is Maildir-style: tmp -> new -> cur. Readers must ignore tmp.
- Cleanup is explicit and never automatic; do not delete files without the cleanup command.
- Thread naming for p2p: `p2p/<lowerA>__<lowerB>` (sorted lexicographically).
- Handles must be lowercase and match `[a-z0-9_-]+`.
