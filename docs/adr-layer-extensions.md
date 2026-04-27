# ADR: Layer Extension Surfaces

## Status

Accepted. Implementation is split across follow-up issues.

## Date

2026-04-27

## Context

AMQ is the local message transport for agent sessions. Higher-level tools such
as team launchers, task boards, and workflow adapters sit above AMQ and need to
discover routing state, store layer-owned metadata, and show users the command
AMQ would run.

That boundary is important:

- AMQ owns mailbox layout, delivery, session routing, cross-project routing,
  replies, receipts, presence, and operational diagnostics.
- Layers own team composition, roles, task decomposition, launch UX, restore
  UX, project-specific rules, and workflow policy.

The current implicit integration points are too fragile for external layers:

- `amq env --json` is useful but not documented as a stable machine contract.
- Layers that write metadata under AMQ-owned mailbox directories can collide
  with future AMQ files or cleanup logic.
- Layers that explain routes must reimplement AMQ's root, session, project,
  and peer routing rules.
- Sending from a setup terminal to another session still lacks a clean way to
  stamp the source session before the sender process is inside `coop exec`.

## Decision

AMQ will add four small, additive surfaces for layer interoperability:

1. A versioned `amq env --json` v1 contract.
2. Reserved extension metadata directories plus a passive manifest convention.
3. A structured `amq route explain --json` command.
4. Explicit `amq send --from-session` support for pre-boot cross-session sends.

AMQ will not become team-aware. It will not read layer-specific files such as
`team.json`, infer persona rosters, or run extension hooks.

## `amq env --json` v1

`amq env --json` is a stable machine-readable contract for scripts and layers.
Existing keys remain compatible. New keys are additive, and consumers must
ignore unknown fields.

The v1 output includes:

| Field | Type | Meaning |
|------|------|---------|
| `schema_version` | integer | JSON contract version. Current value: `1`. |
| `amq_version` | string | AMQ CLI version that produced the response. |
| `root` | string | Resolved active queue root. In a session, this is the session root. |
| `base_root` | string | Base queue root when known, for example `.agent-mail`. Empty when unknown. |
| `session_name` | string | Current session name when `root` is a session root. Empty outside a session. |
| `in_session` | boolean | True when `root` is classified as a session root. |
| `me` | string | Resolved and validated sender handle. |
| `project` | string | Project identity from `.amqrc`, or its documented fallback when available. |
| `root_source` | string | Source used to resolve `root`, for diagnostics. |
| `peers` | object | Peer project map from `.amqrc`, when configured. |

Allowed `root_source` values are:

- `flag`
- `env`
- `project_amqrc`
- `global_env`
- `global_amqrc`
- `auto_detect`

Example:

```json
{
  "schema_version": 1,
  "amq_version": "0.33.0",
  "root": "/repo/.agent-mail/cto",
  "base_root": "/repo/.agent-mail",
  "session_name": "cto",
  "in_session": true,
  "me": "cto",
  "project": "app",
  "root_source": "project_amqrc",
  "peers": {
    "infra": "/Users/me/src/infra/.agent-mail"
  }
}
```

Layer guidance:

- Use `me` from this response rather than re-validating divergent identity
  rules.
- Use `base_root`, `session_name`, and `in_session` instead of inferring session
  state from path shape.
- Treat missing optional fields as unknown, not as a reason to guess silently.

## Extension Metadata Directories

AMQ reserves extension-owned directories so layers can store metadata without
writing into AMQ's own mailbox namespace.

Per-agent metadata:

```text
<AM_ROOT>/agents/<handle>/extensions/<layer>/
```

Root or session metadata:

```text
<AM_ROOT>/extensions/<layer>/
```

The `<layer>` name must be identifier-like: lowercase ASCII letters, digits,
hyphen, underscore, and dot. Reverse-DNS names are allowed, for example
`io.github.omriariav.amq-squad`.

AMQ guarantees:

- Core AMQ will not create layer-owned files inside these directories.
- Cleanup will not remove extension directories unless a future command
  explicitly says it is cleaning extension metadata.
- Doctor commands may inspect manifests, report unknown extension files, and
  warn about malformed extension metadata.

Layer guidance:

- Store layer metadata under the reserved extension namespace, not directly
  beside `inbox`, `outbox`, `receipts`, `dlq`, or future AMQ-owned files.
- Use the per-agent directory for launch records, role metadata, restore state,
  and agent-local indexes.
- Use the root or session directory for layer-wide indexes or manifests.

### Layer Security Expectations

AMQ does not interpret extension file contents. Layers that render extension
data into agent prompts, shell commands, or UI must validate or escape it. AMQ
provides no automatic sanitization for layer-owned data.

## Passive Extension Manifest

Extensions may write a passive manifest at:

```text
<AM_ROOT>/extensions/<layer>/manifest.json
```

AMQ may report this manifest in `amq doctor --json`, but AMQ must not execute
extension code or call extension hooks.

Manifest v1:

```json
{
  "schema_version": 1,
  "layer": "io.github.omriariav.amq-squad",
  "version": "0.3.1",
  "owns": [
    "agents/*/extensions/io.github.omriariav.amq-squad/launch.json",
    "agents/*/extensions/io.github.omriariav.amq-squad/role.md"
  ]
}
```

Fields:

| Field | Type | Meaning |
|------|------|---------|
| `schema_version` | integer | Manifest version. Current value: `1`. |
| `layer` | string | Extension layer name. Must match the directory name. |
| `version` | string | Layer version, when known. |
| `owns` | array of strings | Informational relative path patterns owned by the layer. |

This manifest is intentionally passive. It supports diagnostics and provenance,
not lifecycle callbacks.

## `amq route explain --json`

AMQ owns canonical route explanation because AMQ owns root resolution, session
classification, project identity, peer lookup, and reply routing semantics.

Command shape:

```bash
amq route explain --to <handle> [--project <project>] [--session <session>] --json
```

Optional tooling flags may include:

- `--from-root <path>` to explain from an explicit source root.
- `--from-cwd <path>` to resolve `.amqrc` and auto-detection from another
  working directory.
- `--me <handle>` to explain the sender identity used in generated argv.

The JSON output includes:

| Field | Type | Meaning |
|------|------|---------|
| `schema_version` | integer | Route explanation contract version. Current value: `1`. |
| `routable` | boolean | True when AMQ can construct a valid route. |
| `argv` | array of strings | Canonical command argv. This is the source of truth. |
| `display_command` | string | Shell-quoted command for humans. Presentation only. |
| `source_root` | string | Resolved source root. |
| `delivery_root` | string | Resolved target delivery root. |
| `source_project` | string | Source project, when known. |
| `target_project` | string | Target project, when known. |
| `source_session` | string | Source session, when known. |
| `target_session` | string | Target session, when set or inferred. |
| `error` | string | Human-readable explanation when `routable` is false. |

Example:

```json
{
  "schema_version": 1,
  "routable": true,
  "argv": [
    "amq",
    "send",
    "--to",
    "qa",
    "--project",
    "project-b",
    "--session",
    "qa"
  ],
  "display_command": "amq send --to qa --project project-b --session qa",
  "source_root": "/repo-a/.agent-mail/cto",
  "delivery_root": "/repo-b/.agent-mail/qa",
  "source_project": "project-a",
  "target_project": "project-b",
  "source_session": "cto",
  "target_session": "qa"
}
```

Non-routable example:

```json
{
  "schema_version": 1,
  "routable": false,
  "argv": [],
  "display_command": "",
  "source_root": "/repo-a/.agent-mail/cto",
  "delivery_root": "",
  "source_project": "project-a",
  "target_project": "project-b",
  "source_session": "cto",
  "target_session": "qa",
  "error": "peer project \"project-b\" is not configured"
}
```

Layer guidance:

- Treat `argv` as canonical. Shell strings are for display only.
- Do not reimplement AMQ project, peer, or session routing in layer code.
- Surface non-routable explanations to users instead of generating plausible
  but guessed commands.

## Pre-Boot `--from-session` Sends

AMQ will support explicit source-session stamping from setup terminals:

```bash
amq send \
  --root <base-root> \
  --from-session <source-session> \
  --me <sender> \
  --to <target> \
  --session <target-session> \
  --body "..."
```

Semantics:

- `--root` is the base root for the source and target sessions.
- `--from-session` is an explicit local-user claim about the source session.
- AMQ validates the source session root exists under the base root.
- AMQ validates the sender handle under the source session using existing
  handle rules.
- AMQ writes the sender outbox copy under the source session.
- AMQ delivers to the target session.
- AMQ stamps `reply_to` as `<sender>@<source-session>` so replies route back to
  the intended source session.

This closes the setup-terminal cross-session case without making AMQ aware of
team rosters or launch tools.

Non-goals:

- AMQ does not create missing source or target sessions as a side effect of
  `send`.
- AMQ does not accept `--from-session` for cross-project peer claims unless a
  future ADR defines that contract.
- AMQ does not read layer metadata to decide the source session.

## Alternatives Considered

### Make AMQ Team-Aware

Rejected. Team composition, roles, launch commands, and workflow policy are
layer responsibilities. Making AMQ read `team.json` or equivalent files would
couple the transport to one orchestrator model.

### Let Layers Keep Reimplementing Routing

Rejected. It creates plausible wrong commands when `.amqrc`, peer config,
custom roots, global roots, or duplicate project basenames are involved.
Routing is already AMQ's domain, so AMQ should expose route explanation.

### Add Active Extension Hooks

Deferred. Hooks around `coop exec`, `doctor`, cleanup, or delivery are easy to
overfit and create execution-order and security problems. Passive manifests and
reserved directories are enough until at least two independent layers need the
same lifecycle event.

### Keep Extension Files in Agent Roots

Rejected. Files directly under `<AM_ROOT>/agents/<handle>/` compete with AMQ's
current and future mailbox namespace. Reserved extension directories give
layers a durable place to write metadata while preserving AMQ's ability to
evolve the mailbox.

## Consequences

- External layers get stable, minimal AMQ-owned APIs for discovery, storage,
  routing explanation, and setup-terminal sends.
- AMQ keeps the product boundary: it remains a message transport, not a task or
  team orchestrator.
- Existing commands remain compatible because all surfaces are additive.
- Layers should move metadata into extension directories and delete duplicated
  AMQ layout or route inference code.
- `amq doctor --json` can report passive extension metadata without executing
  layer code.

## Follow-Up Issues

Implementation should be tracked as separate issues:

- Add `amq env --json` v1 fields and stability documentation.
- Reserve extension metadata directories and support passive manifests in
  diagnostics.
- Add `amq route explain --json` with canonical `argv` output.
- Add `amq send --from-session` for pre-boot cross-session sends and link it to
  the existing cross-session bootstrap issue.
