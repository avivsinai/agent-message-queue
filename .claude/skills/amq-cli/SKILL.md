---
name: amq-cli
version: 1.1.0
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

Verify: `amq --version`

## Quick Start

```bash
# One-time project setup
amq coop init

# Per-session (one command per terminal)
amq coop exec claude -- --dangerously-skip-permissions  # Terminal 1
amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox  # Terminal 2
```

That's it. `coop exec` auto-initializes if needed, sets `AM_ROOT`/`AM_ME`, starts wake notifications, and execs into the agent.

## Messaging

```bash
amq send --to codex --body "Message"              # Send
amq drain --include-body                          # Receive (one-shot, silent when empty)
amq reply --id <msg_id> --body "Response"          # Reply in thread
amq watch --timeout 60s                           # Block until message arrives
amq list --new                                    # Peek without side effects
```

Root and agent handle are auto-detected from `.amqrc` and `AM_ME`. Commands work from any subdirectory.

## Isolated Sessions (Multiple Pairs)

```bash
amq coop exec --root .agent-mail/auth claude      # Pair A
amq coop exec --root .agent-mail/auth codex
amq coop exec --root .agent-mail/api claude       # Pair B
amq coop exec --root .agent-mail/api codex
```

Each `--root` has isolated inboxes. Messages stay within their root.

## For Scripts/CI

When you can't use `exec` (non-interactive environments):
```bash
amq coop init
eval "$(amq env --me claude)"    # Set env vars manually
```

## Co-op Protocol

### Core Rules

1. **Initiator rule** — reply to the initiator; ask the initiator for clarifications
2. **Never branch** — always work on same branch
3. **Code phase = split** — divide files/modules to avoid conflicts
4. **Shared workspace** — reference file paths, don't paste code in messages

### Priority Handling

| Priority | Action |
|----------|--------|
| `urgent` | Interrupt current work, respond now |
| `normal` | Add to TODOs, respond after current task |
| `low` | Batch for session end |

### Progress Updates

```bash
amq reply --id <msg_id> --kind status --body "Started, eta ~20m"
amq reply --id <msg_id> --kind answer --body "Summary: ..."
```

## Commands Reference

### Send
```bash
amq send --to codex --body "Quick message"
amq send --to codex --subject "Review" --kind review_request --body @file.md
amq send --to codex --priority urgent --kind question --body "Blocked on API"
amq send --to codex --labels "bug,parser" --body "Found issue"
amq send --to codex --context '{"paths": ["internal/cli/"]}' --body "Review these"
```

### Filter
```bash
amq list --new --priority urgent
amq list --new --from codex --kind review_request
amq list --new --label bug --label critical
```

### Reply
```bash
amq reply --id <msg_id> --body "LGTM"
amq reply --id <msg_id> --kind review_response --body "See comments..."
```

### Dead Letter Queue
```bash
amq dlq list                                      # List failed messages
amq dlq retry --id <dlq_id>                       # Retry one
amq dlq retry --all                               # Retry all
amq dlq purge --older-than 24h --yes              # Clean old entries
```

### Other
```bash
amq thread --id p2p/claude__codex --include-body  # View thread
amq presence set --status busy --note "reviewing" # Set presence
amq cleanup --tmp-older-than 36h --yes            # Clean stale tmp
amq upgrade                                       # Self-update
```

## Message Kinds

| Kind | Reply Kind | Default Priority |
|------|------------|------------------|
| `review_request` | `review_response` | normal |
| `question` | `answer` | normal |
| `decision` | — | normal |
| `todo` | — | normal |
| `status` | — | low |
| `brainstorm` | — | low |

## Swarm Mode: Agent Teams

Enable external agents to participate in Claude Code Agent Teams.

```bash
amq swarm list                                    # Discover teams
amq swarm join --team my-team --me codex          # Join team
amq swarm tasks --team my-team                    # View tasks
amq swarm claim --team my-team --task t1 --me codex  # Claim work
amq swarm complete --team my-team --task t1 --me codex  # Mark done
amq swarm bridge --team my-team --me codex        # Run task notification bridge
```

Communication is asymmetric: bridge delivers task notifications only. Claude Code teammates can `amq send` to external agents. External agents relay messages to the team leader's inbox.

## References

- `references/coop-mode.md` — Phased workflow, collaboration modes, detailed coordination patterns
- `references/message-format.md` — Frontmatter schema cheat sheet (fields, types, defaults)
