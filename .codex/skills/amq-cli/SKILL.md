---
name: amq-cli
description: Coordinate agents via the AMQ CLI for file-based inter-agent messaging. Use when you need to send messages to another agent (Claude/Codex), receive messages from partner agents, set up co-op mode between Claude Code and Codex CLI, or manage agent-to-agent communication in any multi-agent workflow. Triggers include "message codex", "talk to claude", "collaborate with partner agent", "AMQ", "inter-agent messaging", or "agent coordination".
metadata:
  short-description: Inter-agent messaging via AMQ CLI
---

# AMQ CLI Skill

File-based message queue for agent-to-agent coordination.

## When to Use

- Coordinating with another agent via AMQ (send, reply, drain, watch, wake).
- Running Claude/Codex co-op in a shared repo.
- Handling message queues, acknowledgments, or DLQ recovery.

## When Not to Use

- Tasks that do not involve AMQ or agent-to-agent messaging.
- Manual edits to message files (always use the CLI).
- One-off, single-agent work with no coordination needs.

## Prerequisites

Requires `amq` binary in PATH. Install:
```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
```

Verify: `amq --version`

## Quick Reference

```bash
# One-liner setup (run once per terminal session)
eval "$(amq env --me claude --wake)"   # For Claude Code
eval "$(amq env --me codex --wake)"    # For Codex CLI

# Send and receive messages
amq send --to codex --body "Message"           # Send
amq drain --include-body                       # Receive (recommended)
amq reply --id <msg_id> --body "Response"      # Reply
amq watch --timeout 60s                        # Wait for messages
```

**Note**: After setup, all commands work from any subdirectory.

## Examples

- "message claude to review" → `amq send --to claude --kind review_request --body "..."`
- "check for new messages" → `amq drain --include-body`
- "wait for a reply" → `amq watch --timeout 60s` then `amq drain --include-body`

## Instructions

### Default Behavior (Autonomous)

When you see an AMQ notification or any mention of new messages:

1. Run `amq drain --include-body`.
2. Classify the message and act immediately if it is actionable.
3. Reply to the sender with results (or send a `status` update for long work).
4. Ask the user only for credentials, product decisions, or unclear requirements.

Do **not** ask the user what to do with AMQ messages. Coordinate with the partner
agent via AMQ and use the shared workspace to read changes directly.

This skill must be **self-contained**. Do not assume repo-local docs are present
for installed users; rely only on this `SKILL.md` and `references/`.

### Co-op Mode (Autonomous Multi-Agent)

In co-op mode, agents work autonomously. **Message your partner, not the user.**

If you need a structured loop for handling messages, follow `references/claw.md`.

#### Shared Workspace

**Both agents work in the same project folder.** Files are shared automatically:
- If partner says "done with X" → check the files directly, don't ask for code
- If partner says "see my changes" → read the files, they're already there
- Don't send code snippets in messages → just reference file paths

Only use messages for: coordination, questions, review requests, status updates.

| Situation | Action |
|----------|--------|
| Blocked | Message partner |
| Need review | Send `kind: review_request` with file paths |
| Partner done | Read files directly (they're local) |
| Done | Signal completion |
| Ask user only for | credentials, unclear requirements |

#### Setup

Run once per project:
```bash
curl -sL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/setup-coop.sh | bash
export AM_ROOT=.agent-mail AM_ME=claude   # or: codex
```

#### Wake Notifications (Experimental)

Start a background waker before your CLI to receive notifications when messages arrive:

```bash
amq wake &
claude
```

When messages arrive, you'll see a notification injected into your terminal (best-effort):
```
AMQ: message from codex - Review complete. Run: amq drain --include-body
```

Then run `amq drain --include-body` to read messages.

**Inject Modes**: The wake command auto-detects your CLI type:
- `--inject-mode=auto` (default): Uses `raw` for Claude Code/Codex, `paste` for others
- `--inject-mode=raw`: Plain text + CR (best for Ink-based CLIs like Claude Code)
- `--inject-mode=paste`: Bracketed paste with delayed CR (best for crossterm CLIs)

If notifications require manual Enter, try `--inject-mode=raw`.

#### Priority Handling

| Priority | Action |
|----------|--------|
| `urgent` | Interrupt, respond now |
| `normal` | Add to TODOs, respond after current task |
| `low` | Batch for session end |

#### Progress Updates

When starting long work, send a status message so the sender knows you're working:

```bash
# Signal you've started (with optional ETA)
amq reply --id <msg_id> --kind status --body "Started processing, eta ~20m"

# If blocked, update status
amq reply --id <msg_id> --kind status --body "Blocked: waiting for API clarification"

# When done, send the final response (use appropriate kind: answer, review_response, etc.)
amq reply --id <msg_id> --kind answer --body "Here's my response..."
```

The sender can check your progress via:
```bash
amq thread --id <thread_id> --include-body --limit 5
```

#### Work Splitting

If a task should be divided between agents, follow `references/split.md` for a
clean split protocol (scope, ownership, ETA, and definition of done).

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
amq env --wake               # Include 'amq wake &' in output
amq env --me codex           # Override agent handle
amq env --shell fish         # Fish shell syntax
amq env --json               # Machine-readable output
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

## Guidelines

- Keep usage focused on AMQ coordination; avoid one-off or unrelated tasks.
- Prefer `amq drain` for processing; use `amq list` only for a quick peek.
- Use `amq reply` to preserve threading and refs.
- Include `kind`, `priority`, paths, and commands in messages for clarity.
- For long work, send a `status` update with ETA.
- Load reference files on demand; avoid reading all of `references/` unless needed.
- Keep `SKILL.md` focused; push deep detail into `references/` to minimize context.
- Keep references one level deep (link directly from `SKILL.md`) to avoid partial reads.
- Do not add or request secrets in skill assets/scripts; keep sensitive data out of skill files.

## Version History

- 2026-01-20: Added autonomous handling, new references (claw/split/edges), and best-practice structure.

## References

Read these when you need deeper context:

- `references/coop-mode.md` — Read when setting up or debugging co-op workflows between agents
- `references/message-format.md` — Read when you need the full frontmatter schema (all fields, types, defaults)
- `references/claw.md` — Message handling loop (drain → classify → act → reply)
- `references/split.md` — Work splitting protocol for multi-agent tasks
- `references/edges.md` — Edge cases and recovery checklist
