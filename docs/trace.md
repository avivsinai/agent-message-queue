# Message trace

`amq trace <message-id> [--root <path>] [--json]` performs a read-only join over
evidence already present in one AMQ root. It does not run a daemon, repair a
mailbox, retry delivery, or infer an event that has no durable artifact.

Phase A inspects:

- message copies in inboxes and sender outboxes;
- addressing fields persisted in message headers;
- current inbox and DLQ file visibility;
- DLQ envelopes and their embedded original messages;
- drained and DLQ delivery receipts;
- messages connected by `refs`;
- notification history, which is explicitly `no_evidence` until AMQ has a
  durable notification-attempt ledger.

## JSON contract

The top-level schema is `amq/trace/v1`:

```json
{
  "schema": "amq/trace/v1",
  "message_id": "2026-07-26T12-00-00.000Z_pid1_example",
  "status": "found",
  "root": "/path/to/.agent-mail/session",
  "root_identity": "opaque-platform-token",
  "legs": {
    "message": {
      "status": "evidence",
      "evidence": []
    },
    "notification": {
      "status": "no_evidence",
      "evidence": [],
      "detail": "notification attempt history is unavailable because Phase A has no durable attempt ledger",
      "next_step": "run 'amq doctor --ops' for current wake health; do not infer historical notification success"
    }
  }
}
```

`legs` always contains `message`, `route`, `delivery`, `dlq`, `receipts`,
`thread`, and `notification`. Every leg has one of these statuses:

- `evidence` — one or more durable artifacts support the leg;
- `no_evidence` — no supporting artifact was found;
- `error` — a candidate path could not be inspected safely.

Every `no_evidence` or `error` leg includes `detail` and `next_step`.
`evidence` is always an array, including when it is empty.

The top-level status is:

- `found` when at least one non-notification leg has evidence;
- `partial` when evidence exists but at least one leg encountered an inspection
  error;
- `not_found` when no non-notification evidence exists.

`not_found` still emits the complete text or JSON result, then exits with code
3. This lets a human see the next steps while scripts receive the existing AMQ
not-found signal.

## Evidence limits

A message header records resulting addressing metadata. It is not a historical
record of the resolver decision made at send time. `route` states that limit and
points to `amq route explain` only as a view of current routing.

An inbox or DLQ file proves current visibility. It does not prove the historical
directory-sync result from the original delivery. The delivery evidence marks
durability as `no_evidence`; do not infer retry safety from current file
presence.

Notification success is never inferred from wake state, a mailbox file, or a
delivery receipt. Phase A reports the notification leg as `no_evidence`.
