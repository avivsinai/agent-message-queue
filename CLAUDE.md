# CLAUDE.md

This is the master agent instruction file for this repository. Both Claude Code and Codex should follow these guidelines. `AGENTS.md` exists only as a Codex compatibility shim and should contain only Codex-specific notes.

## Project Overview

Agent Message Queue (AMQ) is a lightweight, file-based message delivery system for local inter-agent communication. It uses Maildir-style atomic delivery (tmp→new→cur) for crash-safe messaging between coding agents on the same machine. No daemon, database, or server required. Conceptually, it is a local interoperability bus for agent sessions: AMQ manages the conversation, while orchestrators such as Claude Code teams, Codex workflows, Kanban, and Symphony keep owning the task.

AMQ owns agent-to-agent messaging, thread continuity, cross-project/session routing, handoff state, and `doctor --ops`. It does not own task decomposition, worktree management, dependency scheduling, PR landing, or cron/scheduler execution.

## Release Contract

- Release from `main` only through `./scripts/release.sh X.Y.Z` and the resulting release PR; do not create manual tags or GitHub releases.
- A push to `main` updates the AvivSinai marketplace immediately for `amq-cli` and `amq-spec`.
- Keep one version across `CHANGELOG.md`, skill/plugin metadata, and the release commit; after the release PR merges, CI validates the merged commit, creates the matching tag, publishes GitHub/Homebrew artifacts from that exact SHA, and uses the committed changelog entry as the GitHub release notes.

## Operational Constraints

- Handles must be lowercase and match `[a-z0-9_-]+`.
- Never edit queue files directly; use the CLI for all mailbox operations.
- Cleanup is explicit via `amq cleanup`; do not add automatic deletion behavior.

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
├── cli/           → Command handlers (send, list, read, drain, thread, presence, cleanup, init, watch, monitor, reply, dlq, wake, coop, swarm, integration, receipts, doctor)
├── fsq/           → File system queue (Maildir delivery, atomic ops, scanning)
├── format/        → Message serialization (JSON frontmatter + Markdown body)
├── config/        → Config management (meta/config.json)
├── receipt/       → Delivery receipt ledger (`drained`, `dlq`)
├── integration/   → Shared integration helpers plus Symphony and Kanban adapters
├── swarm/         → Claude Code Agent Teams interop (team config, tasks, bridge, paths)
├── thread/        → Thread collection across mailboxes
└── presence/      → Agent presence metadata
```

**Mailbox Layout**:
```
<root>/agents/<agent>/inbox/{tmp,new,cur}/  → Incoming messages
<root>/agents/<agent>/outbox/sent/          → Sent copies
<root>/agents/<agent>/receipts/             → Consumer-local delivery receipts
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
- `refs` — Related message IDs
- `priority` — `urgent`, `normal`, or `low`
- `kind` — Message type (see Message Kinds below)
- `labels` — Arbitrary tags for filtering
- `context` — JSON object with additional context (paths, focus, etc.)
- `reply_to` — Cross-session/cross-project reply routing hint
- `reply_project` — Sender project for cross-project replies
- `from_project` — Sender project for cross-project identity

**Thread Naming**: P2P threads use lexicographic ordering: `p2p/<lower_agent>__<higher_agent>`

**Receipt Ledger**: Delivery outcomes are recorded as consumer-local receipts under `agents/<consumer>/receipts/`. Stages are `drained` and `dlq`. Use `amq receipts list`, `amq receipts wait`, or `amq send --wait-for <stage>` to query or block on them.

**Extension Metadata Namespaces**: Higher-level layers may store their own metadata under reserved extension directories:
```
<AM_ROOT>/extensions/<layer>/
<AM_ROOT>/agents/<handle>/extensions/<layer>/
```
Layer names must use lowercase ASCII letters, digits, hyphen, underscore, and dot (for example `io.github.omriariav.amq-squad`). AMQ will not create files inside layer-owned directories, and `amq cleanup` does not remove extension directories unless a future command explicitly targets extension metadata. Layers may publish a passive root manifest at `<AM_ROOT>/extensions/<layer>/manifest.json`; `amq doctor --json` may report it, but AMQ must not execute extension code or invoke hooks from manifests.

**Environment Variables**: `AM_ROOT` (queue root, e.g., `.agent-mail/collab`), `AM_ME` (agent handle), `AM_BASE_ROOT` (base root set by `coop exec` for cross-session resolution; only trusted when the current root still lives under it), `AMQ_GLOBAL_ROOT` (global root fallback for orchestrator-spawned agents), `AMQ_NO_UPDATE_CHECK` (disable update check)

**Session Layout**: The default base root directory is `.agent-mail/`. `.amqrc` can configure that root explicitly, but the default `.agent-mail/<session>` layout is also recognized without `.amqrc`. `coop exec` defaults to `--session collab`, so agents get session isolation without explicit flags. Use `--session` to override:
```
.agent-mail/              ← default base root (configurable in `.amqrc`)
.agent-mail/collab/       ← default session (coop exec without --session or --root)
.agent-mail/auth/         ← isolated session (via --session auth)
.agent-mail/api/          ← isolated session (via --session api)
```

`AM_ROOT` points to the queue root. When `--root` is explicitly provided, it takes precedence over `AM_ROOT`. `send` and `reply` emit a `note:` on stderr when an explicit `--root` overrides `AM_ROOT`.

**Session Configuration**: The `amq env` command outputs shell commands to set environment variables. It reads configuration from (highest to lowest precedence):
- **Root**: flags > env (`AM_ROOT`) > project `.amqrc` > `AMQ_GLOBAL_ROOT` > `~/.amqrc` > auto-detect
- **Me**: flags > env (`AM_ME`)

Note: `.amqrc` configures the root directory. Agent identity (`me`) is set per-terminal via `--me` or `AM_ME`.
Auto-detect covers the default `.agent-mail` layout in the current tree; `.amqrc` is still required for custom root names and peer configuration.

The `.amqrc` file is JSON:
```json
{"root": ".agent-mail"}
```

For cross-project federation, `.amqrc` can also include `project` and `peers`:
```json
{
  "root": ".agent-mail",
  "project": "app",
  "peers": {
    "infra-lib": "/Users/me/src/infra-lib/.agent-mail"
  }
}
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

When `--kind` is set but `--priority` is not, priority defaults to `normal`.

**Progress updates**: Use `status` kind to signal you've started working on a message (e.g., `amq reply --id <msg_id> --kind status --body "Started, eta ~20m"`). This helps when one agent is faster than another—the sender can check progress via `amq thread`.

## CLI Commands

```bash
amq init --root <path> --agents a,b,c [--force]
amq send --me <agent> --to <recipients> [--subject <str>] [--thread <id>] [--body <str|@file|stdin>] [--priority <p>] [--kind <k>] [--labels <l>] [--context <json>] [--wait-for <stage>] [--wait-timeout <duration>]
amq list --me <agent> [--new | --cur] [--priority <p>] [--from <h>] [--kind <k>] [--label <l>...] [--limit N] [--offset N] [--json]
amq read --me <agent> --id <msg_id> [--json]
amq drain --me <agent> [--limit N] [--include-body] [--json]
amq thread --id <thread_id> [--limit N] [--include-body] [--json]
amq receipts list --me <agent> [--msg-id <id>] [--stage <stage>] [--json]
amq receipts wait --me <agent> --msg-id <id> [--stage <stage>] [--timeout <duration>] [--poll-interval <duration>] [--json]
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
amq wake --me <agent> [--inject-cmd <cmd>] [--bell] [--debounce <duration>] [--preview-len <n>] [--defer-while-input] [--input-quiet-for <duration>] [--input-poll-interval <duration>] [--input-max-hold <duration>]
amq upgrade
amq env [--me <agent>] [--root <path>] [--session <name>] [--shell sh|bash|zsh|fish] [--wake] [--json]
amq shell-setup [--shell bash|zsh|fish] [--claude-alias <name>] [--codex-alias <name>]
amq coop init [--root <path>] [--agents <a,b,c>] [--force] [--json]
amq coop exec [--root <path>] [--session <name>] [--me <handle>] [--no-init] [--no-wake] [-y] <command> [-- <command-flags>]
amq swarm list [--json]
amq swarm join --team <name> --me <agent> [--agent-id <id>] [--type codex|external] [--json]
amq swarm leave --team <name> --agent-id <id> [--json]
amq swarm tasks --team <name> [--status pending|in_progress|completed|failed|blocked] [--json]
amq swarm claim --team <name> --task <id> --me <agent> [--agent-id <id>] [--json]
amq swarm complete --team <name> --task <id> --me <agent> [--agent-id <id>] [--evidence <json|@file>] [--json]
amq swarm fail --team <name> --task <id> --me <agent> [--agent-id <id>] [--reason <str>] [--json]
amq swarm block --team <name> --task <id> --me <agent> [--agent-id <id>] [--reason <str>] [--json]
amq swarm bridge --team <name> --me <agent> [--agent-id <id>] [--poll] [--poll-interval <duration>] [--root <path>] [--strict] [--json]
amq integration symphony init [--workflow <path>] --me <agent> [--root <path>] [--check] [--force] [--json]
amq integration symphony emit --event <after_create|before_run|after_run|before_remove> --me <agent> [--root <path>] [--workspace <path>] [--identifier <key>] [--json]
amq integration kanban bridge --me <agent> [--root <path>] [--url <ws://...>] [--workspace-id <id>] [--reconnect <duration>] [--json]
amq who [--json]
amq doctor [--ops] [--json]
```

Common flags: `--root`, `--json`, `--strict` (error instead of warn on unknown handles or unreadable/corrupt config). Global option: `--no-update-check`. Note: `init` has its own flags and doesn't accept these.

Swarm-specific flags: `--team`, `--task`, `--agent-id`, `--type`, `--status`, `--poll`, `--poll-interval`, `--reason`, `--evidence`.

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

### Orchestrator Integration Metadata

All integration metadata lives under `context.orchestrator`.

The formal contract lives in [docs/adapter-contract.md](docs/adapter-contract.md). The short version is:

- AMQ transports messages
- Adapters convert external lifecycle or task events into AMQ messages
- Symphony is a lightweight hook adapter
- Kanban is experimental and depends on a preview WebSocket surface

Canonical fields:

```json
{
  "orchestrator": {
    "version": 1,
    "name": "symphony",
    "transport": "hook",
    "event": "before_run",
    "workspace": {
      "path": "/abs/path/to/workspace",
      "key": "workspace-name"
    },
    "task": {
      "id": "workspace-name",
      "state": "running"
    }
  }
}
```

Kanban uses the same top-level object with `name: "kanban"` and `transport: "bridge"`, but its task payload may additionally include `prompt`, `column`, `review_reason`, and `agent_id`. Kanban workspace metadata may include `id`/`path`; Symphony uses `path`/`key`.

Standard label conventions for integration messages:

- Always: `orchestrator`, `orchestrator:<name>`
- When task state is known: `task-state:<state>`
- Handoff / review-ready events: add `handoff`
- Failed / interrupted / blocked events: add `blocking`

These labels are intentionally generic so `amq list --label orchestrator --label handoff` works across integrations.

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

`amq read`, `amq drain`, and `amq monitor` now share the same strict header validation. If a message in `inbox/new` is corrupt or has malformed headers, the command moves it to DLQ and emits a `dlq` receipt instead of leaving it in place.

## Doctor / Ops

`amq doctor` verifies installation, root configuration, permissions, config, and skill setup.

`amq doctor --ops` adds runtime checks:

- Queue depth and oldest unread per agent
- DLQ count and oldest age
- Presence freshness
- Integration hints for Kanban and Symphony

Use `amq doctor --ops --json` for machine-readable health output.

## Multi-Agent Coordination

Commands below assume `AM_ME` is set (e.g., `export AM_ME=claude`).

**Preferred: Use `drain`** - One-shot ingestion that reads, moves to cur, and emits `drained` or `dlq` receipts. Silent when empty (hook-friendly).

**During active work**: Use `amq drain --include-body` to ingest messages between steps.

**Waiting for reply**: Use `amq watch --timeout 60s` which blocks until a message arrives, then `amq drain` to process.

| Situation | Command | Behavior |
|-----------|---------|----------|
| Ingest messages | `amq drain --include-body` | One-shot: read+move+receipt |
| Wait for delivery | `amq send --to codex --wait-for drained --wait-timeout 60s ...` | Send then block for `drained`/`dlq` |
| Inspect delivery | `amq receipts list --me codex --msg-id <msg_id>` | Show receipt history for one message |
| Waiting for reply | `amq watch --timeout 60s` | Blocks until message |
| Quick peek only | `amq list --new` | Non-blocking, no side effects |
| Filter messages | `amq list --new --priority urgent` | Show only urgent messages |
| Background wake | `amq wake --me <agent> &` | Injects notification via TIOCSTI with best-effort input deferral (experimental) |
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
amq coop exec claude -- --dangerously-skip-permissions  # session=collab by default

# Terminal 2 - Codex CLI
amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox  # Same, with codex flags
```

Without `--session` or `--root`, `coop exec` defaults to `--session collab`.

Use `--session` for a different isolated session:
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

- **Task workflow**: `amq swarm join`, `amq swarm tasks`, `amq swarm claim`, `amq swarm complete`, `amq swarm fail`, `amq swarm block`
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

- Use descriptive commit messages (e.g., `fix: handle corrupt receipt files gracefully`)
- Run `make ci` before committing
- Do not edit message files in place; always use the CLI
- Cleanup is explicit (`amq cleanup`), never automatic

## Documentation Policy

- Keep `docs/` evergreen. It should contain durable reference material such as architecture notes, protocol contracts, and operational guidance.
- Do not commit frozen specs, design drafts, or implementation plans into `docs/`.
- Do not merge spec documents to `main`, and do not keep them in feature branches intended for PRs either.
- Spec work belongs in the AMQ spec workflow, PR descriptions, issues, or other ephemeral collaboration artifacts rather than committed repository docs.

## Skill Development

This repo includes skills for Claude Code and Codex CLI, distributed via the [skills-marketplace](https://github.com/avivsinai/skills-marketplace).

### Structure

```
.claude-plugin/plugin.json     → Plugin manifest for marketplace
skills/amq-cli/                → Canonical skill contents
├── SKILL.md
└── references/
    ├── coop-mode.md
    ├── integrations.md
    ├── message-format.md
    └── swarm-mode.md
skills/amq-spec/               → Canonical spec skill contents
.claude/skills/amq-cli/        → Symlink to ../../skills/amq-cli
.claude/skills/amq-spec/       → Symlink to ../../skills/amq-spec
.agents/skills/amq-cli/        → Symlink to ../../skills/amq-cli
.agents/skills/amq-spec/       → Symlink to ../../skills/amq-spec
```

### Current Skill Layout

`skills/` holds the canonical contents in this repo. The project-local `.claude/skills/` and `.agents/skills/` entries are symlinks to those same directories so local agent runs pick up in-repo changes immediately.

### Dev vs Installed Skills

When working in this repo, **project-level skills take precedence** over user-level installed skills:

- `.claude/skills/amq-cli/` loads instead of `~/.claude/skills/amq-cli/`
- `.claude/skills/amq-spec/` loads instead of `~/.claude/skills/amq-spec/`
- `.agents/skills/amq-cli/` loads the same in-repo contents via symlink
- `.agents/skills/amq-spec/` loads the same in-repo contents via symlink

This lets you test skill changes locally before publishing.

### Editing Skills

1. Edit files in `skills/<skill-name>/` (or the equivalent `.claude/skills/<skill-name>/` symlink)
2. If publishing: bump `version:` in the skill's `SKILL.md`
3. Verify the symlinked views still match: `make check-skills`
4. Test locally by running Claude Code or Codex in this repo
5. Commit and push the canonical `skills/` changes

**Important**: Do not create divergent copies. The symlinked paths should always reflect the same `skills/` content.

### Installing Skills

See README.md for installation instructions for Claude Code and Codex CLI.
