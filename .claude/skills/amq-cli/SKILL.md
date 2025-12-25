---
name: amq-cli
description: Use when coordinating agents via the AMQ CLI in this repo, including initializing mailboxes, sending/reading/acking messages, listing inboxes, viewing threads, setting presence, and cleaning stale tmp files. Covers Maildir tmp/new/cur semantics and safe, non-destructive usage conventions.
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

### Presence (optional)

```bash
./amq presence set --me codex --status busy --note "reviewing"
./amq presence list
```

### Cleanup (explicit only)

```bash
./amq cleanup --tmp-older-than 36h
```

## Conventions and safety

- Do not edit message files in place; always use the CLI.
- Delivery is Maildir-style: tmp -> new -> cur. Readers must ignore tmp.
- Cleanup is explicit and never automatic; do not delete files without the cleanup command.
- Thread naming for p2p: `p2p/<lowerA>__<lowerB>` (sorted lexicographically).
- Handles must be lowercase and match `[a-z0-9_-]+`.
