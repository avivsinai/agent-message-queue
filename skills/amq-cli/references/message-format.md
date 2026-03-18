# Message Format Cheatsheet

AMQ messages are Markdown files with a JSON frontmatter header:

```text
---json
{
  "schema": 1,
  "id": "<msg_id>",
  "from": "claude",
  "to": ["codex"],
  "thread": "p2p/claude__codex",
  "subject": "Optional summary",
  "created": "<RFC3339 timestamp>",
  "ack_required": false,
  "refs": ["<related_msg_id>"],

  "priority": "normal",
  "kind": "question",
  "labels": ["bug", "parser"],
  "context": {"paths": ["internal/cli/send.go"], "focus": "error handling"}
}
---
<markdown body>
```

Field notes:
- `schema`: integer schema version (currently 1).
- `id`: globally unique message id (also the filename stem on disk).
- `from`: sender handle.
- `to`: list of receiver handles.
- `thread`: thread id string. For p2p, use `p2p/<a>__<b>` with lexicographic ordering.
- `subject`: optional short summary.
- `created`: RFC3339 timestamp.
- `ack_required`: whether an ack is requested (`amq ack`).
- `refs`: optional list of related message ids (e.g., replies).
- `priority`: optional (`urgent`, `normal`, `low`).
- `kind`: optional (e.g., `review_request`, `review_response`, `question`, `answer`, `status`, `todo`).
- `labels`: optional list of tags for filtering.
- `context`: optional JSON object for structured metadata.

### Federation Fields (Schema 2)

Federated messages (cross-session or cross-project) include two additional header objects:

```text
---json
{
  "schema": 2,
  "id": "<msg_id>",
  "from": "claude",
  "to": ["codex"],
  "thread": "p2p/claude__codex",
  "created": "<RFC3339 timestamp>",

  "origin": {
    "project": "my-api",
    "project_id": "abc123",
    "session": "collab",
    "agent": "claude",
    "reply_to": "claude@my-api:collab",
    "ack_to": "claude@my-api:collab"
  },
  "delivery": {
    "requested_to": ["codex@auth"],
    "resolved_to": ["codex@auth"],
    "scope": "cross-session",
    "channel": "#events",
    "fanout_index": 1,
    "fanout_total": 3
  }
}
---
```

Origin fields:
- `project`: source project slug.
- `project_id`: stable project identifier (populated on discovery fallback path, not when AM_PROJECT is set via coop exec).
- `session`: source session name.
- `agent`: source agent handle.
- `reply_to`: qualified address for routing replies back to the sender.
- `ack_to`: qualified address for routing acks (set when ack_required is true).

Delivery fields:
- `requested_to`: original address(es) as specified by the sender.
- `resolved_to`: concrete resolved target(s) after address resolution.
- `scope`: `local`, `cross-session`, or `cross-project`.
- `channel`: channel address (for channel/announce sends).
- `fanout_index` / `fanout_total`: per-target index for multi-target fan-out.

Notes:
- Schema 1 messages are still fully supported for reading. Schema 2 is written by default for federated sends.
- Don’t edit message files directly; use the CLI.
- The CLI auto-fills `id`, `created`, and a default `thread` when not provided.
