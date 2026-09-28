# pi bridge protocol (`amq:pi-bridge:v1`)

The pi bridge protocol lets `amq-remote` reach a live
[pi](https://github.com/earendil-works/pi) coding-agent session. Two parties
share one directory:

- the **adapter**: the `amq-remote` attachment for target kind `pi`;
- the **extension**: a pi extension that runs inside the pi process, such as
  [`integrations/pi/amq-bridge.ts`](../integrations/pi/amq-bridge.ts).

They never talk directly. Every fact crosses the filesystem as a complete,
atomically published file or as an fsynced line. Anyone can implement either
side from this document.

## Identity and layout

The extension takes its identity from the AMQ participation contract: `AM_ROOT`
(the queue root) and `AM_ME` (the handle, `[a-z0-9_-]+`, not starting with
`-`). pi runs under `amq coop exec`, which sets both. Without them the
extension stays inactive. The adapter takes the handle from its manifest entry
and never from its own environment.

```text
<AM_ROOT>/agents/<handle>/extensions/pi-bridge/
  requests/<ref>.json    adapter -> extension: one request per ref
  receipts/<ref>.json    extension -> adapter: admission proof
  events/<ref>.jsonl     extension -> adapter: lifecycle and terminal evidence
  bridge.liveness        extension -> adapter: heartbeat
```

Directories are mode 0700 and files are mode 0600.

`<ref>` is the request reference: `amqr1_` followed by lowercase base32 without
padding (`^amqr1_[a-z2-7]{16,472}$`). It encodes the request key and is already
a safe file name, so the file name is the ref. A reader derives the ref from
the file name and ignores every name that does not match, including the
adapter's temporary files.

## Ownership

The adapter writes only `requests/`. The extension writes only `receipts/`,
`events/`, and `bridge.liveness`. Neither side deletes or rewrites a file the
other side owns. The extension never deletes or rewrites a request; the adapter
never deletes a request it published. Cleanup of old files is an explicit
operator action outside this protocol.

## Protocol string

Every receipt, event line, and liveness record carries
`"protocol": "amq:pi-bridge:v1"`. A reader refuses a record with a different
non-empty protocol string instead of guessing its meaning:

- a foreign liveness record reads as "no live bridge";
- a foreign receipt is an error for that ref, never admission evidence;
- one foreign event line refuses the whole event stream for that ref. The
  refusal holds until a present stream validates as v1 end to end; an absent
  stream does not lift it.

## `requests/<ref>.json`

```json
{
  "ref": "amqr1_...",
  "text": "summarize the diff",
  "deliver_as": "followUp",
  "not_after": "2026-01-01T00:00:00.123456789Z",
  "epoch_hint": "3f0c...",
  "created_at": "2026-01-01T00:00:00Z"
}
```

| Field | Meaning |
| --- | --- |
| `ref` | Equal to the file name without `.json`. |
| `text` | The user message to submit, verbatim. |
| `deliver_as` | Always `followUp` in v1. |
| `not_after` | RFC 3339 deadline. The extension does not deliver after it. |
| `epoch_hint` | The session generation the adapter has pinned, or absent/empty for first contact. |
| `created_at` | RFC 3339 publication time; informational. |

The adapter publishes create-new: it writes and fsyncs a temporary file in
`requests/`, hard-links it onto `<ref>.json`, removes the temporary name, and
fsyncs the directory. An existing `<ref>.json` means the ref was already
published; the adapter never overwrites it and never re-sends a ref.

Before a fresh publication the adapter checks liveness. With no live bridge it
refuses the submit and writes nothing.

## `receipts/<ref>.json`

```json
{
  "protocol": "amq:pi-bridge:v1",
  "ref": "amqr1_...",
  "session_generation": "3f0c...",
  "delivered_at": "2026-01-01T00:00:00.500Z",
  "pid": 4242
}
```

The receipt is the only admission proof. pi's `sendUserMessage` returns
nothing and reports failures out of band, so no other signal proves that a
request reached the session. The extension writes the receipt create-new
(temporary file, hard link, directory fsync) immediately before it hands the
text to pi, in the same synchronous step. A link that fails because the
receipt exists means another bridge process claimed the ref; the extension
then does not deliver. This ordering makes delivery at-most-once.

`session_generation` is the generation live when the request was delivered.
`pid` is the pi process.

## `events/<ref>.jsonl`

One JSON object per line, appended and fsynced line by line:

```json
{"protocol":"amq:pi-bridge:v1","ref":"amqr1_...","event":"started","at":"..."}
{"protocol":"amq:pi-bridge:v1","ref":"amqr1_...","event":"completed","text":"two files changed","at":"..."}
```

| `event` | Fields | Adapter state |
| --- | --- | --- |
| `started` | - | running |
| `completed` | `text`: the final assistant text | completed, with the text as the result |
| `failed` | `error` | failed |
| `cancelled` | - | cancelled |
| `refused` | `reason`, optional `error` | rejected with a typed code |

`refused` reasons map to typed codes:

| `reason` | Code |
| --- | --- |
| `expired` | `expired` |
| `generation` | `stale_epoch` |
| `busy` | `busy` |
| anything else (for example `invalid`, `unsupported`) | `native_error` |

The first terminal event (`completed`, `failed`, `cancelled`, `refused`) is
final for the ref; later lines are not evidence. Unknown event types are
ignored. A line whose `ref` names another request is ignored. A partial
trailing line from a crash mid-append is skipped; because each terminal line
is fsynced, a skipped line is never the terminal evidence. `text` and `error`
are at most 256 KiB, cut on a UTF-8 boundary.

A missing events file means "no events yet", never an error. The adapter
reads only the current file, so rotating or truncating it loses history but
never fabricates a state.

## `bridge.liveness`

```json
{
  "protocol": "amq:pi-bridge:v1",
  "live": true,
  "at": "2026-01-01T00:00:00.000Z",
  "pid": 4242,
  "surface": "tui",
  "session_generation": "3f0c..."
}
```

The extension replaces the whole file (temporary file plus rename) every 2
seconds. Freshness is the file's modification time: a record older than 5
seconds is stale. The record is live only when it is fresh, parses, carries
this protocol, and has `live: true` and `pid > 0`. On shutdown the extension
writes `live: false`, so the adapter sees the bridge offline at once.
`surface` is the pi run mode (`tui`, `rpc`, `json`, or `print`).

## Session generation and epoch

A session generation is an opaque token (`^[A-Za-z0-9_.:-]{1,128}$`) that
names one pi session runtime. The extension mints a fresh generation on every
`session_start`: startup, resume, new session, reload, and fork each start a
new extension runtime. It writes liveness immediately with the new
generation.

The adapter's epoch rules:

1. **Only receipts pin.** Before it observes any receipt the adapter publishes
   the sentinel epoch `unpinned` and sends requests with an empty
   `epoch_hint`. The first receipt pins its `session_generation`. On restart
   the adapter rebuilds from `receipts/` oldest first, so the newest receipt
   sets the pin.
2. **Liveness may only drop a pin.** A live record whose `session_generation`
   differs from the pin, or whose `pid` differs from the pinning receipt's
   `pid`, drops the pin back to `unpinned`. Liveness never sets a pin.
3. **A stale hint is refused.** The extension delivers a request with an empty
   `epoch_hint` (first contact) or a hint equal to its current generation. Any
   other hint gets a `refused` event with reason `generation` and **no
   receipt**. The adapter maps it to `stale_epoch` and drops its pin; the next
   delivered request's receipt pins the live generation.

## Extension processing rules

The extension polls `requests/` often enough that a receipt lands well inside
the adapter's 2-second receipt wait. For each request with neither a receipt
nor an events file, in publication order, it:

1. validates the shape: `ref` matches the file name, `text` is non-empty,
   `deliver_as` is `followUp`, and `not_after` parses. A failure is a `refused`
   event (`invalid` or `unsupported`).
2. refuses with reason `expired` when `not_after` has passed.
3. applies the epoch rule above.
4. claims the receipt and calls `sendUserMessage(text, { deliverAs: "followUp" })`
   without prompt-template expansion, so a body that starts with `/` stays
   text.

A request that already has a receipt or an events file is handled and is never
examined or delivered again. Refusals write no receipt.

The reference extension keeps one delivered request in flight. Other requests
wait in `requests/` (still subject to `not_after` and the epoch rule) until the
in-flight request is terminal. It correlates the run like this:

- `started` when pi emits `message_start` for a user message whose text equals
  the request text;
- the final assistant text from `message_end` and `agent_end`;
- the outcome from `agent_before_settle` (`completed`, `aborted`, `error`);
- the terminal event at `agent_settled`, the notification that pi will not
  continue on its own: `completed`, `cancelled` for `aborted`, or `failed` for
  `error`. A settle that arrives before `started` while pi still holds queued
  messages belongs to earlier work and is not used.

pi reports a failed `sendUserMessage` out of band, so a follow-up can be lost
without a signal. When the in-flight request has not started 5 seconds after
delivery and pi is idle with no queued messages, the extension appends
`failed` and moves on to the next request.

On `session_shutdown` with a request in flight, the extension appends `failed`
naming the shutdown reason. On `session_start` it appends `failed` to every
receipt from another generation that has no terminal event, so a crash never
leaves a ref running forever.

## Adapter recovery and evidence

The adapter keeps no durable state of its own; the seam is the record. On
attach it rebuilds every receipt-backed ref from `receipts/` and `events/` and
never redispatches.

| Seam state for a ref | Adapter answer |
| --- | --- |
| Nothing retained (no request known, no receipt) | unknown. pi cannot prove non-admission, so the adapter never reports "none" for it. |
| Request published, no receipt, live bridge | uncertain. The extension may still deliver it. |
| Request published, no receipt, no live bridge | `attachment_lost`, uncertain. The file stays for a later bridge. |
| Receipt, no terminal event | admitted, confirmed running. |
| Terminal `completed`, `failed`, or `cancelled` | admitted, terminal, with the result. |
| `refused` | rejected with the mapped code; admitted only if a receipt exists. |
| Unreadable or foreign receipt, or refused event stream | error for that ref; the adapter keeps the record uncertain. |

`Inspect` advertises submit evidence `submitted` and completion evidence
`run_terminal`. The adapter never advertises `admitted` for the session:
admission is proven per ref by its receipt, not by a session capability.

## Capabilities

v1 supports inspect and submit with `deliver_as: followUp` only. Steering,
exact cancellation, tool approval, question answers, and terminal access are
not part of the protocol; the adapter refuses a steer submit before it writes
anything.
