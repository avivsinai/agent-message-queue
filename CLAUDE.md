# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Agent Message Queue (AMQ) is a lightweight, file-based message delivery system for local inter-agent communication. It uses Maildir-style atomic delivery (tmp→new→cur) for crash-safe messaging between coding agents on the same machine. No daemon, database, or server required.

## Build & Development Commands

```bash
make build          # Compile: go build -o amq ./cmd/amq
make test           # Run tests: go test ./...
make fmt            # Format code: gofmt -w
make vet            # Run go vet
make lint           # Run golangci-lint
make ci             # Full CI: fmt-check → vet → lint → test
```

Requires Go 1.25+ and optionally golangci-lint.

## Architecture

```
cmd/amq/           → Entry point (delegates to cli.Run())
internal/
├── cli/           → Command handlers (send, list, read, ack, thread, presence, cleanup, init, watch)
├── fsq/           → File system queue (Maildir delivery, atomic ops, scanning)
├── format/        → Message serialization (JSON frontmatter + Markdown body)
├── config/        → Config management (meta/config.json)
├── ack/           → Acknowledgment tracking
├── thread/        → Thread collection across mailboxes
└── presence/      → Agent presence metadata
```

**Mailbox Layout**:
```
<root>/agents/<agent>/inbox/{tmp,new,cur}/  → Incoming messages
<root>/agents/<agent>/outbox/sent/          → Sent copies
<root>/agents/<agent>/acks/{received,sent}/ → Acknowledgments
```

## Core Concepts

**Atomic Delivery**: Messages written to `tmp/`, fsynced, then atomically renamed to `new/`. Readers only scan `new/` and `cur/`, never seeing incomplete writes.

**Message Format**: JSON frontmatter (schema, id, from, to, thread, subject, created, ack_required, refs) followed by `---` and Markdown body.

**Thread Naming**: P2P threads use lexicographic ordering: `p2p/<lower_agent>__<higher_agent>`

**Environment Variables**: `AM_ROOT` (default root dir), `AM_ME` (default agent handle)

## CLI Commands

```bash
amq init --root <path> --agents a,b,c [--force]
amq send --me <agent> --to <recipients> [--subject <str>] [--thread <id>] [--body <str|@file|stdin>] [--ack]
amq list --me <agent> [--new | --cur] [--json]
amq read --me <agent> --id <msg_id> [--json]
amq ack --me <agent> --id <msg_id>
amq thread --id <thread_id> [--limit N] [--include-body] [--json]
amq presence set --me <agent> --status <busy|idle|...> [--note <str>]
amq presence list [--json]
amq cleanup --tmp-older-than <duration> [--dry-run] [--yes]
amq watch --me <agent> [--timeout <duration>] [--poll] [--json]
```

## Multi-Agent Coordination

Commands below assume `AM_ME` is set (e.g., `export AM_ME=claude`).

**During active work**: Check inbox between steps with `amq list --new` (non-blocking).

**Waiting for reply**: Use `amq watch --timeout 60s` which blocks until a message arrives.

| Situation | Command | Behavior |
|-----------|---------|----------|
| Working, quick check | `amq list --new` | Non-blocking |
| Waiting for reply | `amq watch --timeout 60s` | Blocks until message |

## Testing

Run individual test: `go test ./internal/fsq -run TestMaildir`

Key test files:
- `internal/fsq/maildir_test.go` - Atomic delivery semantics
- `internal/format/message_test.go` - Message serialization
- `internal/thread/thread_test.go` - Thread collection
- `internal/cli/watch_test.go` - Watch command with fsnotify
