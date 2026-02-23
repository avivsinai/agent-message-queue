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
├── cli/           → Command handlers (send, list, read, ack, drain, thread, presence, cleanup, init, watch, monitor, reply, dlq, wake, coop, swarm, doctor)
├── fsq/           → File system queue (Maildir delivery, atomic ops, scanning)
├── format/        → Message serialization (JSON frontmatter + Markdown body)
├── config/        → Config management (meta/config.json)
├── ack/           → Acknowledgment tracking
├── swarm/         → Claude Code Agent Teams interop (team config, tasks, bridge, paths)
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

**Message Format**: JSON frontmatter followed by `---` and Markdown body:
- `schema` — Format version (currently 1)
- `id` — Unique message ID (timestamp + pid + random)
- `from` — Sender handle
- `to` — Recipient handles (array)
- `thread` — Thread ID (auto-generated for P2P: `p2p/<a>__<b>`)
- `subject` — Message subject
- `created` — RFC3339Nano timestamp
- `ack_required` — Whether sender expects acknowledgment
- `refs` — Related message IDs
- `priority` — `urgent`, `normal`, or `low`
- `kind` — Message type (see Message Kinds below)
- `labels` — Arbitrary tags for filtering
- `context` — JSON object with additional context (paths, focus, etc.)

**Thread Naming**: P2P threads use lexicographic ordering: `p2p/<lower_agent>__<higher_agent>`

**Environment Variables**: `AM_ROOT` (queue root, e.g., `.agent-mail`), `AM_ME` (agent handle), `AMQ_NO_UPDATE_CHECK` (disable update check)

**Deterministic Routing**: Message-routing commands (`send`, `list`, `read`, `reply`, `drain`, etc.) require `AM_ROOT` or `--root` to be set explicitly. The `.amqrc` fallback is only used by initialization commands (`env`, `init`, `coop init`, `coop exec`). This prevents silent misrouting when `.amqrc` changes between commands. Use `eval "$(amq env --me <agent>)"` or `amq coop exec` to pin `AM_ROOT` for your shell session.

**Session Layout**: The root directory (`.agent-mail/`) is the default queue root. Use `--session` to create isolated subdirectories:
```
.agent-mail/              ← default root (configured in .amqrc)
.agent-mail/auth/         ← isolated session (via --session auth)
.agent-mail/api/          ← isolated session (via --session api)
```

`AM_ROOT` points to the queue root. If `AM_ROOT` is set and `--root` conflicts, the command errors.

**Session Configuration**: The `amq env` command outputs shell commands to set environment variables. It reads configuration from (highest to lowest precedence):
- **Root**: flags > env (`AM_ROOT`) > `.amqrc` > auto-detect
- **Me**: flags > env (`AM_ME`)

Note: `.amqrc` configures the root directory. Agent identity (`me`) is set per-terminal via `--me` or `AM_ME`.

The `.amqrc` file is JSON:
```json
{"root": ".agent-mail"}
```

Usage:
```bash
eval "$(amq env --me claude --wake)"  # Set up for Claude
eval "$(amq env --me codex --wake)"   # Set up for Codex
amq env --shell fish                  # Fish shell syntax
amq env --json                        # Machine-readable output
```

## Message Kinds

| Kind | Reply Kind | Default Priority | Description |
|------|------------|------------------|-------------|
| `review_request` | `review_response` | normal | Request code review |
| `review_response` | — | normal | Code review feedback |
| `question` | `answer` | normal | Question needing answer |
| `answer` | — | normal | Response to a question |
| `decision` | — | normal | Decision request/announcement |
| `brainstorm` | — | low | Open-ended discussion |
| `status` | — | low | Status update/FYI |
| `todo` | — | normal | Task assignment |
| `spec_research` | `spec_research` | normal | Spec research findings |
| `spec_draft` | `spec_review` | normal | Spec draft submission |
| `spec_review` | — | normal | Spec review feedback |
| `spec_decision` | — | normal | Final spec decision |

When `--kind` is set but `--priority` is not, priority defaults to `normal`.

**Progress updates**: Use `status` kind to signal you've started working on a message (e.g., `amq reply --id <msg_id> --kind status --body "Started, eta ~20m"`). This helps when one agent is faster than another—the sender can check progress via `amq thread`.

## CLI Commands

```bash
amq init --root <path> --agents a,b,c [--force]
amq send --me <agent> --to <recipients> [--subject <str>] [--thread <id>] [--body <str|@file|stdin>] [--ack] [--priority <p>] [--kind <k>] [--labels <l>] [--context <json>]
amq list --me <agent> [--new | --cur] [--priority <p>] [--from <h>] [--kind <k>] [--label <l>...] [--limit N] [--offset N] [--json]
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
amq upgrade
amq env [--me <agent>] [--root <path>] [--shell sh|bash|zsh|fish] [--wake] [--json]
amq coop init [--root <path>] [--agents <a,b,c>] [--force] [--json]
amq coop exec [--root <path>] [--me <handle>] [--no-init] [--no-wake] [-y] <command> [-- <command-flags>]
amq coop spec start --topic <name> --partner <agent> [--body <text>] [--json]
amq coop spec status --topic <name> [--json]
amq coop spec submit --topic <name> --phase <research|draft|review|final> [--body <text|@file>] [--json]
amq coop spec present --topic <name> [--json]
amq swarm list [--json]
amq swarm join --team <name> --me <agent> [--agent-id <id>] [--type codex|external] [--json]
amq swarm leave --team <name> --agent-id <id> [--json]
amq swarm tasks --team <name> [--status pending|in_progress|completed] [--json]
amq swarm claim --team <name> --task <id> --me <agent> [--agent-id <id>] [--json]
amq swarm complete --team <name> --task <id> --me <agent> [--agent-id <id>] [--json]
amq swarm bridge --team <name> --me <agent> [--agent-id <id>] [--poll] [--poll-interval <duration>] [--root <path>] [--strict] [--json]
amq doctor [--json]
```

Common flags: `--root`, `--json`, `--strict` (error instead of warn on unknown handles or unreadable/corrupt config). Global option: `--no-update-check`. Note: `init` has its own flags and doesn't accept these.

Swarm-specific flags: `--team`, `--task`, `--agent-id`, `--type`, `--status`, `--poll`, `--poll-interval`.

Use `amq --version` to check the installed version.

### Message Filtering

The `list` command supports filtering messages:

```bash
# Filter by priority
amq list --me claude --new --priority urgent

# Filter by sender
amq list --me claude --new --from codex

# Filter by message kind
amq list --me claude --new --kind review_request

# Filter by label (can be repeated for multiple labels)
amq list --me claude --new --label bug --label critical

# Combine filters
amq list --me claude --new --from codex --priority urgent --kind question
```

### Labels and Context

**Labels** are arbitrary tags for categorizing messages:

```bash
amq send --to codex --labels "bug,urgent,parser" --body "Found critical bug"
```

**Context** provides structured metadata as JSON:

```bash
# Inline JSON
amq send --to codex --kind review_request \
  --context '{"paths": ["internal/cli/send.go"], "focus": "error handling"}' \
  --body "Please review error handling"

# From file
amq send --to codex --context @review-context.json --body "See context file"
```

Recommended context schema:
```json
{
  "paths": ["internal/cli/send.go", "internal/format/message.go"],
  "symbols": ["Header", "runSend"],
  "focus": "error handling in validation",
  "commands": ["go test ./internal/cli/..."],
  "hunks": [{"file": "send.go", "lines": "45-60"}]
}
```

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

**Preferred: Use `drain`** - One-shot ingestion that reads, moves to cur, and optionally acks (with `--ack`, which defaults to true). Silent when empty (hook-friendly).

**During active work**: Use `amq drain --include-body` to ingest messages between steps.

**Waiting for reply**: Use `amq watch --timeout 60s` which blocks until a message arrives, then `amq drain` to process.

| Situation | Command | Behavior |
|-----------|---------|----------|
| Ingest messages | `amq drain --include-body` | One-shot: read+move+ack |
| Waiting for reply | `amq watch --timeout 60s` | Blocks until message |
| Quick peek only | `amq list --new` | Non-blocking, no side effects |
| Filter messages | `amq list --new --priority urgent` | Show only urgent messages |
| Background wake | `amq wake --me <agent> &` | Injects notification via TIOCSTI (experimental) |
| Reply to message | `amq reply --id <msg_id>` | Auto thread/refs handling |

## Co-op Mode (Claude <-> Codex)

Co-op mode enables real-time collaboration between Claude Code and Codex CLI sessions. See `COOP.md` for full documentation.

**Initiator rule**: The initiator (agent or human) receives all updates and decisions. Always reply to the initiator and ask the initiator for clarifications. Do not ask a third party.

**Default pairing note**: Claude is often faster and more decisive, while Codex tends to be deeper but slower. That commonly makes Claude a natural coordinator and Codex a strong worker. This is a default, not a rule — roles are set per task by the initiator.

**Progress protocol**: Start with a `kind=status` + ETA, send heartbeats on phase boundaries or every 10-15 minutes, and finish with Summary / Changes / Tests / Notes.

**Modes of collaboration**: Leader+Worker (default), Co-workers, Duplicate (independent solutions), Driver+Navigator, Spec+Implementer, Reviewer+Implementer. See `COOP.md` for details.

### Quick Start

```bash
# Terminal 1 - Claude Code
amq coop exec claude -- --dangerously-skip-permissions  # Sets env, starts wake, execs into claude

# Terminal 2 - Codex CLI
amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox  # Same, with codex flags
```

Use `--session` for isolated sessions:
```bash
amq coop exec --session feature-a claude               # Isolated session
```

Use `--no-wake` to disable auto-wake (e.g., in CI or non-TTY environments).

**For scripts/CI** (non-interactive):
```bash
amq coop init && eval "$(amq env --me claude)"
```

### Message Priority Handling

When you see a notification, run `amq drain --include-body`:
- **urgent** → Interrupt current work, respond immediately (label `interrupt` enables wake Ctrl+C)
- **normal** → Add to TodoWrite, respond when current task done
- **low** → Batch for end of session

### Fallback: Notify Hook

If `amq wake` fails (TIOCSTI unavailable on hardened Linux), use notify hook:
```toml
# ~/.codex/config.toml
notify = ["python3", "/path/to/repo/scripts/codex-amq-notify.py"]
```

### Plan Mode Prompt Hook (Claude)

When Claude is in plan mode, shell tools are unavailable to the model. You can still
surface AMQ inbox context by using a `UserPromptSubmit` hook:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "AMQ_PROMPT_HOOK_MODE=plan AMQ_PROMPT_HOOK_ACTION=list python3 $CLAUDE_PROJECT_DIR/scripts/claude-amq-user-prompt-submit.py"
          }
        ]
      }
    ]
  }
}
```

Set `AMQ_PROMPT_HOOK_ACTION=drain` if you want the hook to auto-drain on prompt submit.

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

## Swarm Mode (Claude Code Agent Teams)

Swarm mode lets external agents (Codex, etc.) participate in Claude Code Agent Teams by reading/writing the team's local config and task list (under `~/.claude/teams/` and `~/.claude/tasks/`).
See `.claude/skills/amq-cli/SKILL.md` for the agent-facing workflow.

- **Task workflow**: `amq swarm join`, `amq swarm tasks`, `amq swarm claim`, `amq swarm complete`
- **Notifications**: `amq swarm bridge` watches the shared task list and delivers AMQ messages labeled `swarm` into the agent's inbox

**Wake compatibility**: bridge notifications are standard AMQ inbox messages, so `amq wake` will detect them automatically. If you want swarm notifications to trigger wake interrupts, configure wake to match the bridge label and priority:

```bash
amq wake --me codex --interrupt-label swarm --interrupt-priority normal &
```

**Direct messaging (A2A)**: the bridge only emits task lifecycle notifications.

- **Claude Code teammate → external agent**: works directly (Claude Code uses the AMQ skill to `amq send` to the external agent's inbox).
- **External agent → Claude Code teammate**: requires a relay. Claude Code teammates don't drain AMQ; they use internal team messaging. The team leader must `amq drain --include-body` and forward inbound messages to the intended teammate via Claude Code's internal SendMessage.

Recommended convention for external agents (send to the leader's AMQ handle, include the intended teammate):
```bash
amq send --me codex --to claude --thread swarm/my-team --labels swarm \
  --subject "To: builder - question about task t1" \
  --body "..."
```

Leader loop options: periodic `amq drain --include-body`, tighter `amq monitor --include-body` (watch+drain), or `amq wake --me claude &` for terminal notifications.

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
- Path traversal prevented via strict handle/ID validation

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
.claude/skills/amq-cli/        → Claude Code skill (SOURCE OF TRUTH)
├── SKILL.md
└── references/
    ├── coop-mode.md
    └── message-format.md
.codex/skills/amq-cli/         → Codex CLI skill (synced copy)
├── SKILL.md
└── references/
    ├── coop-mode.md
    └── message-format.md
skills/amq-cli/                → Standalone skill (synced copy, for direct install)
├── SKILL.md
└── references/
    ├── coop-mode.md
    └── message-format.md
```

### Why Three Copies?

The skill is distributed through multiple channels:
1. **Claude Code marketplace** — reads from `.claude/skills/`
2. **Codex CLI installer** — reads from `skills/` (standalone) or `.codex/skills/`
3. **Project-local development** — both `.claude/` and `.codex/` override user-installed skills

Each marketplace/installer requires the files to be present (symlinks not universally supported). The `make sync-skills` command keeps all copies identical.

### Dev vs Installed Skills

When working in this repo, **project-level skills take precedence** over user-level installed skills:

- `.claude/skills/amq-cli/` loads instead of `~/.claude/skills/amq-cli/`
- `.codex/skills/amq-cli/` loads instead of `~/.codex/skills/amq-cli/`

This lets you test skill changes locally before publishing.

### Editing Skills

1. Edit files in `.claude/skills/amq-cli/` (source of truth)
2. If publishing: bump `version:` in `.claude/skills/amq-cli/SKILL.md`
3. Sync to other locations: `make sync-skills`
4. Test locally by running Claude Code or Codex in this repo
5. Commit and push (all three locations will be committed)

**Important**: Never edit `.codex/skills/` or `skills/` directly. Always edit `.claude/skills/` and sync.

### Installing Skills

See README.md for installation instructions for Claude Code and Codex CLI.
