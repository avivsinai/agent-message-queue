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
