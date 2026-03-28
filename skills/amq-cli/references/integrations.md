# Orchestrator Integrations

Use these commands when AMQ is the messaging layer underneath an external orchestrator.

## Root Resolution

For orchestrator-spawned agents, make the queue discoverable even when the process starts outside the repo root:

```bash
export AMQ_GLOBAL_ROOT="$HOME/.agent-mail"
```

Or create `~/.amqrc`:

```json
{"root": ".agent-mail"}
```

Root precedence:

```text
flags > AM_ROOT > project .amqrc > AMQ_GLOBAL_ROOT > ~/.amqrc > auto-detect
```

## Symphony

Patch `WORKFLOW.md` once:

```bash
amq integration symphony init --me codex
amq integration symphony init --me codex --check
```

Emit lifecycle events from hooks:

```bash
amq integration symphony emit --event after_create --me codex
amq integration symphony emit --event before_run --me codex
amq integration symphony emit --event after_run --me codex
amq integration symphony emit --event before_remove --me codex
```

## Cline Kanban

Run the websocket bridge:

```bash
amq integration kanban bridge --me codex
amq integration kanban bridge --me codex --workspace-id my-workspace
```

Defaults:

- URL: `ws://127.0.0.1:3484/api/runtime/ws`
- Reconnect delay: `3s`
- Emits only on task session state transitions plus `task_ready_for_review`

## Runtime Diagnostics

```bash
amq doctor --ops
amq doctor --ops --json
```

`doctor --ops` adds queue depth, DLQ state, presence freshness, pending acks, and integration hints on top of the base `doctor` checks.

## Message Shape

Integration messages are self-delivered and carry metadata under `context.orchestrator`:

```json
{
  "orchestrator": {
    "version": 1,
    "name": "kanban",
    "transport": "bridge",
    "event": "task_ready_for_review",
    "workspace": {
      "id": "workspace-123",
      "path": "/abs/path/to/worktree"
    },
    "task": {
      "id": "task-42",
      "prompt": "Review PR #47",
      "column": "review",
      "state": "awaiting_review",
      "review_reason": "task_ready_for_review",
      "agent_id": "codex"
    }
  }
}
```

Common labels:

- `orchestrator`
- `orchestrator:symphony` or `orchestrator:kanban`
- `task-state:<state>`
- `handoff`
- `blocking`
