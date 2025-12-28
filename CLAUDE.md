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
├── cli/           → Command handlers (send, list, read, ack, drain, thread, presence, cleanup, init, watch, monitor, reply)
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
amq monitor --me <agent> [--timeout <duration>] [--poll] [--include-body] [--json]
amq reply --me <agent> --id <msg_id> [--body <str|@file|stdin>] [--priority <p>] [--kind <k>]
```

Common flags: `--root`, `--json`, `--strict` (error instead of warn on unknown handles or unreadable/corrupt config). Note: `init` has its own flags and doesn't accept these.

Use `amq --version` to check the installed version.

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
| Co-op background watch | `amq monitor --json` | Watch + drain combined (one-shot; loop for continuous watch) |
| Reply to message | `amq reply --id <msg_id>` | Auto thread/refs handling |

## Co-op Mode (Claude <-> Codex)

Co-op mode enables real-time collaboration between Claude Code and Codex CLI sessions. See `COOP.md` for full documentation.

### Quick Start

On session start:
1. Set `AM_ME=claude` (or `codex`), `AM_ROOT=.agent-mail`
2. Claude Code: Spawn a background watcher (Task tool with haiku model): "Run amq monitor --timeout 0 --include-body --json and report messages by priority"
3. Codex CLI: Run a background loop (monitor is one-shot): `while true; do amq monitor --timeout 0 --include-body --json; sleep 0.2; done`

### Message Priority Handling

When the watcher returns with messages:
- **urgent** → Interrupt current work, respond immediately
- **normal** → Add to TodoWrite, respond when current task done
- **low** → Batch for end of session

Respawn the Claude Code watcher after each batch. Re-launch after 10-min timeout. For Codex, keep the background loop running.

### References

- [Claude Code Best Practices](https://www.anthropic.com/engineering/claude-code-best-practices) - Headless mode, multi-agent workflows, extended thinking (`ultrathink`)
- [Claude Code Hooks](https://code.claude.com/docs/en/hooks) - Stop hooks, prompt-based hooks, intelligent automation
- [Codex CLI Features](https://developers.openai.com/codex/cli/features/) - Approval modes, `full-auto`, background terminals
- [Ralph Plugin](https://github.com/anthropics/claude-plugins-official/tree/main/plugins/ralph-wiggum) - Self-referential loops, completion promises

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
