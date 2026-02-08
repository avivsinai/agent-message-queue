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

Notes:
- Don’t edit message files directly; use the CLI.
- The CLI auto-fills `id`, `created`, and a default `thread` when not provided.
