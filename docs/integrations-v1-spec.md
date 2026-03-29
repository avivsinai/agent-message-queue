# AMQ Integrations v1 — Frozen Spec

AMQ stays one layer below the orchestrator. Adapters convert external lifecycle and handoff events into ordinary AMQ messages; they do not turn AMQ into a task board, pipeline runner, scheduler, or control plane.

## Scope

Ship in one PR:
- Global root fallback (`AMQ_GLOBAL_ROOT`, `~/.amqrc`)
- `amq doctor --ops`
- `amq integration symphony init`
- `amq integration symphony emit`
- `amq integration kanban bridge`

Out of scope for v1:
- `amq state`
- Kanban chat-message forwarding
- Kanban full board mirroring
- Any daemon/database

## Global Root Resolution

Exact precedence chain:
1. `--root`
2. `AM_ROOT`
3. project `.amqrc` (cwd walk-up; when a root is already known, use root-aware lookup)
4. `AMQ_GLOBAL_ROOT`
5. `~/.amqrc`
6. auto-detect `.agent-mail`

Notes:
- `~/.amqrc` uses the same JSON shape as project `.amqrc`
- Global root is a default queue locator, not a project selector
- Commands should surface the winning source in doctor JSON / check messages

## CLI Surface

New top-level group: `amq integration`

### Subcommands

```
amq integration symphony init [--workflow <path>] --me <agent> [--root <path>] [--check] [--force] [--json]
amq integration symphony emit --event <after_create|before_run|after_run|before_remove> --me <agent> [--root <path>] [--workspace <path>] [--identifier <key>] [--json]
amq integration kanban bridge --me <agent> [--root <path>] [--url <ws://127.0.0.1:3484/api/runtime/ws>] [--reconnect <duration>] [--json]
```

### Extended existing command

```
amq doctor [--ops] [--json]
```

### Flag semantics

**symphony init:**
- Default workflow path: `WORKFLOW.md`
- `--check`: inspect/report whether AMQ-managed hook fragments are installed; do not write
- `--force`: rewrite the AMQ-managed fragment even if already present

**symphony emit:**
- Default workspace: current working directory
- Default identifier: basename of workspace path

**kanban bridge:**
- Default URL: `ws://127.0.0.1:3484/api/runtime/ws`
- Default reconnect delay: `3s`

### Symphony init generated hooks

Generated hook lines must:
- Call `amq integration symphony emit ... || true`
- Prefer explicit pinned `--root <resolved-root>` when available
- Preserve existing user hook content
- Be inserted idempotently as an AMQ-managed fragment

## `context.orchestrator` Schema

All integration metadata lives under `context.orchestrator`. No new top-level context keys.

### Common shape

```json
{
  "orchestrator": {
    "version": 1,
    "name": "<symphony|kanban>",
    "transport": "<hook|bridge>",
    "event": "<event_name>",
    "workspace": {
      "path": "/abs/path",
      "key": "<identifier>"
    },
    "task": {
      "id": "<task_id>",
      "state": "<state>"
    }
  }
}
```

### Symphony variant

- `name`: `symphony`
- `transport`: `hook`
- `event`: one of `after_create|before_run|after_run|before_remove`
- `workspace.path`: absolute workspace path
- `workspace.key`: sanitized issue/workspace identifier
- `task.id`: same as `workspace.key`
- `task.state`: present when confidently known; omit otherwise

### Kanban variant

```json
{
  "orchestrator": {
    "version": 1,
    "name": "kanban",
    "transport": "bridge",
    "event": "task_ready_for_review",
    "workspace": {
      "id": "workspace-1",
      "path": "/abs/path/to/repo"
    },
    "task": {
      "id": "task-42",
      "prompt": "Refactor reply routing",
      "column": "review",
      "state": "awaiting_review",
      "review_reason": "hook",
      "agent_id": "codex"
    }
  }
}
```

### Label rules

- Always: `orchestrator`, `orchestrator:<name>`
- When state is known: `task-state:<state>`
- Add `handoff` for review-ready / awaiting-review messages
- Add `blocking` for failed / interrupted / blocked messages

### Thread rules

- Symphony: `task/<workspace_key>`
- Kanban: `task/<task_id>`

## Kanban v1 Bridge Model

Bridge keeps two normalized caches:
- `cardsByTaskID` from `snapshot` / `workspace_state_updated`
- `sessionsByTaskID` from `task_sessions_updated`

### Event → AMQ Mapping

| Input | Condition | AMQ emission |
|---|---|---|
| `snapshot` | initial connect / reconnect | No message; bootstrap caches |
| `workspace_state_updated` | any | No message; refresh card metadata cache |
| `task_sessions_updated` | task enters `running` | `kind=status`, `priority=low`, labels `orchestrator,orchestrator:kanban,task-state:running` |
| `task_sessions_updated` | task enters `failed` | `kind=todo`, `priority=normal`, labels `orchestrator,orchestrator:kanban,task-state:failed,blocking` |
| `task_sessions_updated` | task enters `interrupted` | `kind=todo`, `priority=normal`, labels `orchestrator,orchestrator:kanban,task-state:interrupted,blocking` |
| `task_sessions_updated` | task enters `awaiting_review` (no paired ready event) | Fallback handoff message |
| `task_ready_for_review` | any | `kind=todo`, `priority=normal`, labels `orchestrator,orchestrator:kanban,task-state:awaiting_review,handoff` |
| `task_chat_message` | any | Out of scope v1 |

## `doctor --ops --json` Shape

`--ops` is additive. Keep existing `checks` and `summary`, add `ops`:

```json
{
  "checks": ["...existing..."],
  "summary": { "ok": 0, "warn": 0, "error": 0 },
  "ops": {
    "root": {
      "path": "/abs/root",
      "source": "project_amqrc"
    },
    "agents": [
      {
        "handle": "codex",
        "unread_count": 3,
        "oldest_unread_age_seconds": 120,
        "dlq_count": 1,
        "oldest_dlq_age_seconds": 7200,
        "presence_status": "active",
        "presence_age_seconds": 15,
        "pending_ack_count": 2,
        "oldest_pending_ack_age_seconds": 300
      }
    ],
    "acks": {
      "pending_count": 2,
      "oldest_pending_age_seconds": 300,
      "recent_latency_ms": null
    },
    "hints": [
      {
        "code": "kanban_detected",
        "status": "warn",
        "message": "Kanban runtime appears to be listening on 127.0.0.1:3484"
      }
    ]
  }
}
```

Root `source` enum: `flag`, `env`, `project_amqrc`, `global_env`, `global_amqrc`, `auto_detect`

Hint codes: `global_root_missing`, `global_root_configured`, `kanban_detected`, `symphony_workflow_detected`, `symphony_hooks_missing`, `symphony_hooks_installed`, `session_mismatch_hint`

## File Layout

### CLI / registry
- `internal/cli/integration.go` — new
- `internal/cli/integration_symphony.go` — new
- `internal/cli/integration_kanban.go` — new
- `internal/cli/doctor_ops.go` — new
- `internal/cli/registry.go` — edit
- `internal/cli/env.go` — edit
- `internal/cli/common.go` — edit
- `internal/cli/doctor.go` — edit

### Shared integration helpers
- `internal/integration/common/context.go` — new
- `internal/integration/common/labels.go` — new
- `internal/integration/common/deliver.go` — new

### Symphony
- `internal/integration/symphony/init.go` — new
- `internal/integration/symphony/workflow.go` — new
- `internal/integration/symphony/emit.go` — new

### Kanban
- `internal/integration/kanban/bridge.go` — new
- `internal/integration/kanban/state.go` — new
- `internal/integration/kanban/messages.go` — new

## Task Split

### Task A: Core / root / doctor / shared helpers
Owner write set:
- `internal/cli/registry.go`
- `internal/cli/env.go`
- `internal/cli/common.go`
- `internal/cli/doctor.go`
- `internal/cli/doctor_ops.go`
- `internal/cli/integration.go`
- `internal/integration/common/*`
- Related tests

### Task B: Symphony integration
Owner write set:
- `internal/cli/integration_symphony.go`
- `internal/integration/symphony/*`
- Symphony-specific tests

### Task C: Kanban bridge
Owner write set:
- `internal/cli/integration_kanban.go`
- `internal/integration/kanban/*`
- Kanban-specific tests

### Dependencies
- Task A shared helper signatures block final B/C integration wiring
- Task B does not depend on Task C
- Task C does not depend on Task B
- Only Task A edits the command registry
- Final smoke/docs depend on A + B + C
