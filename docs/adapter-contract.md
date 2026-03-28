# Adapter Contract v1

This document defines the **v1 adapter contract** for AMQ integrations.

## Product Boundary

AMQ's canonical transport is the **message**.

Adapters do not turn AMQ into a remote workflow engine. They convert external lifecycle or task events into normal AMQ messages so agents can consume them with the same primitives they already use for peer-to-peer coordination:

- `amq drain`
- `amq reply`
- `amq list --label ...`
- `amq thread`

That is the core boundary: AMQ transports messages; adapters annotate those messages with provenance.

## Envelope

Adapter metadata lives under `context.orchestrator`.

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

Required top-level adapter fields:

| Field | Type | Meaning |
|------|------|---------|
| `version` | integer | Adapter envelope version. Current value: `1`. |
| `name` | string | Origin system name, for example `symphony` or `kanban`. |
| `transport` | string | Adapter transport mode, for example `hook` or `bridge`. |
| `event` | string | External lifecycle or task event name. |

Optional top-level adapter fields:

| Field | Type | Meaning |
|------|------|---------|
| `workspace` | object | Adapter-specific workspace provenance such as `path`, `key`, or `id`. |
| `task` | object | Adapter-specific task provenance such as `id`, `state`, `prompt`, or `agent_id`. |

## Built-in Adapters

### Symphony

Symphony is a **lightweight optional hook adapter**.

- `name`: `symphony`
- `transport`: `hook`
- Events: `after_create`, `before_run`, `after_run`, `before_remove`
- Workspace shape: `path`, `key`
- Task shape: `id`, optional `state`

### Cline Kanban

Kanban is **experimental**.

- `name`: `kanban`
- `transport`: `bridge`
- Event sources: `task_sessions_updated`, `task_ready_for_review`
- Workspace shape: `id`, `path`
- Task shape: `id`, optional `prompt`, `column`, `state`, `review_reason`, `agent_id`

Because it depends on a fast-moving preview WebSocket surface, its event mapping may evolve more aggressively than the core AMQ CLI.

## Labels

Adapter messages should remain filterable with generic AMQ primitives. Built-in adapters use:

- `orchestrator`
- `orchestrator:<name>`
- `task-state:<state>` when task state is known
- `handoff` for review-ready transitions
- `blocking` for failed, interrupted, or otherwise blocked work

## Threading and Delivery

Built-in adapters currently use **self-delivery** (`from=<me>`, `to=<me>`) so the receiving agent can process adapter events from its own inbox.

Threading rules:

- Symphony: `task/<workspace-key>`
- Kanban: `task/<task-id>`

## Reply and Relay Semantics

Adapter messages are still AMQ messages, but reply behavior can be asymmetric across systems:

- Self-delivered adapter messages are primarily for local agent consumption
- Swarm external-to-Claude-teammate traffic still requires relay through the team leader's AMQ inbox
- Adapters should document any relay assumptions rather than implying universal direct reply routing

## Explicit Non-Goals for v1

The v1 adapter contract does **not** define:

- a universal remote execution API
- cross-tool task ownership semantics
- a global deduplication protocol across external systems
- adapter capability handshake, schema negotiation, or source-version checks
- built-in fan-out / watcher semantics such as "also notify coordinator X"
- automatic reply routing back into every upstream orchestrator

If AMQ grows additional adapters later, changes should extend this document first and preserve the message-first product boundary.
