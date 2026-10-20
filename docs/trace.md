# Message trace

`amq trace <message-id> [--root <path>] [--json]` performs a read-only join over
evidence already present in one AMQ root. It does not run a daemon, repair a
mailbox, retry delivery, or infer an event that has no durable artifact.

It inspects:

- message copies in inboxes and sender outboxes;
- addressing fields persisted in message headers;
- current inbox and DLQ file visibility;
- DLQ envelopes and their embedded original messages;
- drained and DLQ delivery receipts;
- messages connected by `refs`;
- notification attempts recorded in each agent's notification-attempt ledger
  (`agents/<agent>/receipts/notification-attempts.jsonl` and its one rotated
  copy).

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
      "state": "ledger_absent",
      "evidence": [],
      "detail": "no notification attempt ledger file was found",
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
delivery receipt. The notification leg reads only the notification-attempt
ledger, and its `state` field says what the ledger shows:

- `ledger_absent`: no active or rotated journal file exists;
- `ledger_unsupported`: no journal exists and this platform does not support
  notification-attempt locking, so absence says nothing about attempts;
- `ledger_present_no_record`: a journal exists but has no record for this
  message;
- `record_present`: the journal has at least one record for this message.

The first three report the leg as `no_evidence`. Each record carries its folded
lifecycle state: `accepted` (the external injector declared provider
dispatch), `deferred` or `retried` (the provider was busy before dispatch and
the cohort stays pending), `failed` (terminal), `written` (raw TIOCSTI or a
legacy injector wrote bytes), `indeterminate` (a prepared record with no
result), `write_failed` (the record itself could not be persisted), or
`invalid` (an out-of-order sequence). The ledger contract is in
[wake state invariants](wake-state-invariants.md#notification-attemptsjsonl-durable-notifier-lifecycle-audit).
None of these states proves human or agent consumption.
