# Message Format Cheatsheet

AMQ messages are Markdown files with JSON frontmatter:

```json
{
  "schema": 1,
  "id": "<msg_id>",
  "from": "claude",
  "to": ["codex"],
  "thread": "p2p/claude__codex",
  "subject": "Optional summary",
  "created": "<RFC3339 timestamp>",
  "ack_required": true,
  "priority": "normal",
  "kind": "question",
  "labels": []
}
```

Notes:
- `thread` is required; `p2p/<a>__<b>` is the canonical one-to-one form.
- `ack_required` determines whether `amq ack` is expected.
- `priority`, `kind`, and `labels` are optional but useful for filtering.
