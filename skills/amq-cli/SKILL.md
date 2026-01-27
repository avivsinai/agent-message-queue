---
name: amq-cli
version: 1.0.0
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

## Quick Reference

```bash
# One-time project setup
amq coop init                         # Creates .amqrc, mailboxes, updates .gitignore

# Start agent sessions
amq coop start claude                 # Start Claude Code
amq coop start codex                  # Start Codex CLI

# Pass flags to agent
amq coop start claude -- --dangerously-skip-permissions

# Isolated sessions (multiple pairs)
amq coop init --root .agent-mail/feature-a
amq coop start --root .agent-mail/feature-a claude

# Send and receive messages
amq send --to codex --body "Message"           # Send
amq drain --include-body                       # Receive (recommended)
amq reply --id <msg_id> --body "Response"      # Reply
amq watch --timeout 60s                        # Wait for messages
```

**Note**: After setup, all commands work from any subdirectory.

## Co-op Mode: Phased Parallel Work

Both agents work in parallel where safe, coordinate where risky. Different models = different training = different blind spots. Cross-model work catches errors that same-model review misses.

### Roles

- **Claude Code** = Leader + Worker (coordinates phases, merges, prepares commits, gets user approval)
- **Codex** = Worker (executes phases, reports to leader, awaits next assignment)

### Phased Flow

| Phase | Mode | Description |
|-------|------|-------------|
| **Research** | Parallel | Both explore codebase, read docs, search. No conflicts. |
| **Design** | Parallel → Merge | Both propose approaches. Leader merges/decides. |
| **Code** | Split | Divide by file/module. Never edit same file. |
| **Review** | Parallel | Both review each other's code. Leader decides disputes. |
| **Test** | Parallel | Both run tests, report results to leader. |

```
Research (parallel) → sync findings
    ↓
Design (parallel) → leader merges approach
    ↓
Code (split: e.g., Claude=files A,B; Codex=files C,D)
    ↓
Review (parallel: each reviews other's code)
    ↓
Test (parallel: both run tests)
    ↓
Leader prepares commit → user approves → push
```

### Key Rules

- **Never branch** — always work on same branch (joined work)
- **Code phase = split** — divide files/modules to avoid conflicts
- **File overlap** — if same file unavoidable, assign one owner; other reviews/proposes via message
- **Coordinate between phases** — sync before moving to next phase
- **Leader decides** — Claude Code makes final calls at merge points

### Stay in Sync

- After completing a phase, report to leader and await next assignment
- While waiting, safe to do: review partner's work, run tests, read docs
- If no assignment comes, ask leader (not user) for next task

### Shared Workspace

**Both agents work in the same project folder.** Files are shared automatically:
- If partner says "done with X" → check the files directly, don't ask for code
- Don't send code snippets in messages → just reference file paths

### When to Act

| Agent | Action |
|-------|--------|
| Codex | Complete phase → report to leader → await next assignment |
| Claude Code | Merge own work + codex's → ask user for commit approval |
| Either | Ask user only for: credentials, unclear requirements |

### Setup

Run once per project:
```bash
amq coop init
amq coop start claude   # or: codex
```

### Multiple Pairs (Isolated Sessions)

Run multiple agent pairs on different features using `--root`:

```bash
# Initialize isolated roots
amq coop init --root .agent-mail/auth
amq coop init --root .agent-mail/api

# Start agents with their root
amq coop start --root .agent-mail/auth claude   # Pair A
amq coop start --root .agent-mail/api claude    # Pair B
```

Each root has isolated inboxes. Messages stay within their root.

### Priority Handling

| Priority | Action |
|----------|--------|
| `urgent` | Interrupt, respond now |
| `normal` | Add to TODOs, respond after current task |
| `low` | Batch for session end |

### Progress Updates

When starting long work, send a status message:

```bash
amq reply --id <msg_id> --kind status --body "Started, eta ~20m"
```

### Optional: Wake Notifications

> Co-op works without wake. This is an optional enhancement for interactive terminals.

For human operators, wake provides background notifications:

```bash
amq wake &
claude
```

When messages arrive:
```
AMQ: message from codex - Review complete. Drain with: amq drain --include-body — then act on it
```

If notifications require manual Enter, try `--inject-mode=raw`.

## Commands

### Send
```bash
amq send --to codex --body "Quick message"
amq send --to codex --subject "Review" --kind review_request --body @file.md
amq send --to claude --priority urgent --kind question --body "Blocked on API"
amq send --to codex --labels "bug,parser" --body "Found issue in parser"
amq send --to codex --context '{"paths": ["internal/cli/"]}' --body "Review these"
```

### Receive
```bash
amq drain --include-body         # One-shot, silent when empty
amq watch --timeout 60s          # Block until message arrives
amq list --new                   # Peek without side effects
```

### Filter Messages
```bash
amq list --new --priority urgent              # By priority
amq list --new --from codex                   # By sender
amq list --new --kind review_request          # By kind
amq list --new --label bug --label critical   # By labels (can repeat)
amq list --new --from codex --priority urgent # Combine filters
```

### Reply
```bash
amq reply --id <msg_id> --body "LGTM"
amq reply --id <msg_id> --kind review_response --body "See comments..."
```

### Dead Letter Queue
```bash
amq dlq list                        # List failed messages
amq dlq read --id <dlq_id>          # Inspect failure details
amq dlq retry --id <dlq_id>         # Retry (move back to inbox)
amq dlq retry --all [--force]       # Retry all
amq dlq purge --older-than 24h      # Clean old DLQ entries
```

### Upgrade
```bash
amq upgrade                    # Self-update to latest release
amq --no-update-check ...      # Disable update hint for this command
export AMQ_NO_UPDATE_CHECK=1   # Disable update hints globally
```

### Environment Setup
```bash
amq env                      # Output shell exports (auto-detects .amqrc or .agent-mail/)
amq env --me codex           # Override agent handle
amq env --shell fish         # Fish shell syntax
amq env --json               # Machine-readable output
amq env --wake               # Include 'amq wake &' (interactive terminals only)
```

### Other
```bash
amq thread --id p2p/claude__codex --include-body   # View thread
amq presence set --status busy --note "reviewing"  # Set presence
amq cleanup --tmp-older-than 36h                   # Clean stale tmp
```

## Message Kinds

| Kind | Reply Kind | Default Priority | Use |
|------|------------|------------------|-----|
| `review_request` | `review_response` | normal | Code review |
| `review_response` | — | normal | Review feedback |
| `question` | `answer` | normal | Questions |
| `answer` | — | normal | Answers |
| `decision` | — | normal | Design decisions |
| `brainstorm` | — | low | Open discussion |
| `status` | — | low | FYI updates |
| `todo` | — | normal | Task assignments |

## Labels and Context

**Labels** tag messages for filtering:
```bash
amq send --to codex --labels "bug,urgent" --body "Critical issue"
```

**Context** provides structured metadata:
```bash
amq send --to codex --kind review_request \
  --context '{"paths": ["internal/cli/send.go"], "focus": "error handling"}' \
  --body "Please review"
```

## Conventions

- Handles: lowercase `[a-z0-9_-]+`
- Threads: `p2p/<agentA>__<agentB>` (lexicographic)
- Delivery: atomic Maildir (tmp -> new -> cur)
- Never edit message files directly

## References

Read these when you need deeper context:

- `references/coop-mode.md` — Read when setting up or debugging co-op workflows between agents
- `references/message-format.md` — Read when you need the full frontmatter schema (all fields, types, defaults)
