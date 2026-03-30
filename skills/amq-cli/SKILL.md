---
name: amq-cli
version: 1.9.0
description: >-
  Coordinate agents via the AMQ CLI for file-based inter-agent messaging. Use
  this skill whenever you need to send messages to another agent (codex, claude,
  or any named handle), check your inbox, drain queued messages, set up co-op
  mode between agents, join a swarm team, route messages across projects, or
  diagnose delivery issues. Also use it when you receive a message and need to
  know how to reply, ack, or handle priority. Covers any multi-agent
  coordination task where agents need to talk to each other — review requests,
  questions, status updates, decision threads, wake notifications, and
  orchestrator integration (Symphony, Kanban). For collaborative spec/design
  workflows specifically, prefer the /amq-spec skill which provides structured
  phase-by-phase guidance. Not intended for distributed systems design
  (RabbitMQ, Kafka), CI/CD pipelines, or single-agent tasks with no partner.
metadata:
  short-description: Inter-agent messaging via AMQ CLI
  compatibility: claude-code, codex-cli
---

# AMQ CLI Skill

File-based message queue for agent-to-agent coordination.

AMQ manages the conversation, not the task plan. Use it for messaging, routing, replies, and adapter-emitted lifecycle events; keep work decomposition and execution in the orchestrator above it.

## Prerequisites

Requires `amq` binary in PATH. Install:
```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
```

## Environment Rules

AMQ uses two env vars for routing: `AM_ROOT` (which mailbox tree) and `AM_ME` (which agent). Getting these wrong means messages go to the wrong place or silently disappear, so it matters to let the CLI handle them rather than guessing.

**Inside `coop exec`** — everything is pre-configured. Just run bare commands:
```bash
amq send --to codex --body "hello"     # correct
amq send --me claude --to codex ...    # wrong — --me overrides the env
./amq send ...                         # wrong — use amq from PATH
```
The reason: `coop exec` sets `AM_ROOT` and `AM_ME` precisely for the session. Passing `--root` or `--me` overrides that and can route to the wrong mailbox.

**Outside `coop exec`** — resolve the root from config, don't hardcode it:
```bash
eval "$(amq env --me claude)"          # reads .amqrc chain, sets both vars

# Or pin per-command without polluting the shell (useful in scripts):
AM_ME=claude AM_ROOT=$(amq env --json | jq -r .root) amq send --to codex --body "hello"
```
Why not hardcode? The root path depends on the config chain (project `.amqrc` → `AMQ_GLOBAL_ROOT` → `~/.amqrc`). Hardcoding skips this and breaks when the project moves or config changes.

**Global fallback**: Orchestrator-spawned agents often start outside the repo root where no project `.amqrc` exists. Set `AMQ_GLOBAL_ROOT` or `~/.amqrc` so `amq env` and `amq doctor` still resolve the correct queue.

**Session pitfall**: `coop exec` defaults to `--session collab` (i.e., `.agent-mail/collab`). Outside `coop exec`, the base root is `.agent-mail` (no session suffix). These are different mailbox trees — don't mix them up.

### Root Resolution Truth-Table

| Context | Command | AM_ROOT resolves to |
|---------|---------|---------------------|
| Outside `coop exec` | `amq env --me claude` | resolved base root from project `.amqrc`, `AMQ_GLOBAL_ROOT`, or `~/.amqrc` |
| Outside `coop exec`, no project `.amqrc` | `amq env --me claude` | `AMQ_GLOBAL_ROOT` or `~/.amqrc` |
| Outside `coop exec`, isolated session | `amq env --session auth --me claude` | `<resolved-base-root>/auth` |
| Inside `coop exec` (no flags) | automatic | `.agent-mail/collab` (default session) |
| Inside `coop exec --session X` | automatic | `.agent-mail/X` |

## Task Routing

Before diving in, match the task to the right workflow — this avoids wasted effort:

| Your task | What to do |
|-----------|-----------|
| **"spec", "design with", "collaborative spec"** | Use `/amq-spec` instead — it has structured phase-by-phase guidance for parallel-research workflows. |
| **Send a message, review request, question** | Use `amq send` (see Messaging below) |
| **Swarm / agent teams** | Read [references/swarm-mode.md](references/swarm-mode.md), then use `amq swarm` |
| **Received message with labels `workflow:spec`** | Follow the spec skill protocol: do independent research first, then engage on the `spec/<topic>` thread — don't skip straight to implementation. |

## Quick Start

```bash
# One-time project setup
amq coop init

# Per-session (one command per terminal — defaults to --session collab)
amq coop exec claude -- --dangerously-skip-permissions  # Terminal 1
amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox  # Terminal 2
```

Without `--session` or `--root`, `coop exec` defaults to `--session collab`.

## Integration & Ops Quick Reference

```bash
# Global fallback for orchestrator-spawned agents
export AMQ_GLOBAL_ROOT="$HOME/.agent-mail"

# Symphony hooks
amq integration symphony init --me codex
amq integration symphony emit --event after_run --me codex

# Cline Kanban bridge
amq integration kanban bridge --me codex
amq integration kanban bridge --me codex --workspace-id my-workspace

# Runtime diagnostics
amq doctor --ops
amq doctor --ops --json
```

## Session Layout

By default, `.amqrc` points to a literal root (e.g., `.agent-mail`). Use `--session` to create isolated subdirectories:

```
.agent-mail/              ← default root (configured in .amqrc)
.agent-mail/auth/         ← isolated session (via --session auth)
.agent-mail/api/          ← isolated session (via --session api)
```

- `amq coop exec claude` → `AM_ROOT=.agent-mail/collab` (default session)
- `amq coop exec --session auth claude` → `AM_ROOT=.agent-mail/auth`

Only two env vars: `AM_ROOT` (where) + `AM_ME` (who). The CLI enforces correct routing — just run `amq` commands as-is.

## Cross-Project Routing

Send messages to agents in other projects via `--project` or inline `@project:session` syntax. Requires peer configuration in `.amqrc`.

**When to use `--session` vs `--project`**: `--session` = same project, different session. `--project` = different project. Change one dimension at a time.

### Peer setup

Add `project` and `peers` to your `.amqrc`:
```json
{
  "root": ".agent-mail",
  "project": "my-project",
  "peers": {
    "infra-lib": "/Users/me/projects/infra-lib/.agent-mail"
  }
}
```

Both projects must register each other as peers for round-trip messaging.

### Sending cross-project

```bash
# Flag syntax
amq send --to codex --project infra-lib --body "hello from here"

# Inline syntax (terser)
amq send --to codex@infra-lib:collab --body "inline syntax"

# Same session name as source (default when --session omitted)
amq send --to codex --project infra-lib --body "delivers to same session"
```

### Replies route automatically

When you receive a cross-project message, `reply_project` is set in the header. `amq reply` routes back automatically — no `--project` flag needed:
```bash
amq reply --id <msg_id> --body "got it"  # routes back via reply_project
```

### Thread naming

- **Same project P2P**: `p2p/claude__codex`
- **Cross-project P2P**: `p2p/projA:collab:claude__projB:collab:codex`
- **Topical** (cross-project): use same thread ID across projects, e.g., `decision/release-v0.24`

For full details, see [references/cross-project.md](references/cross-project.md).

### Cross-project identity (IMPORTANT)

When you receive a message where `from` matches your own handle (e.g., `from: "claude"` and you are claude), check `from_project` and `reply_project`. If either is present and names a different project, this is **NOT an echo** — it is a legitimate cross-project message from a different agent instance with the same handle. Process it normally.

### AM_ROOT scoping after cross-project sends

After sending a cross-project message (via `--project`), your `AM_ROOT` still points to YOUR project. To send to your own partner (same project), use plain `amq send --to codex` — do NOT use `--project`. The `--project` flag is ONLY for sending to agents in OTHER projects.

## Decision Threads

Decentralized decision protocol using existing AMQ primitives (no new CLI commands).

- **Thread**: `decision/<topic>`
- **Kind**: `decision` for all messages
- **Labels**: `decision:proposal`, `decision:objection`, `decision:support`, `decision:final`; plus `project:<name>` for cross-project decisions
- **Context** on proposals: `{"proposal_id": "...", "question": "...", "options": [...], "required_projects": [...], "deadline": "..."}`

**Process**: Propose → Review/Object → Resolve objections → Close when all required projects responded and no unresolved blocking objections.

```bash
amq send --to codex --project infra-lib --kind decision \
  --labels "decision:proposal,project:my-project,project:infra-lib" \
  --thread "decision/api-v2" \
  --context '{"proposal_id":"api-v2","question":"Adopt new API?","required_projects":["my-project","infra-lib"]}' \
  --body "Proposal: migrate to API v2. All tests green."
```

## Messaging

```bash
amq send --to codex --body "Message"              # Send (uses AM_ROOT/AM_ME from env)
amq drain --include-body                          # Receive (one-shot, silent when empty)
amq reply --id <msg_id> --body "Response"          # Reply in thread
amq watch --timeout 60s                           # Block until message arrives
amq list --new                                    # Peek without side effects
```

### Send with metadata
```bash
amq send --to codex --subject "Review" --kind review_request --body @file.md
amq send --to codex --priority urgent --kind question --body "Blocked on API"
amq send --to codex --labels "bug,parser" --context '{"paths": ["src/"]}' --body "Found issue"
```

### Filter
```bash
amq list --new --priority urgent
amq list --new --from codex --kind review_request
amq list --new --label bug
```

## Priority Handling

| Priority | Action |
|----------|--------|
| `urgent` | Interrupt current work, respond now |
| `normal` | Add to TODOs, respond after current task |
| `low` | Batch for session end |

## Message Kinds

| Kind | Reply Kind | Default Priority |
|------|------------|------------------|
| `review_request` | `review_response` | normal |
| `question` | `answer` | normal |
| `decision` | — | normal |
| `todo` | — | normal |
| `status` | — | low |
| `brainstorm` | — | low |

## References

For detailed protocols, read the reference file FIRST, then follow its instructions:

- [references/coop-mode.md](references/coop-mode.md) — Co-op protocol: roles, phased flow, collaboration modes
- [references/swarm-mode.md](references/swarm-mode.md) — Swarm mode: agent teams, bridge, task workflow
- [references/integrations.md](references/integrations.md) — Symphony + Kanban integration commands, global root fallback, ops checks
- [references/message-format.md](references/message-format.md) — Message format: frontmatter schema, field reference
- [references/cross-project.md](references/cross-project.md) — Cross-project routing: peer config, addressing, decision threads
