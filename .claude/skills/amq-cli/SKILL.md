---
name: amq-cli
description: Coordinates agents via the AMQ CLI for atomic Maildir-style message delivery. Handles initializing mailboxes, sending/reading/acking messages, listing inboxes, viewing threads, setting presence, and cleaning stale tmp files. Covers tmp/new/cur semantics and safe, non-destructive usage conventions.
---

# AMQ CLI

## Prerequisites

Install `amq` binary (must be in PATH):
```bash
# Download from releases and add to PATH
# https://github.com/avivsinai/agent-message-queue/releases

# Example (macOS arm64):
curl -L https://github.com/avivsinai/agent-message-queue/releases/latest/download/amq_darwin_arm64.tar.gz | tar xz
sudo mv amq /usr/local/bin/
```

Only clone the repo if contributing to the project itself.

## Quick start

Prefer `AM_ROOT` and `AM_ME` environment variables to avoid repeating flags.

```bash
export AM_ROOT=.agent-mail
export AM_ME=claude   # or codex
amq init --root .agent-mail --agents claude,codex
amq send --to codex --body "Quick ping"
```

## Co-op Mode (Claude Code <-> Codex CLI)

Co-op mode enables real-time collaboration between Claude Code and Codex CLI sessions.

### Autonomous Operation

**Critical**: In co-op mode, work autonomously until task completion. Don't ask the user—message your partner agent instead.

- **Blocked?** → Message partner, not user
- **Need review?** → Send `review_request` to partner
- **Complex decision?** → Use `ultrathink`, then decide (or ask partner via `decision` kind)
- **Done?** → Signal completion, don't wait for user confirmation

Only ask user for: external credentials, unclear original requirements.

See `COOP.md` for the full autonomous operation protocol.

### Setup

**First time in a project?** Run the setup script:
```bash
curl -sL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/setup-coop.sh | bash
```

This creates mailboxes, stop hook, and settings. Then set your environment:

```bash
export AM_ME=claude   # or codex
export AM_ROOT=.agent-mail
```

### Background Watcher

Both agents should use a `while true` loop to auto-respawn after each message:

```bash
while true; do amq monitor --timeout 0 --include-body --json; sleep 0.2; done
```

**Claude Code:** Use the `amq-coop-watcher` subagent (Task tool with `subagent_type: amq-coop-watcher`):
> "Run amq-coop-watcher in background while I work"

The watcher auto-respawns after each message. Only limitation is the 10-minute background task timeout - if you're working longer, re-launch the watcher.

**Codex CLI:** Run in a background terminal (requires `unified_exec = true` in config). **Do not** run the monitor as a normal one-shot command; it will end at the end of the turn and **will not** appear in `/ps`.

Enable background terminals:
```toml
# ~/.codex/config.toml
[features]
unified_exec = true
```

Preferred (uses the repo script so it "just works"):
```bash
./scripts/codex-coop-monitor.sh
```

Verify it's actually running in the background:
- Run `/ps` and confirm a background terminal shows `codex-coop-monitor.sh` (or `amq monitor --timeout 0`).

When messages arrive, handle by priority:
- **urgent** -> Interrupt current work, respond immediately
- **normal** -> Add to TODOs, respond when current task done
- **low** -> Batch for end of session

### Monitor (combined watch + drain)

```bash
# Block until message arrives, drain, output JSON
amq monitor --timeout 0 --include-body --json

# With timeout (default 60s)
amq monitor --timeout 30s --json

# Single-shot for scripts
amq monitor --once --json
```

### Reply (auto thread/refs)

```bash
# Reply to a message (auto-sets thread, refs, recipient)
amq reply --id <msg_id> --body "LGTM with minor suggestions..."

# Reply with priority
amq reply --id <msg_id> --priority urgent --body "Found a critical issue..."

# Reply with kind
amq reply --id <msg_id> --kind review_response --body "See my comments..."
```

### Send with co-op fields

```bash
# Request a code review
amq send --to codex \
  --subject "Review: New parser" \
  --priority normal \
  --kind review_request \
  --labels "parser,refactor" \
  --context '{"paths": ["internal/format/message.go"]}' \
  --body "Please review the error handling..."

# Urgent blocking question
amq send --to claude \
  --subject "Blocked: API design" \
  --priority urgent \
  --kind question \
  --body "Need to decide on the API shape..."
```

### Priority levels

| Priority | Behavior | Use when |
|----------|----------|----------|
| `urgent` | Interrupt current work | Blocking issues, critical bugs |
| `normal` | Add to TODO list | Code reviews, questions |
| `low` | Batch/digest later | Status updates, FYIs |

### Message kinds

| Kind | Auto-reply kind | Description |
|------|-----------------|-------------|
| `question` | `answer` | Question needing answer |
| `review_request` | `review_response` | Request code review |
| `brainstorm` | (same) | Open-ended discussion |
| `decision` | (same) | Decision request |
| `status` | (same) | Status update/FYI |
| `todo` | (same) | Task assignment |

## Core Commands

### Drain (recommended for agents)

One-shot ingestion: reads all new messages, moves to cur, optionally acks. Designed for hooks and scripts.

```bash
# Drain all new messages with body, auto-ack
amq drain --include-body --ack

# Limit to 10 messages, JSON output
amq drain --limit 10 --json

# Silent when empty (perfect for hooks)
amq drain --include-body
```

**Flags:**
- `--limit N` (default 20): Max messages to drain (0 = no limit)
- `--include-body`: Include message body in output
- `--ack` (default true): Ack messages that require acknowledgment

### Send

```bash
# Simple send
amq send --to codex --body "Quick message"

# With subject and thread
amq send --to codex --subject "Review needed" --thread project/feature-123 --body @notes.md

# Request acknowledgment
amq send --to codex --body "Please confirm" --ack
```

### List + Read + Ack (manual flow)

```bash
amq list --new              # List new messages
amq list --cur              # List read messages
amq read --id <msg_id>      # Read specific message
amq ack --id <msg_id>       # Acknowledge message
```

### Thread view

```bash
amq thread --id p2p/claude__codex --limit 50 --include-body
```

### Watch for messages

```bash
# Block until message arrives or timeout
amq watch --timeout 60s

# Use polling fallback for network filesystems
amq watch --timeout 60s --poll
```

### Presence (optional)

```bash
amq presence set --status busy --note "reviewing PR"
amq presence list
```

### Cleanup (explicit only)

```bash
amq cleanup --tmp-older-than 36h
amq cleanup --tmp-older-than 24h --dry-run
```

## Workflow Summary

| Situation | Command | Behavior |
|-----------|---------|----------|
| Ingest messages | `amq drain --include-body` | One-shot: read+move+ack |
| Wait for messages | `amq monitor --timeout 0` | Blocks until message, then drains |
| Reply to message | `amq reply --id <id>` | Auto thread/refs handling |
| Quick peek only | `amq list --new` | Non-blocking, no side effects |

## Conventions and Safety

- Use the globally installed `amq` binary (must be in PATH).
- Do not edit message files in place; always use the CLI.
- Delivery is Maildir-style: tmp -> new -> cur. Readers must ignore tmp.
- Cleanup is explicit and never automatic.
- Thread naming for p2p: `p2p/<lowerA>__<lowerB>` (sorted lexicographically).
- Handles must be lowercase and match `[a-z0-9_-]+`.

See `COOP.md` for the full co-op mode specification.
