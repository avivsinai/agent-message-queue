# CLAUDE.md

This is the master agent instruction file for this repository. Both Claude Code and Codex should follow these guidelines. `AGENTS.md` exists only as a Codex compatibility shim and should contain only Codex-specific notes.

## Project Overview

Agent Message Queue (AMQ) is a lightweight, file-based message delivery system for local inter-agent communication. It uses Maildir-style atomic delivery (tmp→new→cur) for crash-safe messaging between coding agents on the same machine. No daemon, database, or server required. Conceptually, it is a local interoperability bus for agent sessions: AMQ manages the conversation, while orchestrators such as Claude Code teams, Codex workflows, Kanban, and Symphony keep owning the task.

AMQ owns agent-to-agent messaging, thread continuity, cross-project/session routing, handoff state, and `doctor --ops`. It does not own task decomposition, worktree management, dependency scheduling, PR landing, or cron/scheduler execution.

## Release Contract

- Release Please maintains the release PR from conventional squash commits on
  `main`. Do not create manual release branches, tags, or GitHub releases.
- A push to `main` updates the AvivSinai marketplace immediately for `amq-cli` and `amq-spec`.
- Keep one version across `CHANGELOG.md`, the release-please manifest,
  skill/plugin metadata, and the release commit. Release Please opens PRs only;
  after a release PR merges, `release.yml` validates that exact commit, creates
  the matching tag, publishes GitHub/Homebrew artifacts, and uses the committed
  changelog entry as the GitHub release notes.
- PR titles must follow `type(scope): description`; the repository uses
  squash-only merges so the title becomes the conventional commit on `main`.
  Use `BEGIN_COMMIT_OVERRIDE` in a merged PR body or edit the release PR when a
  change needs multiple or richer release-note entries.

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

**Environment Variables**: `AM_ROOT` (queue root, e.g., `.agent-mail/collab`), `AM_ME` (agent handle), `AM_BASE_ROOT` (authorized parent for named-session routing, or the exact root for a sessionless pin), `AM_SESSION` (independent session identity set by `coop exec` and every shell-mode `amq env`, empty for sessionless roots), `AMQ_GLOBAL_ROOT` (global root fallback for orchestrator-spawned agents), `AMQ_NO_UPDATE_CHECK` (disable update check)

**Session Layout**: The default base root directory is `.agent-mail/`. `.amqrc` can configure that root explicitly, but the default `.agent-mail/<session>` layout is also recognized without `.amqrc`. `coop exec` defaults to `--session collab`, so agents get session isolation without explicit flags. Use `--session` to override:
```
.agent-mail/              ← default base root (configurable in `.amqrc`)
.agent-mail/collab/       ← default session (coop exec without --session or --root)
.agent-mail/auth/         ← isolated session (via --session auth)
.agent-mail/api/          ← isolated session (via --session api)
```

`AM_ROOT` points to the queue root. When `--root` is explicitly provided, it takes precedence over `AM_ROOT`. `send` and `reply` emit a `note:` on stderr when an explicit `--root` overrides `AM_ROOT`.

**Cross-tree send guard**: A direct `--root` is root *selection*, not federation routing. `send` refuses an explicit `--root` that targets a different base tree than the caller's own active session (`AM_ROOT`/`AM_BASE_ROOT`) when no routing dimension (`--project`/`--session`/`--from-session`) is given — such a message carries no sender-origin metadata, so the recipient could not reply and a naive reply would loop into their own tree. Replyable cross-tree messaging must use `--project`/`--session`, which stamp the routing headers. With no session env set (bare-root scripts/CI), the guard does not fire; a direct `--root` cross-tree send in that case can still produce an unreplyable message — the documented cost of keeping bare-root sends working.

**Session-root guard**: `read`, `drain`, `monitor`, `watch`, and mutating DLQ commands compare their resolved target against the exact context pinned by `AM_BASE_ROOT`/`AM_SESSION` before inspecting or moving mailbox state; `send` and `reply` apply the same check to their local source. For named sessions `AM_BASE_ROOT` is the authorized parent; for sessionless contexts it is the exact root and `AM_SESSION` is empty. A mismatch exits with code 5; use `--session <name>` as the deliberate routing dimension. The raw-root escape hatch is `--ignore-session-pin`, which requires a non-empty explicit `--root`; explicitly blank `--root`/`--session` values are usage errors. Target routing never authorizes a mismatched source. `list` warns instead of refusing so it remains a non-destructive inspection path. With no positive session/tree evidence, scripts and CI remain fail-open. A missing mailbox is a not-found error. Empty `drain` and `list --new` results perform a shallow sibling-session scan and write exact `amq list --session <name> --me <handle> --new` inspection commands to stderr; `doctor --ops` reports the same condition as `sibling_backlog` hints. Known limitation: `send --from-session` remains a double-explicit legacy route from its supplied raw base; callers must ensure that base is intentional until the follow-up resolver work lands.

**Environment context replacement**: Every shell-mode `amq env` output replaces `AM_ROOT`, `AM_ME`, `AM_BASE_ROOT`, and `AM_SESSION` as one context. Sessionless output pins `AM_BASE_ROOT` to the exact root and emits an empty `AM_SESSION`; `--export` additionally prints a pin note. An ambient root that conflicts with an existing pin is rejected unless a non-empty `--root` or `--session` explicitly repins it. `amq env --session` routes from a valid existing pin base before consulting cwd configuration.

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
amq send --me <agent> --to <recipients> [--subject <str>] [--thread <id>] [--refs <ids>] [--body <str|@file|-|stdin>] [--allow-empty] [--priority <p>] [--kind <k>] [--labels <l>] [--context <json>] [--session <target-session>] [--from-session <source-session>] [--project <project>] [--ignore-session-pin] [--wait-for <stage>] [--wait-timeout <duration>]
amq list --me <agent> [--session <name>] [--new | --cur] [--priority <p>] [--from <h>] [--kind <k>] [--label <l>...] [--limit N] [--offset N] [--json]
amq read --me <agent> --id <msg_id> [--session <name>] [--ignore-session-pin] [--json]
amq drain --me <agent> [--session <name>] [--ignore-session-pin] [--limit N] [--include-body] [--json]
amq thread --id <thread_id> [--agents <a,b,c>] [--limit N] [--include-body] [--json]
amq receipts list --me <agent> [--msg-id <id>] [--stage <stage>] [--json]
amq receipts wait --me <agent> --msg-id <id> [--stage <stage>] [--timeout <duration>] [--poll-interval <duration>] [--json]
amq presence set --me <agent> --status <busy|idle|...> [--note <str>]
amq presence list [--json]
amq route explain --to <handle> [--project <project>] [--session <session>] [--from-root <path>] [--from-cwd <path>] [--me <handle>] --json
amq cleanup --tmp-older-than <duration> [--dry-run] [--yes]
amq watch --me <agent> [--session <name>] [--ignore-session-pin] [--timeout <duration>] [--poll] [--json]
amq monitor --me <agent> [--session <name>] [--ignore-session-pin] [--timeout <duration>] [--poll] [--limit N] [--include-body] [--peek] [--json]
amq reply --me <agent> --id <msg_id> [--ignore-session-pin] [--subject <str>] [--body <str|@file|-|stdin>] [--allow-empty] [--priority <p>] [--kind <k>] [--labels <l>] [--context <json>] [--wait-for <stage>] [--wait-timeout <duration>]
amq dlq list --me <agent> [--new | --cur] [--json]
amq dlq read --me <agent> --id <dlq_id> [--session <name>] [--ignore-session-pin] [--json]
amq dlq retry --me <agent> --id <dlq_id> [--session <name>] [--ignore-session-pin] [--all] [--force]
amq dlq purge --me <agent> [--session <name>] [--ignore-session-pin] [--older-than <duration>] [--dry-run] [--yes]
amq wake --me <agent> [--baseline-existing] [--inject-cmd <cmd>] [--inject-mode <auto|raw|paste|none>] [--inject-via <absolute-executable>] [--inject-arg <arg>...] [--inject-timeout <duration>] [--bell] [--debounce <duration>] [--preview-len <n>] [--defer-while-input] [--input-quiet-for <duration>] [--input-poll-interval <duration>] [--input-max-hold <duration>] [--interrupt] [--interrupt-label <label>] [--interrupt-priority <p>] [--interrupt-cmd <ctrl-c|none>] [--interrupt-notice <str>] [--interrupt-cooldown <duration>] [--debug]
amq wake repair --me <agent> [--root <path>] [--json]
amq wake retire --me <agent> --inject-via <absolute-executable> [--inject-arg <arg>...] [--root <path>] [--json]
amq upgrade
amq env [--me <agent>] [--root <path>] [--session <name>] [--shell sh|bash|zsh|fish] [--wake] [--export] [--session-name] [--json]
amq shell-setup [--shell bash|zsh|fish] [--claude-alias <name>] [--codex-alias <name>] [--grok-alias <name>]
amq coop init [--root <path>] [--agents <a,b,c>] [--no-gitignore] [--force] [--json]
amq coop exec [--root <path>] [--session <name>] [--me <handle>] [--no-init] [--no-gitignore] [--no-wake] [--require-wake] [--wake-inject-mode <auto|raw|paste|none>] [--wake-inject-via <absolute-executable>] [--wake-inject-arg <arg>...] [-y] <command> [-- <command-flags>]
amq swarm list [--json]
amq swarm join --team <name> --me <agent> [--agent-id <id>] [--type codex|external|claude-code] [--json]
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
amq doctor [--ops] [--fix-wake-locks] [--json]
```

Common flags: `--root`, `--json`, `--strict` (error instead of warn on unknown handles or unreadable/corrupt config). Global option: `--no-update-check`. Note: `init` has its own flags and doesn't accept these.

**Body resolution (`send`/`reply`)**: `--body` resolves from `@file`, a literal string, or stdin. A bare `--body -`, `--body @-`, or an omitted `--body` reads stdin (standard CLI convention). A send whose resolved body is empty or whitespace-only **fails closed** with a usage error rather than delivering a blank message — pass `--allow-empty` to send a blank body intentionally. This prevents a dropped or mistyped body (e.g. `--body -` with nothing piped) from silently shipping an empty message.

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
- Sibling-session backlogs for the same handles
- Same-session mailbox divergence across git worktrees (diagnostic-only)
- DLQ count and oldest age
- Presence freshness
- Wake lock health (`--fix-wake-locks` removes stale locks)
- Integration hints for Kanban and Symphony

Wake lock states are intentionally conservative:

- `stale`: AMQ proved the recorded PID is gone, mismatched, or not the same `amq wake`; `amq doctor --ops --fix-wake-locks` re-inspects and removes only these locks.
- `unverified`: AMQ could not prove either ownership or staleness, so startup fails closed and doctor leaves the lock in place. Confirm the PID/root/agent manually before removing the `.wake.lock`.

Live wake repair is explicit: `amq wake repair --me <agent>` may remove a
proven-stale lock and start a fresh wake only when the stale lock was created
for `--inject-via`, and `agents/<agent>/.wake.target` exists with a digest that
matches the lock's repair metadata. `.wake.target` is mode `0600` and stores
schema metadata, root, agent, creation time, `mode:"inject-via"`, an absolute
executable path, and fixed argv. Raw TTY wake has no repair target; repair must
refuse raw locks, leftover targets from old locks, and `unverified` locks.
Repaired wake stdout/stderr goes to
`agents/<agent>/.wake.repair.log`; repair itself must not keep stdout/stderr
pipes open after its JSON/text output exits. `doctor --ops` may report
`target_present` and `repair_available`, but must remain diagnostic-only and
never spawn a wake process.

`amq wake retire` requires the expected inject-via executable and ordered fixed
arguments. It stops only an identity-confirmed live inject-via wake with an
unchanged, exactly matching saved target, using Linux pidfd signaling or the
Darwin cooperative control socket; it may also remove an exactly-bound
proven-stale lock. It preserves the mailbox and saved target. The lifecycle
boundaries are: repair = replace a proven-stale inject-via wake; `doctor --ops
--fix-wake-locks` = remove a proven-stale lock; retire = stop an
identity-confirmed live inject-via wake; launchd, systemd, or the owning shell =
stop a raw wake. Retire neither unloads supervisors nor promises that they will
not restart a wake.

`who` and `doctor --ops` presence sources have narrow semantics:
`notifier_live` requires a valid wake lock with process identity confirmed by
the existing wake-lock inspector; it proves prompt notification only, never
consumption. `recent_activity` means a fresh `last_seen` without that proof.
Do not introduce `consumer_live` wording without a separate monitor heartbeat
or lock. Long-running wake/monitor supervision belongs to launchd, systemd, or
another layer above daemon-free AMQ.

Git worktree diagnostics stay in `doctor --ops`, never in the send routing
path. Relative project roots and auto-detected roots are per-worktree; sharing
requires the same absolute `.amqrc` root or `AMQ_GLOBAL_ROOT`. The deep check is
best-effort and warns only when the same session exists under another worktree
root with fresher peer presence. `send --wait-for` timeout text may name the
already-resolved delivery root/session and recommend `doctor --ops`, but must
not scan git worktrees itself.

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
| Background wake | `amq wake --me <agent> &` | Injects notification via TIOCSTI with best-effort input deferral, or via explicit external transport (experimental) |
| Reply to message | `amq reply --id <msg_id>` | Auto thread/refs handling |

## Co-op Mode (Claude <-> Codex, optional peers)

Co-op mode enables real-time collaboration between multiple agent sessions (e.g., Claude Code and Codex CLI, with Grok CLI as an optional peer). See `COOP.md` for full documentation.

**Initiator rule**: The initiator (agent or human) receives all updates and decisions. Always reply to the initiator and ask the initiator for clarifications. Do not ask a third party.

**Default pairing note**: Claude is often faster and more decisive, while Codex tends to be deeper but slower. That commonly makes Claude a natural coordinator and Codex a strong worker. This is a default, not a rule — roles are set per task by the initiator. Grok CLI can join as an additional optional peer/worker (e.g. a third `amq coop exec grok` in a three-way session) without changing this default two-engine pairing note.

**Progress protocol**: Start with a `kind=status` + ETA, send heartbeats on phase boundaries or every 10-15 minutes, and finish with Summary / Changes / Tests / Notes.

**Modes of collaboration**: Leader+Worker (default), Co-workers, Duplicate (independent solutions), Driver+Navigator, Spec+Implementer, Reviewer+Implementer. See `COOP.md` for details.

### Quick Start

```bash
# Terminal 1 - Claude Code
amq coop exec claude -- --dangerously-skip-permissions  # session=collab by default

# Terminal 2 - Codex CLI
amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox  # Same, with codex flags

# Terminal 3 - Grok CLI (optional peer)
amq coop exec grok  # Caller flags forwarded unchanged, no baked-in bypass
```

Without `--session` or `--root`, `coop exec` defaults to `--session collab`.

Use `--session` for a different isolated session:
```bash
amq coop exec --session feature-a claude               # Isolated session
```

Use `--no-gitignore` to opt out of `.gitignore` changes when `coop exec` auto-initializes a project. Use `--no-wake` to disable auto-wake (e.g., in CI or non-TTY environments). Use `--require-wake` in managed launchers that should refuse to start an agent unless the wake process starts and acquires its lock.
Use `--wake-inject-via /absolute/path/to/injector` plus repeated
`--wake-inject-arg` values when a launcher has a terminal-specific injector and
wants later `amq wake repair` to work without restarting the agent TUI. Repair
metadata is written only when this invocation starts a new wake; a reused
existing wake must already have repair metadata to be repairable.

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

For a local terminal transport that can receive notifications without a
controlling TTY, run wake with an explicit external injector:
```bash
amq wake --me orchestrator \
  --inject-via /absolute/path/to/ghostty-bridge \
  --inject-arg exec \
  --inject-arg "$TERMINAL_ID"
```

`--inject-via` is an executable path, not a shell command line. Repeat
`--inject-arg` for fixed argv entries; AMQ appends the sanitized notification
payload as the final argument and bounds each invocation with
`--inject-timeout` (default `5s`). This executes local code for each
notification, and the payload can include sanitized but message-derived sender
and subject header content.

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
