# CLAUDE.md

This is the repository instruction file for Claude Code and Codex. `AGENTS.md`
is a Codex compatibility shim and contains no duplicate project guidance.

## Project Overview

Agent Message Queue (AMQ) is a daemon-free, file-based message bus for local
coding agents. Maildir-style `tmp -> new -> cur` delivery gives crash-safe
messages without a database or server. AMQ owns messaging, threads, routing,
receipts, handoff state, and operational diagnosis. Orchestrators continue to
own worktrees, dependency scheduling, task decomposition, and PR landing.

## Release Contract

- Release Please maintains the release PR from conventional squash commits on
  `main`. Do not create release branches, tags, or GitHub releases manually.
- A push to `main` updates the AvivSinai marketplace for `amq-cli` and
  `amq-spec`.
- Keep one version across `CHANGELOG.md`, `.release-please-manifest.json`, and
  skill/plugin metadata. After the release PR merges, `release.yml` validates
  that commit, creates the tag, and publishes GitHub and Homebrew artifacts.
- `make build` and GoReleaser embed the same bare semver (for example `0.63.0`,
  not `v0.63.0`); the Makefile strips one leading `v` from `VERSION`.
- PR titles use `type(scope): description`. Squash merge makes the title the
  conventional commit on `main`. Use `BEGIN_COMMIT_OVERRIDE` in the PR body,
  or edit the release PR, when release notes need multiple entries. Each entry
  is one conventional header of at most 72 characters, separated by a blank
  line.
- `verify-brew-release.yml` runs when `release.yml` actually publishes a
  release, and on demand, to confirm the published release installs from the
  real `avivsinai/tap/amq` formula. It ignores non-release merges (where
  `release.yml` completes but its own `release` job stays skipped) and
  drafts or prereleases. It retries the tap refresh and install for up to 6
  attempts to absorb tap propagation lag, then requires `amq --version` to
  match the released tag and runs an init/send/drain round trip against a
  throwaway queue root to prove the installed binary works end to end. It
  never writes to the tap.

## Operational Constraints

- Handles are lowercase, match `[a-z0-9_-]+`, and do not start with `-`.
- Use the CLI for mailbox operations. Never edit queue files directly.
- Cleanup is explicit through `amq cleanup`; do not add automatic deletion.
- AMQ remains daemon-free. Supervisors can run `wake` or `monitor`, but AMQ
  does not own their lifecycle.

## Build and Development Commands

```bash
make build          # go build -o amq ./cmd/amq
make test           # go test ./...
make fmt            # gofmt -w
make vet            # go vet
make lint           # golangci-lint
make ci             # fmt-check, vet, lint, test, smoke checks
make check-skills   # validate canonical and symlinked skill content
```

Go 1.25+ is required. `golangci-lint` is optional for local development and
required by `make lint` and `make ci`.

## Architecture

```text
cmd/amq/             CLI entry point
cmd/amq-keepalive/   macOS wake supervisor companion
internal/cli/        Command handlers and routing policy
internal/fsq/        Maildir delivery, atomic operations, and scans
internal/format/     JSON frontmatter plus Markdown message serialization
internal/config/     Queue and project configuration
internal/receipt/    Consumer-local drained and DLQ receipts
internal/integration Shared adapter support for Symphony and Kanban
internal/launch/     Plans, adapters, trust, leases, bindings, and resume state
internal/keepalive/  Companion registry, adapters, hooks, and supervision
internal/sessionguard Shared fail-closed session decision table
internal/swarm/      Claude Code Agent Teams interoperability
internal/thread/     Cross-mailbox thread collection
internal/presence/   Agent presence metadata
```

Mailbox layout:

```text
<root>/agents/<agent>/inbox/{tmp,new,cur}/
<root>/agents/<agent>/outbox/sent/
<root>/agents/<agent>/receipts/
<root>/agents/<agent>/dlq/{tmp,new,cur}/
```

## Core Concepts

**Atomic delivery**: Writers create and sync a file under `tmp`, then rename it
atomically into `new`. Readers scan only `new` and `cur`.

**Message format**: JSON frontmatter is followed by `---` and a Markdown body.
The header contains schema, ID, sender, recipients, thread, subject, creation
time, refs, priority, kind, labels, context, and optional reply-routing fields.
P2P thread names use `p2p/<lower_handle>__<higher_handle>`.

**Receipts**: Consumers write `drained` or `dlq` receipts under their own
mailbox. Use `receipts list`, `receipts wait`, or `send --wait-for` to inspect
delivery without treating a wake notification as consumption proof.

**Sessions**: Named sessions are child queue roots. A participating shell is
pinned by `AM_ROOT`, `AM_BASE_ROOT`, and `AM_SESSION`; AMQ-managed identity
tokens authenticate roots. Participating commands fail closed on a mismatch.
Use `--session` or `--project` for routing, not a foreign raw `--root`. See
[Session routing and safety](docs/session-routing.md).

**Wake state**: Wake ownership, injector targets, lifecycle serialization,
readiness, and repair continuity have separate commit domains. Never infer one
from another or weaken an uncertain state. See [Wake operations](docs/wake-operations.md),
[wake state invariants](docs/wake-state-invariants.md), and the
[doorbell acknowledgement policy](docs/wake-doorbell-acknowledgement.md).

**Managed launch recovery**: A mode-0600 journal closes the create-before-binding
crash window. A matching binding is authoritative; otherwise the backend must
prove absent or adoptable state. Uncertain or partial state is action-required
and preserved. See [Managed launch recovery](docs/launch-recovery.md).

**Extension metadata**: Higher layers own data below
`<AM_ROOT>/extensions/<layer>/` or
`<AM_ROOT>/agents/<handle>/extensions/<layer>/`. AMQ does not execute extension
code and cleanup does not remove extension data. See
[the layer extension ADR](docs/adr-layer-extensions.md).

## CLI Commands

Use `amq <command> --help` for exact flags. This table defines the owning
workflow for each surface.

| Area | Commands | Contract |
| --- | --- | --- |
| Onboarding | `setup`, `launch` | Configure once, then reconcile and print or run the declared launch plan. The [README Quick Start](README.md#quick-start) is canonical. |
| Sessions | `session create`, `session list`, `session resume` | Create explicitly, inspect available sessions, or resume stored conversations. |
| Shell context | `env`, `who`, `shell-setup`, `completion` | Set or inspect a complete context and configure shell integration. |
| Messaging | `send`, `reply`, `list`, `read`, `drain`, `thread`, `trace` | Deliver, ingest, inspect, and follow conversations. Prefer `drain --include-body` for handoffs. |
| Delivery proof | `receipts list`, `receipts wait` | Inspect consumer-local `drained` and `dlq` outcomes. |
| Live waits | `watch`, `monitor` | Wait for arrivals; `monitor` also drains and emits receipts. |
| Routing | `route explain`, `presence set`, `presence list` | Explain a route and publish or inspect advisory presence. |
| Health | `doctor`, `cleanup`, `upgrade` | Diagnose, perform explicit safe cleanup (including exact stuck launch journals), or update the binary. |
| Wake | `wake`, `wake check`, `wake repair`, `wake recover-owner`, `wake retire` | Notify, inspect capability, and make identity-safe lifecycle changes. |
| DLQ | `dlq list`, `dlq read`, `dlq retry`, `dlq purge` | Inspect and explicitly recover or purge corrupt messages. |
| Low-level provisioning | `init`, `coop init`, `coop exec` | Create raw queues or enter an agent process. See [COOP.md](COOP.md). |
| Swarm bridge | `swarm list`, `join`, `leave`, `tasks`, `claim`, `complete`, `fail`, `block`, `bridge` | Interoperate with Claude Code Agent Teams. |
| Integrations | `integration symphony`, `integration kanban` | Convert external lifecycle events into AMQ messages. See [the adapter contract](docs/adapter-contract.md). |

Exit codes are `0` success, `1` general error, `2` usage, `3` not found, `4`
timeout, `5` context mismatch, and `6` action required. JSON output does not
change exit codes. `--json-schema` requires `--json`.

`send` and `reply` resolve `--body` from a literal, `@file`, or stdin. Empty or
whitespace-only input is a usage error unless `--allow-empty` is explicit.

## Message Kinds

| Kind | Reply kind | Default priority | Purpose |
| --- | --- | --- | --- |
| `review_request` | `review_response` | normal | Request review. |
| `review_response` | - | normal | Return review findings. |
| `question` | `answer` | normal | Ask for a decision or fact. |
| `answer` | - | normal | Answer a question. |
| `decision` | - | normal | Record a decision. |
| `brainstorm` | - | normal | Explore options. |
| `status` | - | low | Report progress or state. |
| `todo` | - | normal | Assign work. |

When work starts, reply with `kind=status` and an ETA. Use `urgent` only for
work that must interrupt the current activity.

## Dead Letter Queue

`read`, `drain`, and `monitor` move messages with invalid serialization or
headers to DLQ and emit a `dlq` receipt. The envelope records the parse error,
retry count, and retry state. A successful retry keeps a terminal audit in
`dlq/cur` until explicit purge.

`delivered` is terminal and idempotent. `pending` or legacy `indeterminate`
without a visible inbox destination refuses retry, including with `--force`.
The flag bypasses only the retry-count limit. Bulk JSON separates new retries,
already-delivered records, and skipped records.

## Doctor and Wake Operations

`doctor` checks installation, root selection, permissions, configuration, and
skill setup. `--ops` adds queue depth, sibling backlogs, DLQ age, presence,
wake health, worktree diagnostics, and integration hints.

Mailbox and wake repair preserve existing messages and fail closed on unsafe
paths, symlinks, conflicting session pins, or concurrent replacement. Doctor
can inspect an explicit mismatched root, but mutation requires the
authenticated pinned base or explicit `--root --ignore-session-pin`.

Before wake mutation, run `amq wake check --me <agent> --json`. Only
`restart_capability=agent_safe` permits agent-side action. Repair, owner
recovery, and retirement revalidate exact identity and have distinct scopes.
See [Wake operations](docs/wake-operations.md) for the full contract.

## Coordination Workflows

During active work, run `amq drain --include-body` between phases. Use
`send --wait-for drained` when delivery proof matters and `watch` when waiting
for a response. Reply to the initiator, preserve the existing thread, send
status at start and phase boundaries, and finish with changes and verification.

The [co-op guide](COOP.md) owns peer roles, phased flow, wake use, supervisor
recipes, and troubleshooting. The `amq-cli` skill owns agent-facing command
mechanics. The `amq-spec` skill owns research-discuss-draft-review workflows
and the human approval gate. Swarm mode is a bridge to Claude Code Agent Teams;
AMQ messages to a teammate still require the team leader to relay them.

## Testing

Run the narrowest test that exercises the owning boundary, then run `make ci`
before a commit. Useful focused commands include:

```bash
go test ./internal/fsq -run TestMaildir
go test ./internal/cli -run '<relevant test>'
go test ./internal/sessionguard
make check-skills
```

Opt-in live proofs (`AMQ_CMUX_LIVE`, `AMQ_GHOSTTY_LIVE`, `AMQ_CLAUDE_LIVE`,
`AMQ_CODEX_LIVE`, `AMQ_CURSOR_LIVE`) skip unless the env is `1`. They are not
part of `make ci`. Operator commands are in
[the public launch API guide](docs/launch-api.md).

Tests must include a negative case that a plausible wrong implementation would
fail. For concurrency, routing, wake, and filesystem work, exercise the actual
hostile boundary rather than only a helper or mock.

## Security

- Queue directories use mode 0700 and files use mode 0600.
- Handle and message ID validation prevents path traversal.
- Unknown handles warn by default; `--strict` changes warnings to errors.
- With `--strict`, unreadable or corrupt configuration is also an error.
- External injectors execute local code. Their executable and ordered fixed
  arguments form the saved identity; message-derived payloads are sanitized.
- Preserve uncertain ownership or filesystem state. Never add a force path
  that guesses at process, target, generation, or owner identity.

## Commit Conventions

- Install the repository hooks with `./scripts/install-hooks.sh`.
- Use conventional, descriptive commits such as
  `fix(wake): preserve replacement generation`.
- Run `make ci` before committing and follow every pushed CI run to completion.
- Do not commit placeholders, weaken a validator, regenerate goldens to hide a
  defect, or split code from the tests that prove it.

## Documentation Policy

- Keep `README.md` as the canonical install and onboarding path.
- Keep `COOP.md` as the full co-op and supervisor operations guide.
- Keep `CLAUDE.md` as a concise project index and engineering contract.
- Keep `docs/` evergreen: architecture, protocol contracts, and operational
  references only. Plans and frozen specs belong in issues, PRs, or the AMQ
  spec workflow.
- Describe current behavior in present tense. Put one-sentence known
  limitations beside an issue link instead of adding historical narration.

## Skill Development

Canonical skills live in `skills/amq-cli/` and `skills/amq-spec/`.
`.claude/skills/` and `.agents/skills/` are symlinks to those directories so
local runs use repository changes. Project skills take precedence over user
installations.

Edit only the canonical directory, bump skill metadata when publishing, run
`make check-skills`, and test the project-local skill. Do not create divergent
copies. See the [README installation section](README.md#2-install-skill) for
installed-skill commands.
