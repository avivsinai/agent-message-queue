<file_tree>
agent-message-queue/
├── .claude/
│   ├── agents/
│   │   └── amq-coop-watcher.md
│   └── skills/
│       └── amq-cli/
│           ├── plugin.json
│           └── SKILL.md
├── .claude-plugin/
│   └── plugin.json
├── .codex/
│   └── skills/
│       └── amq-cli/
│           ├── plugin.json
│           └── SKILL.md
├── .prompts/
│   ├── amq-full-context.md
│   └── amq-monitor-research.md
├── cmd/
│   └── amq/
│       └── main.go
├── internal/
│   ├── ack/
│   │   ├── ack_test.go
│   │   ├── ack.go
│   │   └── doc.go
│   ├── cli/
│   │   ├── ack_test.go
│   │   ├── ack.go
│   │   ├── cleanup.go
│   │   ├── cli.go
│   │   ├── common_test.go
│   │   ├── common.go
│   │   ├── drain_test.go
│   │   ├── drain.go
│   │   ├── init.go
│   │   ├── list_test.go
│   │   ├── list.go
│   │   ├── monitor_test.go
│   │   ├── monitor.go
│   │   ├── presence.go
│   │   ├── read.go
│   │   ├── reply_test.go
│   │   ├── reply.go
│   │   ├── send.go
│   │   ├── thread.go
│   │   ├── watch_test.go
│   │   └── watch.go
│   ├── config/
│   │   ├── config_test.go
│   │   └── config.go
│   ├── format/
│   │   ├── doc.go
│   │   ├── id_test.go
│   │   ├── id.go
│   │   ├── message_test.go
│   │   └── message.go
│   ├── fsq/
│   │   ├── atomic.go
│   │   ├── find.go
│   │   ├── layout.go
│   │   ├── maildir_test.go
│   │   ├── maildir.go
│   │   ├── scan_test.go
│   │   ├── scan.go
│   │   ├── sync_unix.go
│   │   └── sync_windows.go
│   ├── presence/
│   │   ├── doc.go
│   │   ├── presence_test.go
│   │   └── presence.go
│   └── thread/
│       ├── doc.go
│       ├── thread_test.go
│       └── thread.go
├── scripts/
│   ├── codex-amq-notify.py
│   ├── codex-coop-monitor.sh
│   ├── install-hooks.sh
│   └── smoke-test.sh
├── skills/
│   └── amq-cli/
│       └── SKILL.md
├── specs/
│   └── 001-local-maildir-queue/
│       ├── data-model.md
│       ├── plan.md
│       ├── quickstart.md
│       ├── research.md
│       ├── spec.md
│       └── tasks.md
├── AGENTS.md
├── CLAUDE.md
├── CODE_OF_CONDUCT.md
├── CONTRIBUTING.md
├── COOP.md
├── gemini-pro3-review.md
├── README.md
└── SECURITY.md</file_tree>

<files>
File: .claude-plugin/plugin.json (122 tokens)
```
{
  "name": "amq-cli",
  "description": "Agent Message Queue - atomic Maildir-style message delivery for inter-agent communication",
  "version": "0.5.1",
  "author": {
    "name": "Aviv Sinai",
    "url": "https://github.com/avivsinai"
  },
  "repository": "https://github.com/avivsinai/agent-message-queue",
  "license": "MIT",
  "keywords": ["messaging", "agents", "maildir", "queue", "inter-agent"]
}

```

File: .claude/agents/amq-coop-watcher.md (481 tokens)
```
---
name: amq-coop-watcher
description: Background watcher for AMQ co-op mode. Monitors inbox and wakes main agent when messages arrive.
tools: Bash
model: haiku
---

# AMQ Co-op Watcher

You are a background watcher agent for the Agent Message Queue (AMQ) co-op mode. Your sole purpose is to monitor the inbox and report back to the main agent when messages arrive.

## Your Task

Run the following command and wait for messages:

```bash
amq monitor --me "$AM_ME" --timeout 0 --include-body --json
```

This command will:
1. Block until a message arrives (no timeout)
2. Drain the message(s) from inbox/new to inbox/cur
3. Auto-acknowledge if required
4. Output structured JSON with message details

## When Messages Arrive

When the command returns with messages, output a structured summary:

1. **URGENT messages first** (priority: "urgent")
   - These should interrupt the main agent's current work
   - List each with: from, subject, kind, brief body preview

2. **NORMAL messages next** (priority: "normal")
   - These should be added to the main agent's TODO list
   - List each with: from, subject, kind

3. **LOW priority messages last** (priority: "low")
   - These can be batched/digested
   - Just count and summarize

## Output Format

```
## AMQ Messages Received

### URGENT (interrupt)
- From: codex | Subject: "Blocking bug found" | Kind: question
  Preview: "I found a critical issue in..."

### NORMAL (add to TODOs)
- From: codex | Subject: "Code review ready" | Kind: review_request

### LOW (digest)
- 2 status updates from codex

---
Raw JSON: <include the full JSON output>
```

## Important

- Do NOT take any actions yourself
- Do NOT try to answer questions or complete tasks
- ONLY report what messages arrived
- Then STOP immediately so the main agent is woken up

The main agent will:
1. Receive your output
2. Decide how to handle each message based on priority
3. Respawn you to continue monitoring

```

File: .claude/skills/amq-cli/plugin.json (71 tokens)
```
{
  "name": "amq-cli",
  "version": "0.5.1",
  "description": "Coordinates agents via the AMQ CLI for atomic Maildir-style message delivery",
  "skills": ["amq-cli"],
  "repository": "https://github.com/avivsinai/agent-message-queue"
}

```

File: .claude/skills/amq-cli/SKILL.md (1046 tokens)
```
---
name: amq-cli
description: Coordinates agents via the AMQ CLI for atomic Maildir-style message delivery. Handles initializing mailboxes, sending/reading/acking messages, listing inboxes, viewing threads, setting presence, and cleaning stale tmp files. Covers tmp/new/cur semantics and safe, non-destructive usage conventions.
---

# AMQ CLI

## Quick start

- Build the binary if needed: `go build -o amq ./cmd/amq`.
- Prefer `AM_ROOT` and `AM_ME` to avoid repeating flags.

```bash
export AM_ROOT=.agent-mail
export AM_ME=codex
./amq init --root .agent-mail --agents codex,cloudcode
./amq send --to cloudcode --body "Quick ping"
```

## Core flows

### Drain (recommended for agents)

One-shot ingestion: reads all new messages, moves to cur, optionally acks. Designed for hooks and scripts.

```bash
# Drain all new messages with body, auto-ack
./amq drain --me cloudcode --include-body --ack

# Limit to 10 messages, JSON output
./amq drain --me cloudcode --limit 10 --json

# Silent when empty (perfect for hooks)
./amq drain --me $AM_ME --include-body
```

**Flags:**
- `--limit N` (default 20): Max messages to drain
- `--include-body`: Include message body in output
- `--ack` (default true): Ack messages that require acknowledgment

### Send

1. Use `--to` (required) and optionally `--thread`.
2. If `--thread` is omitted for a single recipient, use canonical p2p naming.

```bash
./amq send --me codex --to cloudcode --thread p2p/codex__cloudcode --body @notes.md
```

### List + Read + Ack (manual flow)

```bash
./amq list --me cloudcode --new
./amq read --me cloudcode --id <msg_id>
./amq ack  --me cloudcode --id <msg_id>
```

### Thread view

```bash
./amq thread --id p2p/codex__cloudcode --limit 50 --include-body
```

### Watch for messages

Wait for new messages with efficient OS-native notifications (uses fsnotify):

```bash
# Block until message arrives or timeout
./amq watch --me cloudcode --timeout 60s

# Use polling fallback for network filesystems
./amq watch --me cloudcode --timeout 60s --poll
```

### Presence (optional)

```bash
./amq presence set --me codex --status busy --note "reviewing"
./amq presence list
```

### Cleanup (explicit only)

```bash
./amq cleanup --tmp-older-than 36h
```

## Multi-Agent Coordination

### Preferred: Use drain for message ingestion

The `drain` command is designed for agent integration - it does list+read+ack in one atomic operation:

```bash
# Ingest all new messages (silent when empty - hook-friendly)
./amq drain --me $AM_ME --include-body

# With JSON output for programmatic use
./amq drain --me $AM_ME --include-body --json
```

### During active work: Quick inbox check

When doing multi-step work, use drain to check for coordination messages:

```bash
# One-shot: get messages, mark read, ack if needed
./amq drain --me $AM_ME --include-body
```

### Waiting for a reply: Use watch + drain

When you've sent a message and need to wait for a response:

```bash
# Send request
./amq send --to codex --subject "Review this" --body @file.go

# Wait for reply (blocks until message arrives)
./amq watch --me cloudcode --timeout 120s

# Ingest the reply
./amq drain --me cloudcode --include-body
```

### Workflow summary

Commands below assume `AM_ME` is set:

| Situation | Command | Behavior |
|-----------|---------|----------|
| Ingest messages | `amq drain --include-body` | One-shot: read+move+ack |
| Waiting for reply | `amq watch --timeout 60s` | Blocks until message |
| Quick peek only | `amq list --new` | Non-blocking, no side effects |

## Conventions and safety

- Do not edit message files in place; always use the CLI.
- Delivery is Maildir-style: tmp -> new -> cur. Readers must ignore tmp.
- Cleanup is explicit and never automatic; do not delete files without the cleanup command.
- Thread naming for p2p: `p2p/<lowerA>__<lowerB>` (sorted lexicographically).
- Handles must be lowercase and match `[a-z0-9_-]+`.

```

File: .codex/skills/amq-cli/plugin.json (71 tokens)
```
{
  "name": "amq-cli",
  "version": "0.5.1",
  "description": "Coordinates agents via the AMQ CLI for atomic Maildir-style message delivery",
  "skills": ["amq-cli"],
  "repository": "https://github.com/avivsinai/agent-message-queue"
}

```

File: .codex/skills/amq-cli/SKILL.md (1046 tokens)
```
---
name: amq-cli
description: Coordinates agents via the AMQ CLI for atomic Maildir-style message delivery. Handles initializing mailboxes, sending/reading/acking messages, listing inboxes, viewing threads, setting presence, and cleaning stale tmp files. Covers tmp/new/cur semantics and safe, non-destructive usage conventions.
---

# AMQ CLI

## Quick start

- Build the binary if needed: `go build -o amq ./cmd/amq`.
- Prefer `AM_ROOT` and `AM_ME` to avoid repeating flags.

```bash
export AM_ROOT=.agent-mail
export AM_ME=codex
./amq init --root .agent-mail --agents codex,cloudcode
./amq send --to cloudcode --body "Quick ping"
```

## Core flows

### Drain (recommended for agents)

One-shot ingestion: reads all new messages, moves to cur, optionally acks. Designed for hooks and scripts.

```bash
# Drain all new messages with body, auto-ack
./amq drain --me cloudcode --include-body --ack

# Limit to 10 messages, JSON output
./amq drain --me cloudcode --limit 10 --json

# Silent when empty (perfect for hooks)
./amq drain --me $AM_ME --include-body
```

**Flags:**
- `--limit N` (default 20): Max messages to drain
- `--include-body`: Include message body in output
- `--ack` (default true): Ack messages that require acknowledgment

### Send

1. Use `--to` (required) and optionally `--thread`.
2. If `--thread` is omitted for a single recipient, use canonical p2p naming.

```bash
./amq send --me codex --to cloudcode --thread p2p/codex__cloudcode --body @notes.md
```

### List + Read + Ack (manual flow)

```bash
./amq list --me cloudcode --new
./amq read --me cloudcode --id <msg_id>
./amq ack  --me cloudcode --id <msg_id>
```

### Thread view

```bash
./amq thread --id p2p/codex__cloudcode --limit 50 --include-body
```

### Watch for messages

Wait for new messages with efficient OS-native notifications (uses fsnotify):

```bash
# Block until message arrives or timeout
./amq watch --me cloudcode --timeout 60s

# Use polling fallback for network filesystems
./amq watch --me cloudcode --timeout 60s --poll
```

### Presence (optional)

```bash
./amq presence set --me codex --status busy --note "reviewing"
./amq presence list
```

### Cleanup (explicit only)

```bash
./amq cleanup --tmp-older-than 36h
```

## Multi-Agent Coordination

### Preferred: Use drain for message ingestion

The `drain` command is designed for agent integration - it does list+read+ack in one atomic operation:

```bash
# Ingest all new messages (silent when empty - hook-friendly)
./amq drain --me $AM_ME --include-body

# With JSON output for programmatic use
./amq drain --me $AM_ME --include-body --json
```

### During active work: Quick inbox check

When doing multi-step work, use drain to check for coordination messages:

```bash
# One-shot: get messages, mark read, ack if needed
./amq drain --me $AM_ME --include-body
```

### Waiting for a reply: Use watch + drain

When you've sent a message and need to wait for a response:

```bash
# Send request
./amq send --to codex --subject "Review this" --body @file.go

# Wait for reply (blocks until message arrives)
./amq watch --me cloudcode --timeout 120s

# Ingest the reply
./amq drain --me cloudcode --include-body
```

### Workflow summary

Commands below assume `AM_ME` is set:

| Situation | Command | Behavior |
|-----------|---------|----------|
| Ingest messages | `amq drain --include-body` | One-shot: read+move+ack |
| Waiting for reply | `amq watch --timeout 60s` | Blocks until message |
| Quick peek only | `amq list --new` | Non-blocking, no side effects |

## Conventions and safety

- Do not edit message files in place; always use the CLI.
- Delivery is Maildir-style: tmp -> new -> cur. Readers must ignore tmp.
- Cleanup is explicit and never automatic; do not delete files without the cleanup command.
- Thread naming for p2p: `p2p/<lowerA>__<lowerB>` (sorted lexicographically).
- Handles must be lowercase and match `[a-z0-9_-]+`.

```

File: .prompts/amq-full-context.md (39392 tokens)
```
<user_instructions>
Research and help design a workflow where Claude Code/Codex sessions spawn a background subagent to monitor the AMQ queue. When messages arrive, they should be surfaced to the main agent based on priority (urgent=interrupt, normal=add to TODOs). See .prompts/amq-monitor-research.md for full context.
</user_instructions>

<file_tree>
agent-message-queue/
├── .claude/
│   └── skills/
│       └── amq-cli/
│           └── SKILL.md
├── .prompts/
│   └── amq-monitor-research.md
├── cmd/
│   └── amq/
│       └── main.go
├── internal/
│   ├── ack/
│   │   ├── ack_test.go
│   │   ├── ack.go
│   │   └── doc.go
│   ├── cli/
│   │   ├── ack_test.go
│   │   ├── ack.go
│   │   ├── cleanup.go
│   │   ├── cli.go
│   │   ├── common_test.go
│   │   ├── common.go
│   │   ├── drain_test.go
│   │   ├── drain.go
│   │   ├── init.go
│   │   ├── list_test.go
│   │   ├── list.go
│   │   ├── presence.go
│   │   ├── read.go
│   │   ├── send.go
│   │   ├── thread.go
│   │   ├── watch_test.go
│   │   └── watch.go
│   ├── config/
│   │   ├── config_test.go
│   │   └── config.go
│   ├── format/
│   │   ├── doc.go
│   │   ├── id_test.go
│   │   ├── id.go
│   │   ├── message_test.go
│   │   └── message.go
│   ├── fsq/
│   │   ├── atomic.go
│   │   ├── find.go
│   │   ├── layout.go
│   │   ├── maildir_test.go
│   │   ├── maildir.go
│   │   ├── scan_test.go
│   │   ├── scan.go
│   │   ├── sync_unix.go
│   │   └── sync_windows.go
│   ├── presence/
│   │   ├── doc.go
│   │   ├── presence_test.go
│   │   └── presence.go
│   └── thread/
│       ├── doc.go
│       ├── thread_test.go
│       └── thread.go
├── AGENTS.md
├── CLAUDE.md
└── README.md</file_tree>

<files>
File: .claude/skills/amq-cli/SKILL.md (1046 tokens)
```
---
name: amq-cli
description: Coordinates agents via the AMQ CLI for atomic Maildir-style message delivery. Handles initializing mailboxes, sending/reading/acking messages, listing inboxes, viewing threads, setting presence, and cleaning stale tmp files. Covers tmp/new/cur semantics and safe, non-destructive usage conventions.
---

# AMQ CLI

## Quick start

- Build the binary if needed: `go build -o amq ./cmd/amq`.
- Prefer `AM_ROOT` and `AM_ME` to avoid repeating flags.

```bash
export AM_ROOT=.agent-mail
export AM_ME=codex
./amq init --root .agent-mail --agents codex,cloudcode
./amq send --to cloudcode --body "Quick ping"
```

## Core flows

### Drain (recommended for agents)

One-shot ingestion: reads all new messages, moves to cur, optionally acks. Designed for hooks and scripts.

```bash
# Drain all new messages with body, auto-ack
./amq drain --me cloudcode --include-body --ack

# Limit to 10 messages, JSON output
./amq drain --me cloudcode --limit 10 --json

# Silent when empty (perfect for hooks)
./amq drain --me $AM_ME --include-body
```

**Flags:**
- `--limit N` (default 20): Max messages to drain
- `--include-body`: Include message body in output
- `--ack` (default true): Ack messages that require acknowledgment

### Send

1. Use `--to` (required) and optionally `--thread`.
2. If `--thread` is omitted for a single recipient, use canonical p2p naming.

```bash
./amq send --me codex --to cloudcode --thread p2p/codex__cloudcode --body @notes.md
```

### List + Read + Ack (manual flow)

```bash
./amq list --me cloudcode --new
./amq read --me cloudcode --id <msg_id>
./amq ack  --me cloudcode --id <msg_id>
```

### Thread view

```bash
./amq thread --id p2p/codex__cloudcode --limit 50 --include-body
```

### Watch for messages

Wait for new messages with efficient OS-native notifications (uses fsnotify):

```bash
# Block until message arrives or timeout
./amq watch --me cloudcode --timeout 60s

# Use polling fallback for network filesystems
./amq watch --me cloudcode --timeout 60s --poll
```

### Presence (optional)

```bash
./amq presence set --me codex --status busy --note "reviewing"
./amq presence list
```

### Cleanup (explicit only)

```bash
./amq cleanup --tmp-older-than 36h
```

## Multi-Agent Coordination

### Preferred: Use drain for message ingestion

The `drain` command is designed for agent integration - it does list+read+ack in one atomic operation:

```bash
# Ingest all new messages (silent when empty - hook-friendly)
./amq drain --me $AM_ME --include-body

# With JSON output for programmatic use
./amq drain --me $AM_ME --include-body --json
```

### During active work: Quick inbox check

When doing multi-step work, use drain to check for coordination messages:

```bash
# One-shot: get messages, mark read, ack if needed
./amq drain --me $AM_ME --include-body
```

### Waiting for a reply: Use watch + drain

When you've sent a message and need to wait for a response:

```bash
# Send request
./amq send --to codex --subject "Review this" --body @file.go

# Wait for reply (blocks until message arrives)
./amq watch --me cloudcode --timeout 120s

# Ingest the reply
./amq drain --me cloudcode --include-body
```

### Workflow summary

Commands below assume `AM_ME` is set:

| Situation | Command | Behavior |
|-----------|---------|----------|
| Ingest messages | `amq drain --include-body` | One-shot: read+move+ack |
| Waiting for reply | `amq watch --timeout 60s` | Blocks until message |
| Quick peek only | `amq list --new` | Non-blocking, no side effects |

## Conventions and safety

- Do not edit message files in place; always use the CLI.
- Delivery is Maildir-style: tmp -> new -> cur. Readers must ignore tmp.
- Cleanup is explicit and never automatic; do not delete files without the cleanup command.
- Thread naming for p2p: `p2p/<lowerA>__<lowerB>` (sorted lexicographically).
- Handles must be lowercase and match `[a-z0-9_-]+`.

```

File: .prompts/amq-monitor-research.md (1374 tokens)
```
# AMQ Background Monitor: Research & Implementation Prompt

## Context

We're building a workflow where Claude Code (and Codex CLI) sessions automatically monitor an Agent Message Queue (AMQ) for incoming messages from other agents. The goal is:

1. **Non-blocking monitoring**: Main agent works normally while a background watcher monitors the queue
2. **Automatic surfacing**: When messages arrive, they're surfaced to the main agent
3. **Priority-based handling**: Urgent messages interrupt; normal messages queue as TODOs

## What We Know Works (Claude Code Dec 2025)

### Async Subagents
- Subagents can run in background via `Ctrl+B` or `run_in_background: true`
- `/tasks` command shows all background agents with status, progress, token usage
- `AgentOutputTool` automatically surfaces results when agents complete
- Background agents can **wake the main agent** when they finish

### Natural Language Spawning
```
"Run the queue-watcher in the background while I work on this feature"
"Have the amq-monitor check for messages every 30 seconds"
```

### The AMQ CLI
```bash
# Block until message arrives (or timeout)
amq watch --me claude --timeout 60s --json
# Output: {"event":"new_message","messages":[{"id":"...","from":"codex","subject":"..."}]}

# One-shot drain (read + move to cur + ack)
amq drain --me claude --include-body --json
```

## Research Questions

1. **Wake mechanism**: How exactly does a background subagent "wake" the main agent? Is it automatic when the agent completes, or does it require specific output/signaling?

2. **Continuous monitoring**: Can a background agent loop indefinitely (watch → process → watch again), or does it need to complete and be re-spawned?

3. **Interruption**: Can a background agent interrupt the main agent mid-task for urgent messages, or only when the main agent is idle/between turns?

4. **Codex CLI parity**: Does Codex have equivalent async agent capabilities? The experimental background terminals feature suggests partial support.

5. **Context injection**: When a background agent surfaces results, how much context can it inject? Can it directly modify the main agent's TodoWrite list?

## Proposed Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Claude Code / Codex Session                                │
│                                                             │
│  Main Agent (foreground)     AMQ Watcher (background)       │
│  ┌─────────────────────┐     ┌─────────────────────────┐   │
│  │ Working on user     │     │ amq watch --timeout 60s │   │
│  │ requested task...   │     │         ↓               │   │
│  │                     │     │ Message arrives         │   │
│  │                     │  ←──│ AgentOutputTool wakes   │   │
│  │                     │     │ main with result        │   │
│  │ Receives message:   │     └─────────────────────────┘   │
│  │ {priority: urgent}  │                                    │
│  │         ↓           │                                    │
│  │ Decides: interrupt  │                                    │
│  │ or queue to TODOs   │                                    │
│  └─────────────────────┘                                    │
└─────────────────────────────────────────────────────────────┘
```

## Message Format Extension

```yaml
---
schema: amq/v1
id: msg_abc123
from: codex
to: claude
subject: "Code review needed"
priority: urgent    # NEW: urgent | normal | low
thread: p2p/claude__codex
created: 2025-12-26T10:00:00Z
---

Body of the message...
```

## Implementation Options

### Option A: Session-Start Watcher
Add to CLAUDE.md:
```markdown
At session start, spawn a background AMQ watcher:
- Run `amq watch --me $AM_ME --json` in background
- When messages arrive, surface them
- For urgent: interrupt current work
- For normal: add to TodoWrite
- Re-spawn watcher after processing
```

### Option B: Custom Subagent Definition
Create `.claude/agents/amq-watcher.md`:
```markdown
---
name: amq-watcher
description: Monitors AMQ for incoming messages
tools: Bash, TodoWrite
model: haiku
---

Monitor the AMQ inbox using `amq watch`. When messages arrive:
1. Parse priority from message metadata
2. If urgent: return immediately to wake main agent
3. If normal: add to todo list, continue watching
```

### Option C: Hook-Based (Notification)
```json
{
  "hooks": {
    "Notification": [{
      "hooks": [{
        "type": "command",
        "command": "amq list --me $AM_ME --new --json | jq -e 'length > 0'"
      }]
    }]
  }
}
```

## What We Need Clarified

1. Is `run_in_background: true` on the Task tool actually shipped, or still a feature request?
2. Can background agents use TodoWrite to modify the main agent's todo list?
3. What's the actual mechanism for "waking" - is it polling, event-driven, or completion-triggered?
4. For Codex: what's the equivalent pattern?

## Test Plan

1. Spawn background watcher manually with Ctrl+B
2. Send test message via `amq send --to claude --subject "test" --body "hello"`
3. Verify main agent receives notification
4. Test priority handling (urgent vs normal)
5. Verify continuous monitoring (respawn after message processed)

## References

- [Claude Code Async Subagents (Dec 2025)](https://www.zerotopete.com/p/christmas-came-early-async-subagents)
- [Async Workflows Guide](https://claudefa.st/blog/guide/agents/async-workflows)
- [Background Commands Wiki](https://github.com/ruvnet/claude-flow/wiki/background-commands)
- [Feature Request #9905](https://github.com/anthropics/claude-code/issues/9905) - Task tool async support
- [AMQ CLI Documentation](./CLAUDE.md)

```

File: AGENTS.md (256 tokens)
```
# AGENTS.md

This file provides guidance for Codex and other AI coding agents. **The authoritative source is `CLAUDE.md`** - this file exists for compatibility with agents that look for `AGENTS.md`.

## Quick Reference

See [`CLAUDE.md`](./CLAUDE.md) for complete instructions including:

- Project overview and architecture
- Build commands (`make build`, `make test`, `make ci`)
- CLI usage and common flags
- Multi-agent coordination patterns
- Security practices (0700/0600 permissions, `--strict` flag)
- Contributing guidelines and commit conventions
- Skill development workflow (dev vs installed precedence, `make sync-skills`)

## Essential Commands

```bash
make ci              # Run before committing: vet, lint, test, smoke
./amq --version      # Check installed version
```

## Environment

```bash
export AM_ROOT=.agent-mail
export AM_ME=<your-agent-handle>
```

## Key Constraints

- Go 1.25+ required
- Handles must be lowercase: `[a-z0-9_-]+`
- Never edit message files directly; use the CLI
- Cleanup is explicit (`amq cleanup`), never automatic

```

File: CLAUDE.md (1625 tokens)
```
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
├── cli/           → Command handlers (send, list, read, ack, drain, thread, presence, cleanup, init, watch)
├── fsq/           → File system queue (Maildir delivery, atomic ops, scanning)
├── format/        → Message serialization (JSON frontmatter + Markdown body)
├── config/        → Config management (meta/config.json)
├── ack/           → Acknowledgment tracking
├── thread/        → Thread collection across mailboxes
└── presence/      → Agent presence metadata
```

**Mailbox Layout**:
```
<root>/agents/<agent>/inbox/{tmp,new,cur}/  → Incoming messages
<root>/agents/<agent>/outbox/sent/          → Sent copies
<root>/agents/<agent>/acks/{received,sent}/ → Acknowledgments
```

## Core Concepts

**Atomic Delivery**: Messages written to `tmp/`, fsynced, then atomically renamed to `new/`. Readers only scan `new/` and `cur/`, never seeing incomplete writes.

**Message Format**: JSON frontmatter (schema, id, from, to, thread, subject, created, ack_required, refs) followed by `---` and Markdown body.

**Thread Naming**: P2P threads use lexicographic ordering: `p2p/<lower_agent>__<higher_agent>`

**Environment Variables**: `AM_ROOT` (default root dir), `AM_ME` (default agent handle)

## CLI Commands

```bash
amq init --root <path> --agents a,b,c [--force]
amq send --me <agent> --to <recipients> [--subject <str>] [--thread <id>] [--body <str|@file|stdin>] [--ack]
amq list --me <agent> [--new | --cur] [--json]
amq read --me <agent> --id <msg_id> [--json]
amq ack --me <agent> --id <msg_id>
amq drain --me <agent> [--limit N] [--include-body] [--ack] [--json]
amq thread --id <thread_id> [--limit N] [--include-body] [--json]
amq presence set --me <agent> --status <busy|idle|...> [--note <str>]
amq presence list [--json]
amq cleanup --tmp-older-than <duration> [--dry-run] [--yes]
amq watch --me <agent> [--timeout <duration>] [--poll] [--json]
```

Common flags: `--root`, `--json`, `--strict` (error instead of warn on unknown handles or unreadable/corrupt config). Note: `init` has its own flags and doesn't accept these.

Use `amq --version` to check the installed version.

## Multi-Agent Coordination

Commands below assume `AM_ME` is set (e.g., `export AM_ME=claude`).

**Preferred: Use `drain`** - One-shot ingestion that reads, moves to cur, and acks in one atomic operation. Silent when empty (hook-friendly).

**During active work**: Use `amq drain --include-body` to ingest messages between steps.

**Waiting for reply**: Use `amq watch --timeout 60s` which blocks until a message arrives, then `amq drain` to process.

| Situation | Command | Behavior |
|-----------|---------|----------|
| Ingest messages | `amq drain --include-body` | One-shot: read+move+ack |
| Waiting for reply | `amq watch --timeout 60s` | Blocks until message |
| Quick peek only | `amq list --new` | Non-blocking, no side effects |

## Testing

Run individual test: `go test ./internal/fsq -run TestMaildir`

Key test files:
- `internal/fsq/maildir_test.go` - Atomic delivery semantics
- `internal/format/message_test.go` - Message serialization
- `internal/thread/thread_test.go` - Thread collection
- `internal/cli/watch_test.go` - Watch command with fsnotify

## Security

- Directories use 0700 permissions (owner-only access)
- Files use 0600 permissions (owner read/write only)
- Unknown handles trigger a warning by default; use `--strict` to error instead
- With `--strict`, unreadable or corrupt `config.json` also causes an error
- Handles must be lowercase: `[a-z0-9_-]+`

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
.claude/skills/amq-cli/        → Claude Code skill (source of truth)
├── SKILL.md
└── plugin.json
.codex/skills/amq-cli/         → Codex CLI skill (synced copy)
├── SKILL.md
└── plugin.json
```

### Dev vs Installed Skills

When working in this repo, **project-level skills take precedence** over user-level installed skills:

- `.claude/skills/amq-cli/` loads instead of `~/.claude/skills/amq-cli/`
- `.codex/skills/amq-cli/` loads instead of `~/.codex/skills/amq-cli/`

This lets you test skill changes locally before publishing.

### Editing Skills

1. Edit files in `.claude/skills/amq-cli/` (source of truth)
2. Sync to Codex: `make sync-skills`
3. Test locally by running Claude Code or Codex in this repo
4. Bump version in `.claude-plugin/plugin.json` and `.claude/skills/amq-cli/plugin.json`
5. Run `make sync-skills` again to update Codex copies
6. Commit and push

### Installing Skills

See README.md for installation instructions for Claude Code and Codex CLI.

```

File: cmd/amq/main.go (278 tokens)
```
package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/avivsinai/agent-message-queue/internal/cli"
)

var version = "dev"

func getVersion() string {
	// Prefer ldflags-injected version
	if version != "dev" {
		return version
	}
	// Fall back to Go module version (set by go install)
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func main() {
	if len(os.Args) > 1 && isVersionArg(os.Args[1]) {
		if _, err := fmt.Fprintln(os.Stdout, getVersion()); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := cli.Run(os.Args[1:]); err != nil {
		if _, werr := fmt.Fprintln(os.Stderr, err); werr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}

func isVersionArg(arg string) bool {
	switch arg {
	case "--version", "-v", "version":
		return true
	default:
		return false
	}
}

```

File: internal/ack/ack_test.go (212 tokens)
```
package ack

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAckMarshal(t *testing.T) {
	ts := time.Date(2025, 12, 24, 15, 2, 33, 0, time.UTC)
	a := New("msg-1", "p2p/cloudcode__codex", "cloudcode", "codex", ts)
	data, err := a.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("expected trailing newline")
	}
	var out Ack
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.MsgID != "msg-1" || out.Thread == "" || out.From != "cloudcode" || out.To != "codex" {
		t.Fatalf("unexpected ack: %+v", out)
	}
}

```

File: internal/ack/ack.go (293 tokens)
```
package ack

import (
	"encoding/json"
	"os"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

// Ack records that a message was received and acknowledged.
type Ack struct {
	Schema   int    `json:"schema"`
	MsgID    string `json:"msg_id"`
	Thread   string `json:"thread"`
	From     string `json:"from"`
	To       string `json:"to"`
	Received string `json:"received"`
}

func New(msgID, thread, from, to string, received time.Time) Ack {
	return Ack{
		Schema:   format.CurrentSchema,
		MsgID:    msgID,
		Thread:   thread,
		From:     from,
		To:       to,
		Received: received.UTC().Format(time.RFC3339Nano),
	}
}

func (a Ack) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func Read(path string) (Ack, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Ack{}, err
	}
	var out Ack
	if err := json.Unmarshal(data, &out); err != nil {
		return Ack{}, err
	}
	return out, nil
}

```

File: internal/ack/doc.go (39 tokens)
```
// Package ack provides acknowledgment tracking for messages.
// Acks are stored as JSON files in the sender's received/ and
// receiver's sent/ directories for coordination and audit.
package ack

```

File: internal/cli/ack_test.go (1070 tokens)
```
package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunAckRejectsInvalidHeader(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "cloudcode"); err != nil {
		t.Fatalf("EnsureAgentDirs cloudcode: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "bad/../id",
			From:    "cloudcode",
			To:      []string{"codex"},
			Thread:  "p2p/cloudcode__codex",
			Created: "2025-12-24T15:02:33Z",
		},
		Body: "test",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "codex", "bad.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if err := runAck([]string{"--root", root, "--me", "codex", "--id", "bad"}); err == nil {
		t.Fatalf("expected error for invalid message id in header")
	}

	msg.Header.ID = "msg-1"
	msg.Header.From = "CloudCode"
	data, err = msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	badPath := filepath.Join(fsq.AgentInboxNew(root, "codex"), "bad-from.md")
	if _, err := fsq.WriteFileAtomic(filepath.Dir(badPath), filepath.Base(badPath), data, 0o644); err != nil {
		t.Fatalf("write bad-from: %v", err)
	}
	if err := runAck([]string{"--root", root, "--me", "codex", "--id", "bad-from"}); err == nil {
		t.Fatalf("expected error for invalid sender handle in header")
	}
}

func TestRunAckCorruptAckFileRecovery(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "cloudcode"); err != nil {
		t.Fatalf("EnsureAgentDirs cloudcode: %v", err)
	}

	// Create a valid message
	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-recover",
			From:    "cloudcode",
			To:      []string{"codex"},
			Thread:  "p2p/cloudcode__codex",
			Created: "2025-12-25T15:02:33Z",
		},
		Body: "test",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "codex", "msg-recover.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// Create a corrupt ack file
	ackDir := fsq.AgentAcksSent(root, "codex")
	if err := os.MkdirAll(ackDir, 0o700); err != nil {
		t.Fatalf("mkdir ack dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ackDir, "msg-recover.json"), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write corrupt ack: %v", err)
	}

	// Ack should succeed (recover from corrupt file)
	if err := runAck([]string{"--root", root, "--me", "codex", "--id", "msg-recover"}); err != nil {
		t.Fatalf("ack should recover from corrupt file: %v", err)
	}

	// Verify the ack file was rewritten
	ackData, err := os.ReadFile(filepath.Join(ackDir, "msg-recover.json"))
	if err != nil {
		t.Fatalf("read ack file: %v", err)
	}
	if string(ackData) == "not json" {
		t.Errorf("ack file should have been rewritten")
	}
}

```

File: internal/cli/ack.go (844 tokens)
```
package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/ack"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runAck(args []string) error {
	fs := flag.NewFlagSet("ack", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Message id")

	usage := usageWithFlags(fs, "amq ack --me <agent> --id <msg_id> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)

	// Validate handle against config.json
	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	filename, err := ensureFilename(*idFlag)
	if err != nil {
		return err
	}

	path, _, err := fsq.FindMessage(root, common.Me, filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("message not found: %s", *idFlag)
		}
		return err
	}

	header, err := format.ReadHeaderFile(path)
	if err != nil {
		return err
	}
	sender, err := normalizeHandle(header.From)
	if err != nil || sender != header.From {
		return fmt.Errorf("invalid sender handle in message: %s", header.From)
	}
	msgID, err := ensureSafeBaseName(header.ID)
	if err != nil || msgID != header.ID {
		return fmt.Errorf("invalid message id in message: %s", header.ID)
	}
	ackPayload := ack.New(header.ID, header.Thread, common.Me, sender, time.Now())
	receiverDir := fsq.AgentAcksSent(root, common.Me)
	receiverPath := filepath.Join(receiverDir, msgID+".json")
	needsReceiverWrite := true
	if existing, err := ack.Read(receiverPath); err == nil {
		ackPayload = existing
		needsReceiverWrite = false
	} else if !os.IsNotExist(err) {
		// Corrupt ack file - warn and rewrite
		_ = writeStderr("warning: corrupt ack file, rewriting: %v\n", err)
	}

	data, err := ackPayload.Marshal()
	if err != nil {
		return err
	}

	if needsReceiverWrite {
		if _, err := fsq.WriteFileAtomic(receiverDir, msgID+".json", data, 0o600); err != nil {
			return err
		}
	}

	// Best-effort write to sender's received acks; sender may not exist.
	senderDir := fsq.AgentAcksReceived(root, sender)
	senderPath := filepath.Join(senderDir, msgID+".json")
	if _, err := os.Stat(senderPath); err == nil {
		// Already recorded.
	} else if os.IsNotExist(err) {
		if _, err := fsq.WriteFileAtomic(senderDir, msgID+".json", data, 0o600); err != nil {
			if warnErr := writeStderr("warning: unable to write sender ack: %v\n", err); warnErr != nil {
				return warnErr
			}
		}
	} else if err != nil {
		return err
	}

	if common.JSON {
		return writeJSON(os.Stdout, ackPayload)
	}
	if err := writeStdout("Acked %s\n", header.ID); err != nil {
		return err
	}
	return nil
}

```

File: internal/cli/cleanup.go (654 tokens)
```
package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runCleanup(args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	common := addCommonFlags(fs)
	olderFlag := fs.String("tmp-older-than", "", "Duration (e.g. 36h)")
	dryRunFlag := fs.Bool("dry-run", false, "Show what would be removed without deleting")
	yesFlag := fs.Bool("yes", false, "Skip confirmation prompt")
	usage := usageWithFlags(fs, "amq cleanup --tmp-older-than <duration> [--dry-run] [--yes] [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if *olderFlag == "" {
		return errors.New("--tmp-older-than is required")
	}
	dur, err := time.ParseDuration(*olderFlag)
	if err != nil {
		return err
	}
	if dur <= 0 {
		return errors.New("--tmp-older-than must be > 0")
	}
	root := filepath.Clean(common.Root)
	cutoff := time.Now().Add(-dur)

	candidates, err := fsq.FindTmpFilesOlderThan(root, cutoff)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		if err := writeStdoutLine("No tmp files to remove."); err != nil {
			return err
		}
		return nil
	}

	if *dryRunFlag {
		if common.JSON {
			return writeJSON(os.Stdout, map[string]any{
				"candidates": candidates,
				"count":      len(candidates),
			})
		}
		if err := writeStdout("Would remove %d tmp file(s).\n", len(candidates)); err != nil {
			return err
		}
		for _, path := range candidates {
			if err := writeStdout("%s\n", path); err != nil {
				return err
			}
		}
		return nil
	}

	if !*yesFlag {
		ok, err := confirmPrompt(fmt.Sprintf("Delete %d tmp file(s)?", len(candidates)))
		if err != nil {
			return err
		}
		if !ok {
			if err := writeStdoutLine("Aborted."); err != nil {
				return err
			}
			return nil
		}
	}

	removed := 0
	for _, path := range candidates {
		if err := os.Remove(path); err != nil {
			return err
		}
		removed++
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"removed": removed,
		})
	}
	if err := writeStdout("Removed %d tmp file(s).\n", removed); err != nil {
		return err
	}
	return nil
}

```

File: internal/cli/cli.go (697 tokens)
```
package cli

import "fmt"

const (
	envRoot = "AM_ROOT"
	envMe   = "AM_ME"
)

func Run(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printUsage()
	}

	switch args[0] {
	case "init":
		return runInit(args[1:])
	case "send":
		return runSend(args[1:])
	case "list":
		return runList(args[1:])
	case "read":
		return runRead(args[1:])
	case "ack":
		return runAck(args[1:])
	case "thread":
		return runThread(args[1:])
	case "presence":
		return runPresence(args[1:])
	case "cleanup":
		return runCleanup(args[1:])
	case "watch":
		return runWatch(args[1:])
	case "drain":
		return runDrain(args[1:])
	default:
		return fmt.Errorf("unknown command: %s", args[0])
	}
}

func printUsage() error {
	if err := writeStdoutLine("amq - agent message queue"); err != nil {
		return err
	}
	if err := writeStdoutLine(""); err != nil {
		return err
	}
	if err := writeStdoutLine("Usage:"); err != nil {
		return err
	}
	if err := writeStdoutLine("  amq <command> [options]"); err != nil {
		return err
	}
	if err := writeStdoutLine(""); err != nil {
		return err
	}
	if err := writeStdoutLine("Commands:"); err != nil {
		return err
	}
	if err := writeStdoutLine("  init      Initialize the queue root and agent mailboxes"); err != nil {
		return err
	}
	if err := writeStdoutLine("  send      Send a message"); err != nil {
		return err
	}
	if err := writeStdoutLine("  list      List inbox messages"); err != nil {
		return err
	}
	if err := writeStdoutLine("  read      Read a message by id"); err != nil {
		return err
	}
	if err := writeStdoutLine("  ack       Acknowledge a message"); err != nil {
		return err
	}
	if err := writeStdoutLine("  thread    View a thread"); err != nil {
		return err
	}
	if err := writeStdoutLine("  presence  Set or list presence"); err != nil {
		return err
	}
	if err := writeStdoutLine("  cleanup   Remove stale tmp files"); err != nil {
		return err
	}
	if err := writeStdoutLine("  watch     Wait for new messages (uses fsnotify)"); err != nil {
		return err
	}
	if err := writeStdoutLine("  drain     Drain new messages (read, move to cur, ack)"); err != nil {
		return err
	}
	if err := writeStdoutLine(""); err != nil {
		return err
	}
	if err := writeStdoutLine("Environment:"); err != nil {
		return err
	}
	if err := writeStdoutLine("  AM_ROOT   Default root directory for storage"); err != nil {
		return err
	}
	return writeStdoutLine("  AM_ME     Default agent handle")
}

```

File: internal/cli/common_test.go (731 tokens)
```
package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeHandle(t *testing.T) {
	if got, err := normalizeHandle("codex"); err != nil || got != "codex" {
		t.Fatalf("normalizeHandle valid: %v, %v", got, err)
	}
	if _, err := normalizeHandle("Codex"); err == nil {
		t.Fatalf("expected error for uppercase handle")
	}
	if _, err := normalizeHandle("co/dex"); err == nil {
		t.Fatalf("expected error for invalid characters")
	}
	if got, err := normalizeHandle("codex_1"); err != nil || got != "codex_1" {
		t.Fatalf("normalizeHandle underscore: %v, %v", got, err)
	}
}

func TestValidateKnownHandle(t *testing.T) {
	root := t.TempDir()
	metaDir := filepath.Join(root, "meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create config with known agents
	cfg := map[string]any{
		"version": 1,
		"agents":  []string{"alice", "bob"},
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(metaDir, "config.json"), data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Known handle should pass
	if err := validateKnownHandle(root, "alice", false); err != nil {
		t.Errorf("known handle should pass: %v", err)
	}

	// Unknown handle with strict=false should warn but not error
	if err := validateKnownHandle(root, "unknown", false); err != nil {
		t.Errorf("unknown handle with strict=false should warn, not error: %v", err)
	}

	// Unknown handle with strict=true should error
	if err := validateKnownHandle(root, "unknown", true); err == nil {
		t.Errorf("unknown handle with strict=true should error")
	}
}

func TestValidateKnownHandleNoConfig(t *testing.T) {
	root := t.TempDir()

	// No config file - should pass any handle
	if err := validateKnownHandle(root, "anyhandle", true); err != nil {
		t.Errorf("no config should pass any handle: %v", err)
	}
}

func TestValidateKnownHandleCorruptConfig(t *testing.T) {
	root := t.TempDir()
	metaDir := filepath.Join(root, "meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write invalid JSON
	if err := os.WriteFile(filepath.Join(metaDir, "config.json"), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write corrupt config: %v", err)
	}

	// Corrupt config with strict=false should warn but not error
	if err := validateKnownHandle(root, "alice", false); err != nil {
		t.Errorf("corrupt config with strict=false should warn, not error: %v", err)
	}

	// Corrupt config with strict=true should error
	if err := validateKnownHandle(root, "alice", true); err == nil {
		t.Errorf("corrupt config with strict=true should error")
	}
}

```

File: internal/cli/common.go (2536 tokens)
```
package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type commonFlags struct {
	Root   string
	Me     string
	JSON   bool
	Strict bool
}

func addCommonFlags(fs *flag.FlagSet) *commonFlags {
	flags := &commonFlags{}
	fs.StringVar(&flags.Root, "root", defaultRoot(), "Root directory for the queue")
	fs.StringVar(&flags.Me, "me", defaultMe(), "Agent handle (or AM_ME)")
	fs.BoolVar(&flags.JSON, "json", false, "Emit JSON output")
	fs.BoolVar(&flags.Strict, "strict", false, "Error on unknown handles (default: warn)")
	return flags
}

func defaultRoot() string {
	if env := strings.TrimSpace(os.Getenv(envRoot)); env != "" {
		return env
	}
	return ".agent-mail"
}

func defaultMe() string {
	if env := strings.TrimSpace(os.Getenv(envMe)); env != "" {
		return env
	}
	return ""
}

func requireMe(handle string) error {
	if strings.TrimSpace(handle) == "" {
		return errors.New("--me is required (or set AM_ME)")
	}
	return nil
}

// validateKnownHandle checks if the handle is in config.json (if it exists).
// Returns nil if config doesn't exist or handle is known.
// If strict=true, returns an error for unknown handles or unreadable/corrupt config; otherwise warns to stderr.
func validateKnownHandle(root, handle string, strict bool) error {
	configPath := filepath.Join(root, "meta", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No config, no validation
		}
		// Config exists but unreadable
		msg := fmt.Sprintf("cannot read config.json: %v", err)
		if strict {
			return errors.New(msg)
		}
		_ = writeStderr("warning: %s\n", msg)
		return nil
	}

	var cfg struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		// Config exists but invalid JSON
		msg := fmt.Sprintf("invalid config.json: %v", err)
		if strict {
			return errors.New(msg)
		}
		_ = writeStderr("warning: %s\n", msg)
		return nil
	}

	for _, known := range cfg.Agents {
		if known == handle {
			return nil // Handle is known
		}
	}

	msg := fmt.Sprintf("handle %q not in config.json agents %v", handle, cfg.Agents)
	if strict {
		return errors.New(msg)
	}
	_ = writeStderr("warning: %s\n", msg)
	return nil
}

// validateKnownHandles validates multiple handles against config.json.
// If strict=true, returns an error for unknown handles or unreadable/corrupt config.
func validateKnownHandles(root string, handles []string, strict bool) error {
	configPath := filepath.Join(root, "meta", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No config, no validation
		}
		// Config exists but unreadable
		msg := fmt.Sprintf("cannot read config.json: %v", err)
		if strict {
			return errors.New(msg)
		}
		_ = writeStderr("warning: %s\n", msg)
		return nil
	}

	var cfg struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		// Config exists but invalid JSON
		msg := fmt.Sprintf("invalid config.json: %v", err)
		if strict {
			return errors.New(msg)
		}
		_ = writeStderr("warning: %s\n", msg)
		return nil
	}

	known := make(map[string]bool, len(cfg.Agents))
	for _, a := range cfg.Agents {
		known[a] = true
	}

	var unknown []string
	for _, h := range handles {
		if !known[h] {
			unknown = append(unknown, h)
		}
	}

	if len(unknown) == 0 {
		return nil
	}

	msg := fmt.Sprintf("unknown handles %v (known: %v)", unknown, cfg.Agents)
	if strict {
		return errors.New(msg)
	}
	_ = writeStderr("warning: %s\n", msg)
	return nil
}

func normalizeHandle(raw string) (string, error) {
	handle := strings.TrimSpace(raw)
	if handle == "" {
		return "", errors.New("agent handle cannot be empty")
	}
	if strings.ContainsAny(handle, "/\\") {
		return "", fmt.Errorf("invalid handle (slashes not allowed): %s", handle)
	}
	if handle != strings.ToLower(handle) {
		return "", fmt.Errorf("handle must be lowercase: %s", handle)
	}
	for _, r := range handle {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' {
			continue
		}
		return "", fmt.Errorf("invalid handle (allowed: a-z, 0-9, -, _): %s", handle)
	}
	return handle, nil
}

func parseHandles(raw string) ([]string, error) {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		handle, err := normalizeHandle(part)
		if err != nil {
			return nil, err
		}
		out = append(out, handle)
	}
	return out, nil
}

func splitRecipients(raw string) ([]string, error) {
	out, err := parseHandles(raw)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("--to is required")
	}
	return out, nil
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func splitList(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func readBody(bodyFlag string) (string, error) {
	if bodyFlag == "" || bodyFlag == "@-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	if strings.HasPrefix(bodyFlag, "@") {
		path := strings.TrimPrefix(bodyFlag, "@")
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return bodyFlag, nil
}

func isHelp(arg string) bool {
	switch arg {
	case "-h", "--help", "help":
		return true
	default:
		return false
	}
}

func parseFlags(fs *flag.FlagSet, args []string, usage func()) (bool, error) {
	fs.SetOutput(io.Discard)
	if usage != nil {
		fs.Usage = usage
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func usageWithFlags(fs *flag.FlagSet, usage string, notes ...string) func() {
	return func() {
		_ = writeStdoutLine("Usage:")
		_ = writeStdoutLine("  " + usage)
		if len(notes) > 0 {
			_ = writeStdoutLine("")
			for _, note := range notes {
				_ = writeStdoutLine(note)
			}
		}
		_ = writeStdoutLine("")
		_ = writeStdoutLine("Options:")
		_ = writeFlagDefaults(fs)
	}
}

func writeFlagDefaults(fs *flag.FlagSet) error {
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
	if buf.Len() == 0 {
		return nil
	}
	return writeStdout("%s", buf.String())
}

func confirmPrompt(prompt string) (bool, error) {
	if err := writeStdout("%s", prompt); err != nil {
		return false, err
	}
	if err := writeStdout(" [y/N]: "); err != nil {
		return false, err
	}
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes", nil
}

func ensureFilename(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("message id is required")
	}
	if id == "." || id == ".." {
		return "", fmt.Errorf("invalid message id: %s", id)
	}
	if filepath.Base(id) != id {
		return "", fmt.Errorf("invalid message id: %s", id)
	}
	if strings.ContainsAny(id, "/\\") {
		return "", fmt.Errorf("invalid message id: %s", id)
	}
	if !strings.HasSuffix(id, ".md") {
		id += ".md"
	}
	return id, nil
}

func ensureSafeBaseName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name cannot be empty")
	}
	if name == "." || name == ".." {
		return "", fmt.Errorf("invalid name: %s", name)
	}
	if filepath.Base(name) != name {
		return "", fmt.Errorf("invalid name: %s", name)
	}
	if strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("invalid name: %s", name)
	}
	return name, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeStdout(format string, args ...any) error {
	_, err := fmt.Fprintf(os.Stdout, format, args...)
	return err
}

func writeStdoutLine(args ...any) error {
	_, err := fmt.Fprintln(os.Stdout, args...)
	return err
}

func writeStderr(format string, args ...any) error {
	_, err := fmt.Fprintf(os.Stderr, format, args...)
	return err
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

```

File: internal/cli/drain_test.go (4517 tokens)
```
package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/ack"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunDrainEmpty(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	t.Run("empty inbox returns empty JSON", func(t *testing.T) {
		result := runDrainJSON(t, root, "alice", 0, false, true)
		if result.Count != 0 {
			t.Errorf("expected count 0, got %d", result.Count)
		}
		if len(result.Drained) != 0 {
			t.Errorf("expected empty drained, got %d items", len(result.Drained))
		}
	})

	t.Run("empty inbox silent in text mode", func(t *testing.T) {
		output := runDrainText(t, root, "alice", 0, false, true)
		if output != "" {
			t.Errorf("expected empty output, got %q", output)
		}
	})
}

func TestRunDrainMovesToCur(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a message
	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "test-msg-1",
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "Test message",
			Created: "2025-12-24T10:00:00Z",
		},
		Body: "Hello Alice!",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "alice", "test-msg-1.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// Verify message is in new
	newPath := filepath.Join(fsq.AgentInboxNew(root, "alice"), "test-msg-1.md")
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("message should be in new: %v", err)
	}

	// Drain
	result := runDrainJSON(t, root, "alice", 0, false, true)
	if result.Count != 1 {
		t.Fatalf("expected count 1, got %d", result.Count)
	}
	if result.Drained[0].ID != "test-msg-1" {
		t.Errorf("expected ID test-msg-1, got %s", result.Drained[0].ID)
	}
	if !result.Drained[0].MovedToCur {
		t.Errorf("expected MovedToCur=true")
	}

	// Verify message moved to cur
	curPath := filepath.Join(fsq.AgentInboxCur(root, "alice"), "test-msg-1.md")
	if _, err := os.Stat(curPath); err != nil {
		t.Errorf("message should be in cur: %v", err)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Errorf("message should NOT be in new anymore")
	}

	// Second drain should return empty
	result2 := runDrainJSON(t, root, "alice", 0, false, true)
	if result2.Count != 0 {
		t.Errorf("second drain should be empty, got %d", result2.Count)
	}
}

func TestRunDrainWithBody(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "body-test",
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "With body",
			Created: "2025-12-24T11:00:00Z",
		},
		Body: "This is the message body.",
	}
	data, _ := msg.Marshal()
	if _, err := fsq.DeliverToInbox(root, "alice", "body-test.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	t.Run("without include-body", func(t *testing.T) {
		// Need to re-create since previous test moved it
		root := t.TempDir()
		if err := fsq.EnsureRootDirs(root); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
			t.Fatal(err)
		}
		if _, err := fsq.DeliverToInbox(root, "alice", "body-test.md", data); err != nil {
			t.Fatal(err)
		}

		result := runDrainJSON(t, root, "alice", 0, false, true)
		if result.Drained[0].Body != "" {
			t.Errorf("expected empty body, got %q", result.Drained[0].Body)
		}
	})

	t.Run("with include-body", func(t *testing.T) {
		root := t.TempDir()
		if err := fsq.EnsureRootDirs(root); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
			t.Fatal(err)
		}
		if _, err := fsq.DeliverToInbox(root, "alice", "body-test.md", data); err != nil {
			t.Fatal(err)
		}

		result := runDrainJSON(t, root, "alice", 0, true, true)
		if result.Drained[0].Body != "This is the message body.\n" {
			t.Errorf("expected body, got %q", result.Drained[0].Body)
		}
	})
}

func TestRunDrainWithAck(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Message that requires ack
	msg := format.Message{
		Header: format.Header{
			Schema:      1,
			ID:          "ack-test",
			From:        "bob",
			To:          []string{"alice"},
			Thread:      "p2p/alice__bob",
			Subject:     "Please ack",
			Created:     "2025-12-24T12:00:00Z",
			AckRequired: true,
		},
		Body: "Ack me!",
	}
	data, _ := msg.Marshal()
	if _, err := fsq.DeliverToInbox(root, "alice", "ack-test.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	result := runDrainJSON(t, root, "alice", 0, false, true)
	if !result.Drained[0].AckRequired {
		t.Errorf("expected AckRequired=true")
	}
	if !result.Drained[0].Acked {
		t.Errorf("expected Acked=true")
	}

	// Check ack file was created
	ackPath := filepath.Join(fsq.AgentAcksSent(root, "alice"), "ack-test.json")
	if _, err := os.Stat(ackPath); err != nil {
		t.Errorf("ack file should exist: %v", err)
	}
}

func TestRunDrainWithoutAck(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:      1,
			ID:          "no-ack-test",
			From:        "bob",
			To:          []string{"alice"},
			Thread:      "p2p/alice__bob",
			Subject:     "No ack",
			Created:     "2025-12-24T12:30:00Z",
			AckRequired: true,
		},
		Body: "No ack please",
	}
	data, _ := msg.Marshal()
	if _, err := fsq.DeliverToInbox(root, "alice", "no-ack-test.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	result := runDrainJSON(t, root, "alice", 0, false, false)
	if !result.Drained[0].AckRequired {
		t.Errorf("expected AckRequired=true")
	}
	if result.Drained[0].Acked {
		t.Errorf("expected Acked=false")
	}

	ackPath := filepath.Join(fsq.AgentAcksSent(root, "alice"), "no-ack-test.json")
	if _, err := os.Stat(ackPath); !os.IsNotExist(err) {
		t.Errorf("ack file should not exist")
	}
}

func TestRunDrainCorruptAckRewritten(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:      1,
			ID:          "corrupt-ack-test",
			From:        "bob",
			To:          []string{"alice"},
			Thread:      "p2p/alice__bob",
			Created:     "2025-12-24T12:40:00Z",
			AckRequired: true,
		},
		Body: "Ack me",
	}
	data, _ := msg.Marshal()
	if _, err := fsq.DeliverToInbox(root, "alice", "corrupt-ack-test.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	ackPath := filepath.Join(fsq.AgentAcksSent(root, "alice"), "corrupt-ack-test.json")
	if err := os.WriteFile(ackPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt ack: %v", err)
	}

	result := runDrainJSON(t, root, "alice", 0, false, true)
	if !result.Drained[0].Acked {
		t.Errorf("expected Acked=true")
	}
	if _, err := os.Stat(ackPath); err != nil {
		t.Fatalf("ack file should exist: %v", err)
	}
	if _, err := ack.Read(ackPath); err != nil {
		t.Fatalf("ack file should be valid json: %v", err)
	}
}

func TestRunDrainLimit(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create 5 messages
	for i := 0; i < 5; i++ {
		msg := format.Message{
			Header: format.Header{
				Schema:  1,
				ID:      "msg-" + string(rune('a'+i)),
				From:    "bob",
				To:      []string{"alice"},
				Thread:  "p2p/alice__bob",
				Subject: "Test " + string(rune('A'+i)),
				Created: "2025-12-24T10:00:0" + string(rune('0'+i)) + "Z",
			},
			Body: "body",
		}
		data, _ := msg.Marshal()
		if _, err := fsq.DeliverToInbox(root, "alice", "msg-"+string(rune('a'+i))+".md", data); err != nil {
			t.Fatalf("deliver msg %d: %v", i, err)
		}
	}

	// Drain with limit 2
	result := runDrainJSON(t, root, "alice", 2, false, true)
	if result.Count != 2 {
		t.Errorf("expected count 2, got %d", result.Count)
	}

	// Verify only 2 moved to cur
	curEntries, _ := os.ReadDir(fsq.AgentInboxCur(root, "alice"))
	if len(curEntries) != 2 {
		t.Errorf("expected 2 in cur, got %d", len(curEntries))
	}

	// Verify 3 still in new
	newEntries, _ := os.ReadDir(fsq.AgentInboxNew(root, "alice"))
	if len(newEntries) != 3 {
		t.Errorf("expected 3 in new, got %d", len(newEntries))
	}
}

func TestRunDrainCorruptMessage(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Write a corrupt message directly
	newDir := fsq.AgentInboxNew(root, "alice")
	corruptPath := filepath.Join(newDir, "corrupt.md")
	if err := os.WriteFile(corruptPath, []byte("not valid frontmatter"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	result := runDrainJSON(t, root, "alice", 0, false, true)
	if result.Count != 1 {
		t.Fatalf("expected count 1, got %d", result.Count)
	}
	if result.Drained[0].ParseError == "" {
		t.Errorf("expected parse error for corrupt message")
	}
	if !result.Drained[0].MovedToCur {
		t.Errorf("corrupt message should still be moved to cur")
	}

	// Verify corrupt message moved to cur
	curPath := filepath.Join(fsq.AgentInboxCur(root, "alice"), "corrupt.md")
	if _, err := os.Stat(curPath); err != nil {
		t.Errorf("corrupt message should be in cur: %v", err)
	}
}

func TestRunDrainSorting(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create messages out of order (filesystem order != timestamp order)
	timestamps := []string{
		"2025-12-24T10:00:03Z",
		"2025-12-24T10:00:01Z",
		"2025-12-24T10:00:02Z",
	}
	for i, ts := range timestamps {
		msg := format.Message{
			Header: format.Header{
				Schema:  1,
				ID:      "msg-" + string(rune('a'+i)),
				From:    "bob",
				To:      []string{"alice"},
				Thread:  "p2p/alice__bob",
				Created: ts,
			},
			Body: "body",
		}
		data, _ := msg.Marshal()
		if _, err := fsq.DeliverToInbox(root, "alice", "msg-"+string(rune('a'+i))+".md", data); err != nil {
			t.Fatalf("deliver msg %d: %v", i, err)
		}
	}

	result := runDrainJSON(t, root, "alice", 0, false, true)
	if result.Count != 3 {
		t.Fatalf("expected 3, got %d", result.Count)
	}

	// Should be sorted by timestamp: b (01), c (02), a (03)
	expected := []string{"msg-b", "msg-c", "msg-a"}
	for i, exp := range expected {
		if result.Drained[i].ID != exp {
			t.Errorf("position %d: expected %s, got %s", i, exp, result.Drained[i].ID)
		}
	}
}

func runDrainJSON(t *testing.T, root, agent string, limit int, includeBody, ack bool) drainResult {
	t.Helper()
	args := []string{"--root", root, "--me", agent, "--json"}
	if limit > 0 {
		args = append(args, "--limit", itoa(limit))
	}
	if includeBody {
		args = append(args, "--include-body")
	}
	if !ack {
		args = append(args, "--ack=false")
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runDrain(args)

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runDrain: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var result drainResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v (output: %s)", err, buf.String())
	}
	return result
}

func runDrainText(t *testing.T, root, agent string, limit int, includeBody, ack bool) string {
	t.Helper()
	args := []string{"--root", root, "--me", agent}
	if limit > 0 {
		args = append(args, "--limit", itoa(limit))
	}
	if includeBody {
		args = append(args, "--include-body")
	}
	if !ack {
		args = append(args, "--ack=false")
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runDrain(args)

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runDrain: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String()
}

```

File: internal/cli/drain.go (2219 tokens)
```
package cli

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/ack"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type drainItem struct {
	ID          string    `json:"id"`
	From        string    `json:"from"`
	To          []string  `json:"to"`
	Thread      string    `json:"thread"`
	Subject     string    `json:"subject"`
	Created     string    `json:"created"`
	Body        string    `json:"body,omitempty"`
	AckRequired bool      `json:"ack_required"`
	MovedToCur  bool      `json:"moved_to_cur"`
	Acked       bool      `json:"acked"`
	ParseError  string    `json:"parse_error,omitempty"`
	Filename    string    `json:"-"` // actual filename on disk
	SortKey     time.Time `json:"-"`
}

type drainResult struct {
	Drained []drainItem `json:"drained"`
	Count   int         `json:"count"`
}

func runDrain(args []string) error {
	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	common := addCommonFlags(fs)
	limitFlag := fs.Int("limit", 20, "Max messages to drain (0 = no limit)")
	includeBodyFlag := fs.Bool("include-body", false, "Include message body in output")
	ackFlag := fs.Bool("ack", true, "Acknowledge messages that require ack")

	usage := usageWithFlags(fs, "amq drain --me <agent> [options]",
		"Drains new messages: reads, moves to cur, optionally acks.",
		"Designed for hook/script integration. Quiet when empty.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)

	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	newDir := fsq.AgentInboxNew(root, common.Me)
	entries, err := os.ReadDir(newDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No inbox/new directory - nothing to drain
			if common.JSON {
				return writeJSON(os.Stdout, drainResult{Drained: []drainItem{}, Count: 0})
			}
			// Silent for text mode when empty (hook-friendly)
			return nil
		}
		return err
	}

	// Collect items with timestamps for sorting
	items := make([]drainItem, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := entry.Name()
		path := filepath.Join(newDir, filename)

		item := drainItem{
			ID:       filename, // fallback to filename if parse fails
			Filename: filename,
		}

		// Try to parse the message
		var header format.Header
		var body string
		var parseErr error

		if *includeBodyFlag {
			msg, err := format.ReadMessageFile(path)
			if err != nil {
				parseErr = err
			} else {
				header = msg.Header
				body = msg.Body
			}
		} else {
			header, parseErr = format.ReadHeaderFile(path)
		}

		if parseErr != nil {
			item.ParseError = parseErr.Error()
			// Still move corrupt message to cur to avoid reprocessing
		} else {
			item.ID = header.ID
			item.From = header.From
			item.To = header.To
			item.Thread = header.Thread
			item.Subject = header.Subject
			item.Created = header.Created
			item.AckRequired = header.AckRequired
			if *includeBodyFlag {
				item.Body = body
			}
			if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
				item.SortKey = ts
			}
		}

		items = append(items, item)
	}

	// Sort by timestamp (oldest first)
	sort.Slice(items, func(i, j int) bool {
		if !items[i].SortKey.IsZero() && !items[j].SortKey.IsZero() {
			if items[i].SortKey.Equal(items[j].SortKey) {
				return items[i].ID < items[j].ID
			}
			return items[i].SortKey.Before(items[j].SortKey)
		}
		if items[i].Created == items[j].Created {
			return items[i].ID < items[j].ID
		}
		return items[i].Created < items[j].Created
	})

	// Apply limit
	if *limitFlag > 0 && len(items) > *limitFlag {
		items = items[:*limitFlag]
	}

	// Nothing to drain
	if len(items) == 0 {
		if common.JSON {
			return writeJSON(os.Stdout, drainResult{Drained: []drainItem{}, Count: 0})
		}
		// Silent for text mode when empty (hook-friendly)
		return nil
	}

	// Process each message: move to cur, optionally ack
	for i := range items {
		item := &items[i]

		// Move new -> cur
		if err := fsq.MoveNewToCur(root, common.Me, item.Filename); err != nil {
			if os.IsNotExist(err) {
				// Likely moved by another drain; check if it's already in cur.
				curPath := filepath.Join(fsq.AgentInboxCur(root, common.Me), item.Filename)
				if _, statErr := os.Stat(curPath); statErr == nil {
					item.MovedToCur = true
				} else if statErr != nil && !os.IsNotExist(statErr) {
					_ = writeStderr("warning: failed to stat %s in cur: %v\n", item.Filename, statErr)
				}
			} else {
				// Log warning but continue
				_ = writeStderr("warning: failed to move %s to cur: %v\n", item.Filename, err)
			}
		} else {
			item.MovedToCur = true
		}

		// Ack if required and --ack is set
		if *ackFlag && item.AckRequired && item.ParseError == "" && item.MovedToCur {
			if err := ackMessage(root, common.Me, item); err != nil {
				_ = writeStderr("warning: failed to ack %s: %v\n", item.ID, err)
			} else {
				item.Acked = true
			}
		}
	}

	if common.JSON {
		return writeJSON(os.Stdout, drainResult{Drained: items, Count: len(items)})
	}

	// Text output
	if err := writeStdout("[AMQ] %d new message(s) for %s:\n\n", len(items), common.Me); err != nil {
		return err
	}
	for _, item := range items {
		if item.ParseError != "" {
			if err := writeStdout("- ID: %s\n  ERROR: %s\n---\n", item.ID, item.ParseError); err != nil {
				return err
			}
			continue
		}
		subject := item.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		if err := writeStdout("- From: %s\n  Thread: %s\n  ID: %s\n  Subject: %s\n  Created: %s\n",
			item.From, item.Thread, item.ID, subject, item.Created); err != nil {
			return err
		}
		if *includeBodyFlag && item.Body != "" {
			if err := writeStdout("  Body:\n%s\n", item.Body); err != nil {
				return err
			}
		}
		if err := writeStdout("---\n"); err != nil {
			return err
		}
	}
	return nil
}

func ackMessage(root, me string, item *drainItem) error {
	sender, err := normalizeHandle(item.From)
	if err != nil {
		return err
	}
	msgID, err := ensureSafeBaseName(item.ID)
	if err != nil {
		return err
	}

	ackPayload := ack.New(item.ID, item.Thread, me, sender, time.Now())

	// Write to receiver's sent acks
	receiverDir := fsq.AgentAcksSent(root, me)
	receiverPath := filepath.Join(receiverDir, msgID+".json")
	needsReceiverWrite := true
	if existing, err := ack.Read(receiverPath); err == nil {
		ackPayload = existing
		needsReceiverWrite = false
	} else if !os.IsNotExist(err) {
		// Corrupt ack file - warn and rewrite
		_ = writeStderr("warning: corrupt ack file, rewriting: %v\n", err)
	}

	data, err := ackPayload.Marshal()
	if err != nil {
		return err
	}

	if needsReceiverWrite {
		if _, err := fsq.WriteFileAtomic(receiverDir, msgID+".json", data, 0o600); err != nil {
			return err
		}
	}

	// Best-effort write to sender's received acks
	senderDir := fsq.AgentAcksReceived(root, sender)
	senderPath := filepath.Join(senderDir, msgID+".json")
	if _, err := os.Stat(senderPath); err == nil {
		// Already recorded.
	} else if os.IsNotExist(err) {
		if _, err := fsq.WriteFileAtomic(senderDir, msgID+".json", data, 0o600); err != nil {
			_ = writeStderr("warning: unable to write sender ack: %v\n", err)
		}
	} else if err != nil {
		return err
	}

	return nil
}

```

File: internal/cli/init.go (444 tokens)
```
package cli

import (
	"errors"
	"flag"
	"path/filepath"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	rootFlag := fs.String("root", defaultRoot(), "Root directory for the queue")
	agentsFlag := fs.String("agents", "", "Comma-separated agent handles (required)")
	forceFlag := fs.Bool("force", false, "Overwrite existing config.json if present")

	usage := usageWithFlags(fs, "amq init --root <path> --agents a,b,c [--force]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	agents, err := parseHandles(*agentsFlag)
	if err != nil {
		return err
	}
	if len(agents) == 0 {
		return errors.New("--agents is required")
	}
	agents = dedupeStrings(agents)
	sort.Strings(agents)

	root := filepath.Clean(*rootFlag)
	if err := fsq.EnsureRootDirs(root); err != nil {
		return err
	}

	for _, agent := range agents {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			return err
		}
	}

	cfgPath := filepath.Join(root, "meta", "config.json")
	cfg := config.Config{
		Version:    format.CurrentVersion,
		CreatedUTC: time.Now().UTC().Format(time.RFC3339),
		Agents:     agents,
	}
	if err := config.WriteConfig(cfgPath, cfg, *forceFlag); err != nil {
		return err
	}

	if err := writeStdout("Initialized AMQ root at %s\n", root); err != nil {
		return err
	}
	return nil
}

```

File: internal/cli/list_test.go (1197 tokens)
```
package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunListPagination(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create 5 messages with distinct timestamps
	for i := 0; i < 5; i++ {
		msg := format.Message{
			Header: format.Header{
				Schema:  1,
				ID:      "msg-" + string(rune('a'+i)),
				From:    "bob",
				To:      []string{"alice"},
				Thread:  "p2p/alice__bob",
				Subject: "Test " + string(rune('A'+i)),
				Created: "2025-12-24T10:00:0" + string(rune('0'+i)) + "Z",
			},
			Body: "body",
		}
		data, err := msg.Marshal()
		if err != nil {
			t.Fatalf("marshal msg %d: %v", i, err)
		}
		if _, err := fsq.DeliverToInbox(root, "alice", "msg-"+string(rune('a'+i))+".md", data); err != nil {
			t.Fatalf("deliver msg %d: %v", i, err)
		}
	}

	t.Run("no pagination returns all", func(t *testing.T) {
		items := runListJSON(t, root, "alice", 0, 0)
		if len(items) != 5 {
			t.Errorf("expected 5 items, got %d", len(items))
		}
	})

	t.Run("limit 2 returns first 2", func(t *testing.T) {
		items := runListJSON(t, root, "alice", 2, 0)
		if len(items) != 2 {
			t.Errorf("expected 2 items, got %d", len(items))
		}
		if items[0].ID != "msg-a" {
			t.Errorf("expected first item msg-a, got %s", items[0].ID)
		}
		if items[1].ID != "msg-b" {
			t.Errorf("expected second item msg-b, got %s", items[1].ID)
		}
	})

	t.Run("offset 2 skips first 2", func(t *testing.T) {
		items := runListJSON(t, root, "alice", 0, 2)
		if len(items) != 3 {
			t.Errorf("expected 3 items, got %d", len(items))
		}
		if items[0].ID != "msg-c" {
			t.Errorf("expected first item msg-c, got %s", items[0].ID)
		}
	})

	t.Run("limit 2 offset 2 returns middle 2", func(t *testing.T) {
		items := runListJSON(t, root, "alice", 2, 2)
		if len(items) != 2 {
			t.Errorf("expected 2 items, got %d", len(items))
		}
		if items[0].ID != "msg-c" {
			t.Errorf("expected first item msg-c, got %s", items[0].ID)
		}
		if items[1].ID != "msg-d" {
			t.Errorf("expected second item msg-d, got %s", items[1].ID)
		}
	})

	t.Run("offset beyond range returns empty", func(t *testing.T) {
		items := runListJSON(t, root, "alice", 0, 100)
		if len(items) != 0 {
			t.Errorf("expected 0 items, got %d", len(items))
		}
	})
}

func runListJSON(t *testing.T, root, agent string, limit, offset int) []listItem {
	t.Helper()
	args := []string{"--root", root, "--me", agent, "--json", "--new"}
	if limit > 0 {
		args = append(args, "--limit", itoa(limit))
	}
	if offset > 0 {
		args = append(args, "--offset", itoa(offset))
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runList(args)

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runList: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var items []listItem
	if err := json.Unmarshal(buf.Bytes(), &items); err != nil {
		t.Fatalf("unmarshal: %v (output: %s)", err, buf.String())
	}
	return items
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

```

File: internal/cli/list.go (1140 tokens)
```
package cli

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type listItem struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`
	Subject string    `json:"subject"`
	Thread  string    `json:"thread"`
	Created string    `json:"created"`
	Box     string    `json:"box"`
	Path    string    `json:"path"`
	SortKey time.Time `json:"-"`
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	common := addCommonFlags(fs)
	newFlag := fs.Bool("new", false, "List messages in inbox/new")
	curFlag := fs.Bool("cur", false, "List messages in inbox/cur")
	limitFlag := fs.Int("limit", 0, "Limit number of messages (0 = no limit)")
	offsetFlag := fs.Int("offset", 0, "Offset into sorted results (0 = start)")

	usage := usageWithFlags(fs, "amq list --me <agent> [--new | --cur] [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)

	// Validate handle against config.json
	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	box := "new"
	if *newFlag && *curFlag {
		return errors.New("use only one of --new or --cur")
	}
	if *curFlag {
		box = "cur"
	}
	if *limitFlag < 0 {
		return errors.New("--limit must be >= 0")
	}
	if *offsetFlag < 0 {
		return errors.New("--offset must be >= 0")
	}

	var dir string
	if box == "new" {
		dir = fsq.AgentInboxNew(root, common.Me)
	} else {
		dir = fsq.AgentInboxCur(root, common.Me)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if common.JSON {
				return writeJSON(os.Stdout, []listItem{})
			}
			if err := writeStdoutLine("No messages."); err != nil {
				return err
			}
			return nil
		}
		return err
	}

	items := make([]listItem, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		header, err := format.ReadHeaderFile(path)
		if err != nil {
			if err := writeStderr("warning: skipping corrupt message %s: %v\n", entry.Name(), err); err != nil {
				return err
			}
			continue
		}
		item := listItem{
			ID:      header.ID,
			From:    header.From,
			Subject: header.Subject,
			Thread:  header.Thread,
			Created: header.Created,
			Box:     box,
			Path:    path,
		}
		if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
			item.SortKey = ts
		}
		items = append(items, item)
	}

	sort.Slice(items, func(i, j int) bool {
		if !items[i].SortKey.IsZero() && !items[j].SortKey.IsZero() {
			if items[i].SortKey.Equal(items[j].SortKey) {
				return items[i].ID < items[j].ID
			}
			return items[i].SortKey.Before(items[j].SortKey)
		}
		if items[i].Created == items[j].Created {
			return items[i].ID < items[j].ID
		}
		return items[i].Created < items[j].Created
	})

	if *offsetFlag > 0 {
		if *offsetFlag >= len(items) {
			items = []listItem{}
		} else {
			items = items[*offsetFlag:]
		}
	}
	if *limitFlag > 0 && len(items) > *limitFlag {
		items = items[:*limitFlag]
	}

	if common.JSON {
		return writeJSON(os.Stdout, items)
	}

	if len(items) == 0 {
		if err := writeStdoutLine("No messages."); err != nil {
			return err
		}
		return nil
	}
	for _, item := range items {
		subject := item.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		if err := writeStdout("%s  %s  %s  %s\n", item.Created, item.From, item.ID, strings.TrimSpace(subject)); err != nil {
			return err
		}
	}
	return nil
}

```

File: internal/cli/presence.go (963 tokens)
```
package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

func runPresence(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		printPresenceUsage()
		return nil
	}
	switch args[0] {
	case "set":
		return runPresenceSet(args[1:])
	case "list":
		return runPresenceList(args[1:])
	default:
		return fmt.Errorf("unknown presence command: %s", args[0])
	}
}

func runPresenceSet(args []string) error {
	fs := flag.NewFlagSet("presence set", flag.ContinueOnError)
	common := addCommonFlags(fs)
	statusFlag := fs.String("status", "", "Status string")
	noteFlag := fs.String("note", "", "Optional note")
	usage := usageWithFlags(fs, "amq presence set --me <agent> --status <status> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)

	// Validate handle against config.json
	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	status := strings.TrimSpace(*statusFlag)
	if status == "" {
		return fmt.Errorf("--status is required")
	}
	p := presence.New(common.Me, status, strings.TrimSpace(*noteFlag), time.Now())
	if err := presence.Write(root, p); err != nil {
		return err
	}
	if common.JSON {
		return writeJSON(os.Stdout, p)
	}
	if err := writeStdout("Presence updated for %s\n", common.Me); err != nil {
		return err
	}
	return nil
}

func runPresenceList(args []string) error {
	fs := flag.NewFlagSet("presence list", flag.ContinueOnError)
	common := addCommonFlags(fs)
	usage := usageWithFlags(fs, "amq presence list [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	root := filepath.Clean(common.Root)

	var agents []string
	if cfg, err := config.LoadConfig(filepath.Join(root, "meta", "config.json")); err == nil {
		agents = cfg.Agents
	} else {
		agents, _ = fsq.ListAgents(root)
	}

	items := make([]presence.Presence, 0, len(agents))
	for _, raw := range agents {
		agent, err := normalizeHandle(raw)
		if err != nil {
			if err := writeStderr("warning: skipping invalid handle %s: %v\n", raw, err); err != nil {
				return err
			}
			continue
		}
		p, err := presence.Read(root, agent)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		items = append(items, p)
	}

	if common.JSON {
		return writeJSON(os.Stdout, items)
	}
	if len(items) == 0 {
		if err := writeStdoutLine("No presence data."); err != nil {
			return err
		}
		return nil
	}
	for _, item := range items {
		if err := writeStdout("%s  %s  %s\n", item.Handle, item.Status, item.LastSeen); err != nil {
			return err
		}
		if item.Note != "" {
			if err := writeStdout("  %s\n", item.Note); err != nil {
				return err
			}
		}
	}
	return nil
}

func printPresenceUsage() {
	_ = writeStdoutLine("amq presence <command> [options]")
	_ = writeStdoutLine("")
	_ = writeStdoutLine("Commands:")
	_ = writeStdoutLine("  set   Update presence")
	_ = writeStdoutLine("  list  List presence")
}

```

File: internal/cli/read.go (464 tokens)
```
package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runRead(args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Message id")

	usage := usageWithFlags(fs, "amq read --me <agent> --id <msg_id> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)

	// Validate handle against config.json
	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	filename, err := ensureFilename(*idFlag)
	if err != nil {
		return err
	}

	path, box, err := fsq.FindMessage(root, common.Me, filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("message not found: %s", *idFlag)
		}
		return err
	}

	if box == fsq.BoxNew {
		if err := fsq.MoveNewToCur(root, common.Me, filename); err != nil {
			return err
		}
		path = filepath.Join(fsq.AgentInboxCur(root, common.Me), filename)
	}

	msg, err := format.ReadMessageFile(path)
	if err != nil {
		return err
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"header": msg.Header,
			"body":   msg.Body,
		})
	}

	if err := writeStdout("%s", msg.Body); err != nil {
		return err
	}
	return nil
}

```

File: internal/cli/send.go (1003 tokens)
```
package cli

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	common := addCommonFlags(fs)
	toFlag := fs.String("to", "", "Receiver handle (comma-separated)")
	subjectFlag := fs.String("subject", "", "Message subject")
	threadFlag := fs.String("thread", "", "Thread id (optional; default p2p/<a>__<b>)")
	bodyFlag := fs.String("body", "", "Body string, @file, or empty to read stdin")
	ackFlag := fs.Bool("ack", false, "Request ack")
	refsFlag := fs.String("refs", "", "Comma-separated related message ids")

	usage := usageWithFlags(fs, "amq send --me <agent> --to <recipients> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)
	recipients, err := splitRecipients(*toFlag)
	if err != nil {
		return err
	}
	recipients = dedupeStrings(recipients)

	// Validate handles against config.json
	allHandles := append([]string{me}, recipients...)
	if err := validateKnownHandles(root, allHandles, common.Strict); err != nil {
		return err
	}

	body, err := readBody(*bodyFlag)
	if err != nil {
		return err
	}

	threadID := strings.TrimSpace(*threadFlag)
	if threadID == "" {
		if len(recipients) == 1 {
			threadID = canonicalP2P(common.Me, recipients[0])
		} else {
			return errors.New("--thread is required when sending to multiple recipients")
		}
	}

	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return err
	}

	msg := format.Message{
		Header: format.Header{
			Schema:      format.CurrentSchema,
			ID:          id,
			From:        common.Me,
			To:          recipients,
			Thread:      threadID,
			Subject:     strings.TrimSpace(*subjectFlag),
			Created:     now.UTC().Format(time.RFC3339Nano),
			AckRequired: *ackFlag,
			Refs:        splitList(*refsFlag),
		},
		Body: body,
	}

	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	filename := id + ".md"
	// Deliver to each recipient.
	if _, err := fsq.DeliverToInboxes(root, recipients, filename, data); err != nil {
		return err
	}

	// Copy to sender outbox/sent for audit.
	outboxDir := fsq.AgentOutboxSent(root, common.Me)
	outboxErr := error(nil)
	if _, err := fsq.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"id":      id,
			"thread":  threadID,
			"to":      recipients,
			"subject": msg.Header.Subject,
			"outbox": map[string]any{
				"written": outboxErr == nil,
				"error":   errString(outboxErr),
			},
		})
	}
	if outboxErr != nil {
		if err := writeStderr("warning: outbox write failed: %v\n", outboxErr); err != nil {
			return err
		}
	}
	if err := writeStdout("Sent %s to %s\n", id, strings.Join(recipients, ",")); err != nil {
		return err
	}
	return nil
}

func canonicalP2P(a, b string) string {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == b {
		return "p2p/" + a + "__" + b
	}
	if a < b {
		return "p2p/" + a + "__" + b
	}
	return "p2p/" + b + "__" + a
}

```

File: internal/cli/thread.go (687 tokens)
```
package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

func runThread(args []string) error {
	fs := flag.NewFlagSet("thread", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Thread id")
	agentsFlag := fs.String("agents", "", "Comma-separated agent handles (optional)")
	includeBody := fs.Bool("include-body", false, "Include body in output")
	limitFlag := fs.Int("limit", 0, "Limit number of messages (0 = no limit)")

	usage := usageWithFlags(fs, "amq thread --id <thread_id> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	threadID := strings.TrimSpace(*idFlag)
	if threadID == "" {
		return fmt.Errorf("--id is required")
	}
	if *limitFlag < 0 {
		return fmt.Errorf("--limit must be >= 0")
	}
	root := filepath.Clean(common.Root)

	agents, err := parseHandles(*agentsFlag)
	if err != nil {
		return err
	}
	if len(agents) == 0 {
		if cfg, err := config.LoadConfig(filepath.Join(root, "meta", "config.json")); err == nil {
			agents, err = parseHandles(strings.Join(cfg.Agents, ","))
			if err != nil {
				return err
			}
		} else {
			var listErr error
			agents, listErr = fsq.ListAgents(root)
			if listErr != nil {
				return fmt.Errorf("list agents: %w", listErr)
			}
		}
	}
	if len(agents) == 0 {
		return fmt.Errorf("no agents found; provide --agents")
	}

	entries, err := thread.Collect(root, threadID, agents, *includeBody, func(path string, parseErr error) error {
		return writeStderr("warning: skipping corrupt message %s: %v\n", filepath.Base(path), parseErr)
	})
	if err != nil {
		return err
	}
	if *limitFlag > 0 && len(entries) > *limitFlag {
		entries = entries[len(entries)-*limitFlag:]
	}

	if common.JSON {
		return writeJSON(os.Stdout, entries)
	}

	for _, entry := range entries {
		subject := entry.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		if err := writeStdout("%s  %s  %s\n", entry.Created, entry.From, subject); err != nil {
			return err
		}
		if *includeBody {
			if err := writeStdoutLine(entry.Body); err != nil {
				return err
			}
			if err := writeStdoutLine("---"); err != nil {
				return err
			}
		}
	}
	return nil
}

```

File: internal/cli/watch_test.go (1487 tokens)
```
package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunWatchExistingMessages(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a message before watching
	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-existing",
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "Existing message",
			Created: "2025-12-25T10:00:00Z",
		},
		Body: "This message exists before watch",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "alice", "msg-existing.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err = runWatch([]string{"--root", root, "--me", "alice", "--json", "--timeout", "1s"})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runWatch: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var result watchResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v (output: %s)", err, buf.String())
	}

	if result.Event != "existing" {
		t.Errorf("expected event 'existing', got %s", result.Event)
	}
	if len(result.Messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(result.Messages))
	}
	if result.Messages[0].ID != "msg-existing" {
		t.Errorf("expected message ID 'msg-existing', got %s", result.Messages[0].ID)
	}
}

func TestRunWatchTimeout(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	start := time.Now()
	err := runWatch([]string{"--root", root, "--me", "alice", "--json", "--timeout", "100ms"})
	elapsed := time.Since(start)

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runWatch: %v", err)
	}

	// Should timeout around 100ms
	if elapsed < 90*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("expected timeout around 100ms, got %v", elapsed)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var result watchResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v (output: %s)", err, buf.String())
	}

	if result.Event != "timeout" {
		t.Errorf("expected event 'timeout', got %s", result.Event)
	}
}

func TestRunWatchNewMessage(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	var wg sync.WaitGroup
	var watchErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Use polling mode for test reliability
		watchErr = runWatch([]string{"--root", root, "--me", "alice", "--json", "--timeout", "5s", "--poll"})
	}()

	// Wait a bit then send a message
	time.Sleep(200 * time.Millisecond)

	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-new",
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "New message",
			Created: "2025-12-25T10:01:00Z",
		},
		Body: "This is a new message",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "alice", "msg-new.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	wg.Wait()

	_ = w.Close()
	os.Stdout = oldStdout

	if watchErr != nil {
		t.Fatalf("runWatch: %v", watchErr)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var result watchResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v (output: %s)", err, buf.String())
	}

	if result.Event != "new_message" {
		t.Errorf("expected event 'new_message', got %s", result.Event)
	}
	if len(result.Messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(result.Messages))
	}
	if len(result.Messages) > 0 && result.Messages[0].ID != "msg-new" {
		t.Errorf("expected message ID 'msg-new', got %s", result.Messages[0].ID)
	}
}

```

File: internal/cli/watch.go (1604 tokens)
```
package cli

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
)

type watchResult struct {
	Event    string    `json:"event"`
	Messages []msgInfo `json:"messages,omitempty"`
}

type msgInfo struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	Subject string `json:"subject"`
	Thread  string `json:"thread"`
	Created string `json:"created"`
	Path    string `json:"path"`
}

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	common := addCommonFlags(fs)
	timeoutFlag := fs.Duration("timeout", 60*time.Second, "Maximum time to wait for messages (0 = wait forever)")
	pollFlag := fs.Bool("poll", false, "Use polling fallback instead of fsnotify (for network filesystems)")

	usage := usageWithFlags(fs, "amq watch --me <agent> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me

	root := filepath.Clean(common.Root)

	// Validate handle against config.json
	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	inboxNew := fsq.AgentInboxNew(root, common.Me)

	// Ensure inbox directory exists
	if err := os.MkdirAll(inboxNew, 0o700); err != nil {
		return err
	}

	// Set up context with timeout
	ctx := context.Background()
	if *timeoutFlag > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeoutFlag)
		defer cancel()
	}

	// Watch for new messages (includes initial check after watcher setup)
	var messages []msgInfo
	var event string
	var watchErr error

	if *pollFlag {
		messages, event, watchErr = watchWithPolling(ctx, inboxNew)
	} else {
		messages, event, watchErr = watchWithFsnotify(ctx, inboxNew)
	}

	if watchErr != nil {
		if errors.Is(watchErr, context.DeadlineExceeded) {
			return outputWatchResult(common.JSON, "timeout", nil)
		}
		return watchErr
	}

	return outputWatchResult(common.JSON, event, messages)
}

func watchWithFsnotify(ctx context.Context, inboxNew string) ([]msgInfo, string, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		// Fall back to polling if fsnotify fails
		return watchWithPolling(ctx, inboxNew)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(inboxNew); err != nil {
		return watchWithPolling(ctx, inboxNew)
	}

	// Check for existing messages AFTER watcher is set up to avoid race condition.
	// Any message arriving after this check will trigger a watcher event.
	existing, err := listNewMessages(inboxNew)
	if err != nil {
		return nil, "", err
	}
	if len(existing) > 0 {
		return existing, "existing", nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case event, ok := <-watcher.Events:
			if !ok {
				return nil, "", errors.New("watcher closed")
			}
			// Only care about new files (Create or Rename into directory)
			if event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
				// Small delay to ensure file is fully written
				time.Sleep(10 * time.Millisecond)
				messages, err := listNewMessages(inboxNew)
				if err != nil {
					return nil, "", err
				}
				if len(messages) > 0 {
					return messages, "new_message", nil
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil, "", errors.New("watcher closed")
			}
			return nil, "", err
		}
	}
}

func watchWithPolling(ctx context.Context, inboxNew string) ([]msgInfo, string, error) {
	// Check for existing messages first
	existing, err := listNewMessages(inboxNew)
	if err != nil {
		return nil, "", err
	}
	if len(existing) > 0 {
		return existing, "existing", nil
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-ticker.C:
			messages, err := listNewMessages(inboxNew)
			if err != nil {
				return nil, "", err
			}
			if len(messages) > 0 {
				return messages, "new_message", nil
			}
		}
	}
}

func listNewMessages(inboxNew string) ([]msgInfo, error) {
	entries, err := os.ReadDir(inboxNew)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var messages []msgInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(inboxNew, entry.Name())
		header, err := format.ReadHeaderFile(path)
		if err != nil {
			continue // Skip corrupt messages
		}

		messages = append(messages, msgInfo{
			ID:      header.ID,
			From:    header.From,
			Subject: header.Subject,
			Thread:  header.Thread,
			Created: header.Created,
			Path:    path,
		})
	}

	// Sort by creation time
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Created < messages[j].Created
	})

	return messages, nil
}

func outputWatchResult(jsonOutput bool, event string, messages []msgInfo) error {
	result := watchResult{
		Event:    event,
		Messages: messages,
	}

	if jsonOutput {
		return writeJSON(os.Stdout, result)
	}

	switch event {
	case "timeout":
		return writeStdoutLine("No new messages (timeout)")
	case "existing":
		if err := writeStdoutLine("Found existing messages:"); err != nil {
			return err
		}
	case "new_message":
		if err := writeStdoutLine("New message(s) received:"); err != nil {
			return err
		}
	}

	for _, msg := range messages {
		subject := msg.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		if err := writeStdout("  %s  %s  %s  %s\n", msg.Created, msg.From, msg.ID, subject); err != nil {
			return err
		}
	}

	return nil
}

```

File: internal/config/config_test.go (251 tokens)
```
package config

import (
	"path/filepath"
	"testing"
)

func TestConfigWriteRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "meta", "config.json")
	cfg := Config{
		Version:    1,
		CreatedUTC: "2025-12-24T15:02:33Z",
		Agents:     []string{"codex", "cloudcode"},
	}
	if err := WriteConfig(path, cfg, false); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(loaded.Agents) != 2 || loaded.Agents[0] != "codex" {
		t.Fatalf("unexpected agents: %+v", loaded.Agents)
	}
	if err := WriteConfig(path, cfg, false); err == nil {
		t.Fatalf("expected error on overwrite without force")
	}
	if err := WriteConfig(path, cfg, true); err != nil {
		t.Fatalf("WriteConfig with force: %v", err)
	}
}

```

File: internal/config/config.go (270 tokens)
```
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Config is persisted to meta/config.json and captures the initial setup.
type Config struct {
	Version    int      `json:"version"`
	CreatedUTC string   `json:"created_utc"`
	Agents     []string `json:"agents"`
}

func WriteConfig(path string, cfg Config, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("config already exists at %s (use --force to overwrite)", path)
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), data, 0o600)
	return err
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

```

File: internal/format/doc.go (35 tokens)
```
// Package format handles message serialization using JSON frontmatter
// with Markdown body content. It provides message ID generation,
// parsing, and timestamp-based sorting utilities.
package format

```

File: internal/format/id_test.go (150 tokens)
```
package format

import (
	"strings"
	"testing"
	"time"
)

func TestNewMessageID(t *testing.T) {
	id, err := NewMessageID(time.Date(2025, 12, 24, 15, 2, 33, 123000000, time.UTC))
	if err != nil {
		t.Fatalf("NewMessageID: %v", err)
	}
	if !strings.Contains(id, "_pid") {
		t.Fatalf("expected pid marker in id: %s", id)
	}
	if !strings.HasPrefix(id, "2025-12-24T15-02-33.123Z_") {
		t.Fatalf("unexpected prefix: %s", id)
	}
}

```

File: internal/format/id.go (167 tokens)
```
package format

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

func NewMessageID(now time.Time) (string, error) {
	stamp := now.UTC().Format("2006-01-02T15-04-05.000Z")
	pid := os.Getpid()
	suffix, err := randSuffix(4)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s_pid%d_%s", stamp, pid, suffix), nil
}

func randSuffix(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return strings.ToLower(hex.EncodeToString(buf)), nil
}

```

File: internal/format/message_test.go (262 tokens)
```
package format

import (
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:      1,
			ID:          "2025-12-24T15-02-33.123Z_pid1234_abcd",
			From:        "codex",
			To:          []string{"cloudcode"},
			Thread:      "p2p/cloudcode__codex",
			Subject:     "Hello",
			Created:     "2025-12-24T15:02:33.123Z",
			AckRequired: true,
			Refs:        []string{"ref1"},
		},
		Body: "Line one\nLine two\n",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := ParseMessage(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Header.ID != msg.Header.ID {
		t.Fatalf("id mismatch: %s", parsed.Header.ID)
	}
	if parsed.Body != msg.Body {
		t.Fatalf("body mismatch: %q", parsed.Body)
	}
}

```

File: internal/format/message.go (1391 tokens)
```
package format

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Schema and version constants.
const (
	CurrentSchema  = 1
	CurrentVersion = 1
)

const (
	frontmatterStartLine = "---json"
	frontmatterStart     = frontmatterStartLine + "\n"
	frontmatterEnd       = "\n---\n"
)

// Sentinel errors for message parsing.
var (
	ErrMissingFrontmatterStart = errors.New("missing frontmatter start")
	ErrMissingFrontmatterEnd   = errors.New("missing frontmatter end")
)

// Header is the JSON frontmatter stored at the top of each message file.
type Header struct {
	Schema      int      `json:"schema"`
	ID          string   `json:"id"`
	From        string   `json:"from"`
	To          []string `json:"to"`
	Thread      string   `json:"thread"`
	Subject     string   `json:"subject,omitempty"`
	Created     string   `json:"created"`
	AckRequired bool     `json:"ack_required"`
	Refs        []string `json:"refs,omitempty"`
}

// Message is the in-memory representation of a message file.
type Message struct {
	Header Header
	Body   string
}

func (m Message) Marshal() ([]byte, error) {
	if m.Header.Schema == 0 {
		m.Header.Schema = CurrentSchema
	}
	if m.Header.Created == "" {
		m.Header.Created = time.Now().UTC().Format(time.RFC3339Nano)
	}
	b, err := json.MarshalIndent(m.Header, "", "  ")
	if err != nil {
		return nil, err
	}
	body := m.Body
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	out := make([]byte, 0, len(frontmatterStart)+len(b)+len(frontmatterEnd)+len(body))
	out = append(out, frontmatterStart...)
	out = append(out, b...)
	out = append(out, frontmatterEnd...)
	out = append(out, body...)
	return out, nil
}

func ParseMessage(data []byte) (Message, error) {
	headerBytes, body, err := splitFrontmatter(data)
	if err != nil {
		return Message{}, err
	}
	var header Header
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Message{}, fmt.Errorf("parse frontmatter: %w", err)
	}
	return Message{Header: header, Body: string(body)}, nil
}

func ParseHeader(data []byte) (Header, error) {
	headerBytes, _, err := splitFrontmatter(data)
	if err != nil {
		return Header{}, err
	}
	var header Header
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Header{}, fmt.Errorf("parse frontmatter: %w", err)
	}
	return header, nil
}

func ReadMessageFile(path string) (Message, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Message{}, err
	}
	return ParseMessage(data)
}

func ReadHeaderFile(path string) (Header, error) {
	file, err := os.Open(path)
	if err != nil {
		return Header{}, err
	}
	defer func() { _ = file.Close() }()
	return ReadHeader(file)
}

func ReadHeader(r io.Reader) (Header, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return Header{}, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line != frontmatterStartLine {
		return Header{}, ErrMissingFrontmatterStart
	}
	if errors.Is(err, io.EOF) {
		return Header{}, ErrMissingFrontmatterEnd
	}

	dec := json.NewDecoder(br)
	var header Header
	if err := dec.Decode(&header); err != nil {
		return Header{}, fmt.Errorf("parse frontmatter: %w", err)
	}
	rest := io.MultiReader(dec.Buffered(), br)
	if err := consumeFrontmatterEnd(bufio.NewReader(rest)); err != nil {
		return Header{}, err
	}
	return header, nil
}

func splitFrontmatter(data []byte) ([]byte, []byte, error) {
	if !bytes.HasPrefix(data, []byte(frontmatterStart)) {
		return nil, nil, ErrMissingFrontmatterStart
	}
	payload := data[len(frontmatterStart):]
	dec := json.NewDecoder(bytes.NewReader(payload))
	var header json.RawMessage
	if err := dec.Decode(&header); err != nil {
		return nil, nil, fmt.Errorf("parse frontmatter json: %w", err)
	}
	rest := payload[dec.InputOffset():]
	rest = bytes.TrimLeft(rest, " \t\r\n")
	if !bytes.HasPrefix(rest, []byte("---\n")) {
		return nil, nil, ErrMissingFrontmatterEnd
	}
	body := rest[len("---\n"):]
	return header, body, nil
}

func consumeFrontmatterEnd(br *bufio.Reader) error {
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return ErrMissingFrontmatterEnd
			}
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "---" {
			return nil
		}
		return ErrMissingFrontmatterEnd
	}
}

// Timestamped is implemented by types that have a Created timestamp and ID for sorting.
type Timestamped interface {
	GetCreated() string
	GetID() string
	GetRawTime() time.Time
}

// SortByTimestamp sorts a slice of Timestamped items by time, then by ID for stability.
func SortByTimestamp[T Timestamped](items []T) {
	sort.Slice(items, func(i, j int) bool {
		ti, tj := items[i].GetRawTime(), items[j].GetRawTime()
		if !ti.IsZero() && !tj.IsZero() {
			if ti.Equal(tj) {
				return items[i].GetID() < items[j].GetID()
			}
			return ti.Before(tj)
		}
		ci, cj := items[i].GetCreated(), items[j].GetCreated()
		if ci == cj {
			return items[i].GetID() < items[j].GetID()
		}
		return ci < cj
	})
}

```

File: internal/fsq/atomic.go (539 tokens)
```
package fsq

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WriteFileAtomic writes data to a temporary file in dir and renames it into place.
func WriteFileAtomic(dir, filename string, data []byte, perm os.FileMode) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmpName := fmt.Sprintf(".%s.tmp-%d", filename, time.Now().UnixNano())
	tmpPath := filepath.Join(dir, tmpName)
	finalPath := filepath.Join(dir, filename)

	if err := writeAndSync(tmpPath, data, perm); err != nil {
		return "", err
	}
	if err := SyncDir(dir); err != nil {
		return "", cleanupTemp(tmpPath, err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		// On Windows, rename to existing file may return access denied instead of ErrExist.
		// Check if destination exists and retry after removal.
		if _, statErr := os.Stat(finalPath); statErr == nil {
			if removeErr := os.Remove(finalPath); removeErr != nil && !os.IsNotExist(removeErr) {
				return "", cleanupTemp(tmpPath, err)
			}
			if err := os.Rename(tmpPath, finalPath); err != nil {
				return "", cleanupTemp(tmpPath, err)
			}
		} else {
			return "", cleanupTemp(tmpPath, err)
		}
	}
	if err := SyncDir(dir); err != nil {
		return "", err
	}
	return finalPath, nil
}

func writeAndSync(path string, data []byte, perm os.FileMode) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil && err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func cleanupTemp(path string, primary error) error {
	if primary == nil {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w (cleanup: %v)", primary, err)
	}
	return primary
}

```

File: internal/fsq/find.go (333 tokens)
```
package fsq

import (
	"os"
	"path/filepath"
)

const (
	BoxNew = "new"
	BoxCur = "cur"
)

func FindMessage(root, agent, filename string) (string, string, error) {
	newPath := filepath.Join(root, "agents", agent, "inbox", "new", filename)
	if _, err := os.Stat(newPath); err == nil {
		return newPath, BoxNew, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	curPath := filepath.Join(root, "agents", agent, "inbox", "cur", filename)
	if _, err := os.Stat(curPath); err == nil {
		return curPath, BoxCur, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	return "", "", os.ErrNotExist
}

func MoveNewToCur(root, agent, filename string) error {
	newPath := filepath.Join(root, "agents", agent, "inbox", "new", filename)
	curDir := filepath.Join(root, "agents", agent, "inbox", "cur")
	curPath := filepath.Join(curDir, filename)
	if err := os.MkdirAll(curDir, 0o700); err != nil {
		return err
	}
	if err := os.Rename(newPath, curPath); err != nil {
		return err
	}
	if err := SyncDir(filepath.Dir(newPath)); err != nil {
		return err
	}
	return SyncDir(curDir)
}

```

File: internal/fsq/layout.go (440 tokens)
```
package fsq

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Path helpers for standard mailbox directories.

func AgentBase(root, agent string) string {
	return filepath.Join(root, "agents", agent)
}

func AgentInboxTmp(root, agent string) string {
	return filepath.Join(root, "agents", agent, "inbox", "tmp")
}

func AgentInboxNew(root, agent string) string {
	return filepath.Join(root, "agents", agent, "inbox", "new")
}

func AgentInboxCur(root, agent string) string {
	return filepath.Join(root, "agents", agent, "inbox", "cur")
}

func AgentOutboxSent(root, agent string) string {
	return filepath.Join(root, "agents", agent, "outbox", "sent")
}

func AgentAcksReceived(root, agent string) string {
	return filepath.Join(root, "agents", agent, "acks", "received")
}

func AgentAcksSent(root, agent string) string {
	return filepath.Join(root, "agents", agent, "acks", "sent")
}

func EnsureRootDirs(root string) error {
	for _, dir := range []string{
		filepath.Join(root, "agents"),
		filepath.Join(root, "threads"),
		filepath.Join(root, "meta"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func EnsureAgentDirs(root, agent string) error {
	if strings.TrimSpace(agent) == "" {
		return fmt.Errorf("agent handle is empty")
	}
	dirs := []string{
		AgentInboxTmp(root, agent),
		AgentInboxNew(root, agent),
		AgentInboxCur(root, agent),
		AgentOutboxSent(root, agent),
		AgentAcksReceived(root, agent),
		AgentAcksSent(root, agent),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

```

File: internal/fsq/maildir_test.go (853 tokens)
```
package fsq

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDeliverToInbox(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	data := []byte("hello")
	filename := "test.md"
	path, err := DeliverToInbox(root, "codex", filename, data)
	if err != nil {
		t.Fatalf("DeliverToInbox: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected message in new: %v", err)
	}
	tmpDir := filepath.Join(root, "agents", "codex", "inbox", "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir tmp: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected tmp empty, got %d", len(entries))
	}
}

func TestMoveNewToCur(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	data := []byte("hello")
	filename := "move.md"
	if _, err := DeliverToInbox(root, "codex", filename, data); err != nil {
		t.Fatalf("DeliverToInbox: %v", err)
	}
	if err := MoveNewToCur(root, "codex", filename); err != nil {
		t.Fatalf("MoveNewToCur: %v", err)
	}
	newPath := filepath.Join(root, "agents", "codex", "inbox", "new", filename)
	curPath := filepath.Join(root, "agents", "codex", "inbox", "cur", filename)
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("expected new missing")
	}
	if _, err := os.Stat(curPath); err != nil {
		t.Fatalf("expected cur present: %v", err)
	}
}

func TestDeliverToInboxesRollback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod permissions are unreliable on Windows")
	}
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}
	if err := EnsureAgentDirs(root, "cloudcode"); err != nil {
		t.Fatalf("EnsureAgentDirs cloudcode: %v", err)
	}

	cloudNew := AgentInboxNew(root, "cloudcode")
	if err := os.Chmod(cloudNew, 0o555); err != nil {
		t.Fatalf("chmod cloudcode new: %v", err)
	}

	filename := "multi.md"
	if _, err := DeliverToInboxes(root, []string{"codex", "cloudcode"}, filename, []byte("hello")); err == nil {
		t.Fatalf("expected delivery error")
	}

	codexNew := filepath.Join(AgentInboxNew(root, "codex"), filename)
	if _, err := os.Stat(codexNew); !os.IsNotExist(err) {
		t.Fatalf("expected rollback to remove %s", codexNew)
	}

	cloudTmp := filepath.Join(AgentInboxTmp(root, "cloudcode"), filename)
	if _, err := os.Stat(cloudTmp); !os.IsNotExist(err) {
		t.Fatalf("expected rollback to remove %s", cloudTmp)
	}
}

```

File: internal/fsq/maildir.go (1124 tokens)
```
package fsq

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// DeliverToInbox writes a message using Maildir semantics (tmp -> new).
// It returns the final path in inbox/new.
func DeliverToInbox(root, agent, filename string, data []byte) (string, error) {
	paths, err := DeliverToInboxes(root, []string{agent}, filename, data)
	if err != nil {
		return "", err
	}
	return paths[agent], nil
}

type stagedDelivery struct {
	recipient string
	tmpDir    string
	newDir    string
	tmpPath   string
	newPath   string
}

// DeliverToInboxes writes a message to multiple inboxes.
// On failure, it attempts to roll back any prior deliveries.
func DeliverToInboxes(root string, recipients []string, filename string, data []byte) (map[string]string, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no recipients provided")
	}
	stages := make([]stagedDelivery, 0, len(recipients))
	for _, recipient := range recipients {
		tmpDir := AgentInboxTmp(root, recipient)
		newDir := AgentInboxNew(root, recipient)
		if err := os.MkdirAll(tmpDir, 0o700); err != nil {
			return nil, cleanupStagedTmp(stages, err)
		}
		if err := os.MkdirAll(newDir, 0o700); err != nil {
			return nil, cleanupStagedTmp(stages, err)
		}
		tmpPath := filepath.Join(tmpDir, filename)
		newPath := filepath.Join(newDir, filename)
		if err := writeAndSync(tmpPath, data, 0o600); err != nil {
			return nil, cleanupStagedTmp(stages, err)
		}
		if err := SyncDir(tmpDir); err != nil {
			return nil, cleanupStagedTmp(stages, cleanupTemp(tmpPath, err))
		}
		stages = append(stages, stagedDelivery{
			recipient: recipient,
			tmpDir:    tmpDir,
			newDir:    newDir,
			tmpPath:   tmpPath,
			newPath:   newPath,
		})
	}

	for i, stage := range stages {
		if err := os.Rename(stage.tmpPath, stage.newPath); err != nil {
			if errors.Is(err, syscall.EXDEV) {
				err = fmt.Errorf("rename tmp->new for %s: different filesystems: %w", stage.recipient, err)
			} else {
				err = fmt.Errorf("rename tmp->new for %s: %w", stage.recipient, err)
			}
			return nil, rollbackDeliveries(stages[:i], stages[i:], err)
		}
		if err := SyncDir(stage.newDir); err != nil {
			return nil, rollbackDeliveries(stages[:i+1], stages[i+1:], fmt.Errorf("sync new dir for %s: %w", stage.recipient, err))
		}
	}

	paths := make(map[string]string, len(stages))
	for _, stage := range stages {
		paths[stage.recipient] = stage.newPath
	}
	return paths, nil
}

func cleanupStagedTmp(stages []stagedDelivery, primary error) error {
	var cleanupErr error
	for _, stage := range stages {
		if err := os.Remove(stage.tmpPath); err != nil && !os.IsNotExist(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup tmp %s: %w", stage.tmpPath, err))
		}
		if err := SyncDir(stage.tmpDir); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync tmp dir %s: %w", stage.tmpDir, err))
		}
	}
	if cleanupErr == nil {
		return primary
	}
	return fmt.Errorf("%w (cleanup: %v)", primary, cleanupErr)
}

func rollbackDeliveries(committed, pending []stagedDelivery, primary error) error {
	var cleanupErr error
	for _, stage := range committed {
		if err := os.Remove(stage.newPath); err != nil && !os.IsNotExist(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("rollback new %s: %w", stage.newPath, err))
		}
		if err := SyncDir(stage.newDir); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync new dir %s: %w", stage.newDir, err))
		}
	}
	for _, stage := range pending {
		if err := os.Remove(stage.tmpPath); err != nil && !os.IsNotExist(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup tmp %s: %w", stage.tmpPath, err))
		}
		if err := SyncDir(stage.tmpDir); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync tmp dir %s: %w", stage.tmpDir, err))
		}
	}
	if cleanupErr == nil {
		return primary
	}
	return fmt.Errorf("%w (rollback: %v)", primary, cleanupErr)
}

```

File: internal/fsq/scan_test.go (346 tokens)
```
package fsq

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFindTmpFilesOlderThan(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	tmpDir := filepath.Join(root, "agents", "codex", "inbox", "tmp")
	oldPath := filepath.Join(tmpDir, "old.tmp")
	newPath := filepath.Join(tmpDir, "new.tmp")
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("new"), 0o644); err != nil {
		t.Fatalf("write new: %v", err)
	}

	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes old: %v", err)
	}

	cutoff := time.Now().Add(-36 * time.Hour)
	matches, err := FindTmpFilesOlderThan(root, cutoff)
	if err != nil {
		t.Fatalf("FindTmpFilesOlderThan: %v", err)
	}
	if len(matches) != 1 || matches[0] != oldPath {
		t.Fatalf("unexpected matches: %v", matches)
	}
}

```

File: internal/fsq/scan.go (330 tokens)
```
package fsq

import (
	"os"
	"path/filepath"
	"time"
)

func ListAgents(root string) ([]string, error) {
	agentsDir := filepath.Join(root, "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			out = append(out, entry.Name())
		}
	}
	return out, nil
}

func FindTmpFilesOlderThan(root string, cutoff time.Time) ([]string, error) {
	agents, err := ListAgents(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	matches := []string{}
	for _, agent := range agents {
		tmpDir := AgentInboxTmp(root, agent)
		entries, err := os.ReadDir(tmpDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue // skip unreadable files instead of failing entire scan
			}
			if info.ModTime().Before(cutoff) {
				matches = append(matches, filepath.Join(tmpDir, entry.Name()))
			}
		}
	}
	return matches, nil
}

```

File: internal/fsq/sync_unix.go (143 tokens)
```
//go:build !windows

package fsq

import (
	"errors"
	"os"
	"syscall"
)

// SyncDir fsyncs a directory to ensure directory entries are durable.
func SyncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		if isSyncUnsupported(syncErr) {
			return nil
		}
		return syncErr
	}
	return closeErr
}

func isSyncUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)
}

```

File: internal/fsq/sync_windows.go (32 tokens)
```
//go:build windows

package fsq

// SyncDir is a no-op on Windows.
func SyncDir(_ string) error {
	return nil
}

```

File: internal/presence/doc.go (39 tokens)
```
// Package presence manages agent presence metadata. Each agent
// can set a status and optional note, stored as presence.json
// in their mailbox directory with a last_seen timestamp.
package presence

```

File: internal/presence/presence_test.go (187 tokens)
```
package presence

import (
	"testing"
	"time"
)

func TestPresenceWriteRead(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2025, 12, 24, 15, 2, 33, 0, time.UTC)
	p := New("codex", "busy", "reviewing", now)
	if err := Write(root, p); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(root, "codex")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Handle != "codex" || got.Status != "busy" || got.Note != "reviewing" {
		t.Fatalf("unexpected presence: %+v", got)
	}
	if got.LastSeen == "" {
		t.Fatalf("expected LastSeen to be set")
	}
}

```

File: internal/presence/presence.go (330 tokens)
```
package presence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Presence captures the current presence for an agent handle.
type Presence struct {
	Schema   int    `json:"schema"`
	Handle   string `json:"handle"`
	Status   string `json:"status"`
	LastSeen string `json:"last_seen"`
	Note     string `json:"note,omitempty"`
}

func New(handle, status, note string, now time.Time) Presence {
	return Presence{
		Schema:   1,
		Handle:   handle,
		Status:   status,
		LastSeen: now.UTC().Format(time.RFC3339Nano),
		Note:     note,
	}
}

func Write(root string, p Presence) error {
	path := filepath.Join(root, "agents", p.Handle, "presence.json")
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), data, 0o600)
	return err
}

func Read(root, handle string) (Presence, error) {
	path := filepath.Join(root, "agents", handle, "presence.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return Presence{}, err
	}
	var p Presence
	if err := json.Unmarshal(data, &p); err != nil {
		return Presence{}, err
	}
	return p, nil
}

```

File: internal/thread/doc.go (43 tokens)
```
// Package thread collects and aggregates messages across agent
// mailboxes by thread ID. It scans inbox and outbox directories,
// deduplicates by message ID, and returns entries sorted by timestamp.
package thread

```

File: internal/thread/thread_test.go (978 tokens)
```
package thread

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestCollectThread(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "cloudcode"); err != nil {
		t.Fatalf("EnsureAgentDirs cloudcode: %v", err)
	}

	now := time.Date(2025, 12, 24, 15, 2, 33, 0, time.UTC)
	msg1 := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-1",
			From:    "codex",
			To:      []string{"cloudcode"},
			Thread:  "p2p/cloudcode__codex",
			Subject: "Hello",
			Created: now.Format(time.RFC3339Nano),
		},
		Body: "First",
	}
	msg2 := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-2",
			From:    "cloudcode",
			To:      []string{"codex"},
			Thread:  "p2p/cloudcode__codex",
			Subject: "Re: Hello",
			Created: now.Add(2 * time.Second).Format(time.RFC3339Nano),
		},
		Body: "Second",
	}

	data1, err := msg1.Marshal()
	if err != nil {
		t.Fatalf("marshal msg1: %v", err)
	}
	data2, err := msg2.Marshal()
	if err != nil {
		t.Fatalf("marshal msg2: %v", err)
	}

	if _, err := fsq.DeliverToInbox(root, "cloudcode", "msg-1.md", data1); err != nil {
		t.Fatalf("deliver msg1: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "codex", "msg-2.md", data2); err != nil {
		t.Fatalf("deliver msg2: %v", err)
	}

	entries, err := Collect(root, "p2p/cloudcode__codex", []string{"codex", "cloudcode"}, false, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].ID != "msg-1" || entries[1].ID != "msg-2" {
		t.Fatalf("unexpected order: %v, %v", entries[0].ID, entries[1].ID)
	}
}

func TestCollectThreadCorruptMessage(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}

	path := filepath.Join(fsq.AgentInboxNew(root, "codex"), "bad.md")
	if err := os.WriteFile(path, []byte("not a message"), 0o644); err != nil {
		t.Fatalf("write bad message: %v", err)
	}

	if _, err := Collect(root, "p2p/cloudcode__codex", []string{"codex"}, false, nil); err == nil {
		t.Fatalf("expected error for corrupt message")
	}

	called := false
	entries, err := Collect(root, "p2p/cloudcode__codex", []string{"codex"}, false, func(path string, err error) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("Collect with onError: %v", err)
	}
	if !called {
		t.Fatalf("expected onError to be called")
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
}

```

File: internal/thread/thread.go (952 tokens)
```
package thread

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Entry is a thread message entry.
type Entry struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`
	To      []string  `json:"to"`
	Thread  string    `json:"thread"`
	Subject string    `json:"subject"`
	Created string    `json:"created"`
	Body    string    `json:"body,omitempty"`
	RawTime time.Time `json:"-"`
}

// Collect scans agent mailboxes and returns messages for a thread.
// onError is called when a message cannot be parsed; returning a non-nil error aborts the scan.
func Collect(root, threadID string, agents []string, includeBody bool, onError func(path string, err error) error) ([]Entry, error) {
	entries := []Entry{}
	seen := make(map[string]struct{})
	for _, agent := range agents {
		dirs := []string{
			fsq.AgentInboxNew(root, agent),
			fsq.AgentInboxCur(root, agent),
			fsq.AgentOutboxSent(root, agent),
		}
		for _, dir := range dirs {
			files, err := os.ReadDir(dir)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			for _, file := range files {
				if file.IsDir() {
					continue
				}
				path := filepath.Join(dir, file.Name())
				if includeBody {
					msg, err := format.ReadMessageFile(path)
					if err != nil {
						if onError == nil {
							return nil, fmt.Errorf("parse message %s: %w", path, err)
						}
						if cbErr := onError(path, err); cbErr != nil {
							return nil, cbErr
						}
						continue
					}
					if msg.Header.Thread != threadID {
						continue
					}
					if _, ok := seen[msg.Header.ID]; ok {
						continue
					}
					seen[msg.Header.ID] = struct{}{}
					entry := Entry{
						ID:      msg.Header.ID,
						From:    msg.Header.From,
						To:      msg.Header.To,
						Thread:  msg.Header.Thread,
						Subject: msg.Header.Subject,
						Created: msg.Header.Created,
						Body:    msg.Body,
					}
					if ts, err := time.Parse(time.RFC3339Nano, msg.Header.Created); err == nil {
						entry.RawTime = ts
					}
					entries = append(entries, entry)
					continue
				}

				header, err := format.ReadHeaderFile(path)
				if err != nil {
					if onError == nil {
						return nil, fmt.Errorf("parse message %s: %w", path, err)
					}
					if cbErr := onError(path, err); cbErr != nil {
						return nil, cbErr
					}
					continue
				}
				if header.Thread != threadID {
					continue
				}
				if _, ok := seen[header.ID]; ok {
					continue
				}
				seen[header.ID] = struct{}{}
				entry := Entry{
					ID:      header.ID,
					From:    header.From,
					To:      header.To,
					Thread:  header.Thread,
					Subject: header.Subject,
					Created: header.Created,
				}
				if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
					entry.RawTime = ts
				}
				entries = append(entries, entry)
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].RawTime.IsZero() && !entries[j].RawTime.IsZero() {
			if entries[i].RawTime.Equal(entries[j].RawTime) {
				return entries[i].ID < entries[j].ID
			}
			return entries[i].RawTime.Before(entries[j].RawTime)
		}
		if entries[i].Created == entries[j].Created {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Created < entries[j].Created
	})

	return entries, nil
}

```

File: README.md (1374 tokens)
```
# Agent Message Queue (AMQ)

A local, file-queue mailbox for two or more coding agents running on the same machine. AMQ uses Maildir-style atomic delivery (tmp -> new -> cur) with a minimal CLI so agents can exchange messages without a server, database, or daemon.

Status: implementation-ready. The detailed plan is in `specs/001-local-maildir-queue`.

Requires Go 1.25+.

## Goals

- Minimal, robust file-based message exchange between agents on the same machine
- Atomic delivery semantics and durable writes
- No background services or network dependencies
- Human-auditable artifacts (plain text + JSON frontmatter)

## Non-goals

- Global search or indexing across repos
- Long-running daemons or background workers
- Complex auth, ACLs, or multi-tenant isolation

## Quickstart

```bash
# Build the CLI

go build -o amq ./cmd/amq

# Initialize a root with two agent mailboxes
./amq init --root .agent-mail --agents codex,cloudcode

# Send a message
./amq send --me codex --to cloudcode --body "Quick ping"
```

## Environment variables

- `AM_ROOT`: default root directory for storage
- `AM_ME`: default agent handle

Handles must be lowercase and match `[a-z0-9_-]+`.

## Security

- Directories use 0700 permissions (owner-only access)
- Files use 0600 permissions (owner read/write only)
- Unknown handles trigger a warning by default; use `--strict` to error instead
- With `--strict`, corrupt or unreadable `config.json` also causes an error

These defaults are suitable for single-user machines. For shared systems, ensure the AMQ root is in a user-owned directory.

## CLI

- `amq init --root <path> --agents a,b,c`
- `amq send --me codex --to cloudcode --subject "Review notes" --thread p2p/cloudcode__codex --body @notes.md`
- `amq list --me cloudcode --new`
- `amq list --me cloudcode --new --limit 50 --offset 50`
- `amq read --me cloudcode --id <msg_id>`
- `amq ack --me cloudcode --id <msg_id>`
- `amq thread --id p2p/cloudcode__codex --include-body`
- `amq thread --id p2p/cloudcode__codex --limit 50`
- `amq presence set --me codex --status busy`
- `amq cleanup --tmp-older-than 36h [--dry-run]`
- `amq watch --me cloudcode --timeout 60s`
- `amq --version`

Most commands accept `--root`, `--json`, and `--strict` (except `init` which has its own flags).

See `specs/001-local-maildir-queue/quickstart.md` for the full contract.

## Multi-Agent Workflows

AMQ supports two primary coordination patterns:

### Pattern 1: Active Agent Loop

When an agent is actively working, integrate quick inbox checks between steps.
Commands below assume `AM_ME` is set (e.g., `export AM_ME=claude`):

```bash
# Agent work loop (pseudocode):
# 1. Check inbox (non-blocking)
amq list --new --json
# 2. Process any messages
# 3. Do work
# 4. Send updates to other agents
amq send --to codex --subject "Progress" --body "Completed task X"
# 5. Repeat
```

### Pattern 2: Explicit Wait for Reply

When waiting for a response from another agent (assumes `AM_ME` is set):

```bash
# Send request
amq send --to codex --subject "Review request" --body @file.go

# Wait for reply (blocks until message arrives or timeout)
amq watch --timeout 120s

# Process the reply
amq read --id <msg_id>
```

### Watch Command Details

The `watch` command uses fsnotify for efficient OS-native file notifications:

```bash
# Wait up to 60 seconds for new messages
amq watch --me claude --timeout 60s

# Use polling fallback for network filesystems (NFS, etc.)
amq watch --me claude --timeout 60s --poll

# JSON output for scripting
amq watch --me claude --timeout 60s --json
```

Returns:
- `existing` - Found messages already in inbox
- `new_message` - Message arrived during watch
- `timeout` - No messages within timeout period

## Build, lint, test

```bash
make build
make test
make vet
make lint
make ci
make smoke
```

`make lint` expects `golangci-lint` to be installed. See https://golangci-lint.run/usage/install/

Install git hooks to enforce checks before push:

```bash
./scripts/install-hooks.sh
```

## Skills

This repo includes ready-to-use skills for AI coding assistants.

### Claude Code

```bash
# Add the marketplace (one-time)
/plugin marketplace add avivsinai/skills-marketplace

# Install this plugin
/plugin install amq-cli@avivsinai/skills-marketplace
```

### Codex CLI

```bash
# Install directly from this repo
$skill-installer avivsinai/agent-message-queue/.codex/skills/amq-cli
```

Note: The skills marketplace is a registry for Claude Code only. Codex CLI must install directly from source repos.

### Manual Installation

```bash
git clone https://github.com/avivsinai/agent-message-queue
ln -s $(pwd)/agent-message-queue/.claude/skills/amq-cli ~/.claude/skills/amq-cli
ln -s $(pwd)/agent-message-queue/.codex/skills/amq-cli ~/.codex/skills/amq-cli
```

### Development

When working in this repo, local skills in `.claude/skills/` and `.codex/skills/` take precedence over user-level installed skills. This lets you test changes before publishing.

The source of truth is `.claude/skills/amq-cli/`. After making changes, sync to Codex:

```bash
make sync-skills
```

## License

MIT (see `LICENSE`).

```

</files>


```

File: .prompts/amq-monitor-research.md (1374 tokens)
```
# AMQ Background Monitor: Research & Implementation Prompt

## Context

We're building a workflow where Claude Code (and Codex CLI) sessions automatically monitor an Agent Message Queue (AMQ) for incoming messages from other agents. The goal is:

1. **Non-blocking monitoring**: Main agent works normally while a background watcher monitors the queue
2. **Automatic surfacing**: When messages arrive, they're surfaced to the main agent
3. **Priority-based handling**: Urgent messages interrupt; normal messages queue as TODOs

## What We Know Works (Claude Code Dec 2025)

### Async Subagents
- Subagents can run in background via `Ctrl+B` or `run_in_background: true`
- `/tasks` command shows all background agents with status, progress, token usage
- `AgentOutputTool` automatically surfaces results when agents complete
- Background agents can **wake the main agent** when they finish

### Natural Language Spawning
```
"Run the queue-watcher in the background while I work on this feature"
"Have the amq-monitor check for messages every 30 seconds"
```

### The AMQ CLI
```bash
# Block until message arrives (or timeout)
amq watch --me claude --timeout 60s --json
# Output: {"event":"new_message","messages":[{"id":"...","from":"codex","subject":"..."}]}

# One-shot drain (read + move to cur + ack)
amq drain --me claude --include-body --json
```

## Research Questions

1. **Wake mechanism**: How exactly does a background subagent "wake" the main agent? Is it automatic when the agent completes, or does it require specific output/signaling?

2. **Continuous monitoring**: Can a background agent loop indefinitely (watch → process → watch again), or does it need to complete and be re-spawned?

3. **Interruption**: Can a background agent interrupt the main agent mid-task for urgent messages, or only when the main agent is idle/between turns?

4. **Codex CLI parity**: Does Codex have equivalent async agent capabilities? The experimental background terminals feature suggests partial support.

5. **Context injection**: When a background agent surfaces results, how much context can it inject? Can it directly modify the main agent's TodoWrite list?

## Proposed Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Claude Code / Codex Session                                │
│                                                             │
│  Main Agent (foreground)     AMQ Watcher (background)       │
│  ┌─────────────────────┐     ┌─────────────────────────┐   │
│  │ Working on user     │     │ amq watch --timeout 60s │   │
│  │ requested task...   │     │         ↓               │   │
│  │                     │     │ Message arrives         │   │
│  │                     │  ←──│ AgentOutputTool wakes   │   │
│  │                     │     │ main with result        │   │
│  │ Receives message:   │     └─────────────────────────┘   │
│  │ {priority: urgent}  │                                    │
│  │         ↓           │                                    │
│  │ Decides: interrupt  │                                    │
│  │ or queue to TODOs   │                                    │
│  └─────────────────────┘                                    │
└─────────────────────────────────────────────────────────────┘
```

## Message Format Extension

```yaml
---
schema: amq/v1
id: msg_abc123
from: codex
to: claude
subject: "Code review needed"
priority: urgent    # NEW: urgent | normal | low
thread: p2p/claude__codex
created: 2025-12-26T10:00:00Z
---

Body of the message...
```

## Implementation Options

### Option A: Session-Start Watcher
Add to CLAUDE.md:
```markdown
At session start, spawn a background AMQ watcher:
- Run `amq watch --me $AM_ME --json` in background
- When messages arrive, surface them
- For urgent: interrupt current work
- For normal: add to TodoWrite
- Re-spawn watcher after processing
```

### Option B: Custom Subagent Definition
Create `.claude/agents/amq-watcher.md`:
```markdown
---
name: amq-watcher
description: Monitors AMQ for incoming messages
tools: Bash, TodoWrite
model: haiku
---

Monitor the AMQ inbox using `amq watch`. When messages arrive:
1. Parse priority from message metadata
2. If urgent: return immediately to wake main agent
3. If normal: add to todo list, continue watching
```

### Option C: Hook-Based (Notification)
```json
{
  "hooks": {
    "Notification": [{
      "hooks": [{
        "type": "command",
        "command": "amq list --me $AM_ME --new --json | jq -e 'length > 0'"
      }]
    }]
  }
}
```

## What We Need Clarified

1. Is `run_in_background: true` on the Task tool actually shipped, or still a feature request?
2. Can background agents use TodoWrite to modify the main agent's todo list?
3. What's the actual mechanism for "waking" - is it polling, event-driven, or completion-triggered?
4. For Codex: what's the equivalent pattern?

## Test Plan

1. Spawn background watcher manually with Ctrl+B
2. Send test message via `amq send --to claude --subject "test" --body "hello"`
3. Verify main agent receives notification
4. Test priority handling (urgent vs normal)
5. Verify continuous monitoring (respawn after message processed)

## References

- [Claude Code Async Subagents (Dec 2025)](https://www.zerotopete.com/p/christmas-came-early-async-subagents)
- [Async Workflows Guide](https://claudefa.st/blog/guide/agents/async-workflows)
- [Background Commands Wiki](https://github.com/ruvnet/claude-flow/wiki/background-commands)
- [Feature Request #9905](https://github.com/anthropics/claude-code/issues/9905) - Task tool async support
- [AMQ CLI Documentation](./CLAUDE.md)

```

File: AGENTS.md (256 tokens)
```
# AGENTS.md

This file provides guidance for Codex and other AI coding agents. **The authoritative source is `CLAUDE.md`** - this file exists for compatibility with agents that look for `AGENTS.md`.

## Quick Reference

See [`CLAUDE.md`](./CLAUDE.md) for complete instructions including:

- Project overview and architecture
- Build commands (`make build`, `make test`, `make ci`)
- CLI usage and common flags
- Multi-agent coordination patterns
- Security practices (0700/0600 permissions, `--strict` flag)
- Contributing guidelines and commit conventions
- Skill development workflow (dev vs installed precedence, `make sync-skills`)

## Essential Commands

```bash
make ci              # Run before committing: vet, lint, test, smoke
./amq --version      # Check installed version
```

## Environment

```bash
export AM_ROOT=.agent-mail
export AM_ME=<your-agent-handle>
```

## Key Constraints

- Go 1.25+ required
- Handles must be lowercase: `[a-z0-9_-]+`
- Never edit message files directly; use the CLI
- Cleanup is explicit (`amq cleanup`), never automatic

```

File: CLAUDE.md (2014 tokens)
```
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
├── cli/           → Command handlers (send, list, read, ack, drain, thread, presence, cleanup, init, watch, monitor, reply)
├── fsq/           → File system queue (Maildir delivery, atomic ops, scanning)
├── format/        → Message serialization (JSON frontmatter + Markdown body)
├── config/        → Config management (meta/config.json)
├── ack/           → Acknowledgment tracking
├── thread/        → Thread collection across mailboxes
└── presence/      → Agent presence metadata
```

**Mailbox Layout**:
```
<root>/agents/<agent>/inbox/{tmp,new,cur}/  → Incoming messages
<root>/agents/<agent>/outbox/sent/          → Sent copies
<root>/agents/<agent>/acks/{received,sent}/ → Acknowledgments
```

## Core Concepts

**Atomic Delivery**: Messages written to `tmp/`, fsynced, then atomically renamed to `new/`. Readers only scan `new/` and `cur/`, never seeing incomplete writes.

**Message Format**: JSON frontmatter (schema, id, from, to, thread, subject, created, ack_required, refs, priority, kind, labels, context) followed by `---` and Markdown body.

**Thread Naming**: P2P threads use lexicographic ordering: `p2p/<lower_agent>__<higher_agent>`

**Environment Variables**: `AM_ROOT` (default root dir), `AM_ME` (default agent handle)

## CLI Commands

```bash
amq init --root <path> --agents a,b,c [--force]
amq send --me <agent> --to <recipients> [--subject <str>] [--thread <id>] [--body <str|@file|stdin>] [--ack] [--priority <p>] [--kind <k>] [--labels <l>] [--context <json>]
amq list --me <agent> [--new | --cur] [--json]
amq read --me <agent> --id <msg_id> [--json]
amq ack --me <agent> --id <msg_id>
amq drain --me <agent> [--limit N] [--include-body] [--ack] [--json]
amq thread --id <thread_id> [--limit N] [--include-body] [--json]
amq presence set --me <agent> --status <busy|idle|...> [--note <str>]
amq presence list [--json]
amq cleanup --tmp-older-than <duration> [--dry-run] [--yes]
amq watch --me <agent> [--timeout <duration>] [--poll] [--json]
amq monitor --me <agent> [--timeout <duration>] [--poll] [--include-body] [--json]
amq reply --me <agent> --id <msg_id> [--body <str|@file|stdin>] [--priority <p>] [--kind <k>]
```

Common flags: `--root`, `--json`, `--strict` (error instead of warn on unknown handles or unreadable/corrupt config). Note: `init` has its own flags and doesn't accept these.

Use `amq --version` to check the installed version.

## Multi-Agent Coordination

Commands below assume `AM_ME` is set (e.g., `export AM_ME=claude`).

**Preferred: Use `drain`** - One-shot ingestion that reads, moves to cur, and acks in one atomic operation. Silent when empty (hook-friendly).

**During active work**: Use `amq drain --include-body` to ingest messages between steps.

**Waiting for reply**: Use `amq watch --timeout 60s` which blocks until a message arrives, then `amq drain` to process.

| Situation | Command | Behavior |
|-----------|---------|----------|
| Ingest messages | `amq drain --include-body` | One-shot: read+move+ack |
| Waiting for reply | `amq watch --timeout 60s` | Blocks until message |
| Quick peek only | `amq list --new` | Non-blocking, no side effects |
| Co-op background watch | `amq monitor --json` | Watch + drain combined |
| Reply to message | `amq reply --id <msg_id>` | Auto thread/refs handling |

## Co-op Mode (Claude <-> Codex)

Co-op mode enables real-time collaboration between Claude Code and Codex CLI sessions. See `COOP.md` for full documentation.

### Quick Start

On session start:
1. Set `AM_ME=claude` (or `codex`), `AM_ROOT=.agent-mail`
2. Spawn watcher: "Run amq-coop-watcher in background while I work"

### Message Priority Handling

When the watcher returns with messages:
- **urgent** → Interrupt current work, respond immediately
- **normal** → Add to TodoWrite, respond when current task done
- **low** → Batch for end of session

After handling messages, **respawn the watcher immediately**.

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

## Testing

Run individual test: `go test ./internal/fsq -run TestMaildir`

Key test files:
- `internal/fsq/maildir_test.go` - Atomic delivery semantics
- `internal/format/message_test.go` - Message serialization
- `internal/thread/thread_test.go` - Thread collection
- `internal/cli/watch_test.go` - Watch command with fsnotify

## Security

- Directories use 0700 permissions (owner-only access)
- Files use 0600 permissions (owner read/write only)
- Unknown handles trigger a warning by default; use `--strict` to error instead
- With `--strict`, unreadable or corrupt `config.json` also causes an error
- Handles must be lowercase: `[a-z0-9_-]+`

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
.claude/skills/amq-cli/        → Claude Code skill (source of truth)
├── SKILL.md
└── plugin.json
.codex/skills/amq-cli/         → Codex CLI skill (synced copy)
├── SKILL.md
└── plugin.json
```

### Dev vs Installed Skills

When working in this repo, **project-level skills take precedence** over user-level installed skills:

- `.claude/skills/amq-cli/` loads instead of `~/.claude/skills/amq-cli/`
- `.codex/skills/amq-cli/` loads instead of `~/.codex/skills/amq-cli/`

This lets you test skill changes locally before publishing.

### Editing Skills

1. Edit files in `.claude/skills/amq-cli/` (source of truth)
2. Sync to Codex: `make sync-skills`
3. Test locally by running Claude Code or Codex in this repo
4. Bump version in `.claude-plugin/plugin.json` and `.claude/skills/amq-cli/plugin.json`
5. Run `make sync-skills` again to update Codex copies
6. Commit and push

### Installing Skills

See README.md for installation instructions for Claude Code and Codex CLI.

```

File: cmd/amq/main.go (278 tokens)
```
package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/avivsinai/agent-message-queue/internal/cli"
)

var version = "dev"

func getVersion() string {
	// Prefer ldflags-injected version
	if version != "dev" {
		return version
	}
	// Fall back to Go module version (set by go install)
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func main() {
	if len(os.Args) > 1 && isVersionArg(os.Args[1]) {
		if _, err := fmt.Fprintln(os.Stdout, getVersion()); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := cli.Run(os.Args[1:]); err != nil {
		if _, werr := fmt.Fprintln(os.Stderr, err); werr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}

func isVersionArg(arg string) bool {
	switch arg {
	case "--version", "-v", "version":
		return true
	default:
		return false
	}
}

```

File: CODE_OF_CONDUCT.md (851 tokens)
```
# Contributor Covenant Code of Conduct

## Our Pledge

We as members, contributors, and leaders pledge to make participation in our
community a harassment-free experience for everyone, regardless of age, body
size, visible or invisible disability, ethnicity, sex characteristics, gender
identity and expression, level of experience, education, socio-economic status,
nationality, personal appearance, race, religion, or sexual identity
and orientation.

We pledge to act and interact in ways that contribute to an open, welcoming,
diverse, inclusive, and healthy community.

## Our Standards

Examples of behavior that contributes to a positive environment include:

- Demonstrating empathy and kindness toward other people
- Being respectful of differing opinions, viewpoints, and experiences
- Giving and gracefully accepting constructive feedback
- Accepting responsibility and apologizing to those affected by our mistakes,
and learning from the experience
- Focusing on what is best not just for us as individuals, but for the
overall community

Examples of unacceptable behavior include:

- The use of sexualized language or imagery, and sexual attention or
advances of any kind
- Trolling, insulting or derogatory comments, and personal or political attacks
- Public or private harassment
- Publishing others' private information, such as a physical or email
address, without their explicit permission
- Other conduct which could reasonably be considered inappropriate in a
professional setting

## Enforcement Responsibilities

Community leaders are responsible for clarifying and enforcing our standards of
acceptable behavior and will take appropriate and fair corrective action in
response to any behavior that they deem inappropriate, threatening, offensive,
or harmful.

Community leaders have the right and responsibility to remove, edit, or reject
comments, commits, code, wiki edits, issues, and other contributions that are
not aligned to this Code of Conduct, and will communicate reasons for moderation
decisions when appropriate.

## Scope

This Code of Conduct applies within all community spaces, and also applies when
an individual is officially representing the community in public spaces.
Examples of representing our community include using an official e-mail address,
posting via an official social media account, or acting as an appointed
representative at an online or offline event.

## Enforcement

Instances of abusive, harassing, or otherwise unacceptable behavior may be
reported by opening a GitHub issue in this repository. All complaints will be
reviewed and investigated promptly and fairly.

## Enforcement Guidelines

Community leaders will follow these Community Impact Guidelines in determining
the consequences for any action they deem in violation of this Code of Conduct:

### 1. Correction

**Community Impact**: Use of inappropriate language or other behavior deemed
unprofessional or unwelcome in the community.

**Consequence**: A private, written warning providing clarity around the nature
of the violation and an explanation of why the behavior was inappropriate. A
public apology may be requested.

### 2. Warning

**Community Impact**: A violation through a single incident or series
of actions.

**Consequence**: A warning with consequences for continued behavior. No
interaction with the people involved, including unsolicited interaction with
those enforcing the Code of Conduct, for a specified period of time. This
includes avoiding interactions in community spaces as well as external channels
like social media.

### 3. Temporary Ban

**Community Impact**: A serious violation of community standards, including
sustained inappropriate behavior.

**Consequence**: A temporary ban from any sort of interaction or public
communication with the community for a specified period of time. No public or
private interaction with the people involved, including unsolicited interaction
with those enforcing the Code of Conduct, is allowed during this period.

### 4. Permanent Ban

**Community Impact**: Demonstrating a pattern of violation of community
standards, including sustained inappropriate behavior.

**Consequence**: A permanent ban from any sort of public interaction within the
community.

## Attribution

This Code of Conduct is adapted from the Contributor Covenant, version 2.1,
available at https://www.contributor-covenant.org/version/2/1/code_of_conduct.html.

Community Impact Guidelines were inspired by Mozilla's code of conduct
enforcement ladder.

```

File: CONTRIBUTING.md (127 tokens)
```
# Contributing

Thanks for your interest in AMQ!

## Ground rules

- Be respectful and professional. See `CODE_OF_CONDUCT.md`.
- Keep changes focused and small; prefer incremental PRs.

## Development

```bash
make fmt
make test
make vet
make lint
```

## Pull requests

- Include a clear description of the change and why it matters.
- Add or update tests when behavior changes.
- Avoid reformatting unrelated code.

## Reporting issues

Please include:
- OS + Go version
- Exact command(s) run
- Expected vs actual behavior
- Any relevant logs

```

File: COOP.md (1913 tokens)
```
# Co-op Mode: Inter-Agent Collaboration Protocol

Co-op mode enables Claude Code and Codex CLI to collaborate as **review partners**, sharing context, requesting reviews, and coordinating work through the Agent Message Queue.

## Quick Start

### Claude Code Session

```bash
export AM_ME=claude
export AM_ROOT=.agent-mail

# Start the co-op watcher in background
# "Run amq-coop-watcher in the background while I work"

# When messages arrive, handle by priority:
# - urgent → interrupt and respond
# - normal → add to TODOs
# - low → batch for later
```

### Codex CLI Session

```bash
export AM_ME=codex
export AM_ROOT=.agent-mail

# Enable background terminals in ~/.codex/config.toml:
# [features]
# unified_exec = true

# Run monitor in background terminal:
./scripts/codex-coop-monitor.sh

# Or use the notify hook for turn-based checking
```

## Message Format

Co-op mode extends the standard AMQ message format with optional fields:

```json
{
  "schema": 1,
  "id": "2025-12-27T12-00-00.000Z_pid1234_abcd",
  "from": "codex",
  "to": ["claude"],
  "thread": "p2p/claude__codex",
  "subject": "Code review needed",
  "created": "2025-12-27T12:00:00Z",
  "ack_required": true,
  "refs": ["msg_prev_123"],

  "priority": "urgent",
  "kind": "review_request",
  "labels": ["parser", "edge-cases"],
  "context": {
    "paths": ["internal/cli/drain.go"],
    "focus": "sorting + ack behavior"
  }
}
```

### Priority Levels

| Priority | Behavior | Use When |
|----------|----------|----------|
| `urgent` | Interrupt current work | Blocking issues, critical bugs, time-sensitive decisions |
| `normal` | Add to TODO list | Code reviews, questions, standard requests |
| `low` | Batch/digest later | Status updates, FYIs, non-blocking info |

### Message Kinds

| Kind | Default Priority | Description |
|------|------------------|-------------|
| `review_request` | normal | Request code review |
| `review_response` | normal | Code review feedback |
| `question` | normal | Question needing answer |
| `decision` | normal (urgent if blocking) | Decision request |
| `brainstorm` | low | Open-ended discussion |
| `status` | low | Status update/FYI |
| `todo` | normal | Task assignment |

## CLI Commands

### Send with Co-op Fields

```bash
# Request a code review
amq send --me claude --to codex \
  --subject "Review: New parser" \
  --priority normal \
  --kind review_request \
  --labels "parser,refactor" \
  --context '{"paths": ["internal/format/message.go"], "focus": "error handling"}' \
  --body "Please review the new message parser..."

# Urgent blocking question
amq send --me codex --to claude \
  --subject "Blocked: API design question" \
  --priority urgent \
  --kind question \
  --body "I need to decide on the API shape before continuing..."
```

### Monitor (Combined Watch + Drain)

```bash
# Block until message arrives, drain, output JSON
amq monitor --me claude --timeout 0 --include-body --json

# With timeout (60s default)
amq monitor --me codex --timeout 30s --json

# For scripts/hooks
amq monitor --me claude --json
```

### Reply (Auto Thread/Refs)

```bash
# Reply to a message (auto-sets thread, refs, recipient)
amq reply --me codex --id "msg_123" \
  --kind review_response \
  --body "LGTM with minor suggestions..."

# Reply with urgency
amq reply --me claude --id "msg_456" \
  --priority urgent \
  --body "Found a critical issue..."
```

## Claude Code Integration

### Background Watcher Agent

The `amq-coop-watcher` agent (`.claude/agents/amq-coop-watcher.md`) runs in the background and wakes the main agent when messages arrive.

**In your session:**
```
"Run amq-coop-watcher in the background while I work on this feature"
```

**When watcher returns:**
1. Parse the message summary
2. Handle by priority:
   - `urgent` → Stop current work, address immediately
   - `normal` → Add to TodoWrite, continue current work
   - `low` → Note for later, continue
3. Respawn the watcher

### CLAUDE.md Co-op Section

Add to your project's CLAUDE.md:

```markdown
## Co-op Mode (Claude <-> Codex)

On session start:
1. Set AM_ME=claude, AM_ROOT=.agent-mail
2. Spawn watcher: "Run amq-coop-watcher in background"

When watcher returns with messages:
- urgent → interrupt, respond now
- normal → add to TodoWrite, respond when current task done
- low → batch, respond at end of session

After handling, respawn watcher immediately.
```

## Codex CLI Integration

### Background Monitor

1. Enable in `~/.codex/config.toml`:
   ```toml
   [features]
   unified_exec = true
   ```

2. Run the monitor script:
   ```bash
   ./scripts/codex-coop-monitor.sh
   ```

### Notify Hook (Fallback)

For guaranteed message checking on each turn:

1. Add to `~/.codex/config.toml`:
   ```toml
   notify = ["python3", "/path/to/repo/scripts/codex-amq-notify.py"]
   ```

2. The script will:
   - Check for pending messages after each Codex turn
   - Send desktop notifications for new messages
   - Write to `.agent-mail/meta/amq-bulletin.json`

## Review Partner Workflow

### Requesting a Review

```bash
# Claude requests review from Codex
amq send --me claude --to codex \
  --subject "Review: Authentication refactor" \
  --kind review_request \
  --priority normal \
  --labels "auth,security" \
  --context '{"paths": ["internal/auth/"], "focus": "JWT handling"}' \
  --body @review-request.md \
  --ack
```

**review-request.md:**
```markdown
## Changes
- Refactored JWT token validation
- Added refresh token support
- Updated error handling

## What to Review
1. Security implications of refresh tokens
2. Error message exposure
3. Token expiration logic

## Context
See git diff in `internal/auth/` directory.
```

### Responding to a Review

```bash
# Codex responds
amq reply --me codex --id "msg_review_123" \
  --kind review_response \
  --body @review-response.md
```

## Context Object Schema

The `context` field accepts any JSON object. Recommended structure:

```json
{
  "paths": ["internal/cli/send.go", "internal/format/message.go"],
  "symbols": ["Header", "runSend"],
  "focus": "error handling in validation",
  "commands": ["go test ./internal/cli/..."],
  "hunks": [
    {"file": "send.go", "lines": "45-60"}
  ]
}
```

## Best Practices

1. **Always set priority** - Helps receiving agent triage
2. **Use appropriate kind** - Enables smart auto-responses
3. **Include context.paths** - Helps focus the review
4. **Keep bodies concise** - Agent context is precious
5. **Use --ack for important messages** - Ensures delivery confirmation
6. **Respawn watcher immediately** - Don't miss messages
7. **Thread replies** - Use `amq reply` for conversation continuity

## Troubleshooting

### Watcher not starting
```bash
# Check if inbox exists
ls -la .agent-mail/agents/claude/inbox/

# Initialize if needed
amq init --root .agent-mail --agents claude,codex
```

### Messages not appearing
```bash
# Check inbox directly
amq list --me claude --new --json

# Force poll mode if fsnotify issues
amq monitor --me claude --poll --json
```

### Codex notify hook not working
```bash
# Test the script directly
python3 scripts/codex-amq-notify.py '{"type": "agent-turn-complete"}'

# Check bulletin file
cat .agent-mail/meta/amq-bulletin.json
```

```

File: gemini-pro3-review.md (1932 tokens)
```
Here is a comprehensive review of the `agent-message-queue` codebase.

### Summary
The codebase is clean, well-structured, and strictly adheres to the provided specification. The implementation of Maildir-style atomic writes (`tmp` -> `new` -> `cur`) and `fsync` usage demonstrates a strong understanding of POSIX durability requirements.

However, there are significant performance bottlenecks regarding how data is queried (O(N) disk reads), an invalid Go version, and strictness issues with the "Read" lifecycle that could lead to double-processing in failure scenarios.

---

### 1. Critical & Major Issues

#### 1.1. Invalid Go Version
**File:** `go.mod`
**Issue:** `go 1.25`
**Details:** Go 1.25 has not been released yet (current stable is 1.24). This will prevent the project from building on standard toolchains.
**Fix:** Downgrade to `1.24` or `1.23`.

#### 1.2. Scalability Bottleneck (O(N) Disk Reads)
**File:** `internal/cli/list.go`, `internal/thread/thread.go`
**Issue:** The `list` and `thread` commands iterate over directory entries and open/parse **every single file** to check headers (Subject, Thread ID, etc.).
**Impact:** As the mailbox grows (e.g., >1000 messages), CLI responsiveness will degrade linearly. `amq thread` is particularly expensive as it scans *all* folders of *all* agents.
**Recommendation:**
1.  **Short term:** Implement a "lazy load" that trusts the file modification time (`os.Stat`) for sorting and only reads headers for the requested page/limit.
2.  **Long term:** Maintain a separate sidecar index (e.g., `sqlite` or a simple `jsonl` log) updated on `send`/`ack`, as noted in your research notes.

#### 1.3. "At-Least-Once" vs "Exactly-Once" Processing Risk
**File:** `internal/cli/read.go`
**Context:**
```go
msg, err := format.ReadMessageFile(path) // 1. Read content
if err != nil { return err }

if box == fsq.BoxNew {
    if err := fsq.MoveNewToCur(root, common.Me, filename); err != nil { // 2. Change state
        return err
    }
}
```
**Issue:** If the agent crashes or the network fails between Step 1 (Read) and Step 2 (Move), the message remains in `new`. The agent will process the message again next time.
**Fix:** In strict Maildir consumer implementations, the atomic move (`new` -> `cur` or `new` -> `tmp_processing`) happens **before** returning the content to the logic layer.
*   **Proposed Flow:** Move `new` -> `cur`. If successful, read from `cur`. If move fails (file gone), error.

#### 1.4. Linter Configuration
**File:** `.golangci.yml`
**Issue:** `errcheck` is explicitly disabled.
```yaml
disable:
  - errcheck
```
**Impact:** In a filesystem-based queue, ignoring errors (especially from `file.Close()` or `fmt.Fprint`) can hide data corruption or "disk full" scenarios.
**Fix:** Enable `errcheck` and explicitly handle or ignore errors in code (e.g., `_ = file.Close()`).

---

### 2. correctness & Logic

#### 2.1. Frontmatter Parsing Fragility
**File:** `internal/format/message.go`
**Function:** `splitFrontmatter`
**Issue:** The parser looks for the byte sequence `\n---\n`.
```go
idx := bytes.Index(payload, []byte(frontmatterEnd))
```
**Risk:** If the JSON frontmatter itself (e.g., inside the `subject` or `refs` strings) contains the sequence `\n---\n`, the parser will split prematurely, returning invalid JSON.
**Fix:** While unlikely in this specific schema, a robust implementation should decode the JSON stream incrementally using `json.Decoder` until it finishes the object, rather than byte splitting.

#### 2.2. Atomic Write Race Conditions (Send)
**File:** `internal/cli/send.go`
**Issue:** The `send` command performs three distinct write operations:
1. Deliver to Recipient A
2. Deliver to Recipient B (if multiple)
3. Write to Sender Outbox
**Risk:** If step 1 succeeds but step 3 fails (disk full), the message is sent but not recorded in `sent`. There is no transaction rollback.
**Fix:** This is acceptable for a file-based system, but the CLI should output a warning to `stderr` if the outbox write fails, alerting the user that the audit trail is incomplete.

#### 2.3. Thread ID Canonicalization
**File:** `internal/cli/send.go`
**Function:** `canonicalP2P`
**Issue:** The function lowercases agent handles for the thread ID (`strings.ToLower`). However, the directory paths rely on the exact casing of the agent handle.
**Risk:** If `AM_ME` is "Codex" but the directory is "codex", `canonicalP2P` works, but file operations might fail on case-sensitive filesystems (Linux) if not consistent.
**Fix:** Enforce lowercase handles globally during `init` and `send`, or treat handles as case-insensitive everywhere.

---

### 3. Code Quality & Best Practices

#### 3.1. Missing Timeout on Lock/Sync
**File:** `internal/fsq/atomic.go`
**Issue:** File operations like `SyncDir` and `os.Rename` are blocking system calls.
**Recommendation:** While not strictly required for a CLI, wrapping operations in a context or having a global timeout (like the 5m in golangci-lint) prevents the CLI from hanging indefinitely on a bad NFS mount.

#### 3.2. Hardcoded Permissions
**File:** Various
**Issue:** `0o755` and `0o644` are hardcoded.
**Recommendation:** This assumes a specific umask/group setup. It is generally safe for personal tools, but for a shared "agent queue," ensuring the group bit is set correctly (which you do) is important. No action needed unless strict multi-user Unix security is a goal.

#### 3.3. Test Coverage Gaps
**File:** `internal/fsq/maildir_test.go`
**Issue:** Tests cover the "Happy Path".
**Missing:**
1.  **Disk Full:** What happens if `Write` fails halfway?
2.  **Permission Denied:** What if `new` is not writable?
3.  **Corrupt JSON:** What does `list` do if a file in `new` is 0 bytes or garbage? (Currently, `runList` returns an error and stops listing entirely, which renders the mailbox unusable).

**Fix for 3.3 (List Robustness):**
In `internal/cli/list.go`, if `format.ReadHeaderFile` fails, log a warning to stderr and **continue** to the next message. Do not block the entire list command due to one bad file.

---

### 4. OSS Readiness

1.  **License:** The repo is missing a `LICENSE` file.
2.  **Documentation:** The `README.md` is excellent.
3.  **Module Path:** `github.com/avivsinai/agent-message-queue`. Ensure this repository exists or the `go mod init` path matches your intended distribution.

### 5. Refactoring Suggestions (Actionable)

#### Fix the List/Read Loop (Robustness)
*In `internal/cli/list.go`:*

```go
// Current
header, err := format.ReadHeaderFile(path)
if err != nil {
    return err // <--- One bad file breaks the whole inbox
}

// Recommended
header, err := format.ReadHeaderFile(path)
if err != nil {
    fmt.Fprintf(os.Stderr, "skipping corrupt message %s: %v\n", entry.Name(), err)
    continue
}
```

#### Atomic Read Logic
*In `internal/cli/read.go`:*

```go
// Recommended Logic
targetPath := path
if box == fsq.BoxNew {
    // Attempt move first
    if err := fsq.MoveNewToCur(root, common.Me, filename); err != nil {
         return fmt.Errorf("failed to mark message as read: %w", err)
    }
    // Update path to point to cur
    targetPath = filepath.Join(root, "agents", common.Me, "inbox", "cur", filename)
}

msg, err := format.ReadMessageFile(targetPath)
// ...
```

### Conclusion
The project is fundamentally sound and well-designed for its specific "local agent IPC" use case. The atomic file operations are handled correctly. The primary risks are the `go 1.25` version and the O(N) scaling issues in `list/thread` commands. Fixing the `list` error handling and the `read` atomicity will make it production-ready for local workloads.
```

File: internal/ack/ack_test.go (212 tokens)
```
package ack

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAckMarshal(t *testing.T) {
	ts := time.Date(2025, 12, 24, 15, 2, 33, 0, time.UTC)
	a := New("msg-1", "p2p/cloudcode__codex", "cloudcode", "codex", ts)
	data, err := a.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("expected trailing newline")
	}
	var out Ack
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.MsgID != "msg-1" || out.Thread == "" || out.From != "cloudcode" || out.To != "codex" {
		t.Fatalf("unexpected ack: %+v", out)
	}
}

```

File: internal/ack/ack.go (293 tokens)
```
package ack

import (
	"encoding/json"
	"os"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

// Ack records that a message was received and acknowledged.
type Ack struct {
	Schema   int    `json:"schema"`
	MsgID    string `json:"msg_id"`
	Thread   string `json:"thread"`
	From     string `json:"from"`
	To       string `json:"to"`
	Received string `json:"received"`
}

func New(msgID, thread, from, to string, received time.Time) Ack {
	return Ack{
		Schema:   format.CurrentSchema,
		MsgID:    msgID,
		Thread:   thread,
		From:     from,
		To:       to,
		Received: received.UTC().Format(time.RFC3339Nano),
	}
}

func (a Ack) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func Read(path string) (Ack, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Ack{}, err
	}
	var out Ack
	if err := json.Unmarshal(data, &out); err != nil {
		return Ack{}, err
	}
	return out, nil
}

```

File: internal/ack/doc.go (39 tokens)
```
// Package ack provides acknowledgment tracking for messages.
// Acks are stored as JSON files in the sender's received/ and
// receiver's sent/ directories for coordination and audit.
package ack

```

File: internal/cli/ack_test.go (1070 tokens)
```
package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunAckRejectsInvalidHeader(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "cloudcode"); err != nil {
		t.Fatalf("EnsureAgentDirs cloudcode: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "bad/../id",
			From:    "cloudcode",
			To:      []string{"codex"},
			Thread:  "p2p/cloudcode__codex",
			Created: "2025-12-24T15:02:33Z",
		},
		Body: "test",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "codex", "bad.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if err := runAck([]string{"--root", root, "--me", "codex", "--id", "bad"}); err == nil {
		t.Fatalf("expected error for invalid message id in header")
	}

	msg.Header.ID = "msg-1"
	msg.Header.From = "CloudCode"
	data, err = msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	badPath := filepath.Join(fsq.AgentInboxNew(root, "codex"), "bad-from.md")
	if _, err := fsq.WriteFileAtomic(filepath.Dir(badPath), filepath.Base(badPath), data, 0o644); err != nil {
		t.Fatalf("write bad-from: %v", err)
	}
	if err := runAck([]string{"--root", root, "--me", "codex", "--id", "bad-from"}); err == nil {
		t.Fatalf("expected error for invalid sender handle in header")
	}
}

func TestRunAckCorruptAckFileRecovery(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "cloudcode"); err != nil {
		t.Fatalf("EnsureAgentDirs cloudcode: %v", err)
	}

	// Create a valid message
	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-recover",
			From:    "cloudcode",
			To:      []string{"codex"},
			Thread:  "p2p/cloudcode__codex",
			Created: "2025-12-25T15:02:33Z",
		},
		Body: "test",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "codex", "msg-recover.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// Create a corrupt ack file
	ackDir := fsq.AgentAcksSent(root, "codex")
	if err := os.MkdirAll(ackDir, 0o700); err != nil {
		t.Fatalf("mkdir ack dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ackDir, "msg-recover.json"), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write corrupt ack: %v", err)
	}

	// Ack should succeed (recover from corrupt file)
	if err := runAck([]string{"--root", root, "--me", "codex", "--id", "msg-recover"}); err != nil {
		t.Fatalf("ack should recover from corrupt file: %v", err)
	}

	// Verify the ack file was rewritten
	ackData, err := os.ReadFile(filepath.Join(ackDir, "msg-recover.json"))
	if err != nil {
		t.Fatalf("read ack file: %v", err)
	}
	if string(ackData) == "not json" {
		t.Errorf("ack file should have been rewritten")
	}
}

```

File: internal/cli/ack.go (844 tokens)
```
package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/ack"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runAck(args []string) error {
	fs := flag.NewFlagSet("ack", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Message id")

	usage := usageWithFlags(fs, "amq ack --me <agent> --id <msg_id> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)

	// Validate handle against config.json
	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	filename, err := ensureFilename(*idFlag)
	if err != nil {
		return err
	}

	path, _, err := fsq.FindMessage(root, common.Me, filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("message not found: %s", *idFlag)
		}
		return err
	}

	header, err := format.ReadHeaderFile(path)
	if err != nil {
		return err
	}
	sender, err := normalizeHandle(header.From)
	if err != nil || sender != header.From {
		return fmt.Errorf("invalid sender handle in message: %s", header.From)
	}
	msgID, err := ensureSafeBaseName(header.ID)
	if err != nil || msgID != header.ID {
		return fmt.Errorf("invalid message id in message: %s", header.ID)
	}
	ackPayload := ack.New(header.ID, header.Thread, common.Me, sender, time.Now())
	receiverDir := fsq.AgentAcksSent(root, common.Me)
	receiverPath := filepath.Join(receiverDir, msgID+".json")
	needsReceiverWrite := true
	if existing, err := ack.Read(receiverPath); err == nil {
		ackPayload = existing
		needsReceiverWrite = false
	} else if !os.IsNotExist(err) {
		// Corrupt ack file - warn and rewrite
		_ = writeStderr("warning: corrupt ack file, rewriting: %v\n", err)
	}

	data, err := ackPayload.Marshal()
	if err != nil {
		return err
	}

	if needsReceiverWrite {
		if _, err := fsq.WriteFileAtomic(receiverDir, msgID+".json", data, 0o600); err != nil {
			return err
		}
	}

	// Best-effort write to sender's received acks; sender may not exist.
	senderDir := fsq.AgentAcksReceived(root, sender)
	senderPath := filepath.Join(senderDir, msgID+".json")
	if _, err := os.Stat(senderPath); err == nil {
		// Already recorded.
	} else if os.IsNotExist(err) {
		if _, err := fsq.WriteFileAtomic(senderDir, msgID+".json", data, 0o600); err != nil {
			if warnErr := writeStderr("warning: unable to write sender ack: %v\n", err); warnErr != nil {
				return warnErr
			}
		}
	} else if err != nil {
		return err
	}

	if common.JSON {
		return writeJSON(os.Stdout, ackPayload)
	}
	if err := writeStdout("Acked %s\n", header.ID); err != nil {
		return err
	}
	return nil
}

```

File: internal/cli/cleanup.go (654 tokens)
```
package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runCleanup(args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	common := addCommonFlags(fs)
	olderFlag := fs.String("tmp-older-than", "", "Duration (e.g. 36h)")
	dryRunFlag := fs.Bool("dry-run", false, "Show what would be removed without deleting")
	yesFlag := fs.Bool("yes", false, "Skip confirmation prompt")
	usage := usageWithFlags(fs, "amq cleanup --tmp-older-than <duration> [--dry-run] [--yes] [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if *olderFlag == "" {
		return errors.New("--tmp-older-than is required")
	}
	dur, err := time.ParseDuration(*olderFlag)
	if err != nil {
		return err
	}
	if dur <= 0 {
		return errors.New("--tmp-older-than must be > 0")
	}
	root := filepath.Clean(common.Root)
	cutoff := time.Now().Add(-dur)

	candidates, err := fsq.FindTmpFilesOlderThan(root, cutoff)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		if err := writeStdoutLine("No tmp files to remove."); err != nil {
			return err
		}
		return nil
	}

	if *dryRunFlag {
		if common.JSON {
			return writeJSON(os.Stdout, map[string]any{
				"candidates": candidates,
				"count":      len(candidates),
			})
		}
		if err := writeStdout("Would remove %d tmp file(s).\n", len(candidates)); err != nil {
			return err
		}
		for _, path := range candidates {
			if err := writeStdout("%s\n", path); err != nil {
				return err
			}
		}
		return nil
	}

	if !*yesFlag {
		ok, err := confirmPrompt(fmt.Sprintf("Delete %d tmp file(s)?", len(candidates)))
		if err != nil {
			return err
		}
		if !ok {
			if err := writeStdoutLine("Aborted."); err != nil {
				return err
			}
			return nil
		}
	}

	removed := 0
	for _, path := range candidates {
		if err := os.Remove(path); err != nil {
			return err
		}
		removed++
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"removed": removed,
		})
	}
	if err := writeStdout("Removed %d tmp file(s).\n", removed); err != nil {
		return err
	}
	return nil
}

```

File: internal/cli/cli.go (784 tokens)
```
package cli

import "fmt"

const (
	envRoot = "AM_ROOT"
	envMe   = "AM_ME"
)

func Run(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printUsage()
	}

	switch args[0] {
	case "init":
		return runInit(args[1:])
	case "send":
		return runSend(args[1:])
	case "list":
		return runList(args[1:])
	case "read":
		return runRead(args[1:])
	case "ack":
		return runAck(args[1:])
	case "thread":
		return runThread(args[1:])
	case "presence":
		return runPresence(args[1:])
	case "cleanup":
		return runCleanup(args[1:])
	case "watch":
		return runWatch(args[1:])
	case "drain":
		return runDrain(args[1:])
	case "monitor":
		return runMonitor(args[1:])
	case "reply":
		return runReply(args[1:])
	default:
		return fmt.Errorf("unknown command: %s", args[0])
	}
}

func printUsage() error {
	if err := writeStdoutLine("amq - agent message queue"); err != nil {
		return err
	}
	if err := writeStdoutLine(""); err != nil {
		return err
	}
	if err := writeStdoutLine("Usage:"); err != nil {
		return err
	}
	if err := writeStdoutLine("  amq <command> [options]"); err != nil {
		return err
	}
	if err := writeStdoutLine(""); err != nil {
		return err
	}
	if err := writeStdoutLine("Commands:"); err != nil {
		return err
	}
	if err := writeStdoutLine("  init      Initialize the queue root and agent mailboxes"); err != nil {
		return err
	}
	if err := writeStdoutLine("  send      Send a message"); err != nil {
		return err
	}
	if err := writeStdoutLine("  list      List inbox messages"); err != nil {
		return err
	}
	if err := writeStdoutLine("  read      Read a message by id"); err != nil {
		return err
	}
	if err := writeStdoutLine("  ack       Acknowledge a message"); err != nil {
		return err
	}
	if err := writeStdoutLine("  thread    View a thread"); err != nil {
		return err
	}
	if err := writeStdoutLine("  presence  Set or list presence"); err != nil {
		return err
	}
	if err := writeStdoutLine("  cleanup   Remove stale tmp files"); err != nil {
		return err
	}
	if err := writeStdoutLine("  watch     Wait for new messages (uses fsnotify)"); err != nil {
		return err
	}
	if err := writeStdoutLine("  drain     Drain new messages (read, move to cur, ack)"); err != nil {
		return err
	}
	if err := writeStdoutLine("  monitor   Combined watch+drain for co-op mode"); err != nil {
		return err
	}
	if err := writeStdoutLine("  reply     Reply to a message (auto thread/refs)"); err != nil {
		return err
	}
	if err := writeStdoutLine(""); err != nil {
		return err
	}
	if err := writeStdoutLine("Environment:"); err != nil {
		return err
	}
	if err := writeStdoutLine("  AM_ROOT   Default root directory for storage"); err != nil {
		return err
	}
	return writeStdoutLine("  AM_ME     Default agent handle")
}

```

File: internal/cli/common_test.go (731 tokens)
```
package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeHandle(t *testing.T) {
	if got, err := normalizeHandle("codex"); err != nil || got != "codex" {
		t.Fatalf("normalizeHandle valid: %v, %v", got, err)
	}
	if _, err := normalizeHandle("Codex"); err == nil {
		t.Fatalf("expected error for uppercase handle")
	}
	if _, err := normalizeHandle("co/dex"); err == nil {
		t.Fatalf("expected error for invalid characters")
	}
	if got, err := normalizeHandle("codex_1"); err != nil || got != "codex_1" {
		t.Fatalf("normalizeHandle underscore: %v, %v", got, err)
	}
}

func TestValidateKnownHandle(t *testing.T) {
	root := t.TempDir()
	metaDir := filepath.Join(root, "meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create config with known agents
	cfg := map[string]any{
		"version": 1,
		"agents":  []string{"alice", "bob"},
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(metaDir, "config.json"), data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Known handle should pass
	if err := validateKnownHandle(root, "alice", false); err != nil {
		t.Errorf("known handle should pass: %v", err)
	}

	// Unknown handle with strict=false should warn but not error
	if err := validateKnownHandle(root, "unknown", false); err != nil {
		t.Errorf("unknown handle with strict=false should warn, not error: %v", err)
	}

	// Unknown handle with strict=true should error
	if err := validateKnownHandle(root, "unknown", true); err == nil {
		t.Errorf("unknown handle with strict=true should error")
	}
}

func TestValidateKnownHandleNoConfig(t *testing.T) {
	root := t.TempDir()

	// No config file - should pass any handle
	if err := validateKnownHandle(root, "anyhandle", true); err != nil {
		t.Errorf("no config should pass any handle: %v", err)
	}
}

func TestValidateKnownHandleCorruptConfig(t *testing.T) {
	root := t.TempDir()
	metaDir := filepath.Join(root, "meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write invalid JSON
	if err := os.WriteFile(filepath.Join(metaDir, "config.json"), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write corrupt config: %v", err)
	}

	// Corrupt config with strict=false should warn but not error
	if err := validateKnownHandle(root, "alice", false); err != nil {
		t.Errorf("corrupt config with strict=false should warn, not error: %v", err)
	}

	// Corrupt config with strict=true should error
	if err := validateKnownHandle(root, "alice", true); err == nil {
		t.Errorf("corrupt config with strict=true should error")
	}
}

```

File: internal/cli/common.go (2706 tokens)
```
package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type commonFlags struct {
	Root   string
	Me     string
	JSON   bool
	Strict bool
}

func addCommonFlags(fs *flag.FlagSet) *commonFlags {
	flags := &commonFlags{}
	fs.StringVar(&flags.Root, "root", defaultRoot(), "Root directory for the queue")
	fs.StringVar(&flags.Me, "me", defaultMe(), "Agent handle (or AM_ME)")
	fs.BoolVar(&flags.JSON, "json", false, "Emit JSON output")
	fs.BoolVar(&flags.Strict, "strict", false, "Error on unknown handles (default: warn)")
	return flags
}

func defaultRoot() string {
	if env := strings.TrimSpace(os.Getenv(envRoot)); env != "" {
		return env
	}
	return ".agent-mail"
}

func defaultMe() string {
	if env := strings.TrimSpace(os.Getenv(envMe)); env != "" {
		return env
	}
	return ""
}

func requireMe(handle string) error {
	if strings.TrimSpace(handle) == "" {
		return errors.New("--me is required (or set AM_ME)")
	}
	return nil
}

// validateKnownHandle checks if the handle is in config.json (if it exists).
// Returns nil if config doesn't exist or handle is known.
// If strict=true, returns an error for unknown handles or unreadable/corrupt config; otherwise warns to stderr.
func validateKnownHandle(root, handle string, strict bool) error {
	configPath := filepath.Join(root, "meta", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No config, no validation
		}
		// Config exists but unreadable
		msg := fmt.Sprintf("cannot read config.json: %v", err)
		if strict {
			return errors.New(msg)
		}
		_ = writeStderr("warning: %s\n", msg)
		return nil
	}

	var cfg struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		// Config exists but invalid JSON
		msg := fmt.Sprintf("invalid config.json: %v", err)
		if strict {
			return errors.New(msg)
		}
		_ = writeStderr("warning: %s\n", msg)
		return nil
	}

	for _, known := range cfg.Agents {
		if known == handle {
			return nil // Handle is known
		}
	}

	msg := fmt.Sprintf("handle %q not in config.json agents %v", handle, cfg.Agents)
	if strict {
		return errors.New(msg)
	}
	_ = writeStderr("warning: %s\n", msg)
	return nil
}

// validateKnownHandles validates multiple handles against config.json.
// If strict=true, returns an error for unknown handles or unreadable/corrupt config.
func validateKnownHandles(root string, handles []string, strict bool) error {
	configPath := filepath.Join(root, "meta", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No config, no validation
		}
		// Config exists but unreadable
		msg := fmt.Sprintf("cannot read config.json: %v", err)
		if strict {
			return errors.New(msg)
		}
		_ = writeStderr("warning: %s\n", msg)
		return nil
	}

	var cfg struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		// Config exists but invalid JSON
		msg := fmt.Sprintf("invalid config.json: %v", err)
		if strict {
			return errors.New(msg)
		}
		_ = writeStderr("warning: %s\n", msg)
		return nil
	}

	known := make(map[string]bool, len(cfg.Agents))
	for _, a := range cfg.Agents {
		known[a] = true
	}

	var unknown []string
	for _, h := range handles {
		if !known[h] {
			unknown = append(unknown, h)
		}
	}

	if len(unknown) == 0 {
		return nil
	}

	msg := fmt.Sprintf("unknown handles %v (known: %v)", unknown, cfg.Agents)
	if strict {
		return errors.New(msg)
	}
	_ = writeStderr("warning: %s\n", msg)
	return nil
}

func normalizeHandle(raw string) (string, error) {
	handle := strings.TrimSpace(raw)
	if handle == "" {
		return "", errors.New("agent handle cannot be empty")
	}
	if strings.ContainsAny(handle, "/\\") {
		return "", fmt.Errorf("invalid handle (slashes not allowed): %s", handle)
	}
	if handle != strings.ToLower(handle) {
		return "", fmt.Errorf("handle must be lowercase: %s", handle)
	}
	for _, r := range handle {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' {
			continue
		}
		return "", fmt.Errorf("invalid handle (allowed: a-z, 0-9, -, _): %s", handle)
	}
	return handle, nil
}

func parseHandles(raw string) ([]string, error) {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		handle, err := normalizeHandle(part)
		if err != nil {
			return nil, err
		}
		out = append(out, handle)
	}
	return out, nil
}

func splitRecipients(raw string) ([]string, error) {
	out, err := parseHandles(raw)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("--to is required")
	}
	return out, nil
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func splitList(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func readBody(bodyFlag string) (string, error) {
	if bodyFlag == "" || bodyFlag == "@-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	if strings.HasPrefix(bodyFlag, "@") {
		path := strings.TrimPrefix(bodyFlag, "@")
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return bodyFlag, nil
}

func isHelp(arg string) bool {
	switch arg {
	case "-h", "--help", "help":
		return true
	default:
		return false
	}
}

func parseFlags(fs *flag.FlagSet, args []string, usage func()) (bool, error) {
	fs.SetOutput(io.Discard)
	if usage != nil {
		fs.Usage = usage
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func usageWithFlags(fs *flag.FlagSet, usage string, notes ...string) func() {
	return func() {
		_ = writeStdoutLine("Usage:")
		_ = writeStdoutLine("  " + usage)
		if len(notes) > 0 {
			_ = writeStdoutLine("")
			for _, note := range notes {
				_ = writeStdoutLine(note)
			}
		}
		_ = writeStdoutLine("")
		_ = writeStdoutLine("Options:")
		_ = writeFlagDefaults(fs)
	}
}

func writeFlagDefaults(fs *flag.FlagSet) error {
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
	if buf.Len() == 0 {
		return nil
	}
	return writeStdout("%s", buf.String())
}

func confirmPrompt(prompt string) (bool, error) {
	if err := writeStdout("%s", prompt); err != nil {
		return false, err
	}
	if err := writeStdout(" [y/N]: "); err != nil {
		return false, err
	}
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes", nil
}

func ensureFilename(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("message id is required")
	}
	if id == "." || id == ".." {
		return "", fmt.Errorf("invalid message id: %s", id)
	}
	if filepath.Base(id) != id {
		return "", fmt.Errorf("invalid message id: %s", id)
	}
	if strings.ContainsAny(id, "/\\") {
		return "", fmt.Errorf("invalid message id: %s", id)
	}
	if !strings.HasSuffix(id, ".md") {
		id += ".md"
	}
	return id, nil
}

func ensureSafeBaseName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name cannot be empty")
	}
	if name == "." || name == ".." {
		return "", fmt.Errorf("invalid name: %s", name)
	}
	if filepath.Base(name) != name {
		return "", fmt.Errorf("invalid name: %s", name)
	}
	if strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("invalid name: %s", name)
	}
	return name, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeStdout(format string, args ...any) error {
	_, err := fmt.Fprintf(os.Stdout, format, args...)
	return err
}

func writeStdoutLine(args ...any) error {
	_, err := fmt.Fprintln(os.Stdout, args...)
	return err
}

func writeStderr(format string, args ...any) error {
	_, err := fmt.Fprintf(os.Stderr, format, args...)
	return err
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// parseContext parses a context JSON string or @file.json.
func parseContext(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var data []byte
	if strings.HasPrefix(raw, "@") {
		path := strings.TrimPrefix(raw, "@")
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read context file: %w", err)
		}
	} else {
		data = []byte(raw)
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse context JSON: %w", err)
	}
	return result, nil
}

```

File: internal/cli/drain_test.go (4517 tokens)
```
package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/ack"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunDrainEmpty(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	t.Run("empty inbox returns empty JSON", func(t *testing.T) {
		result := runDrainJSON(t, root, "alice", 0, false, true)
		if result.Count != 0 {
			t.Errorf("expected count 0, got %d", result.Count)
		}
		if len(result.Drained) != 0 {
			t.Errorf("expected empty drained, got %d items", len(result.Drained))
		}
	})

	t.Run("empty inbox silent in text mode", func(t *testing.T) {
		output := runDrainText(t, root, "alice", 0, false, true)
		if output != "" {
			t.Errorf("expected empty output, got %q", output)
		}
	})
}

func TestRunDrainMovesToCur(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a message
	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "test-msg-1",
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "Test message",
			Created: "2025-12-24T10:00:00Z",
		},
		Body: "Hello Alice!",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "alice", "test-msg-1.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// Verify message is in new
	newPath := filepath.Join(fsq.AgentInboxNew(root, "alice"), "test-msg-1.md")
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("message should be in new: %v", err)
	}

	// Drain
	result := runDrainJSON(t, root, "alice", 0, false, true)
	if result.Count != 1 {
		t.Fatalf("expected count 1, got %d", result.Count)
	}
	if result.Drained[0].ID != "test-msg-1" {
		t.Errorf("expected ID test-msg-1, got %s", result.Drained[0].ID)
	}
	if !result.Drained[0].MovedToCur {
		t.Errorf("expected MovedToCur=true")
	}

	// Verify message moved to cur
	curPath := filepath.Join(fsq.AgentInboxCur(root, "alice"), "test-msg-1.md")
	if _, err := os.Stat(curPath); err != nil {
		t.Errorf("message should be in cur: %v", err)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Errorf("message should NOT be in new anymore")
	}

	// Second drain should return empty
	result2 := runDrainJSON(t, root, "alice", 0, false, true)
	if result2.Count != 0 {
		t.Errorf("second drain should be empty, got %d", result2.Count)
	}
}

func TestRunDrainWithBody(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "body-test",
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "With body",
			Created: "2025-12-24T11:00:00Z",
		},
		Body: "This is the message body.",
	}
	data, _ := msg.Marshal()
	if _, err := fsq.DeliverToInbox(root, "alice", "body-test.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	t.Run("without include-body", func(t *testing.T) {
		// Need to re-create since previous test moved it
		root := t.TempDir()
		if err := fsq.EnsureRootDirs(root); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
			t.Fatal(err)
		}
		if _, err := fsq.DeliverToInbox(root, "alice", "body-test.md", data); err != nil {
			t.Fatal(err)
		}

		result := runDrainJSON(t, root, "alice", 0, false, true)
		if result.Drained[0].Body != "" {
			t.Errorf("expected empty body, got %q", result.Drained[0].Body)
		}
	})

	t.Run("with include-body", func(t *testing.T) {
		root := t.TempDir()
		if err := fsq.EnsureRootDirs(root); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
			t.Fatal(err)
		}
		if _, err := fsq.DeliverToInbox(root, "alice", "body-test.md", data); err != nil {
			t.Fatal(err)
		}

		result := runDrainJSON(t, root, "alice", 0, true, true)
		if result.Drained[0].Body != "This is the message body.\n" {
			t.Errorf("expected body, got %q", result.Drained[0].Body)
		}
	})
}

func TestRunDrainWithAck(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Message that requires ack
	msg := format.Message{
		Header: format.Header{
			Schema:      1,
			ID:          "ack-test",
			From:        "bob",
			To:          []string{"alice"},
			Thread:      "p2p/alice__bob",
			Subject:     "Please ack",
			Created:     "2025-12-24T12:00:00Z",
			AckRequired: true,
		},
		Body: "Ack me!",
	}
	data, _ := msg.Marshal()
	if _, err := fsq.DeliverToInbox(root, "alice", "ack-test.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	result := runDrainJSON(t, root, "alice", 0, false, true)
	if !result.Drained[0].AckRequired {
		t.Errorf("expected AckRequired=true")
	}
	if !result.Drained[0].Acked {
		t.Errorf("expected Acked=true")
	}

	// Check ack file was created
	ackPath := filepath.Join(fsq.AgentAcksSent(root, "alice"), "ack-test.json")
	if _, err := os.Stat(ackPath); err != nil {
		t.Errorf("ack file should exist: %v", err)
	}
}

func TestRunDrainWithoutAck(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:      1,
			ID:          "no-ack-test",
			From:        "bob",
			To:          []string{"alice"},
			Thread:      "p2p/alice__bob",
			Subject:     "No ack",
			Created:     "2025-12-24T12:30:00Z",
			AckRequired: true,
		},
		Body: "No ack please",
	}
	data, _ := msg.Marshal()
	if _, err := fsq.DeliverToInbox(root, "alice", "no-ack-test.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	result := runDrainJSON(t, root, "alice", 0, false, false)
	if !result.Drained[0].AckRequired {
		t.Errorf("expected AckRequired=true")
	}
	if result.Drained[0].Acked {
		t.Errorf("expected Acked=false")
	}

	ackPath := filepath.Join(fsq.AgentAcksSent(root, "alice"), "no-ack-test.json")
	if _, err := os.Stat(ackPath); !os.IsNotExist(err) {
		t.Errorf("ack file should not exist")
	}
}

func TestRunDrainCorruptAckRewritten(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:      1,
			ID:          "corrupt-ack-test",
			From:        "bob",
			To:          []string{"alice"},
			Thread:      "p2p/alice__bob",
			Created:     "2025-12-24T12:40:00Z",
			AckRequired: true,
		},
		Body: "Ack me",
	}
	data, _ := msg.Marshal()
	if _, err := fsq.DeliverToInbox(root, "alice", "corrupt-ack-test.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	ackPath := filepath.Join(fsq.AgentAcksSent(root, "alice"), "corrupt-ack-test.json")
	if err := os.WriteFile(ackPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt ack: %v", err)
	}

	result := runDrainJSON(t, root, "alice", 0, false, true)
	if !result.Drained[0].Acked {
		t.Errorf("expected Acked=true")
	}
	if _, err := os.Stat(ackPath); err != nil {
		t.Fatalf("ack file should exist: %v", err)
	}
	if _, err := ack.Read(ackPath); err != nil {
		t.Fatalf("ack file should be valid json: %v", err)
	}
}

func TestRunDrainLimit(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create 5 messages
	for i := 0; i < 5; i++ {
		msg := format.Message{
			Header: format.Header{
				Schema:  1,
				ID:      "msg-" + string(rune('a'+i)),
				From:    "bob",
				To:      []string{"alice"},
				Thread:  "p2p/alice__bob",
				Subject: "Test " + string(rune('A'+i)),
				Created: "2025-12-24T10:00:0" + string(rune('0'+i)) + "Z",
			},
			Body: "body",
		}
		data, _ := msg.Marshal()
		if _, err := fsq.DeliverToInbox(root, "alice", "msg-"+string(rune('a'+i))+".md", data); err != nil {
			t.Fatalf("deliver msg %d: %v", i, err)
		}
	}

	// Drain with limit 2
	result := runDrainJSON(t, root, "alice", 2, false, true)
	if result.Count != 2 {
		t.Errorf("expected count 2, got %d", result.Count)
	}

	// Verify only 2 moved to cur
	curEntries, _ := os.ReadDir(fsq.AgentInboxCur(root, "alice"))
	if len(curEntries) != 2 {
		t.Errorf("expected 2 in cur, got %d", len(curEntries))
	}

	// Verify 3 still in new
	newEntries, _ := os.ReadDir(fsq.AgentInboxNew(root, "alice"))
	if len(newEntries) != 3 {
		t.Errorf("expected 3 in new, got %d", len(newEntries))
	}
}

func TestRunDrainCorruptMessage(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Write a corrupt message directly
	newDir := fsq.AgentInboxNew(root, "alice")
	corruptPath := filepath.Join(newDir, "corrupt.md")
	if err := os.WriteFile(corruptPath, []byte("not valid frontmatter"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	result := runDrainJSON(t, root, "alice", 0, false, true)
	if result.Count != 1 {
		t.Fatalf("expected count 1, got %d", result.Count)
	}
	if result.Drained[0].ParseError == "" {
		t.Errorf("expected parse error for corrupt message")
	}
	if !result.Drained[0].MovedToCur {
		t.Errorf("corrupt message should still be moved to cur")
	}

	// Verify corrupt message moved to cur
	curPath := filepath.Join(fsq.AgentInboxCur(root, "alice"), "corrupt.md")
	if _, err := os.Stat(curPath); err != nil {
		t.Errorf("corrupt message should be in cur: %v", err)
	}
}

func TestRunDrainSorting(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create messages out of order (filesystem order != timestamp order)
	timestamps := []string{
		"2025-12-24T10:00:03Z",
		"2025-12-24T10:00:01Z",
		"2025-12-24T10:00:02Z",
	}
	for i, ts := range timestamps {
		msg := format.Message{
			Header: format.Header{
				Schema:  1,
				ID:      "msg-" + string(rune('a'+i)),
				From:    "bob",
				To:      []string{"alice"},
				Thread:  "p2p/alice__bob",
				Created: ts,
			},
			Body: "body",
		}
		data, _ := msg.Marshal()
		if _, err := fsq.DeliverToInbox(root, "alice", "msg-"+string(rune('a'+i))+".md", data); err != nil {
			t.Fatalf("deliver msg %d: %v", i, err)
		}
	}

	result := runDrainJSON(t, root, "alice", 0, false, true)
	if result.Count != 3 {
		t.Fatalf("expected 3, got %d", result.Count)
	}

	// Should be sorted by timestamp: b (01), c (02), a (03)
	expected := []string{"msg-b", "msg-c", "msg-a"}
	for i, exp := range expected {
		if result.Drained[i].ID != exp {
			t.Errorf("position %d: expected %s, got %s", i, exp, result.Drained[i].ID)
		}
	}
}

func runDrainJSON(t *testing.T, root, agent string, limit int, includeBody, ack bool) drainResult {
	t.Helper()
	args := []string{"--root", root, "--me", agent, "--json"}
	if limit > 0 {
		args = append(args, "--limit", itoa(limit))
	}
	if includeBody {
		args = append(args, "--include-body")
	}
	if !ack {
		args = append(args, "--ack=false")
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runDrain(args)

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runDrain: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var result drainResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v (output: %s)", err, buf.String())
	}
	return result
}

func runDrainText(t *testing.T, root, agent string, limit int, includeBody, ack bool) string {
	t.Helper()
	args := []string{"--root", root, "--me", agent}
	if limit > 0 {
		args = append(args, "--limit", itoa(limit))
	}
	if includeBody {
		args = append(args, "--include-body")
	}
	if !ack {
		args = append(args, "--ack=false")
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runDrain(args)

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runDrain: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String()
}

```

File: internal/cli/drain.go (2354 tokens)
```
package cli

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/ack"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type drainItem struct {
	ID          string         `json:"id"`
	From        string         `json:"from"`
	To          []string       `json:"to"`
	Thread      string         `json:"thread"`
	Subject     string         `json:"subject"`
	Created     string         `json:"created"`
	Body        string         `json:"body,omitempty"`
	AckRequired bool           `json:"ack_required"`
	MovedToCur  bool           `json:"moved_to_cur"`
	Acked       bool           `json:"acked"`
	ParseError  string         `json:"parse_error,omitempty"`
	Priority    string         `json:"priority,omitempty"`
	Kind        string         `json:"kind,omitempty"`
	Labels      []string       `json:"labels,omitempty"`
	Context     map[string]any `json:"context,omitempty"`
	Filename    string         `json:"-"` // actual filename on disk
	SortKey     time.Time      `json:"-"`
}

type drainResult struct {
	Drained []drainItem `json:"drained"`
	Count   int         `json:"count"`
}

func runDrain(args []string) error {
	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	common := addCommonFlags(fs)
	limitFlag := fs.Int("limit", 20, "Max messages to drain (0 = no limit)")
	includeBodyFlag := fs.Bool("include-body", false, "Include message body in output")
	ackFlag := fs.Bool("ack", true, "Acknowledge messages that require ack")

	usage := usageWithFlags(fs, "amq drain --me <agent> [options]",
		"Drains new messages: reads, moves to cur, optionally acks.",
		"Designed for hook/script integration. Quiet when empty.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)

	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	newDir := fsq.AgentInboxNew(root, common.Me)
	entries, err := os.ReadDir(newDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No inbox/new directory - nothing to drain
			if common.JSON {
				return writeJSON(os.Stdout, drainResult{Drained: []drainItem{}, Count: 0})
			}
			// Silent for text mode when empty (hook-friendly)
			return nil
		}
		return err
	}

	// Collect items with timestamps for sorting
	items := make([]drainItem, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := entry.Name()
		path := filepath.Join(newDir, filename)

		item := drainItem{
			ID:       filename, // fallback to filename if parse fails
			Filename: filename,
		}

		// Try to parse the message
		var header format.Header
		var body string
		var parseErr error

		if *includeBodyFlag {
			msg, err := format.ReadMessageFile(path)
			if err != nil {
				parseErr = err
			} else {
				header = msg.Header
				body = msg.Body
			}
		} else {
			header, parseErr = format.ReadHeaderFile(path)
		}

		if parseErr != nil {
			item.ParseError = parseErr.Error()
			// Still move corrupt message to cur to avoid reprocessing
		} else {
			item.ID = header.ID
			item.From = header.From
			item.To = header.To
			item.Thread = header.Thread
			item.Subject = header.Subject
			item.Created = header.Created
			item.AckRequired = header.AckRequired
			item.Priority = header.Priority
			item.Kind = header.Kind
			item.Labels = header.Labels
			item.Context = header.Context
			if *includeBodyFlag {
				item.Body = body
			}
			if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
				item.SortKey = ts
			}
		}

		items = append(items, item)
	}

	// Sort by timestamp (oldest first)
	sort.Slice(items, func(i, j int) bool {
		if !items[i].SortKey.IsZero() && !items[j].SortKey.IsZero() {
			if items[i].SortKey.Equal(items[j].SortKey) {
				return items[i].ID < items[j].ID
			}
			return items[i].SortKey.Before(items[j].SortKey)
		}
		if items[i].Created == items[j].Created {
			return items[i].ID < items[j].ID
		}
		return items[i].Created < items[j].Created
	})

	// Apply limit
	if *limitFlag > 0 && len(items) > *limitFlag {
		items = items[:*limitFlag]
	}

	// Nothing to drain
	if len(items) == 0 {
		if common.JSON {
			return writeJSON(os.Stdout, drainResult{Drained: []drainItem{}, Count: 0})
		}
		// Silent for text mode when empty (hook-friendly)
		return nil
	}

	// Process each message: move to cur, optionally ack
	for i := range items {
		item := &items[i]

		// Move new -> cur
		if err := fsq.MoveNewToCur(root, common.Me, item.Filename); err != nil {
			if os.IsNotExist(err) {
				// Likely moved by another drain; check if it's already in cur.
				curPath := filepath.Join(fsq.AgentInboxCur(root, common.Me), item.Filename)
				if _, statErr := os.Stat(curPath); statErr == nil {
					item.MovedToCur = true
				} else if statErr != nil && !os.IsNotExist(statErr) {
					_ = writeStderr("warning: failed to stat %s in cur: %v\n", item.Filename, statErr)
				}
			} else {
				// Log warning but continue
				_ = writeStderr("warning: failed to move %s to cur: %v\n", item.Filename, err)
			}
		} else {
			item.MovedToCur = true
		}

		// Ack if required and --ack is set
		if *ackFlag && item.AckRequired && item.ParseError == "" && item.MovedToCur {
			if err := ackMessage(root, common.Me, item); err != nil {
				_ = writeStderr("warning: failed to ack %s: %v\n", item.ID, err)
			} else {
				item.Acked = true
			}
		}
	}

	if common.JSON {
		return writeJSON(os.Stdout, drainResult{Drained: items, Count: len(items)})
	}

	// Text output
	if err := writeStdout("[AMQ] %d new message(s) for %s:\n\n", len(items), common.Me); err != nil {
		return err
	}
	for _, item := range items {
		if item.ParseError != "" {
			if err := writeStdout("- ID: %s\n  ERROR: %s\n---\n", item.ID, item.ParseError); err != nil {
				return err
			}
			continue
		}
		subject := item.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		priority := item.Priority
		if priority == "" {
			priority = "-"
		}
		kind := item.Kind
		if kind == "" {
			kind = "-"
		}
		if err := writeStdout("- From: %s\n  Thread: %s\n  ID: %s\n  Subject: %s\n  Priority: %s\n  Kind: %s\n  Created: %s\n",
			item.From, item.Thread, item.ID, subject, priority, kind, item.Created); err != nil {
			return err
		}
		if *includeBodyFlag && item.Body != "" {
			if err := writeStdout("  Body:\n%s\n", item.Body); err != nil {
				return err
			}
		}
		if err := writeStdout("---\n"); err != nil {
			return err
		}
	}
	return nil
}

func ackMessage(root, me string, item *drainItem) error {
	sender, err := normalizeHandle(item.From)
	if err != nil {
		return err
	}
	msgID, err := ensureSafeBaseName(item.ID)
	if err != nil {
		return err
	}

	ackPayload := ack.New(item.ID, item.Thread, me, sender, time.Now())

	// Write to receiver's sent acks
	receiverDir := fsq.AgentAcksSent(root, me)
	receiverPath := filepath.Join(receiverDir, msgID+".json")
	needsReceiverWrite := true
	if existing, err := ack.Read(receiverPath); err == nil {
		ackPayload = existing
		needsReceiverWrite = false
	} else if !os.IsNotExist(err) {
		// Corrupt ack file - warn and rewrite
		_ = writeStderr("warning: corrupt ack file, rewriting: %v\n", err)
	}

	data, err := ackPayload.Marshal()
	if err != nil {
		return err
	}

	if needsReceiverWrite {
		if _, err := fsq.WriteFileAtomic(receiverDir, msgID+".json", data, 0o600); err != nil {
			return err
		}
	}

	// Best-effort write to sender's received acks
	senderDir := fsq.AgentAcksReceived(root, sender)
	senderPath := filepath.Join(senderDir, msgID+".json")
	if _, err := os.Stat(senderPath); err == nil {
		// Already recorded.
	} else if os.IsNotExist(err) {
		if _, err := fsq.WriteFileAtomic(senderDir, msgID+".json", data, 0o600); err != nil {
			_ = writeStderr("warning: unable to write sender ack: %v\n", err)
		}
	} else if err != nil {
		return err
	}

	return nil
}

```

File: internal/cli/init.go (444 tokens)
```
package cli

import (
	"errors"
	"flag"
	"path/filepath"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	rootFlag := fs.String("root", defaultRoot(), "Root directory for the queue")
	agentsFlag := fs.String("agents", "", "Comma-separated agent handles (required)")
	forceFlag := fs.Bool("force", false, "Overwrite existing config.json if present")

	usage := usageWithFlags(fs, "amq init --root <path> --agents a,b,c [--force]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	agents, err := parseHandles(*agentsFlag)
	if err != nil {
		return err
	}
	if len(agents) == 0 {
		return errors.New("--agents is required")
	}
	agents = dedupeStrings(agents)
	sort.Strings(agents)

	root := filepath.Clean(*rootFlag)
	if err := fsq.EnsureRootDirs(root); err != nil {
		return err
	}

	for _, agent := range agents {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			return err
		}
	}

	cfgPath := filepath.Join(root, "meta", "config.json")
	cfg := config.Config{
		Version:    format.CurrentVersion,
		CreatedUTC: time.Now().UTC().Format(time.RFC3339),
		Agents:     agents,
	}
	if err := config.WriteConfig(cfgPath, cfg, *forceFlag); err != nil {
		return err
	}

	if err := writeStdout("Initialized AMQ root at %s\n", root); err != nil {
		return err
	}
	return nil
}

```

File: internal/cli/list_test.go (1197 tokens)
```
package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunListPagination(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create 5 messages with distinct timestamps
	for i := 0; i < 5; i++ {
		msg := format.Message{
			Header: format.Header{
				Schema:  1,
				ID:      "msg-" + string(rune('a'+i)),
				From:    "bob",
				To:      []string{"alice"},
				Thread:  "p2p/alice__bob",
				Subject: "Test " + string(rune('A'+i)),
				Created: "2025-12-24T10:00:0" + string(rune('0'+i)) + "Z",
			},
			Body: "body",
		}
		data, err := msg.Marshal()
		if err != nil {
			t.Fatalf("marshal msg %d: %v", i, err)
		}
		if _, err := fsq.DeliverToInbox(root, "alice", "msg-"+string(rune('a'+i))+".md", data); err != nil {
			t.Fatalf("deliver msg %d: %v", i, err)
		}
	}

	t.Run("no pagination returns all", func(t *testing.T) {
		items := runListJSON(t, root, "alice", 0, 0)
		if len(items) != 5 {
			t.Errorf("expected 5 items, got %d", len(items))
		}
	})

	t.Run("limit 2 returns first 2", func(t *testing.T) {
		items := runListJSON(t, root, "alice", 2, 0)
		if len(items) != 2 {
			t.Errorf("expected 2 items, got %d", len(items))
		}
		if items[0].ID != "msg-a" {
			t.Errorf("expected first item msg-a, got %s", items[0].ID)
		}
		if items[1].ID != "msg-b" {
			t.Errorf("expected second item msg-b, got %s", items[1].ID)
		}
	})

	t.Run("offset 2 skips first 2", func(t *testing.T) {
		items := runListJSON(t, root, "alice", 0, 2)
		if len(items) != 3 {
			t.Errorf("expected 3 items, got %d", len(items))
		}
		if items[0].ID != "msg-c" {
			t.Errorf("expected first item msg-c, got %s", items[0].ID)
		}
	})

	t.Run("limit 2 offset 2 returns middle 2", func(t *testing.T) {
		items := runListJSON(t, root, "alice", 2, 2)
		if len(items) != 2 {
			t.Errorf("expected 2 items, got %d", len(items))
		}
		if items[0].ID != "msg-c" {
			t.Errorf("expected first item msg-c, got %s", items[0].ID)
		}
		if items[1].ID != "msg-d" {
			t.Errorf("expected second item msg-d, got %s", items[1].ID)
		}
	})

	t.Run("offset beyond range returns empty", func(t *testing.T) {
		items := runListJSON(t, root, "alice", 0, 100)
		if len(items) != 0 {
			t.Errorf("expected 0 items, got %d", len(items))
		}
	})
}

func runListJSON(t *testing.T, root, agent string, limit, offset int) []listItem {
	t.Helper()
	args := []string{"--root", root, "--me", agent, "--json", "--new"}
	if limit > 0 {
		args = append(args, "--limit", itoa(limit))
	}
	if offset > 0 {
		args = append(args, "--offset", itoa(offset))
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runList(args)

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runList: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var items []listItem
	if err := json.Unmarshal(buf.Bytes(), &items); err != nil {
		t.Fatalf("unmarshal: %v (output: %s)", err, buf.String())
	}
	return items
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

```

File: internal/cli/list.go (1229 tokens)
```
package cli

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type listItem struct {
	ID       string    `json:"id"`
	From     string    `json:"from"`
	Subject  string    `json:"subject"`
	Thread   string    `json:"thread"`
	Created  string    `json:"created"`
	Box      string    `json:"box"`
	Path     string    `json:"path"`
	Priority string    `json:"priority,omitempty"`
	Kind     string    `json:"kind,omitempty"`
	Labels   []string  `json:"labels,omitempty"`
	SortKey  time.Time `json:"-"`
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	common := addCommonFlags(fs)
	newFlag := fs.Bool("new", false, "List messages in inbox/new")
	curFlag := fs.Bool("cur", false, "List messages in inbox/cur")
	limitFlag := fs.Int("limit", 0, "Limit number of messages (0 = no limit)")
	offsetFlag := fs.Int("offset", 0, "Offset into sorted results (0 = start)")

	usage := usageWithFlags(fs, "amq list --me <agent> [--new | --cur] [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)

	// Validate handle against config.json
	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	box := "new"
	if *newFlag && *curFlag {
		return errors.New("use only one of --new or --cur")
	}
	if *curFlag {
		box = "cur"
	}
	if *limitFlag < 0 {
		return errors.New("--limit must be >= 0")
	}
	if *offsetFlag < 0 {
		return errors.New("--offset must be >= 0")
	}

	var dir string
	if box == "new" {
		dir = fsq.AgentInboxNew(root, common.Me)
	} else {
		dir = fsq.AgentInboxCur(root, common.Me)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if common.JSON {
				return writeJSON(os.Stdout, []listItem{})
			}
			if err := writeStdoutLine("No messages."); err != nil {
				return err
			}
			return nil
		}
		return err
	}

	items := make([]listItem, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		header, err := format.ReadHeaderFile(path)
		if err != nil {
			if err := writeStderr("warning: skipping corrupt message %s: %v\n", entry.Name(), err); err != nil {
				return err
			}
			continue
		}
		item := listItem{
			ID:       header.ID,
			From:     header.From,
			Subject:  header.Subject,
			Thread:   header.Thread,
			Created:  header.Created,
			Box:      box,
			Path:     path,
			Priority: header.Priority,
			Kind:     header.Kind,
			Labels:   header.Labels,
		}
		if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
			item.SortKey = ts
		}
		items = append(items, item)
	}

	sort.Slice(items, func(i, j int) bool {
		if !items[i].SortKey.IsZero() && !items[j].SortKey.IsZero() {
			if items[i].SortKey.Equal(items[j].SortKey) {
				return items[i].ID < items[j].ID
			}
			return items[i].SortKey.Before(items[j].SortKey)
		}
		if items[i].Created == items[j].Created {
			return items[i].ID < items[j].ID
		}
		return items[i].Created < items[j].Created
	})

	if *offsetFlag > 0 {
		if *offsetFlag >= len(items) {
			items = []listItem{}
		} else {
			items = items[*offsetFlag:]
		}
	}
	if *limitFlag > 0 && len(items) > *limitFlag {
		items = items[:*limitFlag]
	}

	if common.JSON {
		return writeJSON(os.Stdout, items)
	}

	if len(items) == 0 {
		if err := writeStdoutLine("No messages."); err != nil {
			return err
		}
		return nil
	}
	for _, item := range items {
		subject := item.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		priority := item.Priority
		if priority == "" {
			priority = "-"
		}
		if err := writeStdout("%s  %-6s  %s  %s  %s\n", item.Created, priority, item.From, item.ID, strings.TrimSpace(subject)); err != nil {
			return err
		}
	}
	return nil
}

```

File: internal/cli/monitor_test.go (1635 tokens)
```
package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestMonitor_ExistingMessages(t *testing.T) {
	root := t.TempDir()
	agent := "alice"

	// Initialize mailbox
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a test message with co-op fields
	now := time.Now()
	id, _ := format.NewMessageID(now)
	msg := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       id,
			From:     "bob",
			To:       []string{agent},
			Thread:   "p2p/alice__bob",
			Subject:  "Test review",
			Created:  now.UTC().Format(time.RFC3339Nano),
			Priority: format.PriorityUrgent,
			Kind:     format.KindReviewRequest,
			Labels:   []string{"test", "urgent"},
		},
		Body: "Please review this.",
	}
	data, _ := msg.Marshal()
	filename := id + ".md"
	if _, err := fsq.DeliverToInboxes(root, []string{agent}, filename, data); err != nil {
		t.Fatalf("DeliverToInboxes: %v", err)
	}

	// Capture output
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Run monitor (should drain existing and exit)
	err := runMonitor([]string{
		"--me", agent,
		"--root", root,
		"--json",
		"--timeout", "1s",
		"--include-body",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runMonitor: %v", err)
	}

	// Read output
	var buf [4096]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	// Parse JSON
	var result monitorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\noutput: %s", err, output)
	}

	// Verify result
	if result.Event != "messages" {
		t.Errorf("expected event=messages, got %s", result.Event)
	}
	if result.WatchEvent != "existing" {
		t.Errorf("expected watch_event=existing, got %s", result.WatchEvent)
	}
	if result.Count != 1 {
		t.Errorf("expected count=1, got %d", result.Count)
	}
	if len(result.Drained) != 1 {
		t.Fatalf("expected 1 drained item, got %d", len(result.Drained))
	}

	item := result.Drained[0]
	if item.Priority != format.PriorityUrgent {
		t.Errorf("expected priority=urgent, got %s", item.Priority)
	}
	if item.Kind != format.KindReviewRequest {
		t.Errorf("expected kind=review_request, got %s", item.Kind)
	}
	if len(item.Labels) != 2 {
		t.Errorf("expected 2 labels, got %d", len(item.Labels))
	}
	if item.Body != "Please review this.\n" {
		t.Errorf("unexpected body: %q", item.Body)
	}

	// Verify message moved to cur
	curPath := filepath.Join(fsq.AgentInboxCur(root, agent), filename)
	if _, err := os.Stat(curPath); os.IsNotExist(err) {
		t.Error("message not moved to cur")
	}
}

func TestMonitor_Timeout(t *testing.T) {
	root := t.TempDir()
	agent := "alice"

	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Capture output
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Run monitor with short timeout (no messages)
	err := runMonitor([]string{
		"--me", agent,
		"--root", root,
		"--json",
		"--timeout", "100ms",
		"--poll",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runMonitor: %v", err)
	}

	// Read output
	var buf [4096]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	// Parse JSON
	var result monitorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\noutput: %s", err, output)
	}

	// Verify timeout result
	if result.Event != "timeout" {
		t.Errorf("expected event=timeout, got %s", result.Event)
	}
	if result.Count != 0 {
		t.Errorf("expected count=0, got %d", result.Count)
	}
}

func TestMonitor_PriorityInOutput(t *testing.T) {
	root := t.TempDir()
	agent := "alice"

	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create messages with different priorities
	priorities := []string{format.PriorityLow, format.PriorityNormal, format.PriorityUrgent}
	for i, p := range priorities {
		now := time.Now().Add(time.Duration(i) * time.Second)
		id, _ := format.NewMessageID(now)
		msg := format.Message{
			Header: format.Header{
				Schema:   format.CurrentSchema,
				ID:       id,
				From:     "bob",
				To:       []string{agent},
				Thread:   "p2p/alice__bob",
				Subject:  "Priority " + p,
				Created:  now.UTC().Format(time.RFC3339Nano),
				Priority: p,
			},
			Body: "Test",
		}
		data, _ := msg.Marshal()
		_, _ = fsq.DeliverToInboxes(root, []string{agent}, id+".md", data)
	}

	// Capture output
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runMonitor([]string{
		"--me", agent,
		"--root", root,
		"--json",
		"--timeout", "1s",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runMonitor: %v", err)
	}

	var buf [8192]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	var result monitorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v", err)
	}

	if result.Count != 3 {
		t.Errorf("expected 3 messages, got %d", result.Count)
	}

	// Verify all priorities present
	foundPriorities := make(map[string]bool)
	for _, item := range result.Drained {
		foundPriorities[item.Priority] = true
	}
	for _, p := range priorities {
		if !foundPriorities[p] {
			t.Errorf("priority %s not found in output", p)
		}
	}
}

```

File: internal/cli/monitor.go (2944 tokens)
```
package cli

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/ack"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
)

type monitorItem struct {
	ID          string         `json:"id"`
	From        string         `json:"from"`
	To          []string       `json:"to"`
	Thread      string         `json:"thread"`
	Subject     string         `json:"subject"`
	Created     string         `json:"created"`
	Body        string         `json:"body,omitempty"`
	AckRequired bool           `json:"ack_required"`
	Priority    string         `json:"priority,omitempty"`
	Kind        string         `json:"kind,omitempty"`
	Labels      []string       `json:"labels,omitempty"`
	Context     map[string]any `json:"context,omitempty"`
	MovedToCur  bool           `json:"moved_to_cur"`
	Acked       bool           `json:"acked"`
	ParseError  string         `json:"parse_error,omitempty"`
	Filename    string         `json:"-"`
	SortKey     time.Time      `json:"-"`
}

type monitorResult struct {
	Event      string        `json:"event"`                 // "messages", "timeout", "empty"
	WatchEvent string        `json:"watch_event,omitempty"` // "existing", "new_message", ""
	Me         string        `json:"me"`
	Count      int           `json:"count"`
	Drained    []monitorItem `json:"drained"`
}

func runMonitor(args []string) error {
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	common := addCommonFlags(fs)
	timeoutFlag := fs.Duration("timeout", 60*time.Second, "Max time to wait for messages (0 = wait forever)")
	pollFlag := fs.Bool("poll", false, "Use polling fallback instead of fsnotify")
	includeBodyFlag := fs.Bool("include-body", false, "Include message body in output")
	ackFlag := fs.Bool("ack", true, "Acknowledge messages that require ack")
	limitFlag := fs.Int("limit", 20, "Max messages to drain (0 = no limit)")

	usage := usageWithFlags(fs, "amq monitor --me <agent> [options]",
		"Combined watch+drain: waits for messages, drains them, outputs structured payload.",
		"Ideal for co-op mode background watchers in Claude Code or Codex.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)

	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	inboxNew := fsq.AgentInboxNew(root, common.Me)
	if err := os.MkdirAll(inboxNew, 0o700); err != nil {
		return err
	}

	// First, try to drain existing messages
	items, err := drainMessages(root, common.Me, *includeBodyFlag, *ackFlag, *limitFlag)
	if err != nil {
		return err
	}

	if len(items) > 0 {
		return outputMonitorResult(common.JSON, monitorResult{
			Event:      "messages",
			WatchEvent: "existing",
			Me:         common.Me,
			Count:      len(items),
			Drained:    items,
		})
	}

	// No existing messages - wait for new ones
	ctx := context.Background()
	if *timeoutFlag > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeoutFlag)
		defer cancel()
	}

	var watchEvent string
	var watchErr error

	if *pollFlag {
		watchEvent, watchErr = monitorWithPolling(ctx, inboxNew)
	} else {
		watchEvent, watchErr = monitorWithFsnotify(ctx, inboxNew)
	}

	if watchErr != nil {
		if errors.Is(watchErr, context.DeadlineExceeded) {
			return outputMonitorResult(common.JSON, monitorResult{
				Event:   "timeout",
				Me:      common.Me,
				Count:   0,
				Drained: []monitorItem{},
			})
		}
		return watchErr
	}

	// New message arrived - drain it
	items, err = drainMessages(root, common.Me, *includeBodyFlag, *ackFlag, *limitFlag)
	if err != nil {
		return err
	}

	result := monitorResult{
		Event:      "messages",
		WatchEvent: watchEvent,
		Me:         common.Me,
		Count:      len(items),
		Drained:    items,
	}

	if len(items) == 0 {
		result.Event = "empty"
	}

	return outputMonitorResult(common.JSON, result)
}

func drainMessages(root, me string, includeBody, doAck bool, limit int) ([]monitorItem, error) {
	newDir := fsq.AgentInboxNew(root, me)
	entries, err := os.ReadDir(newDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []monitorItem{}, nil
		}
		return nil, err
	}

	items := make([]monitorItem, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := entry.Name()
		path := filepath.Join(newDir, filename)

		item := monitorItem{
			ID:       filename, // fallback to filename if parse fails
			Filename: filename,
		}

		// Try to parse the message
		var header format.Header
		var body string
		var parseErr error

		if includeBody {
			msg, err := format.ReadMessageFile(path)
			if err != nil {
				parseErr = err
			} else {
				header = msg.Header
				body = msg.Body
			}
		} else {
			header, parseErr = format.ReadHeaderFile(path)
		}

		if parseErr != nil {
			item.ParseError = parseErr.Error()
			// Still move corrupt message to cur to avoid reprocessing
		} else {
			item.ID = header.ID
			item.From = header.From
			item.To = header.To
			item.Thread = header.Thread
			item.Subject = header.Subject
			item.Created = header.Created
			item.AckRequired = header.AckRequired
			item.Priority = header.Priority
			item.Kind = header.Kind
			item.Labels = header.Labels
			item.Context = header.Context
			if includeBody {
				item.Body = body
			}
			if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
				item.SortKey = ts
			}
		}

		items = append(items, item)
	}

	// Sort by timestamp (oldest first)
	sort.Slice(items, func(i, j int) bool {
		if !items[i].SortKey.IsZero() && !items[j].SortKey.IsZero() {
			if items[i].SortKey.Equal(items[j].SortKey) {
				return items[i].ID < items[j].ID
			}
			return items[i].SortKey.Before(items[j].SortKey)
		}
		if items[i].Created == items[j].Created {
			return items[i].ID < items[j].ID
		}
		return items[i].Created < items[j].Created
	})

	// Apply limit
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}

	// Process each message: move to cur, optionally ack
	for i := range items {
		item := &items[i]

		// Move new -> cur
		if err := fsq.MoveNewToCur(root, me, item.Filename); err != nil {
			if os.IsNotExist(err) {
				// Likely moved by another drain; check if it's already in cur.
				curPath := filepath.Join(fsq.AgentInboxCur(root, me), item.Filename)
				if _, statErr := os.Stat(curPath); statErr == nil {
					item.MovedToCur = true
				} else if statErr != nil && !os.IsNotExist(statErr) {
					_ = writeStderr("warning: failed to stat %s in cur: %v\n", item.Filename, statErr)
				}
			} else {
				_ = writeStderr("warning: failed to move %s to cur: %v\n", item.Filename, err)
			}
		} else {
			item.MovedToCur = true
		}

		// Ack if required and move succeeded (gate acking on successful move)
		if doAck && item.AckRequired && item.ParseError == "" && item.MovedToCur {
			if err := monitorAckMessage(root, me, item); err != nil {
				_ = writeStderr("warning: failed to ack %s: %v\n", item.ID, err)
			} else {
				item.Acked = true
			}
		}
	}

	return items, nil
}

func monitorAckMessage(root, me string, item *monitorItem) error {
	sender, err := normalizeHandle(item.From)
	if err != nil {
		return err
	}
	msgID, err := ensureSafeBaseName(item.ID)
	if err != nil {
		return err
	}

	ackPayload := ack.New(item.ID, item.Thread, me, sender, time.Now())

	receiverDir := fsq.AgentAcksSent(root, me)
	receiverPath := filepath.Join(receiverDir, msgID+".json")
	if existing, err := ack.Read(receiverPath); err == nil {
		ackPayload = existing
	}

	data, err := ackPayload.Marshal()
	if err != nil {
		return err
	}

	if _, err := fsq.WriteFileAtomic(receiverDir, msgID+".json", data, 0o600); err != nil {
		return err
	}

	// Best-effort write to sender's received acks
	senderDir := fsq.AgentAcksReceived(root, sender)
	_, _ = fsq.WriteFileAtomic(senderDir, msgID+".json", data, 0o600) // ignore error

	return nil
}

func monitorWithFsnotify(ctx context.Context, inboxNew string) (string, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return monitorWithPolling(ctx, inboxNew)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(inboxNew); err != nil {
		return monitorWithPolling(ctx, inboxNew)
	}

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case event, ok := <-watcher.Events:
			if !ok {
				return "", errors.New("watcher closed")
			}
			if event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
				time.Sleep(10 * time.Millisecond)
				return "new_message", nil
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return "", errors.New("watcher closed")
			}
			return "", err
		}
	}
}

func monitorWithPolling(ctx context.Context, inboxNew string) (string, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			entries, err := os.ReadDir(inboxNew)
			if err != nil && !os.IsNotExist(err) {
				return "", err
			}
			for _, entry := range entries {
				if !entry.IsDir() {
					return "new_message", nil
				}
			}
		}
	}
}

func outputMonitorResult(jsonOutput bool, result monitorResult) error {
	if jsonOutput {
		return writeJSON(os.Stdout, result)
	}

	switch result.Event {
	case "timeout":
		return writeStdoutLine("No new messages (timeout)")
	case "empty":
		return writeStdoutLine("No messages to drain")
	case "messages":
		if err := writeStdout("[AMQ] %d message(s) for %s:\n\n", result.Count, result.Me); err != nil {
			return err
		}
		for _, item := range result.Drained {
			subject := item.Subject
			if subject == "" {
				subject = "(no subject)"
			}
			priority := item.Priority
			if priority == "" {
				priority = "-"
			}
			kind := item.Kind
			if kind == "" {
				kind = "-"
			}
			if err := writeStdout("- From: %s\n  ID: %s\n  Subject: %s\n  Priority: %s\n  Kind: %s\n  Thread: %s\n",
				item.From, item.ID, subject, priority, kind, item.Thread); err != nil {
				return err
			}
			if item.Body != "" {
				if err := writeStdout("  Body:\n%s\n", item.Body); err != nil {
					return err
				}
			}
			if err := writeStdout("---\n"); err != nil {
				return err
			}
		}
	}
	return nil
}

```

File: internal/cli/presence.go (963 tokens)
```
package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

func runPresence(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		printPresenceUsage()
		return nil
	}
	switch args[0] {
	case "set":
		return runPresenceSet(args[1:])
	case "list":
		return runPresenceList(args[1:])
	default:
		return fmt.Errorf("unknown presence command: %s", args[0])
	}
}

func runPresenceSet(args []string) error {
	fs := flag.NewFlagSet("presence set", flag.ContinueOnError)
	common := addCommonFlags(fs)
	statusFlag := fs.String("status", "", "Status string")
	noteFlag := fs.String("note", "", "Optional note")
	usage := usageWithFlags(fs, "amq presence set --me <agent> --status <status> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)

	// Validate handle against config.json
	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	status := strings.TrimSpace(*statusFlag)
	if status == "" {
		return fmt.Errorf("--status is required")
	}
	p := presence.New(common.Me, status, strings.TrimSpace(*noteFlag), time.Now())
	if err := presence.Write(root, p); err != nil {
		return err
	}
	if common.JSON {
		return writeJSON(os.Stdout, p)
	}
	if err := writeStdout("Presence updated for %s\n", common.Me); err != nil {
		return err
	}
	return nil
}

func runPresenceList(args []string) error {
	fs := flag.NewFlagSet("presence list", flag.ContinueOnError)
	common := addCommonFlags(fs)
	usage := usageWithFlags(fs, "amq presence list [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	root := filepath.Clean(common.Root)

	var agents []string
	if cfg, err := config.LoadConfig(filepath.Join(root, "meta", "config.json")); err == nil {
		agents = cfg.Agents
	} else {
		agents, _ = fsq.ListAgents(root)
	}

	items := make([]presence.Presence, 0, len(agents))
	for _, raw := range agents {
		agent, err := normalizeHandle(raw)
		if err != nil {
			if err := writeStderr("warning: skipping invalid handle %s: %v\n", raw, err); err != nil {
				return err
			}
			continue
		}
		p, err := presence.Read(root, agent)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		items = append(items, p)
	}

	if common.JSON {
		return writeJSON(os.Stdout, items)
	}
	if len(items) == 0 {
		if err := writeStdoutLine("No presence data."); err != nil {
			return err
		}
		return nil
	}
	for _, item := range items {
		if err := writeStdout("%s  %s  %s\n", item.Handle, item.Status, item.LastSeen); err != nil {
			return err
		}
		if item.Note != "" {
			if err := writeStdout("  %s\n", item.Note); err != nil {
				return err
			}
		}
	}
	return nil
}

func printPresenceUsage() {
	_ = writeStdoutLine("amq presence <command> [options]")
	_ = writeStdoutLine("")
	_ = writeStdoutLine("Commands:")
	_ = writeStdoutLine("  set   Update presence")
	_ = writeStdoutLine("  list  List presence")
}

```

File: internal/cli/read.go (464 tokens)
```
package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runRead(args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Message id")

	usage := usageWithFlags(fs, "amq read --me <agent> --id <msg_id> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)

	// Validate handle against config.json
	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	filename, err := ensureFilename(*idFlag)
	if err != nil {
		return err
	}

	path, box, err := fsq.FindMessage(root, common.Me, filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("message not found: %s", *idFlag)
		}
		return err
	}

	if box == fsq.BoxNew {
		if err := fsq.MoveNewToCur(root, common.Me, filename); err != nil {
			return err
		}
		path = filepath.Join(fsq.AgentInboxCur(root, common.Me), filename)
	}

	msg, err := format.ReadMessageFile(path)
	if err != nil {
		return err
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"header": msg.Header,
			"body":   msg.Body,
		})
	}

	if err := writeStdout("%s", msg.Body); err != nil {
		return err
	}
	return nil
}

```

File: internal/cli/reply_test.go (1876 tokens)
```
package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestReply_Basic(t *testing.T) {
	root := t.TempDir()
	alice := "alice"
	bob := "bob"

	// Initialize mailboxes
	for _, agent := range []string{alice, bob} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}

	// Create an original message from Bob to Alice
	now := time.Now()
	originalID, _ := format.NewMessageID(now)
	originalMsg := format.Message{
		Header: format.Header{
			Schema:      format.CurrentSchema,
			ID:          originalID,
			From:        bob,
			To:          []string{alice},
			Thread:      "p2p/alice__bob",
			Subject:     "Question about code",
			Created:     now.UTC().Format(time.RFC3339Nano),
			AckRequired: true,
			Priority:    format.PriorityNormal,
			Kind:        format.KindQuestion,
		},
		Body: "How does the parser work?",
	}
	data, _ := originalMsg.Marshal()
	filename := originalID + ".md"
	if _, err := fsq.DeliverToInboxes(root, []string{alice}, filename, data); err != nil {
		t.Fatalf("DeliverToInboxes: %v", err)
	}

	// Alice replies
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runReply([]string{
		"--me", alice,
		"--root", root,
		"--id", originalID,
		"--body", "It parses JSON frontmatter.",
		"--json",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runReply: %v", err)
	}

	// Parse output
	var buf [4096]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\noutput: %s", err, output)
	}

	// Verify reply metadata
	if result["to"] != bob {
		t.Errorf("expected to=bob, got %v", result["to"])
	}
	if result["thread"] != "p2p/alice__bob" {
		t.Errorf("expected thread=p2p/alice__bob, got %v", result["thread"])
	}
	if result["in_reply_to"] != originalID {
		t.Errorf("expected in_reply_to=%s, got %v", originalID, result["in_reply_to"])
	}
	subject := result["subject"].(string)
	if !strings.HasPrefix(subject, "Re:") {
		t.Errorf("expected subject to start with 'Re:', got %s", subject)
	}

	// Verify message delivered to Bob
	bobInbox := fsq.AgentInboxNew(root, bob)
	entries, _ := os.ReadDir(bobInbox)
	if len(entries) != 1 {
		t.Errorf("expected 1 message in Bob's inbox, got %d", len(entries))
	}

	// Read the reply message
	if len(entries) > 0 {
		replyPath := filepath.Join(bobInbox, entries[0].Name())
		replyMsg, err := format.ReadMessageFile(replyPath)
		if err != nil {
			t.Fatalf("read reply: %v", err)
		}

		// Verify refs contains original ID
		if len(replyMsg.Header.Refs) != 1 || replyMsg.Header.Refs[0] != originalID {
			t.Errorf("expected refs=[%s], got %v", originalID, replyMsg.Header.Refs)
		}

		// Verify kind auto-set to review_response for question
		if replyMsg.Header.Kind != format.KindReviewResponse {
			t.Errorf("expected kind=review_response (auto-set from question), got %s", replyMsg.Header.Kind)
		}
	}
}

func TestReply_ReviewRequest(t *testing.T) {
	root := t.TempDir()
	alice := "alice"
	bob := "bob"

	for _, agent := range []string{alice, bob} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}

	// Create a review request from Bob to Alice
	now := time.Now()
	originalID, _ := format.NewMessageID(now)
	originalMsg := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       originalID,
			From:     bob,
			To:       []string{alice},
			Thread:   "p2p/alice__bob",
			Subject:  "Review: New feature",
			Created:  now.UTC().Format(time.RFC3339Nano),
			Priority: format.PriorityNormal,
			Kind:     format.KindReviewRequest,
			Labels:   []string{"feature", "ui"},
		},
		Body: "Please review this feature.",
	}
	data, _ := originalMsg.Marshal()
	_, _ = fsq.DeliverToInboxes(root, []string{alice}, originalID+".md", data)

	// Alice replies with explicit kind
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runReply([]string{
		"--me", alice,
		"--root", root,
		"--id", originalID,
		"--body", "LGTM!",
		"--priority", "normal",
		"--json",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runReply: %v", err)
	}

	var buf [4096]byte
	n, _ := r.Read(buf[:])

	var result map[string]any
	_ = json.Unmarshal(buf[:n], &result)

	// Verify Bob received the reply
	bobInbox := fsq.AgentInboxNew(root, bob)
	entries, _ := os.ReadDir(bobInbox)
	if len(entries) != 1 {
		t.Fatalf("expected 1 message in Bob's inbox, got %d", len(entries))
	}

	replyPath := filepath.Join(bobInbox, entries[0].Name())
	replyMsg, _ := format.ReadMessageFile(replyPath)

	// Kind should be auto-set to review_response
	if replyMsg.Header.Kind != format.KindReviewResponse {
		t.Errorf("expected kind=review_response, got %s", replyMsg.Header.Kind)
	}
}

func TestReply_PreservesThread(t *testing.T) {
	root := t.TempDir()
	alice := "alice"
	bob := "bob"

	for _, agent := range []string{alice, bob} {
		_ = fsq.EnsureAgentDirs(root, agent)
	}

	// Create message with custom thread
	now := time.Now()
	originalID, _ := format.NewMessageID(now)
	originalMsg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      originalID,
			From:    bob,
			To:      []string{alice},
			Thread:  "project/feature-123",
			Subject: "Feature discussion",
			Created: now.UTC().Format(time.RFC3339Nano),
		},
		Body: "Let's discuss.",
	}
	data, _ := originalMsg.Marshal()
	_, _ = fsq.DeliverToInboxes(root, []string{alice}, originalID+".md", data)

	// Reply
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	_ = runReply([]string{
		"--me", alice,
		"--root", root,
		"--id", originalID,
		"--body", "Sounds good!",
		"--json",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	var buf [4096]byte
	n, _ := r.Read(buf[:])

	var result map[string]any
	_ = json.Unmarshal(buf[:n], &result)

	// Thread should be preserved
	if result["thread"] != "project/feature-123" {
		t.Errorf("expected thread=project/feature-123, got %v", result["thread"])
	}
}

```

File: internal/cli/reply.go (1684 tokens)
```
package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runReply(args []string) error {
	fs := flag.NewFlagSet("reply", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Message ID to reply to")
	bodyFlag := fs.String("body", "", "Body string, @file, or empty to read stdin")
	subjectFlag := fs.String("subject", "", "Override subject (default: Re: <original>)")
	ackFlag := fs.Bool("ack", false, "Request ack for the reply")

	// Co-op mode flags
	priorityFlag := fs.String("priority", "", "Message priority: urgent, normal, low")
	kindFlag := fs.String("kind", "", "Message kind (default: same as original or review_response for review_request)")
	labelsFlag := fs.String("labels", "", "Comma-separated labels/tags")
	contextFlag := fs.String("context", "", "JSON context object or @file.json")

	usage := usageWithFlags(fs, "amq reply --me <agent> --id <msg_id> [options]",
		"Reply to a message with automatic thread/refs handling.",
		"Finds the original message, sets to/thread/refs automatically.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)

	if *idFlag == "" {
		return fmt.Errorf("--id is required")
	}

	// Validate co-op fields
	priority := strings.TrimSpace(*priorityFlag)
	kind := strings.TrimSpace(*kindFlag)
	if !format.IsValidPriority(priority) {
		return fmt.Errorf("--priority must be one of: urgent, normal, low")
	}
	if !format.IsValidKind(kind) {
		return fmt.Errorf("--kind must be one of: brainstorm, review_request, review_response, question, decision, status, todo")
	}

	labels := splitList(*labelsFlag)

	var context map[string]any
	if *contextFlag != "" {
		var err error
		context, err = parseContext(*contextFlag)
		if err != nil {
			return err
		}
	}

	// Find the original message
	originalMsg, originalPath, err := findMessage(root, me, *idFlag)
	if err != nil {
		return err
	}

	// Read the body for the reply
	body, err := readBody(*bodyFlag)
	if err != nil {
		return err
	}

	// Determine recipient (original sender)
	recipient := originalMsg.Header.From
	if recipient == me {
		// Replying to our own message - use the original recipient
		if len(originalMsg.Header.To) > 0 {
			recipient = originalMsg.Header.To[0]
		} else {
			return fmt.Errorf("cannot determine recipient for reply")
		}
	}

	// Validate handles
	if err := validateKnownHandles(root, []string{me, recipient}, common.Strict); err != nil {
		return err
	}

	// Determine subject
	subject := strings.TrimSpace(*subjectFlag)
	if subject == "" {
		origSubject := originalMsg.Header.Subject
		if origSubject == "" {
			subject = "Re: (no subject)"
		} else if !strings.HasPrefix(strings.ToLower(origSubject), "re:") {
			subject = "Re: " + origSubject
		} else {
			subject = origSubject
		}
	}

	// Determine kind for reply
	if kind == "" {
		// Auto-set kind based on original
		switch originalMsg.Header.Kind {
		case format.KindReviewRequest:
			kind = format.KindReviewResponse
		case format.KindQuestion:
			kind = format.KindReviewResponse // Answer is a response
		default:
			kind = originalMsg.Header.Kind // Keep same kind
		}
	}

	// Default priority to normal if kind is set
	if kind != "" && priority == "" {
		priority = format.PriorityNormal
	}

	// Create the reply message
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return err
	}

	msg := format.Message{
		Header: format.Header{
			Schema:      format.CurrentSchema,
			ID:          id,
			From:        me,
			To:          []string{recipient},
			Thread:      originalMsg.Header.Thread,
			Subject:     subject,
			Created:     now.UTC().Format(time.RFC3339Nano),
			AckRequired: *ackFlag,
			Refs:        append(originalMsg.Header.Refs, originalMsg.Header.ID),
			Priority:    priority,
			Kind:        kind,
			Labels:      labels,
			Context:     context,
		},
		Body: body,
	}

	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	filename := id + ".md"

	// Deliver to recipient
	if _, err := fsq.DeliverToInboxes(root, []string{recipient}, filename, data); err != nil {
		return err
	}

	// Copy to sender outbox/sent
	outboxDir := fsq.AgentOutboxSent(root, me)
	outboxErr := error(nil)
	if _, err := fsq.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"id":           id,
			"thread":       msg.Header.Thread,
			"to":           recipient,
			"subject":      subject,
			"in_reply_to":  originalMsg.Header.ID,
			"original_box": filepath.Base(filepath.Dir(originalPath)),
			"outbox": map[string]any{
				"written": outboxErr == nil,
				"error":   errString(outboxErr),
			},
		})
	}

	if outboxErr != nil {
		_ = writeStderr("warning: outbox write failed: %v\n", outboxErr)
	}
	return writeStdout("Replied %s to %s (thread: %s)\n", id, recipient, msg.Header.Thread)
}

// findMessage searches for a message by ID in the agent's inbox (new and cur).
func findMessage(root, me, msgID string) (format.Message, string, error) {
	// Normalize the message ID
	filename, err := ensureFilename(msgID)
	if err != nil {
		return format.Message{}, "", err
	}

	// Try inbox/new first
	newPath := filepath.Join(fsq.AgentInboxNew(root, me), filename)
	if msg, err := format.ReadMessageFile(newPath); err == nil {
		return msg, newPath, nil
	}

	// Try inbox/cur
	curPath := filepath.Join(fsq.AgentInboxCur(root, me), filename)
	if msg, err := format.ReadMessageFile(curPath); err == nil {
		return msg, curPath, nil
	}

	// Try outbox/sent (for replying to our own messages)
	sentPath := filepath.Join(fsq.AgentOutboxSent(root, me), filename)
	if msg, err := format.ReadMessageFile(sentPath); err == nil {
		return msg, sentPath, nil
	}

	return format.Message{}, "", fmt.Errorf("message not found: %s", msgID)
}

```

File: internal/cli/send.go (1315 tokens)
```
package cli

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	common := addCommonFlags(fs)
	toFlag := fs.String("to", "", "Receiver handle (comma-separated)")
	subjectFlag := fs.String("subject", "", "Message subject")
	threadFlag := fs.String("thread", "", "Thread id (optional; default p2p/<a>__<b>)")
	bodyFlag := fs.String("body", "", "Body string, @file, or empty to read stdin")
	ackFlag := fs.Bool("ack", false, "Request ack")
	refsFlag := fs.String("refs", "", "Comma-separated related message ids")

	// Co-op mode flags
	priorityFlag := fs.String("priority", "", "Message priority: urgent, normal, low (default: normal if kind set)")
	kindFlag := fs.String("kind", "", "Message kind: brainstorm, review_request, review_response, question, decision, status, todo")
	labelsFlag := fs.String("labels", "", "Comma-separated labels/tags")
	contextFlag := fs.String("context", "", "JSON context object or @file.json")

	usage := usageWithFlags(fs, "amq send --me <agent> --to <recipients> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)
	recipients, err := splitRecipients(*toFlag)
	if err != nil {
		return err
	}
	recipients = dedupeStrings(recipients)

	// Validate handles against config.json
	allHandles := append([]string{me}, recipients...)
	if err := validateKnownHandles(root, allHandles, common.Strict); err != nil {
		return err
	}

	body, err := readBody(*bodyFlag)
	if err != nil {
		return err
	}

	// Validate and process co-op mode fields
	priority := strings.TrimSpace(*priorityFlag)
	kind := strings.TrimSpace(*kindFlag)
	if !format.IsValidPriority(priority) {
		return errors.New("--priority must be one of: urgent, normal, low")
	}
	if !format.IsValidKind(kind) {
		return errors.New("--kind must be one of: brainstorm, review_request, review_response, question, decision, status, todo")
	}
	// Default priority to "normal" if kind is set but priority is not
	if kind != "" && priority == "" {
		priority = format.PriorityNormal
	}

	labels := splitList(*labelsFlag)

	var context map[string]any
	if *contextFlag != "" {
		var err error
		context, err = parseContext(*contextFlag)
		if err != nil {
			return err
		}
	}

	threadID := strings.TrimSpace(*threadFlag)
	if threadID == "" {
		if len(recipients) == 1 {
			threadID = canonicalP2P(common.Me, recipients[0])
		} else {
			return errors.New("--thread is required when sending to multiple recipients")
		}
	}

	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return err
	}

	msg := format.Message{
		Header: format.Header{
			Schema:      format.CurrentSchema,
			ID:          id,
			From:        common.Me,
			To:          recipients,
			Thread:      threadID,
			Subject:     strings.TrimSpace(*subjectFlag),
			Created:     now.UTC().Format(time.RFC3339Nano),
			AckRequired: *ackFlag,
			Refs:        splitList(*refsFlag),
			Priority:    priority,
			Kind:        kind,
			Labels:      labels,
			Context:     context,
		},
		Body: body,
	}

	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	filename := id + ".md"
	// Deliver to each recipient.
	if _, err := fsq.DeliverToInboxes(root, recipients, filename, data); err != nil {
		return err
	}

	// Copy to sender outbox/sent for audit.
	outboxDir := fsq.AgentOutboxSent(root, common.Me)
	outboxErr := error(nil)
	if _, err := fsq.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"id":      id,
			"thread":  threadID,
			"to":      recipients,
			"subject": msg.Header.Subject,
			"outbox": map[string]any{
				"written": outboxErr == nil,
				"error":   errString(outboxErr),
			},
		})
	}
	if outboxErr != nil {
		if err := writeStderr("warning: outbox write failed: %v\n", outboxErr); err != nil {
			return err
		}
	}
	if err := writeStdout("Sent %s to %s\n", id, strings.Join(recipients, ",")); err != nil {
		return err
	}
	return nil
}

func canonicalP2P(a, b string) string {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == b {
		return "p2p/" + a + "__" + b
	}
	if a < b {
		return "p2p/" + a + "__" + b
	}
	return "p2p/" + b + "__" + a
}

```

File: internal/cli/thread.go (687 tokens)
```
package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

func runThread(args []string) error {
	fs := flag.NewFlagSet("thread", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Thread id")
	agentsFlag := fs.String("agents", "", "Comma-separated agent handles (optional)")
	includeBody := fs.Bool("include-body", false, "Include body in output")
	limitFlag := fs.Int("limit", 0, "Limit number of messages (0 = no limit)")

	usage := usageWithFlags(fs, "amq thread --id <thread_id> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	threadID := strings.TrimSpace(*idFlag)
	if threadID == "" {
		return fmt.Errorf("--id is required")
	}
	if *limitFlag < 0 {
		return fmt.Errorf("--limit must be >= 0")
	}
	root := filepath.Clean(common.Root)

	agents, err := parseHandles(*agentsFlag)
	if err != nil {
		return err
	}
	if len(agents) == 0 {
		if cfg, err := config.LoadConfig(filepath.Join(root, "meta", "config.json")); err == nil {
			agents, err = parseHandles(strings.Join(cfg.Agents, ","))
			if err != nil {
				return err
			}
		} else {
			var listErr error
			agents, listErr = fsq.ListAgents(root)
			if listErr != nil {
				return fmt.Errorf("list agents: %w", listErr)
			}
		}
	}
	if len(agents) == 0 {
		return fmt.Errorf("no agents found; provide --agents")
	}

	entries, err := thread.Collect(root, threadID, agents, *includeBody, func(path string, parseErr error) error {
		return writeStderr("warning: skipping corrupt message %s: %v\n", filepath.Base(path), parseErr)
	})
	if err != nil {
		return err
	}
	if *limitFlag > 0 && len(entries) > *limitFlag {
		entries = entries[len(entries)-*limitFlag:]
	}

	if common.JSON {
		return writeJSON(os.Stdout, entries)
	}

	for _, entry := range entries {
		subject := entry.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		if err := writeStdout("%s  %s  %s\n", entry.Created, entry.From, subject); err != nil {
			return err
		}
		if *includeBody {
			if err := writeStdoutLine(entry.Body); err != nil {
				return err
			}
			if err := writeStdoutLine("---"); err != nil {
				return err
			}
		}
	}
	return nil
}

```

File: internal/cli/watch_test.go (1487 tokens)
```
package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunWatchExistingMessages(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a message before watching
	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-existing",
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "Existing message",
			Created: "2025-12-25T10:00:00Z",
		},
		Body: "This message exists before watch",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "alice", "msg-existing.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err = runWatch([]string{"--root", root, "--me", "alice", "--json", "--timeout", "1s"})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runWatch: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var result watchResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v (output: %s)", err, buf.String())
	}

	if result.Event != "existing" {
		t.Errorf("expected event 'existing', got %s", result.Event)
	}
	if len(result.Messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(result.Messages))
	}
	if result.Messages[0].ID != "msg-existing" {
		t.Errorf("expected message ID 'msg-existing', got %s", result.Messages[0].ID)
	}
}

func TestRunWatchTimeout(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	start := time.Now()
	err := runWatch([]string{"--root", root, "--me", "alice", "--json", "--timeout", "100ms"})
	elapsed := time.Since(start)

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runWatch: %v", err)
	}

	// Should timeout around 100ms
	if elapsed < 90*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("expected timeout around 100ms, got %v", elapsed)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var result watchResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v (output: %s)", err, buf.String())
	}

	if result.Event != "timeout" {
		t.Errorf("expected event 'timeout', got %s", result.Event)
	}
}

func TestRunWatchNewMessage(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	var wg sync.WaitGroup
	var watchErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Use polling mode for test reliability
		watchErr = runWatch([]string{"--root", root, "--me", "alice", "--json", "--timeout", "5s", "--poll"})
	}()

	// Wait a bit then send a message
	time.Sleep(200 * time.Millisecond)

	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-new",
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "New message",
			Created: "2025-12-25T10:01:00Z",
		},
		Body: "This is a new message",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "alice", "msg-new.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	wg.Wait()

	_ = w.Close()
	os.Stdout = oldStdout

	if watchErr != nil {
		t.Fatalf("runWatch: %v", watchErr)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var result watchResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v (output: %s)", err, buf.String())
	}

	if result.Event != "new_message" {
		t.Errorf("expected event 'new_message', got %s", result.Event)
	}
	if len(result.Messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(result.Messages))
	}
	if len(result.Messages) > 0 && result.Messages[0].ID != "msg-new" {
		t.Errorf("expected message ID 'msg-new', got %s", result.Messages[0].ID)
	}
}

```

File: internal/cli/watch.go (1670 tokens)
```
package cli

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
)

type watchResult struct {
	Event    string    `json:"event"`
	Messages []msgInfo `json:"messages,omitempty"`
}

type msgInfo struct {
	ID       string   `json:"id"`
	From     string   `json:"from"`
	Subject  string   `json:"subject"`
	Thread   string   `json:"thread"`
	Created  string   `json:"created"`
	Path     string   `json:"path"`
	Priority string   `json:"priority,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	Labels   []string `json:"labels,omitempty"`
}

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	common := addCommonFlags(fs)
	timeoutFlag := fs.Duration("timeout", 60*time.Second, "Maximum time to wait for messages (0 = wait forever)")
	pollFlag := fs.Bool("poll", false, "Use polling fallback instead of fsnotify (for network filesystems)")

	usage := usageWithFlags(fs, "amq watch --me <agent> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me

	root := filepath.Clean(common.Root)

	// Validate handle against config.json
	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	inboxNew := fsq.AgentInboxNew(root, common.Me)

	// Ensure inbox directory exists
	if err := os.MkdirAll(inboxNew, 0o700); err != nil {
		return err
	}

	// Set up context with timeout
	ctx := context.Background()
	if *timeoutFlag > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeoutFlag)
		defer cancel()
	}

	// Watch for new messages (includes initial check after watcher setup)
	var messages []msgInfo
	var event string
	var watchErr error

	if *pollFlag {
		messages, event, watchErr = watchWithPolling(ctx, inboxNew)
	} else {
		messages, event, watchErr = watchWithFsnotify(ctx, inboxNew)
	}

	if watchErr != nil {
		if errors.Is(watchErr, context.DeadlineExceeded) {
			return outputWatchResult(common.JSON, "timeout", nil)
		}
		return watchErr
	}

	return outputWatchResult(common.JSON, event, messages)
}

func watchWithFsnotify(ctx context.Context, inboxNew string) ([]msgInfo, string, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		// Fall back to polling if fsnotify fails
		return watchWithPolling(ctx, inboxNew)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(inboxNew); err != nil {
		return watchWithPolling(ctx, inboxNew)
	}

	// Check for existing messages AFTER watcher is set up to avoid race condition.
	// Any message arriving after this check will trigger a watcher event.
	existing, err := listNewMessages(inboxNew)
	if err != nil {
		return nil, "", err
	}
	if len(existing) > 0 {
		return existing, "existing", nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case event, ok := <-watcher.Events:
			if !ok {
				return nil, "", errors.New("watcher closed")
			}
			// Only care about new files (Create or Rename into directory)
			if event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
				// Small delay to ensure file is fully written
				time.Sleep(10 * time.Millisecond)
				messages, err := listNewMessages(inboxNew)
				if err != nil {
					return nil, "", err
				}
				if len(messages) > 0 {
					return messages, "new_message", nil
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil, "", errors.New("watcher closed")
			}
			return nil, "", err
		}
	}
}

func watchWithPolling(ctx context.Context, inboxNew string) ([]msgInfo, string, error) {
	// Check for existing messages first
	existing, err := listNewMessages(inboxNew)
	if err != nil {
		return nil, "", err
	}
	if len(existing) > 0 {
		return existing, "existing", nil
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-ticker.C:
			messages, err := listNewMessages(inboxNew)
			if err != nil {
				return nil, "", err
			}
			if len(messages) > 0 {
				return messages, "new_message", nil
			}
		}
	}
}

func listNewMessages(inboxNew string) ([]msgInfo, error) {
	entries, err := os.ReadDir(inboxNew)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var messages []msgInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(inboxNew, entry.Name())
		header, err := format.ReadHeaderFile(path)
		if err != nil {
			continue // Skip corrupt messages
		}

		messages = append(messages, msgInfo{
			ID:       header.ID,
			From:     header.From,
			Subject:  header.Subject,
			Thread:   header.Thread,
			Created:  header.Created,
			Path:     path,
			Priority: header.Priority,
			Kind:     header.Kind,
			Labels:   header.Labels,
		})
	}

	// Sort by creation time
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Created < messages[j].Created
	})

	return messages, nil
}

func outputWatchResult(jsonOutput bool, event string, messages []msgInfo) error {
	result := watchResult{
		Event:    event,
		Messages: messages,
	}

	if jsonOutput {
		return writeJSON(os.Stdout, result)
	}

	switch event {
	case "timeout":
		return writeStdoutLine("No new messages (timeout)")
	case "existing":
		if err := writeStdoutLine("Found existing messages:"); err != nil {
			return err
		}
	case "new_message":
		if err := writeStdoutLine("New message(s) received:"); err != nil {
			return err
		}
	}

	for _, msg := range messages {
		subject := msg.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		if err := writeStdout("  %s  %s  %s  %s\n", msg.Created, msg.From, msg.ID, subject); err != nil {
			return err
		}
	}

	return nil
}

```

File: internal/config/config_test.go (251 tokens)
```
package config

import (
	"path/filepath"
	"testing"
)

func TestConfigWriteRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "meta", "config.json")
	cfg := Config{
		Version:    1,
		CreatedUTC: "2025-12-24T15:02:33Z",
		Agents:     []string{"codex", "cloudcode"},
	}
	if err := WriteConfig(path, cfg, false); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(loaded.Agents) != 2 || loaded.Agents[0] != "codex" {
		t.Fatalf("unexpected agents: %+v", loaded.Agents)
	}
	if err := WriteConfig(path, cfg, false); err == nil {
		t.Fatalf("expected error on overwrite without force")
	}
	if err := WriteConfig(path, cfg, true); err != nil {
		t.Fatalf("WriteConfig with force: %v", err)
	}
}

```

File: internal/config/config.go (270 tokens)
```
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Config is persisted to meta/config.json and captures the initial setup.
type Config struct {
	Version    int      `json:"version"`
	CreatedUTC string   `json:"created_utc"`
	Agents     []string `json:"agents"`
}

func WriteConfig(path string, cfg Config, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("config already exists at %s (use --force to overwrite)", path)
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), data, 0o600)
	return err
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

```

File: internal/format/doc.go (35 tokens)
```
// Package format handles message serialization using JSON frontmatter
// with Markdown body content. It provides message ID generation,
// parsing, and timestamp-based sorting utilities.
package format

```

File: internal/format/id_test.go (150 tokens)
```
package format

import (
	"strings"
	"testing"
	"time"
)

func TestNewMessageID(t *testing.T) {
	id, err := NewMessageID(time.Date(2025, 12, 24, 15, 2, 33, 123000000, time.UTC))
	if err != nil {
		t.Fatalf("NewMessageID: %v", err)
	}
	if !strings.Contains(id, "_pid") {
		t.Fatalf("expected pid marker in id: %s", id)
	}
	if !strings.HasPrefix(id, "2025-12-24T15-02-33.123Z_") {
		t.Fatalf("unexpected prefix: %s", id)
	}
}

```

File: internal/format/id.go (167 tokens)
```
package format

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

func NewMessageID(now time.Time) (string, error) {
	stamp := now.UTC().Format("2006-01-02T15-04-05.000Z")
	pid := os.Getpid()
	suffix, err := randSuffix(4)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s_pid%d_%s", stamp, pid, suffix), nil
}

func randSuffix(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return strings.ToLower(hex.EncodeToString(buf)), nil
}

```

File: internal/format/message_test.go (1030 tokens)
```
package format

import (
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:      1,
			ID:          "2025-12-24T15-02-33.123Z_pid1234_abcd",
			From:        "codex",
			To:          []string{"cloudcode"},
			Thread:      "p2p/cloudcode__codex",
			Subject:     "Hello",
			Created:     "2025-12-24T15:02:33.123Z",
			AckRequired: true,
			Refs:        []string{"ref1"},
		},
		Body: "Line one\nLine two\n",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := ParseMessage(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Header.ID != msg.Header.ID {
		t.Fatalf("id mismatch: %s", parsed.Header.ID)
	}
	if parsed.Body != msg.Body {
		t.Fatalf("body mismatch: %q", parsed.Body)
	}
}

func TestMessageRoundTrip_CoopFields(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:      1,
			ID:          "2025-12-27T10-00-00.000Z_pid5678_efgh",
			From:        "codex",
			To:          []string{"claude"},
			Thread:      "p2p/claude__codex",
			Subject:     "Review request",
			Created:     "2025-12-27T10:00:00.000Z",
			AckRequired: true,
			Priority:    PriorityUrgent,
			Kind:        KindReviewRequest,
			Labels:      []string{"parser", "refactor"},
			Context: map[string]any{
				"paths": []any{"internal/format/message.go"},
				"focus": "error handling",
			},
		},
		Body: "Please review this code.\n",
	}

	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := ParseMessage(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Verify co-op fields
	if parsed.Header.Priority != PriorityUrgent {
		t.Errorf("priority mismatch: expected %s, got %s", PriorityUrgent, parsed.Header.Priority)
	}
	if parsed.Header.Kind != KindReviewRequest {
		t.Errorf("kind mismatch: expected %s, got %s", KindReviewRequest, parsed.Header.Kind)
	}
	if len(parsed.Header.Labels) != 2 {
		t.Errorf("labels count mismatch: expected 2, got %d", len(parsed.Header.Labels))
	}
	if parsed.Header.Labels[0] != "parser" || parsed.Header.Labels[1] != "refactor" {
		t.Errorf("labels mismatch: %v", parsed.Header.Labels)
	}
	if parsed.Header.Context == nil {
		t.Fatal("context is nil")
	}
	if parsed.Header.Context["focus"] != "error handling" {
		t.Errorf("context.focus mismatch: %v", parsed.Header.Context["focus"])
	}
}

func TestValidPriority(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"", true},
		{PriorityUrgent, true},
		{PriorityNormal, true},
		{PriorityLow, true},
		{"invalid", false},
		{"URGENT", false}, // case-sensitive
	}

	for _, tc := range tests {
		got := IsValidPriority(tc.input)
		if got != tc.valid {
			t.Errorf("IsValidPriority(%q) = %v, want %v", tc.input, got, tc.valid)
		}
	}
}

func TestValidKind(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"", true},
		{KindReviewRequest, true},
		{KindReviewResponse, true},
		{KindQuestion, true},
		{KindBrainstorm, true},
		{KindDecision, true},
		{KindStatus, true},
		{KindTodo, true},
		{"invalid", false},
		{"REVIEW_REQUEST", false}, // case-sensitive
	}

	for _, tc := range tests {
		got := IsValidKind(tc.input)
		if got != tc.valid {
			t.Errorf("IsValidKind(%q) = %v, want %v", tc.input, got, tc.valid)
		}
	}
}

```

File: internal/format/message.go (1809 tokens)
```
package format

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Schema and version constants.
const (
	CurrentSchema  = 1
	CurrentVersion = 1
)

const (
	frontmatterStartLine = "---json"
	frontmatterStart     = frontmatterStartLine + "\n"
	frontmatterEnd       = "\n---\n"
)

// Sentinel errors for message parsing.
var (
	ErrMissingFrontmatterStart = errors.New("missing frontmatter start")
	ErrMissingFrontmatterEnd   = errors.New("missing frontmatter end")
)

// Priority constants for co-op mode message handling.
const (
	PriorityUrgent = "urgent"
	PriorityNormal = "normal"
	PriorityLow    = "low"
)

// Kind constants for co-op mode message classification.
const (
	KindBrainstorm     = "brainstorm"
	KindReviewRequest  = "review_request"
	KindReviewResponse = "review_response"
	KindQuestion       = "question"
	KindDecision       = "decision"
	KindStatus         = "status"
	KindTodo           = "todo"
)

// Header is the JSON frontmatter stored at the top of each message file.
type Header struct {
	Schema      int      `json:"schema"`
	ID          string   `json:"id"`
	From        string   `json:"from"`
	To          []string `json:"to"`
	Thread      string   `json:"thread"`
	Subject     string   `json:"subject,omitempty"`
	Created     string   `json:"created"`
	AckRequired bool     `json:"ack_required"`
	Refs        []string `json:"refs,omitempty"`

	// Co-op mode fields (optional, for inter-agent communication)
	Priority string         `json:"priority,omitempty"` // urgent, normal, low
	Kind     string         `json:"kind,omitempty"`     // brainstorm, review_request, review_response, question, decision, status, todo
	Labels   []string       `json:"labels,omitempty"`   // free-form tags
	Context  map[string]any `json:"context,omitempty"`  // structured context (paths, symbols, etc.)
}

// Message is the in-memory representation of a message file.
type Message struct {
	Header Header
	Body   string
}

// ValidPriorities returns the list of valid priority values.
func ValidPriorities() []string {
	return []string{PriorityUrgent, PriorityNormal, PriorityLow}
}

// ValidKinds returns the list of valid kind values.
func ValidKinds() []string {
	return []string{KindBrainstorm, KindReviewRequest, KindReviewResponse, KindQuestion, KindDecision, KindStatus, KindTodo}
}

// IsValidPriority returns true if the priority is valid or empty.
func IsValidPriority(p string) bool {
	if p == "" {
		return true
	}
	for _, v := range ValidPriorities() {
		if p == v {
			return true
		}
	}
	return false
}

// IsValidKind returns true if the kind is valid or empty.
func IsValidKind(k string) bool {
	if k == "" {
		return true
	}
	for _, v := range ValidKinds() {
		if k == v {
			return true
		}
	}
	return false
}

func (m Message) Marshal() ([]byte, error) {
	if m.Header.Schema == 0 {
		m.Header.Schema = CurrentSchema
	}
	if m.Header.Created == "" {
		m.Header.Created = time.Now().UTC().Format(time.RFC3339Nano)
	}
	b, err := json.MarshalIndent(m.Header, "", "  ")
	if err != nil {
		return nil, err
	}
	body := m.Body
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	out := make([]byte, 0, len(frontmatterStart)+len(b)+len(frontmatterEnd)+len(body))
	out = append(out, frontmatterStart...)
	out = append(out, b...)
	out = append(out, frontmatterEnd...)
	out = append(out, body...)
	return out, nil
}

func ParseMessage(data []byte) (Message, error) {
	headerBytes, body, err := splitFrontmatter(data)
	if err != nil {
		return Message{}, err
	}
	var header Header
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Message{}, fmt.Errorf("parse frontmatter: %w", err)
	}
	return Message{Header: header, Body: string(body)}, nil
}

func ParseHeader(data []byte) (Header, error) {
	headerBytes, _, err := splitFrontmatter(data)
	if err != nil {
		return Header{}, err
	}
	var header Header
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Header{}, fmt.Errorf("parse frontmatter: %w", err)
	}
	return header, nil
}

func ReadMessageFile(path string) (Message, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Message{}, err
	}
	return ParseMessage(data)
}

func ReadHeaderFile(path string) (Header, error) {
	file, err := os.Open(path)
	if err != nil {
		return Header{}, err
	}
	defer func() { _ = file.Close() }()
	return ReadHeader(file)
}

func ReadHeader(r io.Reader) (Header, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return Header{}, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line != frontmatterStartLine {
		return Header{}, ErrMissingFrontmatterStart
	}
	if errors.Is(err, io.EOF) {
		return Header{}, ErrMissingFrontmatterEnd
	}

	dec := json.NewDecoder(br)
	var header Header
	if err := dec.Decode(&header); err != nil {
		return Header{}, fmt.Errorf("parse frontmatter: %w", err)
	}
	rest := io.MultiReader(dec.Buffered(), br)
	if err := consumeFrontmatterEnd(bufio.NewReader(rest)); err != nil {
		return Header{}, err
	}
	return header, nil
}

func splitFrontmatter(data []byte) ([]byte, []byte, error) {
	if !bytes.HasPrefix(data, []byte(frontmatterStart)) {
		return nil, nil, ErrMissingFrontmatterStart
	}
	payload := data[len(frontmatterStart):]
	dec := json.NewDecoder(bytes.NewReader(payload))
	var header json.RawMessage
	if err := dec.Decode(&header); err != nil {
		return nil, nil, fmt.Errorf("parse frontmatter json: %w", err)
	}
	rest := payload[dec.InputOffset():]
	rest = bytes.TrimLeft(rest, " \t\r\n")
	if !bytes.HasPrefix(rest, []byte("---\n")) {
		return nil, nil, ErrMissingFrontmatterEnd
	}
	body := rest[len("---\n"):]
	return header, body, nil
}

func consumeFrontmatterEnd(br *bufio.Reader) error {
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return ErrMissingFrontmatterEnd
			}
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "---" {
			return nil
		}
		return ErrMissingFrontmatterEnd
	}
}

// Timestamped is implemented by types that have a Created timestamp and ID for sorting.
type Timestamped interface {
	GetCreated() string
	GetID() string
	GetRawTime() time.Time
}

// SortByTimestamp sorts a slice of Timestamped items by time, then by ID for stability.
func SortByTimestamp[T Timestamped](items []T) {
	sort.Slice(items, func(i, j int) bool {
		ti, tj := items[i].GetRawTime(), items[j].GetRawTime()
		if !ti.IsZero() && !tj.IsZero() {
			if ti.Equal(tj) {
				return items[i].GetID() < items[j].GetID()
			}
			return ti.Before(tj)
		}
		ci, cj := items[i].GetCreated(), items[j].GetCreated()
		if ci == cj {
			return items[i].GetID() < items[j].GetID()
		}
		return ci < cj
	})
}

```

File: internal/fsq/atomic.go (539 tokens)
```
package fsq

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WriteFileAtomic writes data to a temporary file in dir and renames it into place.
func WriteFileAtomic(dir, filename string, data []byte, perm os.FileMode) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmpName := fmt.Sprintf(".%s.tmp-%d", filename, time.Now().UnixNano())
	tmpPath := filepath.Join(dir, tmpName)
	finalPath := filepath.Join(dir, filename)

	if err := writeAndSync(tmpPath, data, perm); err != nil {
		return "", err
	}
	if err := SyncDir(dir); err != nil {
		return "", cleanupTemp(tmpPath, err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		// On Windows, rename to existing file may return access denied instead of ErrExist.
		// Check if destination exists and retry after removal.
		if _, statErr := os.Stat(finalPath); statErr == nil {
			if removeErr := os.Remove(finalPath); removeErr != nil && !os.IsNotExist(removeErr) {
				return "", cleanupTemp(tmpPath, err)
			}
			if err := os.Rename(tmpPath, finalPath); err != nil {
				return "", cleanupTemp(tmpPath, err)
			}
		} else {
			return "", cleanupTemp(tmpPath, err)
		}
	}
	if err := SyncDir(dir); err != nil {
		return "", err
	}
	return finalPath, nil
}

func writeAndSync(path string, data []byte, perm os.FileMode) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil && err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func cleanupTemp(path string, primary error) error {
	if primary == nil {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w (cleanup: %v)", primary, err)
	}
	return primary
}

```

File: internal/fsq/find.go (333 tokens)
```
package fsq

import (
	"os"
	"path/filepath"
)

const (
	BoxNew = "new"
	BoxCur = "cur"
)

func FindMessage(root, agent, filename string) (string, string, error) {
	newPath := filepath.Join(root, "agents", agent, "inbox", "new", filename)
	if _, err := os.Stat(newPath); err == nil {
		return newPath, BoxNew, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	curPath := filepath.Join(root, "agents", agent, "inbox", "cur", filename)
	if _, err := os.Stat(curPath); err == nil {
		return curPath, BoxCur, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	return "", "", os.ErrNotExist
}

func MoveNewToCur(root, agent, filename string) error {
	newPath := filepath.Join(root, "agents", agent, "inbox", "new", filename)
	curDir := filepath.Join(root, "agents", agent, "inbox", "cur")
	curPath := filepath.Join(curDir, filename)
	if err := os.MkdirAll(curDir, 0o700); err != nil {
		return err
	}
	if err := os.Rename(newPath, curPath); err != nil {
		return err
	}
	if err := SyncDir(filepath.Dir(newPath)); err != nil {
		return err
	}
	return SyncDir(curDir)
}

```

File: internal/fsq/layout.go (440 tokens)
```
package fsq

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Path helpers for standard mailbox directories.

func AgentBase(root, agent string) string {
	return filepath.Join(root, "agents", agent)
}

func AgentInboxTmp(root, agent string) string {
	return filepath.Join(root, "agents", agent, "inbox", "tmp")
}

func AgentInboxNew(root, agent string) string {
	return filepath.Join(root, "agents", agent, "inbox", "new")
}

func AgentInboxCur(root, agent string) string {
	return filepath.Join(root, "agents", agent, "inbox", "cur")
}

func AgentOutboxSent(root, agent string) string {
	return filepath.Join(root, "agents", agent, "outbox", "sent")
}

func AgentAcksReceived(root, agent string) string {
	return filepath.Join(root, "agents", agent, "acks", "received")
}

func AgentAcksSent(root, agent string) string {
	return filepath.Join(root, "agents", agent, "acks", "sent")
}

func EnsureRootDirs(root string) error {
	for _, dir := range []string{
		filepath.Join(root, "agents"),
		filepath.Join(root, "threads"),
		filepath.Join(root, "meta"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func EnsureAgentDirs(root, agent string) error {
	if strings.TrimSpace(agent) == "" {
		return fmt.Errorf("agent handle is empty")
	}
	dirs := []string{
		AgentInboxTmp(root, agent),
		AgentInboxNew(root, agent),
		AgentInboxCur(root, agent),
		AgentOutboxSent(root, agent),
		AgentAcksReceived(root, agent),
		AgentAcksSent(root, agent),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

```

File: internal/fsq/maildir_test.go (853 tokens)
```
package fsq

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDeliverToInbox(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	data := []byte("hello")
	filename := "test.md"
	path, err := DeliverToInbox(root, "codex", filename, data)
	if err != nil {
		t.Fatalf("DeliverToInbox: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected message in new: %v", err)
	}
	tmpDir := filepath.Join(root, "agents", "codex", "inbox", "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir tmp: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected tmp empty, got %d", len(entries))
	}
}

func TestMoveNewToCur(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	data := []byte("hello")
	filename := "move.md"
	if _, err := DeliverToInbox(root, "codex", filename, data); err != nil {
		t.Fatalf("DeliverToInbox: %v", err)
	}
	if err := MoveNewToCur(root, "codex", filename); err != nil {
		t.Fatalf("MoveNewToCur: %v", err)
	}
	newPath := filepath.Join(root, "agents", "codex", "inbox", "new", filename)
	curPath := filepath.Join(root, "agents", "codex", "inbox", "cur", filename)
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("expected new missing")
	}
	if _, err := os.Stat(curPath); err != nil {
		t.Fatalf("expected cur present: %v", err)
	}
}

func TestDeliverToInboxesRollback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod permissions are unreliable on Windows")
	}
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}
	if err := EnsureAgentDirs(root, "cloudcode"); err != nil {
		t.Fatalf("EnsureAgentDirs cloudcode: %v", err)
	}

	cloudNew := AgentInboxNew(root, "cloudcode")
	if err := os.Chmod(cloudNew, 0o555); err != nil {
		t.Fatalf("chmod cloudcode new: %v", err)
	}

	filename := "multi.md"
	if _, err := DeliverToInboxes(root, []string{"codex", "cloudcode"}, filename, []byte("hello")); err == nil {
		t.Fatalf("expected delivery error")
	}

	codexNew := filepath.Join(AgentInboxNew(root, "codex"), filename)
	if _, err := os.Stat(codexNew); !os.IsNotExist(err) {
		t.Fatalf("expected rollback to remove %s", codexNew)
	}

	cloudTmp := filepath.Join(AgentInboxTmp(root, "cloudcode"), filename)
	if _, err := os.Stat(cloudTmp); !os.IsNotExist(err) {
		t.Fatalf("expected rollback to remove %s", cloudTmp)
	}
}

```

File: internal/fsq/maildir.go (1124 tokens)
```
package fsq

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// DeliverToInbox writes a message using Maildir semantics (tmp -> new).
// It returns the final path in inbox/new.
func DeliverToInbox(root, agent, filename string, data []byte) (string, error) {
	paths, err := DeliverToInboxes(root, []string{agent}, filename, data)
	if err != nil {
		return "", err
	}
	return paths[agent], nil
}

type stagedDelivery struct {
	recipient string
	tmpDir    string
	newDir    string
	tmpPath   string
	newPath   string
}

// DeliverToInboxes writes a message to multiple inboxes.
// On failure, it attempts to roll back any prior deliveries.
func DeliverToInboxes(root string, recipients []string, filename string, data []byte) (map[string]string, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no recipients provided")
	}
	stages := make([]stagedDelivery, 0, len(recipients))
	for _, recipient := range recipients {
		tmpDir := AgentInboxTmp(root, recipient)
		newDir := AgentInboxNew(root, recipient)
		if err := os.MkdirAll(tmpDir, 0o700); err != nil {
			return nil, cleanupStagedTmp(stages, err)
		}
		if err := os.MkdirAll(newDir, 0o700); err != nil {
			return nil, cleanupStagedTmp(stages, err)
		}
		tmpPath := filepath.Join(tmpDir, filename)
		newPath := filepath.Join(newDir, filename)
		if err := writeAndSync(tmpPath, data, 0o600); err != nil {
			return nil, cleanupStagedTmp(stages, err)
		}
		if err := SyncDir(tmpDir); err != nil {
			return nil, cleanupStagedTmp(stages, cleanupTemp(tmpPath, err))
		}
		stages = append(stages, stagedDelivery{
			recipient: recipient,
			tmpDir:    tmpDir,
			newDir:    newDir,
			tmpPath:   tmpPath,
			newPath:   newPath,
		})
	}

	for i, stage := range stages {
		if err := os.Rename(stage.tmpPath, stage.newPath); err != nil {
			if errors.Is(err, syscall.EXDEV) {
				err = fmt.Errorf("rename tmp->new for %s: different filesystems: %w", stage.recipient, err)
			} else {
				err = fmt.Errorf("rename tmp->new for %s: %w", stage.recipient, err)
			}
			return nil, rollbackDeliveries(stages[:i], stages[i:], err)
		}
		if err := SyncDir(stage.newDir); err != nil {
			return nil, rollbackDeliveries(stages[:i+1], stages[i+1:], fmt.Errorf("sync new dir for %s: %w", stage.recipient, err))
		}
	}

	paths := make(map[string]string, len(stages))
	for _, stage := range stages {
		paths[stage.recipient] = stage.newPath
	}
	return paths, nil
}

func cleanupStagedTmp(stages []stagedDelivery, primary error) error {
	var cleanupErr error
	for _, stage := range stages {
		if err := os.Remove(stage.tmpPath); err != nil && !os.IsNotExist(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup tmp %s: %w", stage.tmpPath, err))
		}
		if err := SyncDir(stage.tmpDir); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync tmp dir %s: %w", stage.tmpDir, err))
		}
	}
	if cleanupErr == nil {
		return primary
	}
	return fmt.Errorf("%w (cleanup: %v)", primary, cleanupErr)
}

func rollbackDeliveries(committed, pending []stagedDelivery, primary error) error {
	var cleanupErr error
	for _, stage := range committed {
		if err := os.Remove(stage.newPath); err != nil && !os.IsNotExist(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("rollback new %s: %w", stage.newPath, err))
		}
		if err := SyncDir(stage.newDir); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync new dir %s: %w", stage.newDir, err))
		}
	}
	for _, stage := range pending {
		if err := os.Remove(stage.tmpPath); err != nil && !os.IsNotExist(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup tmp %s: %w", stage.tmpPath, err))
		}
		if err := SyncDir(stage.tmpDir); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync tmp dir %s: %w", stage.tmpDir, err))
		}
	}
	if cleanupErr == nil {
		return primary
	}
	return fmt.Errorf("%w (rollback: %v)", primary, cleanupErr)
}

```

File: internal/fsq/scan_test.go (346 tokens)
```
package fsq

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFindTmpFilesOlderThan(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	tmpDir := filepath.Join(root, "agents", "codex", "inbox", "tmp")
	oldPath := filepath.Join(tmpDir, "old.tmp")
	newPath := filepath.Join(tmpDir, "new.tmp")
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("new"), 0o644); err != nil {
		t.Fatalf("write new: %v", err)
	}

	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes old: %v", err)
	}

	cutoff := time.Now().Add(-36 * time.Hour)
	matches, err := FindTmpFilesOlderThan(root, cutoff)
	if err != nil {
		t.Fatalf("FindTmpFilesOlderThan: %v", err)
	}
	if len(matches) != 1 || matches[0] != oldPath {
		t.Fatalf("unexpected matches: %v", matches)
	}
}

```

File: internal/fsq/scan.go (330 tokens)
```
package fsq

import (
	"os"
	"path/filepath"
	"time"
)

func ListAgents(root string) ([]string, error) {
	agentsDir := filepath.Join(root, "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			out = append(out, entry.Name())
		}
	}
	return out, nil
}

func FindTmpFilesOlderThan(root string, cutoff time.Time) ([]string, error) {
	agents, err := ListAgents(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	matches := []string{}
	for _, agent := range agents {
		tmpDir := AgentInboxTmp(root, agent)
		entries, err := os.ReadDir(tmpDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue // skip unreadable files instead of failing entire scan
			}
			if info.ModTime().Before(cutoff) {
				matches = append(matches, filepath.Join(tmpDir, entry.Name()))
			}
		}
	}
	return matches, nil
}

```

File: internal/fsq/sync_unix.go (143 tokens)
```
//go:build !windows

package fsq

import (
	"errors"
	"os"
	"syscall"
)

// SyncDir fsyncs a directory to ensure directory entries are durable.
func SyncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		if isSyncUnsupported(syncErr) {
			return nil
		}
		return syncErr
	}
	return closeErr
}

func isSyncUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)
}

```

File: internal/fsq/sync_windows.go (32 tokens)
```
//go:build windows

package fsq

// SyncDir is a no-op on Windows.
func SyncDir(_ string) error {
	return nil
}

```

File: internal/presence/doc.go (39 tokens)
```
// Package presence manages agent presence metadata. Each agent
// can set a status and optional note, stored as presence.json
// in their mailbox directory with a last_seen timestamp.
package presence

```

File: internal/presence/presence_test.go (187 tokens)
```
package presence

import (
	"testing"
	"time"
)

func TestPresenceWriteRead(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2025, 12, 24, 15, 2, 33, 0, time.UTC)
	p := New("codex", "busy", "reviewing", now)
	if err := Write(root, p); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(root, "codex")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Handle != "codex" || got.Status != "busy" || got.Note != "reviewing" {
		t.Fatalf("unexpected presence: %+v", got)
	}
	if got.LastSeen == "" {
		t.Fatalf("expected LastSeen to be set")
	}
}

```

File: internal/presence/presence.go (330 tokens)
```
package presence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Presence captures the current presence for an agent handle.
type Presence struct {
	Schema   int    `json:"schema"`
	Handle   string `json:"handle"`
	Status   string `json:"status"`
	LastSeen string `json:"last_seen"`
	Note     string `json:"note,omitempty"`
}

func New(handle, status, note string, now time.Time) Presence {
	return Presence{
		Schema:   1,
		Handle:   handle,
		Status:   status,
		LastSeen: now.UTC().Format(time.RFC3339Nano),
		Note:     note,
	}
}

func Write(root string, p Presence) error {
	path := filepath.Join(root, "agents", p.Handle, "presence.json")
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), data, 0o600)
	return err
}

func Read(root, handle string) (Presence, error) {
	path := filepath.Join(root, "agents", handle, "presence.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return Presence{}, err
	}
	var p Presence
	if err := json.Unmarshal(data, &p); err != nil {
		return Presence{}, err
	}
	return p, nil
}

```

File: internal/thread/doc.go (43 tokens)
```
// Package thread collects and aggregates messages across agent
// mailboxes by thread ID. It scans inbox and outbox directories,
// deduplicates by message ID, and returns entries sorted by timestamp.
package thread

```

File: internal/thread/thread_test.go (978 tokens)
```
package thread

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestCollectThread(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "cloudcode"); err != nil {
		t.Fatalf("EnsureAgentDirs cloudcode: %v", err)
	}

	now := time.Date(2025, 12, 24, 15, 2, 33, 0, time.UTC)
	msg1 := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-1",
			From:    "codex",
			To:      []string{"cloudcode"},
			Thread:  "p2p/cloudcode__codex",
			Subject: "Hello",
			Created: now.Format(time.RFC3339Nano),
		},
		Body: "First",
	}
	msg2 := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-2",
			From:    "cloudcode",
			To:      []string{"codex"},
			Thread:  "p2p/cloudcode__codex",
			Subject: "Re: Hello",
			Created: now.Add(2 * time.Second).Format(time.RFC3339Nano),
		},
		Body: "Second",
	}

	data1, err := msg1.Marshal()
	if err != nil {
		t.Fatalf("marshal msg1: %v", err)
	}
	data2, err := msg2.Marshal()
	if err != nil {
		t.Fatalf("marshal msg2: %v", err)
	}

	if _, err := fsq.DeliverToInbox(root, "cloudcode", "msg-1.md", data1); err != nil {
		t.Fatalf("deliver msg1: %v", err)
	}
	if _, err := fsq.DeliverToInbox(root, "codex", "msg-2.md", data2); err != nil {
		t.Fatalf("deliver msg2: %v", err)
	}

	entries, err := Collect(root, "p2p/cloudcode__codex", []string{"codex", "cloudcode"}, false, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].ID != "msg-1" || entries[1].ID != "msg-2" {
		t.Fatalf("unexpected order: %v, %v", entries[0].ID, entries[1].ID)
	}
}

func TestCollectThreadCorruptMessage(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}

	path := filepath.Join(fsq.AgentInboxNew(root, "codex"), "bad.md")
	if err := os.WriteFile(path, []byte("not a message"), 0o644); err != nil {
		t.Fatalf("write bad message: %v", err)
	}

	if _, err := Collect(root, "p2p/cloudcode__codex", []string{"codex"}, false, nil); err == nil {
		t.Fatalf("expected error for corrupt message")
	}

	called := false
	entries, err := Collect(root, "p2p/cloudcode__codex", []string{"codex"}, false, func(path string, err error) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("Collect with onError: %v", err)
	}
	if !called {
		t.Fatalf("expected onError to be called")
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
}

```

File: internal/thread/thread.go (1043 tokens)
```
package thread

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Entry is a thread message entry.
type Entry struct {
	ID       string    `json:"id"`
	From     string    `json:"from"`
	To       []string  `json:"to"`
	Thread   string    `json:"thread"`
	Subject  string    `json:"subject"`
	Created  string    `json:"created"`
	Body     string    `json:"body,omitempty"`
	Priority string    `json:"priority,omitempty"`
	Kind     string    `json:"kind,omitempty"`
	Labels   []string  `json:"labels,omitempty"`
	RawTime  time.Time `json:"-"`
}

// Collect scans agent mailboxes and returns messages for a thread.
// onError is called when a message cannot be parsed; returning a non-nil error aborts the scan.
func Collect(root, threadID string, agents []string, includeBody bool, onError func(path string, err error) error) ([]Entry, error) {
	entries := []Entry{}
	seen := make(map[string]struct{})
	for _, agent := range agents {
		dirs := []string{
			fsq.AgentInboxNew(root, agent),
			fsq.AgentInboxCur(root, agent),
			fsq.AgentOutboxSent(root, agent),
		}
		for _, dir := range dirs {
			files, err := os.ReadDir(dir)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			for _, file := range files {
				if file.IsDir() {
					continue
				}
				path := filepath.Join(dir, file.Name())
				if includeBody {
					msg, err := format.ReadMessageFile(path)
					if err != nil {
						if onError == nil {
							return nil, fmt.Errorf("parse message %s: %w", path, err)
						}
						if cbErr := onError(path, err); cbErr != nil {
							return nil, cbErr
						}
						continue
					}
					if msg.Header.Thread != threadID {
						continue
					}
					if _, ok := seen[msg.Header.ID]; ok {
						continue
					}
					seen[msg.Header.ID] = struct{}{}
					entry := Entry{
						ID:       msg.Header.ID,
						From:     msg.Header.From,
						To:       msg.Header.To,
						Thread:   msg.Header.Thread,
						Subject:  msg.Header.Subject,
						Created:  msg.Header.Created,
						Body:     msg.Body,
						Priority: msg.Header.Priority,
						Kind:     msg.Header.Kind,
						Labels:   msg.Header.Labels,
					}
					if ts, err := time.Parse(time.RFC3339Nano, msg.Header.Created); err == nil {
						entry.RawTime = ts
					}
					entries = append(entries, entry)
					continue
				}

				header, err := format.ReadHeaderFile(path)
				if err != nil {
					if onError == nil {
						return nil, fmt.Errorf("parse message %s: %w", path, err)
					}
					if cbErr := onError(path, err); cbErr != nil {
						return nil, cbErr
					}
					continue
				}
				if header.Thread != threadID {
					continue
				}
				if _, ok := seen[header.ID]; ok {
					continue
				}
				seen[header.ID] = struct{}{}
				entry := Entry{
					ID:       header.ID,
					From:     header.From,
					To:       header.To,
					Thread:   header.Thread,
					Subject:  header.Subject,
					Created:  header.Created,
					Priority: header.Priority,
					Kind:     header.Kind,
					Labels:   header.Labels,
				}
				if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
					entry.RawTime = ts
				}
				entries = append(entries, entry)
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].RawTime.IsZero() && !entries[j].RawTime.IsZero() {
			if entries[i].RawTime.Equal(entries[j].RawTime) {
				return entries[i].ID < entries[j].ID
			}
			return entries[i].RawTime.Before(entries[j].RawTime)
		}
		if entries[i].Created == entries[j].Created {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Created < entries[j].Created
	})

	return entries, nil
}

```

File: README.md (1374 tokens)
```
# Agent Message Queue (AMQ)

A local, file-queue mailbox for two or more coding agents running on the same machine. AMQ uses Maildir-style atomic delivery (tmp -> new -> cur) with a minimal CLI so agents can exchange messages without a server, database, or daemon.

Status: implementation-ready. The detailed plan is in `specs/001-local-maildir-queue`.

Requires Go 1.25+.

## Goals

- Minimal, robust file-based message exchange between agents on the same machine
- Atomic delivery semantics and durable writes
- No background services or network dependencies
- Human-auditable artifacts (plain text + JSON frontmatter)

## Non-goals

- Global search or indexing across repos
- Long-running daemons or background workers
- Complex auth, ACLs, or multi-tenant isolation

## Quickstart

```bash
# Build the CLI

go build -o amq ./cmd/amq

# Initialize a root with two agent mailboxes
./amq init --root .agent-mail --agents codex,cloudcode

# Send a message
./amq send --me codex --to cloudcode --body "Quick ping"
```

## Environment variables

- `AM_ROOT`: default root directory for storage
- `AM_ME`: default agent handle

Handles must be lowercase and match `[a-z0-9_-]+`.

## Security

- Directories use 0700 permissions (owner-only access)
- Files use 0600 permissions (owner read/write only)
- Unknown handles trigger a warning by default; use `--strict` to error instead
- With `--strict`, corrupt or unreadable `config.json` also causes an error

These defaults are suitable for single-user machines. For shared systems, ensure the AMQ root is in a user-owned directory.

## CLI

- `amq init --root <path> --agents a,b,c`
- `amq send --me codex --to cloudcode --subject "Review notes" --thread p2p/cloudcode__codex --body @notes.md`
- `amq list --me cloudcode --new`
- `amq list --me cloudcode --new --limit 50 --offset 50`
- `amq read --me cloudcode --id <msg_id>`
- `amq ack --me cloudcode --id <msg_id>`
- `amq thread --id p2p/cloudcode__codex --include-body`
- `amq thread --id p2p/cloudcode__codex --limit 50`
- `amq presence set --me codex --status busy`
- `amq cleanup --tmp-older-than 36h [--dry-run]`
- `amq watch --me cloudcode --timeout 60s`
- `amq --version`

Most commands accept `--root`, `--json`, and `--strict` (except `init` which has its own flags).

See `specs/001-local-maildir-queue/quickstart.md` for the full contract.

## Multi-Agent Workflows

AMQ supports two primary coordination patterns:

### Pattern 1: Active Agent Loop

When an agent is actively working, integrate quick inbox checks between steps.
Commands below assume `AM_ME` is set (e.g., `export AM_ME=claude`):

```bash
# Agent work loop (pseudocode):
# 1. Check inbox (non-blocking)
amq list --new --json
# 2. Process any messages
# 3. Do work
# 4. Send updates to other agents
amq send --to codex --subject "Progress" --body "Completed task X"
# 5. Repeat
```

### Pattern 2: Explicit Wait for Reply

When waiting for a response from another agent (assumes `AM_ME` is set):

```bash
# Send request
amq send --to codex --subject "Review request" --body @file.go

# Wait for reply (blocks until message arrives or timeout)
amq watch --timeout 120s

# Process the reply
amq read --id <msg_id>
```

### Watch Command Details

The `watch` command uses fsnotify for efficient OS-native file notifications:

```bash
# Wait up to 60 seconds for new messages
amq watch --me claude --timeout 60s

# Use polling fallback for network filesystems (NFS, etc.)
amq watch --me claude --timeout 60s --poll

# JSON output for scripting
amq watch --me claude --timeout 60s --json
```

Returns:
- `existing` - Found messages already in inbox
- `new_message` - Message arrived during watch
- `timeout` - No messages within timeout period

## Build, lint, test

```bash
make build
make test
make vet
make lint
make ci
make smoke
```

`make lint` expects `golangci-lint` to be installed. See https://golangci-lint.run/usage/install/

Install git hooks to enforce checks before push:

```bash
./scripts/install-hooks.sh
```

## Skills

This repo includes ready-to-use skills for AI coding assistants.

### Claude Code

```bash
# Add the marketplace (one-time)
/plugin marketplace add avivsinai/skills-marketplace

# Install this plugin
/plugin install amq-cli@avivsinai/skills-marketplace
```

### Codex CLI

```bash
# Install directly from this repo
$skill-installer avivsinai/agent-message-queue/.codex/skills/amq-cli
```

Note: The skills marketplace is a registry for Claude Code only. Codex CLI must install directly from source repos.

### Manual Installation

```bash
git clone https://github.com/avivsinai/agent-message-queue
ln -s $(pwd)/agent-message-queue/.claude/skills/amq-cli ~/.claude/skills/amq-cli
ln -s $(pwd)/agent-message-queue/.codex/skills/amq-cli ~/.codex/skills/amq-cli
```

### Development

When working in this repo, local skills in `.claude/skills/` and `.codex/skills/` take precedence over user-level installed skills. This lets you test changes before publishing.

The source of truth is `.claude/skills/amq-cli/`. After making changes, sync to Codex:

```bash
make sync-skills
```

## License

MIT (see `LICENSE`).

```

File: scripts/codex-amq-notify.py (1519 tokens)
```
#!/usr/bin/env python3
"""
Codex CLI AMQ notification hook.

This script is called by Codex's notify hook on events like agent-turn-complete.
It checks for pending AMQ messages and surfaces them via desktop notification.

Setup:
1. Add to ~/.codex/config.toml:
   notify = ["python3", "/path/to/scripts/codex-amq-notify.py"]

2. Ensure amq binary is in PATH or set AMQ_BIN environment variable.

Environment variables:
- AM_ROOT: Root directory for AMQ (default: .agent-mail)
- AM_ME: Agent handle (default: codex)
- AMQ_BIN: Path to amq binary (default: searches PATH)
- AMQ_BULLETIN: Path to bulletin file (default: .agent-mail/meta/amq-bulletin.json)
"""

import json
import os
import subprocess
import sys
from pathlib import Path


def find_amq_binary() -> str:
    """Find the amq binary."""
    # Check environment variable first
    if amq_bin := os.environ.get("AMQ_BIN"):
        if Path(amq_bin).is_file():
            return amq_bin

    # Check common locations
    candidates = [
        "./amq",
        Path.home() / ".local" / "bin" / "amq",
        "/usr/local/bin/amq",
    ]
    for candidate in candidates:
        if Path(candidate).is_file():
            return str(candidate)

    # Try PATH
    try:
        result = subprocess.run(
            ["which", "amq"], capture_output=True, text=True, check=True
        )
        return result.stdout.strip()
    except subprocess.CalledProcessError:
        pass

    return ""


def send_notification(title: str, message: str) -> None:
    """Send a desktop notification (macOS/Linux)."""
    if sys.platform == "darwin":
        # macOS
        try:
            subprocess.run(
                [
                    "osascript",
                    "-e",
                    f'display notification "{message}" with title "{title}"',
                ],
                check=False,
            )
        except FileNotFoundError:
            pass
    else:
        # Linux (requires notify-send)
        try:
            subprocess.run(["notify-send", title, message], check=False)
        except FileNotFoundError:
            pass


def check_amq_messages(amq_bin: str, root: str, me: str) -> dict:
    """Check for pending AMQ messages using drain (non-destructive peek via list)."""
    try:
        # Use list to peek at messages without moving them
        result = subprocess.run(
            [amq_bin, "list", "--me", me, "--root", root, "--new", "--json"],
            capture_output=True,
            text=True,
            check=True,
            timeout=5,
        )
        messages = json.loads(result.stdout) if result.stdout.strip() else []
        return {"count": len(messages), "messages": messages}
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired, json.JSONDecodeError) as e:
        return {"count": 0, "messages": [], "error": str(e)}


def write_bulletin(bulletin_path: str, data: dict) -> None:
    """Write message summary to bulletin file with secure permissions."""
    parent = Path(bulletin_path).parent
    parent.mkdir(parents=True, exist_ok=True)
    # Ensure directory has 0700 permissions (owner only)
    os.chmod(parent, 0o700)
    # Write file atomically with 0600 permissions
    tmp_path = bulletin_path + ".tmp"
    fd = os.open(tmp_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(data, f, indent=2)
        os.rename(tmp_path, bulletin_path)
    except Exception:
        if os.path.exists(tmp_path):
            os.unlink(tmp_path)
        raise


def clear_bulletin(bulletin_path: str) -> None:
    """Remove bulletin file when there are no messages."""
    try:
        os.unlink(bulletin_path)
    except FileNotFoundError:
        pass


def main():
    # Parse Codex notification event (passed as JSON in argv[1] or stdin)
    event = {}
    if len(sys.argv) > 1:
        try:
            event = json.loads(sys.argv[1])
        except json.JSONDecodeError:
            pass
    elif not sys.stdin.isatty():
        # Read from stdin if available
        try:
            stdin_data = sys.stdin.read().strip()
            if stdin_data:
                event = json.loads(stdin_data)
        except (json.JSONDecodeError, IOError):
            pass

    # Only process on agent-turn-complete events (or all events if not specified)
    event_type = event.get("type", "")
    if event_type and event_type != "agent-turn-complete":
        return

    # Configuration
    root = os.environ.get("AM_ROOT", ".agent-mail")
    me = os.environ.get("AM_ME", "codex")
    bulletin_path = os.environ.get(
        "AMQ_BULLETIN", os.path.join(root, "meta", "amq-bulletin.json")
    )

    # Find amq binary
    amq_bin = find_amq_binary()
    if not amq_bin:
        return  # Silently exit if amq not found

    # Check for messages
    result = check_amq_messages(amq_bin, root, me)

    if result["count"] == 0:
        # Clear stale bulletin when no messages
        clear_bulletin(bulletin_path)
        return

    # Write bulletin
    write_bulletin(bulletin_path, result)

    # Count by priority
    urgent = sum(1 for m in result["messages"] if m.get("priority") == "urgent")
    normal = sum(1 for m in result["messages"] if m.get("priority") == "normal")
    low = result["count"] - urgent - normal

    # Send notification
    title = f"AMQ: {result['count']} message(s)"
    parts = []
    if urgent:
        parts.append(f"{urgent} urgent")
    if normal:
        parts.append(f"{normal} normal")
    if low:
        parts.append(f"{low} low")
    message = ", ".join(parts) if parts else f"{result['count']} new"

    send_notification(title, message)

    # Also print to stdout for Codex to see
    print(f"[AMQ] {result['count']} pending message(s): {message}")
    for msg in result["messages"][:3]:  # Show first 3
        priority = msg.get("priority", "-")
        subject = msg.get("subject", "(no subject)")
        from_agent = msg.get("from", "?")
        print(f"  - [{priority}] {from_agent}: {subject}")
    if result["count"] > 3:
        print(f"  ... and {result['count'] - 3} more")


if __name__ == "__main__":
    main()

```

File: scripts/codex-coop-monitor.sh (346 tokens)
```
#!/usr/bin/env bash
# Codex CLI co-op mode monitor script
# Run this in a background terminal session (requires unified_exec=true in Codex config)
#
# Usage:
#   1. Enable background terminals in ~/.codex/config.toml:
#      [features]
#      unified_exec = true
#
#   2. Run in Codex background terminal:
#      ./scripts/codex-coop-monitor.sh
#
#   3. Or with custom settings:
#      AM_ROOT=/path/to/root AM_ME=codex ./scripts/codex-coop-monitor.sh

set -euo pipefail

: "${AM_ROOT:=.agent-mail}"
: "${AM_ME:=codex}"

# Find amq binary - try local build first, then PATH
AMQ_BIN="./amq"
if [[ ! -x "$AMQ_BIN" ]]; then
    AMQ_BIN="$(command -v amq 2>/dev/null || true)"
fi

if [[ -z "$AMQ_BIN" || ! -x "$AMQ_BIN" ]]; then
    echo "Error: amq binary not found. Run 'make build' first." >&2
    exit 1
fi

echo "[AMQ Co-op Monitor] Starting for agent: $AM_ME"
echo "[AMQ Co-op Monitor] Root: $AM_ROOT"
echo "[AMQ Co-op Monitor] Waiting for messages..."

# Single-shot monitor - exits when message arrives
# Codex should respawn this script or check the output
exec "$AMQ_BIN" monitor --me "$AM_ME" --root "$AM_ROOT" --timeout 0 --include-body --json

```

File: scripts/install-hooks.sh (144 tokens)
```
#!/bin/sh
# Install git hooks for this repository

HOOK_DIR="$(git rev-parse --git-dir)/hooks"

cat > "$HOOK_DIR/pre-push" << 'EOF'
#!/bin/sh
# Pre-push hook: runs lint and tests before allowing push

echo "Running pre-push checks..."

# Run the CI checks (fmt-check, vet, lint, test)
if ! make ci; then
    echo ""
    echo "❌ Pre-push checks failed. Fix the issues above before pushing."
    exit 1
fi

echo "✓ Pre-push checks passed"
EOF

chmod +x "$HOOK_DIR/pre-push"
echo "✓ Installed pre-push hook"

```

File: scripts/smoke-test.sh (572 tokens)
```
#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "$ROOT_DIR"
}
trap cleanup EXIT

BIN="$ROOT_DIR/amq"
go build -o "$BIN" ./cmd/amq

QUEUE_ROOT="$ROOT_DIR/agent-mail"

"$BIN" init --root "$QUEUE_ROOT" --agents codex,cloudcode

send_json="$("$BIN" send --root "$QUEUE_ROOT" --me codex --to cloudcode --body "hello" --json)"
msg_id="$(printf '%s\n' "$send_json" | awk -F'"' '/"id":/ {print $4; exit}')"
thread_id="$(printf '%s\n' "$send_json" | awk -F'"' '/"thread":/ {print $4; exit}')"
if [[ -z "$msg_id" || -z "$thread_id" ]]; then
  echo "failed to parse send output"
  exit 1
fi

"$BIN" list --root "$QUEUE_ROOT" --me cloudcode --new | grep -q "$msg_id"

read_out="$("$BIN" read --root "$QUEUE_ROOT" --me cloudcode --id "$msg_id")"
printf '%s' "$read_out" | grep -q "hello"

"$BIN" list --root "$QUEUE_ROOT" --me cloudcode --cur | grep -q "$msg_id"

"$BIN" ack --root "$QUEUE_ROOT" --me cloudcode --id "$msg_id"
test -f "$QUEUE_ROOT/agents/cloudcode/acks/sent/${msg_id}.json"
test -f "$QUEUE_ROOT/agents/codex/acks/received/${msg_id}.json"

thread_json="$("$BIN" thread --root "$QUEUE_ROOT" --id "$thread_id" --json)"
thread_msg="$(printf '%s\n' "$thread_json" | awk -F'"' '/"id":/ {print $4; exit}')"
if [[ "$thread_msg" != "$msg_id" ]]; then
  echo "thread output missing message"
  exit 1
fi

"$BIN" presence set --root "$QUEUE_ROOT" --me codex --status busy
"$BIN" presence list --root "$QUEUE_ROOT" | grep -q "^codex"

tmpfile="$QUEUE_ROOT/agents/codex/inbox/tmp/old.tmp"
mkdir -p "$(dirname "$tmpfile")"
printf 'tmp' > "$tmpfile"
sleep 1
"$BIN" cleanup --root "$QUEUE_ROOT" --tmp-older-than 1ms --yes
if [[ -f "$tmpfile" ]]; then
  echo "cleanup did not remove tmp file"
  exit 1
fi

echo "smoke test ok"

```

File: SECURITY.md (62 tokens)
```
# Security Policy

## Reporting a Vulnerability

Please report security issues by opening a GitHub Security Advisory for this
repository. If that is not available, open a regular issue and label it
`security`.

We will acknowledge receipt as soon as possible and work to provide a fix or
mitigation.

```

File: skills/amq-cli/SKILL.md (1046 tokens)
```
---
name: amq-cli
description: Coordinates agents via the AMQ CLI for atomic Maildir-style message delivery. Handles initializing mailboxes, sending/reading/acking messages, listing inboxes, viewing threads, setting presence, and cleaning stale tmp files. Covers tmp/new/cur semantics and safe, non-destructive usage conventions.
---

# AMQ CLI

## Quick start

- Build the binary if needed: `go build -o amq ./cmd/amq`.
- Prefer `AM_ROOT` and `AM_ME` to avoid repeating flags.

```bash
export AM_ROOT=.agent-mail
export AM_ME=codex
./amq init --root .agent-mail --agents codex,cloudcode
./amq send --to cloudcode --body "Quick ping"
```

## Core flows

### Drain (recommended for agents)

One-shot ingestion: reads all new messages, moves to cur, optionally acks. Designed for hooks and scripts.

```bash
# Drain all new messages with body, auto-ack
./amq drain --me cloudcode --include-body --ack

# Limit to 10 messages, JSON output
./amq drain --me cloudcode --limit 10 --json

# Silent when empty (perfect for hooks)
./amq drain --me $AM_ME --include-body
```

**Flags:**
- `--limit N` (default 20): Max messages to drain
- `--include-body`: Include message body in output
- `--ack` (default true): Ack messages that require acknowledgment

### Send

1. Use `--to` (required) and optionally `--thread`.
2. If `--thread` is omitted for a single recipient, use canonical p2p naming.

```bash
./amq send --me codex --to cloudcode --thread p2p/codex__cloudcode --body @notes.md
```

### List + Read + Ack (manual flow)

```bash
./amq list --me cloudcode --new
./amq read --me cloudcode --id <msg_id>
./amq ack  --me cloudcode --id <msg_id>
```

### Thread view

```bash
./amq thread --id p2p/codex__cloudcode --limit 50 --include-body
```

### Watch for messages

Wait for new messages with efficient OS-native notifications (uses fsnotify):

```bash
# Block until message arrives or timeout
./amq watch --me cloudcode --timeout 60s

# Use polling fallback for network filesystems
./amq watch --me cloudcode --timeout 60s --poll
```

### Presence (optional)

```bash
./amq presence set --me codex --status busy --note "reviewing"
./amq presence list
```

### Cleanup (explicit only)

```bash
./amq cleanup --tmp-older-than 36h
```

## Multi-Agent Coordination

### Preferred: Use drain for message ingestion

The `drain` command is designed for agent integration - it does list+read+ack in one atomic operation:

```bash
# Ingest all new messages (silent when empty - hook-friendly)
./amq drain --me $AM_ME --include-body

# With JSON output for programmatic use
./amq drain --me $AM_ME --include-body --json
```

### During active work: Quick inbox check

When doing multi-step work, use drain to check for coordination messages:

```bash
# One-shot: get messages, mark read, ack if needed
./amq drain --me $AM_ME --include-body
```

### Waiting for a reply: Use watch + drain

When you've sent a message and need to wait for a response:

```bash
# Send request
./amq send --to codex --subject "Review this" --body @file.go

# Wait for reply (blocks until message arrives)
./amq watch --me cloudcode --timeout 120s

# Ingest the reply
./amq drain --me cloudcode --include-body
```

### Workflow summary

Commands below assume `AM_ME` is set:

| Situation | Command | Behavior |
|-----------|---------|----------|
| Ingest messages | `amq drain --include-body` | One-shot: read+move+ack |
| Waiting for reply | `amq watch --timeout 60s` | Blocks until message |
| Quick peek only | `amq list --new` | Non-blocking, no side effects |

## Conventions and safety

- Do not edit message files in place; always use the CLI.
- Delivery is Maildir-style: tmp -> new -> cur. Readers must ignore tmp.
- Cleanup is explicit and never automatic; do not delete files without the cleanup command.
- Thread naming for p2p: `p2p/<lowerA>__<lowerB>` (sorted lexicographically).
- Handles must be lowercase and match `[a-z0-9_-]+`.

```

File: specs/001-local-maildir-queue/data-model.md (721 tokens)
```
# Data Model

## Directory Layout

<AM_ROOT>/
  agents/
    <handle>/
      inbox/
        tmp/
        new/
        cur/
      outbox/
        sent/
      acks/
        received/
        sent/
  threads/                (optional)
  meta/
    config.json

Handle constraints:
- Handles must be lowercase.
- Allowed characters: `a-z`, `0-9`, `_`, `-`.

## Message Files

Each message is a single Markdown file. The file name equals the message id with a .md suffix.

Example file name:
- 2025-12-24T15-02-33.123Z_pid1234_x7k9.md

### Frontmatter format

The file begins with JSON frontmatter delimited by a JSON fence:

---json
{
  "schema": 1,
  "id": "2025-12-24T15-02-33.123Z_pid1234_x7k9",
  "from": "codex",
  "to": ["cloudcode"],
  "thread": "p2p/cloudcode__codex",
  "subject": "Review notes",
  "created": "2025-12-24T15:02:33.123Z",
  "ack_required": true,
  "refs": []
}
---
<markdown body>

Field notes:
- schema: integer schema version for forward compatibility.
- id: globally unique id and file name stem.
- from: sender handle.
- to: list of receiver handles (for now, 1).
- thread: thread id string.
- subject: optional short summary.
- created: RFC3339 timestamp.
- ack_required: whether an ack is requested.
- refs: optional list of related message ids.

### Thread id conventions

- p2p/<lowerA>__<lowerB> (sorted lexicographically for canonicality)
- group: proj/<slug> or topic/<slug>

### Message id generation

Use time + pid + random suffix to avoid collisions:

<UTC RFC3339 with millis>_pid<PID>_<rand>

Rules:
- Always unique within a root.
- Use the id as the filename stem.
- Keep ids lowercase and ASCII.

## Ack Files

Ack files are JSON files named <msg_id>.json, created in:
- agents/<receiver>/acks/sent/<msg_id>.json
- agents/<sender>/acks/received/<msg_id>.json

Ack JSON schema:
{
  "schema": 1,
  "msg_id": "2025-12-24T15-02-33.123Z_pid1234_x7k9",
  "thread": "p2p/cloudcode__codex",
  "from": "cloudcode",
  "to": "codex",
  "received": "2025-12-24T15:06:10.000Z"
}

## Presence Files (optional)

agents/<handle>/presence.json

Schema:
{
  "schema": 1,
  "handle": "codex",
  "status": "busy",
  "last_seen": "2025-12-24T15:10:00Z",
  "note": "reviewing spec"
}

## Config

meta/config.json

Schema:
{
  "version": 1,
  "created_utc": "2025-12-24T15:00:00Z",
  "agents": ["codex", "cloudcode"]
}

```

File: specs/001-local-maildir-queue/plan.md (631 tokens)
```
# Implementation Plan

## Language choice

Recommendation: Go

Rationale:
- Single static binary for macOS/Linux without runtime installs.
- Fast startup and low overhead for CLI workflows.
- Strong standard library for file IO, path handling, and JSON.
- Simple concurrency primitives if we later add watches or polling.

Alternatives:
- Rust: excellent performance and safety, but higher implementation overhead for a small CLI.
- TypeScript/Node: fast to iterate but requires runtime and has heavier startup for short-lived CLI calls.

## Modules

- cmd/amq: CLI entry point
- internal/cli: argument parsing and command routing
- internal/fsq: filesystem layout and atomic delivery
- internal/format: message frontmatter parsing/serialization
- internal/ack: ack creation
- internal/thread: thread reconstruction
- internal/presence: presence read/write
- internal/config: config load/write

## Command design

Global flags:
- --root <path> or AM_ROOT
- --me <handle> or AM_ME
- --json for machine output

Commands:
- init
- send
- list
- read
- ack
- thread
- presence set|list
- cleanup

## Delivery algorithm (send)

1. Validate inputs (handles, thread id, body source).
2. Generate message id and filename.
3. Create tmp file in receiver inbox/tmp with O_EXCL.
4. Write frontmatter + body.
5. fsync file, close.
6. fsync tmp directory.
7. Rename tmp -> new (same filesystem).
8. fsync new directory.
9. Write copy to sender outbox/sent (same atomic flow).

Note: If rename fails with EXDEV, report error (tmp and new must be same filesystem).

## Read algorithm

1. Locate message by id in inbox/new or inbox/cur.
2. If found in new, rename to cur.
3. Return body (optionally header in --json).

## List algorithm

1. Enumerate inbox/new or inbox/cur.
2. Parse minimal metadata (id, created, subject, from).
3. Sort by created timestamp (fallback to filename).

## Ack algorithm

1. Parse message frontmatter to get from/to/thread.
2. Build ack JSON payload.
3. Write ack to receiver acks/sent and sender acks/received.

## Thread reconstruction

1. Enumerate both agents' message files (inbox new/cur + outbox sent).
2. Parse frontmatter for thread id and created timestamp.
3. Filter by thread id.
4. Sort by created, then id.
5. Output merged list.

## Presence

- `presence set` writes presence.json for handle.
- `presence list` reads all agents and returns their presence files if present.

## Cleanup

- Remove tmp files older than duration.
- Prompt for confirmation unless --yes.

## Testing

- Unit tests for id generation, frontmatter parse/format.
- Integration tests for tmp -> new -> cur delivery.
- Concurrency tests: multiple senders writing to same inbox.

```

File: specs/001-local-maildir-queue/quickstart.md (213 tokens)
```
# Quickstart

## Initialize

```
amq init --root .agent-mail --agents codex,cloudcode
```

## Send

```
amq send --to cloudcode --subject "Review notes" --thread p2p/cloudcode__codex --body @notes.md
amq send --to cloudcode --body "Quick ping"
```

## List / Read / Ack

```
amq list --me cloudcode --new
amq read --me cloudcode --id <msg_id>
amq ack  --me cloudcode --id <msg_id>
```

## Thread

```
amq thread --me codex --id p2p/cloudcode__codex --limit 50
```

## Presence (optional)

```
amq presence set --me codex --status busy
amq presence list
```

## Cleanup

```
amq cleanup --tmp-older-than 36h
amq cleanup --tmp-older-than 36h --dry-run
```

```

File: specs/001-local-maildir-queue/research.md (317 tokens)
```
# Research Notes

This spec relies on well-known file semantics and existing formats.

## Maildir semantics

Maildir defines the tmp -> new -> cur lifecycle and discourages readers from scanning tmp. It recommends writing to tmp, closing, and then atomically moving to new.
Reference: https://manpages.debian.org/jessie/qmail/maildir.5.en.html

## Atomic rename and durability

- POSIX rename is atomic for replacing a path on the same filesystem.
  Reference: https://man7.org/linux/man-pages/man2/rename.2.html

- fsync also applies to directories and is required to ensure directory entries are durable after rename.
  Reference: https://man7.org/linux/man-pages/man2/fsync.2.html

## Go standard library semantics

- os.Rename may not be atomic on non-Unix platforms.
  Reference: https://pkg.go.dev/os#Rename

- File.Sync commits file contents to stable storage.
  Reference: https://pkg.go.dev/os#File.Sync

## Rust standard library semantics

- std::fs::rename is a thin wrapper around platform rename calls.
  Reference: https://doc.rust-lang.org/std/fs/fn.rename.html

## Node.js semantics (for TS alternative)

- fs.rename provides access to the underlying OS rename.
  Reference: https://nodejs.org/api/fs.html#fsrenameoldpath-newpath-callback

## JSON Lines (optional thread index)

- JSON Lines is a convenient append-only record format for logs.
  Reference: https://jsonlines.org/

```

File: specs/001-local-maildir-queue/spec.md (848 tokens)
```
# Feature Specification: Local Maildir Queue

Feature: 001-local-maildir-queue
Status: Draft
Date: 2025-12-24

## Context

We need a local, file-queue mailbox that lets multiple coding agents exchange messages on the same machine without a server, database, or daemon. The design borrows Maildir delivery semantics (tmp -> new -> cur) and uses per-message files with JSON frontmatter and Markdown bodies.

## Goals

- Safe, atomic delivery semantics using tmp/new/cur
- Human-readable message files with durable writes
- Zero background services, all actions via CLI commands
- Explicit, non-destructive cleanup only

## Non-goals

- Global search/indexing across repos
- Long-running daemons or background workers
- Complex auth, ACLs, or multi-tenant isolation

## User Stories and Acceptance

### Story 1: Initialize a shared mailbox root

As a user, I want to initialize a root directory and create mailboxes for a set of agent handles.

Acceptance:
- Given a root path, `amq init --root <path> --agents a,b` creates the full directory layout.
- A meta/config.json is created containing the agent handles and timestamp.

### Story 2: Send a message with atomic delivery

As an agent, I want to send a message to another agent and rely on atomic delivery semantics.

Acceptance:
- Given a message, CLI writes the message to receiver inbox/tmp, fsyncs it, then renames into inbox/new.
- Receiver never scans tmp; only new/cur are visible.
- Sender keeps a copy in outbox/sent.

### Story 3: List new messages

As an agent, I want to list new messages without marking them as read.

Acceptance:
- `amq list --me <handle> --new` lists message ids from inbox/new.
- No files are moved or modified.

### Story 4: Read a message and mark as seen

As an agent, I want to read a specific message by id and mark it as read.

Acceptance:
- `amq read --me <handle> --id <msg_id>` outputs the message body.
- The file is moved from inbox/new to inbox/cur atomically.

### Story 5: Acknowledge a message

As an agent, I want to acknowledge a message for coordination.

Acceptance:
- `amq ack --me <handle> --id <msg_id>` writes an ack file in receiver acks/sent and sender acks/received.
- No edits to the original message file.

### Story 6: View a thread

As an agent, I want to see a thread across both mailboxes.

Acceptance:
- `amq thread --me <handle> --id <thread_id>` scans both agents' inbox/outbox and merges messages by created timestamp.
- Output is stable and deterministic for equal timestamps.

### Story 7: Presence (optional)

As an agent, I want to set presence metadata in a simple file.

Acceptance:
- `amq presence set --me <handle> --status busy` writes presence.json with last_seen and status.
- `amq presence list` reads all presence files and returns their current values.

### Story 8: Cleanup stale tmp files

As an operator, I want to explicitly remove stale tmp files.

Acceptance:
- `amq cleanup --tmp-older-than 36h` deletes tmp files older than the duration.
- Command requires explicit confirmation unless `--yes` is provided.

## Edge Cases

- If tmp/new/cur are on different filesystems, rename must fail with an error.
- If ack is requested but sender mailbox is missing, record the ack locally and warn.
- If message id is not found, read/ack returns a clear error.
- If config.json exists and --force is not set, init fails safely.
- Handles must be lowercase and match `[a-z0-9_-]+`.

## Success Criteria

- Concurrency-safe delivery with atomic rename semantics
- No in-place edits of message files
- Clear, minimal CLI for codex/claude workflows

```

File: specs/001-local-maildir-queue/tasks.md (251 tokens)
```
# Engineering Tasks

## Repo setup

- [ ] Confirm CLI name (amq) and root env vars
- [ ] Add AGENTS.md or CLAUDE.md for tool usage (optional)

## Core packages

- [ ] internal/format: frontmatter marshal/unmarshal
- [ ] internal/fsq: tmp/new/cur operations with fsync
- [ ] internal/config: load/write config
- [ ] internal/ack: create ack files
- [ ] internal/thread: thread reconstruction
- [ ] internal/presence: presence set/list

## CLI commands

- [ ] init
- [ ] send
- [ ] list
- [ ] read
- [ ] ack
- [ ] thread
- [ ] presence set
- [ ] presence list
- [ ] cleanup

## Tests

- [ ] Message id format and uniqueness
- [ ] Frontmatter round-trip
- [ ] Atomic delivery flow
- [ ] Read moves new -> cur
- [ ] Ack writes both files
- [ ] Thread merge ordering
- [ ] Cleanup removes old tmp files only

## Documentation

- [ ] Update README with final CLI usage
- [ ] Add examples for two-agent workflow

```

</files>

