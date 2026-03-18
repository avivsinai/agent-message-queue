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
├── cli/           → Command handlers (send, list, read, ack, drain, thread, presence, cleanup, init, watch, monitor, reply, dlq, wake, coop, swarm, discover, who, resolve, channel, announce, doctor)
├── fsq/           → File system queue (Maildir delivery, atomic ops, scanning)
├── format/        → Message serialization (JSON frontmatter + Markdown body)
├── config/        → Config management (meta/config.json)
├── ack/           → Acknowledgment tracking
├── swarm/         → Claude Code Agent Teams interop (team config, tasks, bridge, paths)
├── resolve/       → Qualified address parsing and resolution (federation)
├── discover/      → Cross-project discovery, scanning, and caching
├── metadata/      → Session and agent advisory metadata (session.json, agent.json)
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
- `schema` — Format version (currently 2; schema 1 is still supported for reading)
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
- `origin` — (schema 2) Source location for federated messages (see Federation below)
- `delivery` — (schema 2) Routing metadata for federated messages (see Federation below)

**Thread Naming**: P2P threads use lexicographic ordering: `p2p/<lower_agent>__<higher_agent>`

**Environment Variables**: `AM_ROOT` (queue root, e.g., `.agent-mail/collab`), `AM_ME` (agent handle), `AM_PROJECT` (project slug for federation), `AM_SESSION` (session name for federation), `AM_BASE_ROOT` (base root before session suffix), `AMQ_NO_UPDATE_CHECK` (disable update check)

**Session Layout**: The base root directory (`.agent-mail/`) is configured in `.amqrc`. `coop exec` defaults to `--session collab`, so agents get session isolation without explicit flags. Use `--session` to override:
```
.agent-mail/              ← base root (configured in .amqrc)
.agent-mail/collab/       ← default session (coop exec without --session or --root)
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

For federation, `.amqrc` supports optional fields:
```json
{"root": ".agent-mail", "project": "my-api", "project_id": "abc123"}
```
- `project` — Human-readable project slug (defaults to directory basename). Used for cross-project addressing.
- `project_id` — Stable unique identifier. Auto-generated by `coop exec` if missing. Used to disambiguate projects with the same directory name.

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
amq env [--me <agent>] [--root <path>] [--session <name>] [--shell sh|bash|zsh|fish] [--wake] [--json]
amq shell-setup [--shell bash|zsh|fish] [--claude-alias <name>] [--codex-alias <name>]
amq coop init [--root <path>] [--agents <a,b,c>] [--force] [--json]
amq coop exec [--root <path>] [--session <name>] [--me <handle>] [--no-init] [--no-wake] [--topic <str>] [--claim <csv>] [--channel <csv>] [-y] <command> [-- <command-flags>]
amq swarm list [--json]
amq swarm join --team <name> --me <agent> [--agent-id <id>] [--type codex|external] [--json]
amq swarm leave --team <name> --agent-id <id> [--json]
amq swarm tasks --team <name> [--status pending|in_progress|completed|failed|blocked] [--json]
amq swarm claim --team <name> --task <id> --me <agent> [--agent-id <id>] [--json]
amq swarm complete --team <name> --task <id> --me <agent> [--agent-id <id>] [--evidence <json|@file>] [--json]
amq swarm fail --team <name> --task <id> --me <agent> [--agent-id <id>] [--reason <str>] [--json]
amq swarm block --team <name> --task <id> --me <agent> [--agent-id <id>] [--reason <str>] [--json]
amq swarm bridge --team <name> --me <agent> [--agent-id <id>] [--poll] [--poll-interval <duration>] [--root <path>] [--strict] [--json]
amq discover [--refresh] [--json]
amq who [--json]
amq resolve <address> [--json]
amq channel <join|leave|list> --name <channel> [--json]
amq announce --channel <name> [--body <str>] [--subject <str>] [--kind <k>] [--priority <p>] [--labels <csv>] [--json]
amq doctor [--json]
```

Common flags: `--root`, `--json`, `--strict` (error instead of warn on unknown handles or unreadable/corrupt config). Global option: `--no-update-check`. Note: `init` has its own flags and doesn't accept these.

Swarm-specific flags: `--team`, `--task`, `--agent-id`, `--type`, `--status`, `--poll`, `--poll-interval`, `--reason`, `--evidence`.

Federation-specific flags (coop exec): `--topic` (session topic), `--claim` (comma-separated claims), `--channel` (comma-separated channel memberships).

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

## Federation (Cross-Session & Cross-Project)

Federation enables agents to send messages across session boundaries (within the same project) and across project boundaries (between sibling projects on the same machine).

### Qualified Addressing

The `--to` flag in `amq send` (and the `amq resolve` command) accepts qualified addresses:

| Address Form | Meaning |
|---|---|
| `codex` | Local agent in current session |
| `codex@auth` | Agent in session "auth" (same project, local-first resolution) |
| `claude@infra-lib:auth` | Agent in project "infra-lib", session "auth" |
| `claude@session/auth` | Explicit: agent in session "auth" |
| `claude@project/infra-lib` | Explicit: agent in project "infra-lib" |
| `claude@project/infra-lib/session/auth` | Explicit: project + session |
| `#events` | Channel "events" in current project (fan-out to subscribed agents) |
| `#all@infra-lib` | Channel "all" in project "infra-lib" |
| `#session/auth` | All agents in session "auth" |

When `agent@name` is ambiguous (could be a session or a project), the resolver checks local sessions first, then the project discovery cache. If both match, the command errors with disambiguation instructions. Use the explicit long forms (`@session/X`, `@project/X`, or `@project:session`) to avoid ambiguity.

### Federation Examples

```bash
# Cross-session: send from collab to auth session
amq send --to codex@auth --body "Auth module ready for review"

# Cross-project: send to agent in a sibling project
amq send --to claude@infra-lib:collab --body "API contract changed"

# Channel broadcast: announce to all agents subscribed to #events
amq announce --channel events --body "Merge complete on main"

# Channel broadcast: all agents in a session
amq send --to '#session/auth' --body "Auth tests passing"

# Reply works across sessions too — replies are routed via Origin.ReplyTo
amq reply --id <msg_id> --body "Acknowledged"
```

### New Federation Commands

- `amq discover [--refresh] [--json]` — Scan sibling directories for AMQ-enabled projects. Results are cached at `~/.cache/amq/discover.json`. Use `--refresh` to force a rescan.
- `amq who [--json]` — List all sessions and agents in the current project, showing session topic, branch, claims, agent activity status (active/stale based on 10-minute TTL), and channel memberships.
- `amq resolve <address> [--json]` — Parse and resolve a qualified address to concrete delivery targets. Useful for debugging federation routing.
- `amq channel join --name <channel>` — Subscribe to a channel (writes to agent.json).
- `amq channel leave --name <channel>` — Unsubscribe from a channel.
- `amq channel list` — List current channel memberships.
- `amq announce --channel <name> [--body <str>]` — Send a message to all agents subscribed to a channel. Uses thread `channel/<name>` and includes per-target fanout metadata.

### Metadata Files

Federation uses two optional advisory metadata files:

**session.json** (at session root, e.g., `.agent-mail/collab/session.json`):
```json
{
  "schema": 1,
  "session": "collab",
  "topic": "Auth rewrite",
  "branch": "feature/auth",
  "claims": ["internal/auth/", "internal/middleware/"],
  "updated": "2026-03-18T10:00:00Z"
}
```

**agent.json** (per agent, e.g., `.agent-mail/collab/agents/claude/agent.json`):
```json
{
  "schema": 1,
  "agent": "claude",
  "last_seen": "2026-03-18T10:05:00Z",
  "channels": ["events", "ci"]
}
```

Both files are written by `coop exec` (using `--topic`, `--claim`, `--channel` flags) and are optional. Sessions and agents created before federation support work normally without these files.

Channel membership is **advisory metadata only**, not an authorization mechanism. Any agent can send to any other agent regardless of channel membership. Channels are used for fan-out discovery: when sending to `#channel`, the resolver checks agent.json files to find which agents have subscribed.

### Message Format v2 (Origin & Delivery)

Schema 2 messages include two new optional header objects. Schema 1 messages continue to parse correctly; schema 2 is written by default for federated sends.

**Origin** — Identifies the source of a cross-session or cross-project message:
```json
{
  "origin": {
    "project": "my-api",
    "project_id": "abc123",
    "session": "collab",
    "agent": "claude",
    "reply_to": "claude@my-api:collab",
    "ack_to": "claude@my-api:collab"
  }
}
```

Note: `Origin.project_id` is only populated on the discovery fallback path (when `AM_PROJECT` is not set via `coop exec`). When `AM_PROJECT` is set, the project slug is used directly without looking up the project_id.

**Delivery** — Records how the message was routed:
```json
{
  "delivery": {
    "requested_to": ["codex@auth"],
    "resolved_to": ["codex@auth"],
    "scope": "cross-session",
    "channel": "#events",
    "fanout_index": 1,
    "fanout_total": 3
  }
}
```

The `scope` field is one of: `local`, `cross-session`, or `cross-project`.

### Federation Environment Variables

`coop exec` sets these additional environment variables for federation:

| Variable | Description | Example |
|---|---|---|
| `AM_PROJECT` | Project slug (directory basename or .amqrc `project` field) | `my-api` |
| `AM_SESSION` | Session name | `collab` |
| `AM_BASE_ROOT` | Base root directory (before session suffix) | `.agent-mail` |

These are used by `send`, `reply`, and `announce` to build origin metadata and resolve cross-session/cross-project addresses.

### Security

- Cross-project delivery verifies same-user ownership (UID check) on both project directories. Messages cannot cross user boundaries.
- Federated delivery to a foreign session uses `DeliverToExistingInbox` which requires the target mailbox to already exist. It never auto-creates directories in foreign sessions.
- Path traversal is prevented by strict handle/session/project name validation (`[a-z0-9_-]+`).

## Testing

Run individual test: `go test ./internal/fsq -run TestMaildir`

Key test files:
- `internal/fsq/maildir_test.go` - Atomic delivery semantics
- `internal/fsq/dlq_test.go` - Dead letter queue operations
- `internal/format/message_test.go` - Message serialization
- `internal/thread/thread_test.go` - Thread collection
- `internal/cli/watch_test.go` - Watch command with fsnotify
- `internal/resolve/address_test.go` - Qualified address parsing
- `internal/resolve/resolver_test.go` - Address resolution and cross-session/project routing
- `internal/cli/federation_test.go` - End-to-end federation send/reply tests
- `internal/discover/discover_test.go` - Project discovery and scanning
- `internal/discover/cache_test.go` - Discovery cache operations

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
    ├── message-format.md
    └── swarm-mode.md
.claude/skills/amq-spec/       → Spec workflow skill (SOURCE OF TRUTH)
├── SKILL.md
└── references/
    └── spec-workflow.md
.codex/skills/amq-cli/         → Codex CLI skill (synced copy)
.codex/skills/amq-spec/        → Spec workflow skill (synced copy)
skills/amq-cli/                → Standalone skill (synced copy, for direct install)
skills/amq-spec/               → Standalone skill (synced copy, for direct install)
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
- `.claude/skills/amq-spec/` loads instead of `~/.claude/skills/amq-spec/`
- `.codex/skills/amq-cli/` loads instead of `~/.codex/skills/amq-cli/`
- `.codex/skills/amq-spec/` loads instead of `~/.codex/skills/amq-spec/`

This lets you test skill changes locally before publishing.

### Editing Skills

1. Edit files in `.claude/skills/<skill-name>/` (source of truth)
2. If publishing: bump `version:` in the skill's `SKILL.md`
3. Sync to other locations: `make sync-skills`
4. Test locally by running Claude Code or Codex in this repo
5. Commit and push (all three locations will be committed)

**Important**: Never edit `.codex/skills/` or `skills/` directly. Always edit `.claude/skills/` and sync.

### Installing Skills

See README.md for installation instructions for Claude Code and Codex CLI.
