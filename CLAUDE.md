# CLAUDE.md

This is the **master agent instruction file** for this repository. Both Claude Code and Codex should follow these guidelines. See also `AGENTS.md` which references this file.

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
├── cli/           → Command handlers (send, list, read, ack, drain, thread, presence, cleanup, init, watch, monitor, reply, dlq, wake)
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
<root>/agents/<agent>/dlq/{tmp,new,cur}/    → Dead letter queue
```

## Core Concepts

**Atomic Delivery**: Messages written to `tmp/`, fsynced, then atomically renamed to `new/`. Readers only scan `new/` and `cur/`, never seeing incomplete writes.

**Message Format**: JSON frontmatter (schema, id, from, to, thread, subject, created, ack_required, refs, priority, kind, labels, context) followed by `---` and Markdown body.

**Thread Naming**: P2P threads use lexicographic ordering: `p2p/<lower_agent>__<higher_agent>`

**Environment Variables**: `AM_ROOT` (default root dir), `AM_ME` (default agent handle)

## CLI Commands

```bash
amq init --root <path> --agents a,b,c [--force]
amq send --me <agent> --to <recipients> [--subject <str>] [--thread <id>] [--body <str|@file|stdin>] [--ack] [--priority <p>] [--kind <k>] [--labels <l>] [--context <json>]
amq list --me <agent> [--new | --cur] [--json]
amq read --me <agent> --id <msg_id> [--json]
amq ack --me <agent> --id <msg_id>
amq drain --me <agent> [--limit N] [--include-body] [--ack] [--json]
amq thread --id <thread_id> [--limit N] [--include-body] [--json]
amq presence set --me <agent> --status <busy|idle|...> [--note <str>]
amq presence list [--json]
amq cleanup --tmp-older-than <duration> [--dry-run] [--yes]
amq watch --me <agent> [--timeout <duration>] [--poll] [--json]
amq monitor --me <agent> [--timeout <duration>] [--poll] [--include-body] [--peek] [--json]
amq reply --me <agent> --id <msg_id> [--body <str|@file|stdin>] [--priority <p>] [--kind <k>]
amq dlq list --me <agent> [--new | --cur] [--json]
amq dlq read --me <agent> --id <dlq_id> [--json]
amq dlq retry --me <agent> --id <dlq_id> [--all] [--force]
amq dlq purge --me <agent> [--older-than <duration>] [--dry-run] [--yes]
amq wake --me <agent> [--inject-cmd <cmd>] [--bell] [--debounce <duration>] [--preview-len <n>]
```

Common flags: `--root`, `--json`, `--strict` (error instead of warn on unknown handles or unreadable/corrupt config). Note: `init` has its own flags and doesn't accept these.

Use `amq --version` to check the installed version.

## Dead Letter Queue (DLQ)

Messages that fail to parse during `drain` or `monitor` are automatically moved to the Dead Letter Queue instead of `cur/`. This prevents corrupt messages from blocking processing while preserving them for inspection.

**DLQ Layout**:
```
<root>/agents/<agent>/dlq/{tmp,new,cur}/
```

**DLQ Envelope**: Wraps original message with failure metadata:
- `failure_reason`: `parse_error`
- `failure_detail`: Specific error message
- `retry_count`: Number of retry attempts (max 3 before permanent DLQ)

**Commands**:
- `amq dlq list` - List dead-lettered messages
- `amq dlq read --id <dlq_id>` - Inspect a DLQ message with failure info
- `amq dlq retry --id <dlq_id>` - Move message back to inbox for reprocessing
- `amq dlq retry --all` - Retry all DLQ messages
- `amq dlq purge` - Permanently remove DLQ messages

Use `--force` with retry to override the max retry limit.

## Multi-Agent Coordination

Commands below assume `AM_ME` is set (e.g., `export AM_ME=claude`).

**Preferred: Use `drain`** - One-shot ingestion that reads, moves to cur, and acks in one atomic operation. Silent when empty (hook-friendly).

**During active work**: Use `amq drain --include-body` to ingest messages between steps.

**Waiting for reply**: Use `amq watch --timeout 60s` which blocks until a message arrives, then `amq drain` to process.

| Situation | Command | Behavior |
|-----------|---------|----------|
| Ingest messages | `amq drain --include-body` | One-shot: read+move+ack |
| Waiting for reply | `amq watch --timeout 60s` | Blocks until message |
| Quick peek only | `amq list --new` | Non-blocking, no side effects |
| Background wake | `amq wake &` | Injects notification via TIOCSTI (experimental) |
| Reply to message | `amq reply --id <msg_id>` | Auto thread/refs handling |

## Co-op Mode (Claude <-> Codex)

Co-op mode enables real-time collaboration between Claude Code and Codex CLI sessions. See `COOP.md` for full documentation.

### Quick Start

```bash
# Terminal 1 - Claude Code
export AM_ME=claude AM_ROOT=.agent-mail
amq wake &
claude

# Terminal 2 - Codex CLI
export AM_ME=codex AM_ROOT=.agent-mail
amq wake &
codex
```

`amq wake` runs as a background job and attempts to inject notifications into your terminal when messages arrive (uses TIOCSTI, experimental).

### Message Priority Handling

When you see a notification, run `amq drain --include-body`:
- **urgent** → Interrupt current work, respond immediately
- **normal** → Add to TodoWrite, respond when current task done
- **low** → Batch for end of session

### Fallback: Codex Notify Hook

If `amq wake` doesn't work, configure the notify hook:
```toml
# ~/.codex/config.toml
notify = ["python3", "/path/to/repo/scripts/codex-amq-notify.py"]
```

### Co-op Commands

```bash
# Send a review request
amq send --me claude --to codex --subject "Review needed" \
  --kind review_request --priority normal \
  --body "Please review internal/cli/send.go..."

# Reply to a message (auto thread/refs)
amq reply --me codex --id "msg_123" --kind review_response \
  --body "LGTM with minor comments..."
```

## Testing

Run individual test: `go test ./internal/fsq -run TestMaildir`

Key test files:
- `internal/fsq/maildir_test.go` - Atomic delivery semantics
- `internal/fsq/dlq_test.go` - Dead letter queue operations
- `internal/format/message_test.go` - Message serialization
- `internal/thread/thread_test.go` - Thread collection
- `internal/cli/watch_test.go` - Watch command with fsnotify

## Security

- Directories use 0700 permissions (owner-only access)
- Files use 0600 permissions (owner read/write only)
- Unknown handles trigger a warning by default; use `--strict` to error instead
- With `--strict`, unreadable or corrupt `config.json` also causes an error
- Handles must be lowercase: `[a-z0-9_-]+`

## Contributing

Install git hooks to enforce checks before push:

```bash
./scripts/install-hooks.sh
```

The pre-push hook runs `make ci` (vet, lint, test, smoke) before allowing pushes.

## Commit Conventions

- Use descriptive commit messages (e.g., `fix: handle corrupt ack files gracefully`)
- Run `make ci` before committing
- Do not edit message files in place; always use the CLI
- Cleanup is explicit (`amq cleanup`), never automatic

## Skill Development

This repo includes skills for Claude Code and Codex CLI, distributed via the [skills-marketplace](https://github.com/avivsinai/skills-marketplace).

### Structure

```
.claude-plugin/plugin.json     → Plugin manifest for marketplace
.claude/skills/amq-cli/        → Claude Code skill (source of truth)
├── SKILL.md
└── plugin.json
.codex/skills/amq-cli/         → Codex CLI skill (synced copy)
├── SKILL.md
└── plugin.json
```

### Dev vs Installed Skills

When working in this repo, **project-level skills take precedence** over user-level installed skills:

- `.claude/skills/amq-cli/` loads instead of `~/.claude/skills/amq-cli/`
- `.codex/skills/amq-cli/` loads instead of `~/.codex/skills/amq-cli/`

This lets you test skill changes locally before publishing.

### Editing Skills

1. Edit files in `.claude/skills/amq-cli/` (source of truth)
2. Sync to Codex: `make sync-skills`
3. Test locally by running Claude Code or Codex in this repo
4. Bump version in `.claude-plugin/plugin.json` and `.claude/skills/amq-cli/plugin.json`
5. Run `make sync-skills` again to update Codex copies
6. Commit and push

### Installing Skills

See README.md for installation instructions for Claude Code and Codex CLI.
