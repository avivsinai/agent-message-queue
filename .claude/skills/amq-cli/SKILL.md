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

### Send

1. Use `--to` (required) and optionally `--thread`.
2. If `--thread` is omitted for a single recipient, use canonical p2p naming.

```bash
./amq send --me codex --to cloudcode --thread p2p/codex__cloudcode --body @notes.md
```

### List + Read + Ack

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

### During active work: Check inbox between steps

When doing multi-step work, **check your inbox between steps** to pick up coordination messages from other agents:

```bash
# Quick, non-blocking check (<10ms)
./amq list --me $AM_ME --new --json
```

If you receive a message, read and process it before continuing your work. This keeps agents synchronized without blocking.

### Waiting for a reply: Use watch

When you've sent a message and need to wait for a response:

```bash
# Send request
./amq send --to codex --subject "Review this" --body @file.go

# Wait for reply (blocks until message arrives, low latency)
./amq watch --me cloudcode --timeout 120s

# Process the reply
./amq read --me cloudcode --id <msg_id>
```

### Workflow summary

| Situation | Command | Behavior |
|-----------|---------|----------|
| Working, quick check | `amq list --new` | Non-blocking, <10ms |
| Waiting for reply | `amq watch --timeout 60s` | Blocks until message |
| Processing message | `amq read --id <id>` | Retrieve full message |

## Conventions and safety

- Do not edit message files in place; always use the CLI.
- Delivery is Maildir-style: tmp -> new -> cur. Readers must ignore tmp.
- Cleanup is explicit and never automatic; do not delete files without the cleanup command.
- Thread naming for p2p: `p2p/<lowerA>__<lowerB>` (sorted lexicographically).
- Handles must be lowercase and match `[a-z0-9_-]+`.
