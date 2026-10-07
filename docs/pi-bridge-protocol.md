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
  answers/<hash>.json    adapter -> extension: one tool-approval answer (revision 4)
  bridge.liveness        extension -> adapter: heartbeat
```

Directories are mode 0700 and files are mode 0600.

`<ref>` is the request reference: `amqr1_` followed by lowercase base32 without
padding (`^amqr1_[a-z2-7]{16,472}$`). It encodes the request key and is already
a safe file name, so the file name is the ref. A reader derives the ref from
the file name and ignores every name that does not match, including the
adapter's temporary files.

## Ownership

The adapter writes only `requests/` and `answers/`. The extension writes only
`receipts/`, `events/`, and `bridge.liveness`. Neither side deletes or rewrites a file the
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
  "created_at": "2026-01-01T00:00:00Z",
  "bridge_revision": 3
}
```

| Field | Meaning |
| --- | --- |
| `ref` | Equal to the file name without `.json`. |
| `text` | The user message to submit, verbatim. |
| `deliver_as` | Always `followUp` in v1. |
| `not_after` | RFC 3339 deadline. The extension does not deliver after it. |
| `epoch_hint` | The session generation the request addresses: the pinned generation, or on first contact the generation the caller's address epoch names. Always set. |
| `created_at` | RFC 3339 publication time; informational. |
| `bridge_revision` | The bridge revision the adapter requires. Always set. |

The adapter publishes create-new: it writes and fsyncs a temporary file in
`requests/`, hard-links it onto `<ref>.json`, removes the temporary name, and
fsyncs the directory. An existing `<ref>.json` means the ref was already
published; the adapter never overwrites it and never re-sends a ref.

Before a fresh publication the adapter checks liveness and writes nothing when
a check fails:

- no live bridge, or a live record with no valid `session_generation`:
  `attachment_lost`;
- a live record below the minimum bridge revision: `unsupported` (see
  [Bridge revision](#bridge-revision));
- on first contact, a live generation other than the one the caller's epoch
  names: `stale_epoch`.

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
| `uncertain` | `error` | uncertain: the extension stopped tracking the ref without a native outcome |

`refused` reasons map to typed codes:

| `reason` | Code |
| --- | --- |
| `expired` | `expired` |
| `generation` | `stale_epoch` |
| `busy` | `busy` |
| `revision` | `unsupported` |
| anything else (for example `invalid`, `unsupported`) | `native_error` |

The first terminal event (`completed`, `failed`, `cancelled`, `refused`,
`uncertain`) is final for the ref; later lines are not evidence. Unknown event
types are ignored. A line whose `ref` names another request is ignored. A line
that does not parse is skipped. `text` and `error` are at most 256 KiB, cut on
a UTF-8 boundary.

The extension is the single writer of a ref's stream. Before every append it
checks the last byte of the file: a trailing fragment without a newline (a
crash or a failed write mid-append) is closed with a newline first, so the
fragment stays one skipped line and the new line always parses. The extension
never truncates or rewrites the stream. An append counts only after the line
is written in full and fsynced. A terminal outcome that fails to append is kept
in memory and appended again on later polls, so a skipped fragment is never
the only terminal evidence while the extension runs.

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
  "session_generation": "3f0c...",
  "bridge_revision": 3
}
```

The extension replaces the whole file (temporary file plus rename) every 2
seconds. Freshness is the file's modification time: a record older than 5
seconds is stale. The record is live only when it is fresh, parses, carries
this protocol, and has `live: true` and `pid > 0`. `session_generation` is the
current generation; the adapter addresses first-contact requests to it. On shutdown the extension
writes `live: false`, so the adapter sees the bridge offline at once.
`surface` is the pi run mode (`tui`, `rpc`, `json`, or `print`).
`bridge_revision` is the extension's implementation revision. An extension
may also publish `upgrade`, its own remedy for an owner whose bridge is too
old (for example the command that updates the pi build that ships it). The
adapter shows it, bounded, in the refusal instead of this repository's install
line. The record may also carry `session_id` (see
[Session identity](#session-identity)); from revision 4 it must.

## Bridge revision

The protocol string names the file formats; `bridge_revision` names the
extension rules the adapter relies on. Revision 3 is the rule set of every
section except [Bridge revision 4](#bridge-revision-4-tool-approval): exact
generation addressing, busy refusal at the admission boundary, request ownership that ends at the next user message and completes
only on a final answer, retained terminal and refusal decisions, and orphan
recovery that retries until it succeeds. The protocol string stays
`amq:pi-bridge:v1`.

Revision 2 is withdrawn: its implementation completed a request with a
tool-use preamble when a user message ended ownership. Adapters and
extensions treat revision 2 like a missing revision.

The fence holds in both directions, and a missing field is revision 0, never
"compatible":

- **Old extension, new adapter.** The adapter sends new requests only to a
  live bridge whose `bridge_revision` is at least 3. Below the minimum,
  `Inspect` advertises no submit and `Submit` refuses `unsupported` with the
  fix: install the extension from this repository at the adapter's release
  tag (`pi install git:github.com/avivsinai/agent-message-queue@v<version>`)
  and reload the pi session.
- **Old adapter, new extension.** Every request carries the
  `bridge_revision` its adapter requires. The extension refuses a request
  without it, or outside the revisions it implements, with a `refused` event
  of reason `revision` and no receipt, on every fresh request and after
  every session switch. A current adapter maps the reason to `unsupported`.
  An adapter that predates the fence cannot: it reports the submission
  uncertain and then `native_error`, with no upgrade text. The extension
  therefore shows a notice in pi, once per extension runtime, that the
  `amq-remote` on the machine is older than the extension and must be
  upgraded and restarted.

A request's `bridge_revision` is the minimum rule set the adapter relies on
for that request. The adapter sends 3 on every request, also to a bridge of a
later revision. A bridge implements every revision from 3 up to the one it
advertises, so a revision-4 bridge accepts a request that carries 3 or 4. It
refuses with reason `revision` only a request with no `bridge_revision`, one
below 3, or one above the revision it advertises. The approval capability
comes from liveness alone: the adapter advertises `approve_tool` only while
the live record's `bridge_revision` is 4 or higher.

A rollout installs the extension, reloads every pi session, and restarts
every running `amq-remote` process. The revision gates only new
submissions: the adapter reads receipts and events already on disk from any
revision.

## Bridge revision 4: tool approval

Revision 4 is revision 3 plus a required session identity in liveness and one seam:
the extension raises a tool approval under a ref it owns, and the adapter
answers it from a remote face. Nothing in
revision 3 changes, and the protocol string stays `amq:pi-bridge:v1`.

A revision-4 extension must hold a tool call until a decision arrives from a
local face or from `answers/`, and apply the first one. Stock pi exposes no
approval that an extension can answer from outside, so the reference extension
in this repository is revision 3.

### Session identity

A liveness record may carry `session_id`, and a revision-4 record must: pi's
own session id, from
`ctx.sessionManager.getSessionId()`. It stays the same across a reload and a
compaction, and it changes on a new session and on a fork. It matches
`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`, at most 128 characters. The
adapter reports it as the target's native session id, which a relay share
pins; without a live record carrying a valid `session_id` the adapter reports
none, and a share that needs one is refused. The reference extension in this
repository publishes it.

### Interaction lines

Two more event lines go in `events/<ref>.jsonl`. The extension writes them only
while it owns the ref: after the receipt and before the ref's terminal event.

```json
{"protocol":"amq:pi-bridge:v1","ref":"amqr1_...","event":"interaction","at":"...","interaction_id":"tool-1","kind":"approval","prompt":"run: go test ./...","options":["Allow once","Block"],"approve_option":"Allow once","reject_option":"Block","manifest_hash":"sha256:...","expires_at":"2026-01-01T00:05:00Z","presence":"remote"}
{"protocol":"amq:pi-bridge:v1","ref":"amqr1_...","event":"interaction_resolved","at":"...","interaction_id":"tool-1","outcome":"answered","option":"Allow once"}
```

| `interaction` field | Meaning |
| --- | --- |
| `interaction_id` | The approval's id: 1 to 128 of `[A-Za-z0-9._-]`. Case is significant. |
| `kind` | `approval`. |
| `prompt` | What the tool call does, as the owner sees it. |
| `options` | The options the extension accepts from a remote face. |
| `approve_option`, `reject_option` | The members of `options` that approve and reject the call. |
| `manifest_hash` | An opaque hash that names exactly this tool call. |
| `expires_at` | RFC 3339. The extension applies no answer at or after it. |
| `presence` | `remote` when a remote answer may approve; `local_only` when only a local face may approve. |

The adapter shows one approval per ref at a time, the oldest open one first.
It offers the remote face only `approve_option` and `reject_option`. It offers
the approve option only when `presence` is `remote`, `prompt` is at most 2000
bytes, and `options` contains `approve_option`; otherwise it offers
`reject_option` alone and cuts `prompt` to 2000 bytes. It marks the approval
answerable from a remote face only while a remote answer can apply: a live
bridge of revision 4 or later, in the session generation of the ref's
receipt, before `expires_at`. Otherwise the approval shows with no remote
answer, and the adapter updates it when that changes. Any other `presence`
reads as `local_only`. The adapter ignores a line with an invalid
`interaction_id`, a second line for an id it already saw, and a line without
`kind: approval`, a `manifest_hash`, a parseable `expires_at`, or a
`reject_option` in `options`; the local face then answers that approval.

`interaction_resolved` closes one open approval. `option` names the option that
applied, when one did.

| `outcome` | Meaning | Adapter |
| --- | --- | --- |
| `answered` | The answer from `answers/` applied. | resolved, answered from AMQ |
| `answered_elsewhere` | A local face, a timeout, or a refusal of the answer file resolved it. | resolved elsewhere |
| `run_ended` | The ref reached its terminal event with the approval open. | run ended |

A terminal event, `uncertain` included, closes every open approval of its ref
as `run_ended`. Neither line is terminal evidence: the first-terminal rule
ignores both, and a ref with an open approval is still running.

The open approval and how each approval ended, with its exact outcome and
option, are evidence the adapter rebuilds from the event stream, on attach and
on every lookup, like every other state. The adapter also publishes each
change at once, but a published change can be lost, for example at shutdown;
the next reconcile recovers it from the evidence. A reported outcome is
recorded as given: an answer AMQ sent never turns `answered_elsewhere` or
`run_ended` into `answered`.

### `answers/<hash>.json`

`<hash>` is the lowercase hex SHA-256 of the ref, one NUL byte, and the
`interaction_id`. The name is bounded and does not depend on case, so two
interaction ids that differ only in case never share a file. The record keeps
the ref and the interaction id.

```json
{
  "protocol": "amq:pi-bridge:v1",
  "ref": "amqr1_...",
  "interaction_id": "tool-1",
  "manifest_hash": "sha256:...",
  "option": "Allow once",
  "at": "2026-01-01T00:01:00Z"
}
```

| Field | Meaning |
| --- | --- |
| `ref`, `interaction_id`, `manifest_hash` | Copied from the `interaction` line. |
| `option` | The answer: one of the options the adapter offered. |
| `subject` | Optional and reserved. Revision 4 never checks it, and the adapter does not write it. |
| `at` | RFC 3339 answer time; informational. |

An answer file already on disk with the same `protocol`, `ref`,
`interaction_id`, `manifest_hash` and `option` is delivered: a replay of it
succeeds whatever the bridge state is now, and whether it applied is for
`interaction_resolved` to say. For a fresh answer the adapter writes only for
the approval it shows for that ref, only with an offered option, and only
while a remote answer can apply. It refuses otherwise and writes nothing:
`attachment_lost` with no live bridge, `already_resolved` below revision 4, in
another session generation, or for an approval that is not the one shown,
`invalid` for an option not offered, and `expired` at or after `expires_at`. It
publishes create-new, with the same steps as a request, and never rewrites an
answer: any other answer finds the first one in place and is
`already_resolved`. First-answer-wins across remote faces belongs to the
`amq-remote` endpoint; the extension decides between the remote answer and the
local faces.

### Binding an answer to one call

An allow is single use, bound to `interaction_id` + `manifest_hash`, valid only
before `expires_at`. The extension applies an answer file only when all of
these hold:

- the ref is one it owns, and the approval is open under it;
- `interaction_id` and `manifest_hash` equal the `interaction` line's;
- the current time is before `expires_at`;
- `option` is one of the options the line offered to a remote face.

An allow grants that one tool call, never a wider scope. The extension applies
the first decision from any face and reports the result with
`interaction_resolved`. It never deletes or rewrites an answer file.

## Session generation and epoch

A session generation is an opaque token (`^[A-Za-z0-9_.:-]{1,128}$`) that
names one pi session runtime. The extension mints a fresh generation on every
`session_start`: startup, resume, new session, reload, and fork each start a
new extension runtime. It writes liveness immediately with the new
generation.

The adapter's epoch rules:

1. **Only receipts pin.** The first receipt pins its `session_generation`,
   and the adapter then publishes it as the epoch. On restart the adapter
   rebuilds from `receipts/` oldest first, so the newest receipt sets the
   pin.
2. **Liveness addresses and may drop a pin, never set one.** Without a pin
   the adapter publishes the address epoch `unpinned.<generation>` for the
   live record's `session_generation`, or the sentinel `unpinned` when no
   live generation is known. A first-contact submit sends the generation
   its caller's address epoch names as `epoch_hint`, never a generation read
   later; when liveness shows another generation at submit time, the submit
   is refused `stale_epoch` and nothing is published. The sentinel addresses
   no generation, so a submit under it is refused `stale_epoch`, including a
   request deferred while the bridge was offline. The address names the
   session the request is for; it is not admission evidence and does not
   pin. A live record whose `session_generation` differs from the pin, or
   whose `pid` differs from the pinning receipt's `pid`, drops the pin.
3. **A hint for another generation is refused.** The extension delivers a
   request only when its `epoch_hint` equals the current generation. An
   absent, empty, or different hint gets a `refused` event with reason
   `generation` and **no receipt**, so a request retained across
   `session_start` never runs in a session it was not sent to. The adapter
   maps the refusal to `stale_epoch` and drops its pin for a request it
   submitted; the next delivered request's receipt pins the live generation.

## Extension processing rules

The extension polls `requests/` often enough that a receipt lands well inside
the adapter's 2-second receipt wait. For each request with neither a receipt
nor a terminal event, in publication order, it:

1. validates the shape: `ref` matches the file name, `text` is non-empty,
   `deliver_as` is `followUp`, and `not_after` parses. A failure is a `refused`
   event (`invalid` or `unsupported`). A missing or unsupported
   `bridge_revision` is a `refused` event with reason `revision`.
2. refuses with reason `expired` when `not_after` has passed.
3. applies the epoch rule above.
4. refuses with reason `busy` when pi is busy: `ctx.isIdle()` is false,
   `ctx.hasPendingMessages()` is true, or another remote request is in
   flight. v1 never queues a request behind other work.
5. claims the receipt and calls `sendUserMessage(text, { deliverAs: "followUp" })`
   without prompt-template expansion, so a body that starts with `/` stays
   text.

Steps 2 to 5 run in one synchronous pass, so `not_after` and idleness are
checked at the admission boundary, immediately before the receipt. A request
that already has a receipt or a terminal event is handled and is never
examined or delivered again. Refusals write no receipt. A refusal is final for
its ref: when the append fails, the extension keeps that refusal and appends
it again on later polls, and never examines the ref again. A failed append
stays pending even when its line is readable, because a line whose fsync
failed is not durable.

The reference extension keeps one delivered request in flight and correlates
its run like this:

- the request starts when pi emits `message_start` for a user message whose
  text equals the request text, after the delivery. The extension appends
  `started`. Output and outcome collected before that point are discarded;
- the final assistant text from `message_end` and `agent_end` after the start;
- the outcome from `agent_before_settle` after the start (`completed`,
  `aborted`, `error`);
- the terminal event at `agent_settled` after the start, the notification that
  pi will not continue on its own: `completed`, `cancelled` for `aborted`, or
  `failed` for `error`. A settle before the start belongs to other work and
  never ends the request;
- the end of ownership at the next user `message_start` after the start, such
  as a local follow-up or local steering between tool turns. The request then
  ends at once. It is `completed` only when its last assistant message is a
  final answer: `stopReason` is `stop`, the message has no `toolCall`
  content, it carries text, and the run recorded no error. Otherwise it is
  `uncertain`; tool-use preamble text is never a final answer. Output after
  that point never belongs to the request.

The request stays in flight until its terminal event is durable.

pi reports a failed `sendUserMessage` out of band, so a follow-up can be lost
without a signal. When the in-flight request has not started 5 seconds after
delivery and pi is idle with no queued messages, the extension appends
`uncertain`: nothing proves the follow-up did not run.

On `session_shutdown` with a request in flight, the extension appends the
outcome it already decided; otherwise `failed` naming the shutdown reason when
the request started, or `uncertain` when it did not. On `session_start` it
appends `uncertain` to every receipt from another generation that has no
terminal event, so a crash never leaves a ref running forever and never
records an outcome nobody observed. A receipt it cannot read or close is
retried on later polls, and a closing line whose append failed is appended
again even when it is readable.

## Adapter recovery and evidence

The adapter keeps no durable state of its own; the seam is the record. On
attach it rebuilds every receipt-backed ref from `receipts/` and `events/`,
and every ref without a receipt whose first terminal event is `refused`, and
never redispatches. A lookup of an unseen ref reads the same files.

| Seam state for a ref | Adapter answer |
| --- | --- |
| Nothing retained (no request known, no receipt) | unknown. pi cannot prove non-admission, so the adapter never reports "none" for it. |
| Request published, no receipt, live bridge | uncertain. The extension may still deliver it. |
| Request published, no receipt, no live bridge | `attachment_lost`, uncertain. The file stays for a later bridge. |
| Receipt, no terminal event | admitted, confirmed running. |
| Terminal `completed`, `failed`, or `cancelled` | admitted, terminal, with the result. |
| Terminal `uncertain` | uncertain. The receipt proves admission; the outcome is unknown. |
| `refused` | rejected with the mapped code; admitted only if a receipt exists. `Submit` returns a refusal it observes during its receipt wait at once. |
| Unreadable or foreign receipt, or refused event stream | error for that ref; the adapter keeps the record uncertain. |

`Inspect` advertises submit evidence `submitted` and completion evidence
`run_terminal`. The adapter never advertises `admitted` for the session:
admission is proven per ref by its receipt, not by a session capability.

## Capabilities

v1 supports inspect and submit with `deliver_as: followUp` only, and tool
approval through `answers/` from bridge revision 4. Steering, exact
cancellation, question answers, and terminal access are not part of the
protocol; the adapter refuses a steer submit before it writes anything.
