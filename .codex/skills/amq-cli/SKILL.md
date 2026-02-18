---
name: amq-cli
version: 1.3.0
description: Coordinate agents via the AMQ CLI for file-based inter-agent messaging. Use when you need to send messages to another agent (Claude/Codex), receive messages from partner agents, set up co-op mode between Claude Code and Codex CLI, or manage agent-to-agent communication in any multi-agent workflow. Triggers include "message codex", "talk to claude", "collaborate with partner agent", "AMQ", "inter-agent messaging", or "agent coordination".
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

## Quick Start

```bash
# One-time project setup
amq coop init

# Per-session (one command per terminal)
amq coop exec claude -- --dangerously-skip-permissions  # Terminal 1
amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox  # Terminal 2
```

## Messaging

```bash
amq send --to codex --body "Message"              # Send
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
| `spec_research` | `spec_research` | normal |
| `spec_draft` | `spec_review` | normal |
| `spec_review` | — | normal |
| `spec_decision` | — | normal |

## Workflows

- **Spec workflow** (`amq coop spec`) — Collaborative specification with parallel research, exchange, drafting, review, and convergence. Read [references/spec-workflow.md](references/spec-workflow.md) for the full protocol, phase rules, and templates.
- **Swarm mode** (`amq swarm`) — Join Claude Code Agent Teams as an external agent. Read [references/swarm-mode.md](references/swarm-mode.md) for commands and task workflow.

## References

- [references/coop-mode.md](references/coop-mode.md) — Co-op protocol: roles, phased flow, collaboration modes
- [references/spec-workflow.md](references/spec-workflow.md) — Spec workflow: phases, parallel discipline, templates
- [references/swarm-mode.md](references/swarm-mode.md) — Swarm mode: agent teams, bridge, task workflow
- [references/message-format.md](references/message-format.md) — Message format: frontmatter schema, field reference
