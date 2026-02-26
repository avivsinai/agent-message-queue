---
name: amq-cli
version: 1.3.0
description: >-
  Coordinate agents via the AMQ CLI for file-based inter-agent messaging.
  Use when you need to send messages to another agent (Claude/Codex),
  receive messages from partner agents, set up co-op mode between Claude
  Code and Codex CLI, or manage agent-to-agent communication in any
  multi-agent workflow. Triggers include "message codex", "talk to claude",
  "collaborate with partner agent", "AMQ", "inter-agent messaging",
  or "agent coordination".
metadata:
  short-description: Inter-agent messaging via AMQ CLI
  compatibility: claude-code, codex-cli
---

# AMQ CLI Skill

File-based message queue for agent-to-agent coordination.

## Prerequisites

Requires `amq` binary in PATH. Install:
```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
```

## Environment Rules (IMPORTANT)

When running inside `coop exec`, the environment is already configured:

- **Always use `amq` from PATH** — never `./amq`, `../amq`, or absolute paths
- **Never override `AM_ROOT` or `AM_ME`** — they are set by `coop exec`
- **Never pass `--root` or `--me` flags** — env vars handle routing
- **Just run commands as-is**: `amq send --to codex --body "hello"`

When running **outside** `coop exec` (e.g. new conversation, manual terminal):

- You **must** set both `AM_ME` and `AM_ROOT` explicitly on every command
- `amq` resolves the root from `.amqrc` at call time — if `.amqrc` was changed mid-conversation, messages silently route to the new root
- **Always use explicit `AM_ROOT`** to avoid routing to the wrong session:
  ```bash
  AM_ME=claude AM_ROOT=/path/to/project/.agent-mail/<session> amq send --to codex --body "hello"
  ```
- **Pitfall**: setting only `AM_ME` without `AM_ROOT` relies on `.amqrc` which may have changed. Messages will land in the wrong session's inbox with no error — the target agent won't see them.

## Quick Start

```bash
# One-time project setup
amq coop init

# Per-session (one command per terminal — defaults to --session collab)
amq coop exec claude -- --dangerously-skip-permissions  # Terminal 1
amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox  # Terminal 2
```

Without `--session` or `--root`, `coop exec` defaults to `--session collab`.

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

## Environment Rules (IMPORTANT)

When running inside `coop exec`, the environment is already configured. Follow these rules:

- **Always use `amq` from PATH** — never `./amq`, `../amq`, or absolute paths to local binaries
- **Never override `AM_ROOT` or `AM_ME`** — they are set by `coop exec` and point to the correct session. Do not prefix commands with `AM_ROOT=... amq ...`
- **Never pass `--root` or `--me` flags** — the env vars handle routing automatically
- **Just run commands as-is**: `amq send --to codex --body "hello"`

Wrong:
```bash
AM_ROOT=.agent-mail AM_ME=claude ./amq send --to codex --body "hello"
```

Right:
```bash
amq send --to codex --body "hello"
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
- [references/message-format.md](references/message-format.md) — Message format: frontmatter schema, field reference
