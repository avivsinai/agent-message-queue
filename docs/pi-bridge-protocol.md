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
extension stays inactive. A host application that embeds the extension may
supply the root and handle itself (see [Embedding](#embedding)). The adapter
takes the handle from its manifest entry and never from its own environment.

```text
<AM_ROOT>/agents/<handle>/extensions/pi-bridge/
  requests/<ref>.json    adapter -> extension: one request per ref
  receipts/<ref>.json    extension -> adapter: admission proof
  events/<ref>.jsonl     extension -> adapter: lifecycle and terminal evidence
  cancels/<ref>.json     adapter -> extension: cancel of one request (revision 5)
  activity/<session_id>.jsonl  extension -> adapter: whole-session activity
  bridge.liveness        extension -> adapter: heartbeat
  bridge.owner           extension only: the one bridge that serves the mailbox
```

Directories are mode 0700 and files are mode 0600.

`<ref>` is the request reference: `amqr1_` followed by lowercase base32 without
padding (`^amqr1_[a-z2-7]{16,472}$`). It encodes the request key and is already
a safe file name, so the file name is the ref. A reader derives the ref from
the file name and ignores every name that does not match, including the
adapter's temporary files.

## Ownership

The adapter writes only `requests/` and `cancels/`. The extension writes only
`receipts/`, `events/`, `activity/`, `bridge.liveness`, and `bridge.owner`.
Neither side deletes or rewrites a file the other side owns. The extension
never deletes or rewrites a request; the adapter never deletes a request it
published. Cleanup of old files is an explicit operator action outside this
protocol.

One extension instance serves a mailbox. Before it writes liveness or closes
orphaned receipts, an instance creates `bridge.owner` create-new (temporary
file, hard link) with its `pid`, `session_generation`, and, where the platform
gives one, its process start token `start`. When the owner is live, the
instance stays inactive for that session and tells the owner once in pi; a
second pi started with the same `AM_ROOT` and `AM_ME`, for example from a pi
tool, never touches the first bridge's requests or liveness. An owner is live
while its pid exists and runs the process that wrote the record (same start
token where one is recorded). A stalled owner stays the owner: stale liveness
never transfers ownership. A fresh `live: true` liveness record from a live
process whose generation has no owner record is an older extension, and it
counts as a live owner too. The record of an owner that is not live is renamed
aside and replaced only when the renamed record is the one judged dead, so two
starters never both own; an instance that loses that race says so in its own
notice. An owner whose record is missing links it again create-new. An
instance whose record names another generation stops at once and tells its pi
once that it no longer serves Buzz requests. On shutdown the owner writes
`live: false` and then removes its own record; it never writes over or
removes another owner's. The adapter does not read `bridge.owner`.

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
| `origin` | Optional: where the task came from, `{"carrier": "link", "name": "example"}`. `name` is the carrier's own name for the source that sent the task (for a link, the link's name in the AMQ manifest); `carrier` is the AMQ carrier that received it. Both are non-empty, and `name` is at most 64 bytes. The adapter writes `origin` only when both hold, and otherwise leaves the whole object out, so a local, mailbox or Buzz request has none. The extension may show "from <name>"; it is display text, never authority. |
| `event` | Optional: `nostr:<eventID>`, where `<eventID>` is the lowercase hex id of the owner's Buzz event that caused the request. The adapter writes it only for a request from the Buzz carrier, and the value is the same as the `nostr:<eventID>` label on a Buzz DM's AMQ prompt. See [The Buzz event](#the-buzz-event). |

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

### The Buzz event

A request's `event` names the owner's Buzz DM that caused it. The adapter
writes it only for the Buzz carrier, with the value `nostr:<eventID>`. It is
a pointer, not proof. The adapter never holds the signed event, and any local
process that can write `requests/` can write any value. The adapter never
adds the id to `text`, and an extension keeps it out of the model's context,
so a model cannot forge it as routing. An extension that ignores the field
works as before, and a record without it is valid.

A layer that must know the owner wrote the request fetches that event from
the relay and checks it itself. AMQ verifies nothing for the layer.

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
| `cancelled` | `cancelled_before_admission` (revision 5) |
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

## `activity/<session_id>.jsonl`

The extension records the whole pi session, not only remote requests, for the
relay's read-only session panel and the session mirror. `<session_id>` is pi's
own session id. The extension creates the file at its first record. One JSON
object per line: `protocol`, `seq`, `at`, `session` (the same session id),
`turn`, `kind`, and optional `run`, `id`, `text`, `tool`, `status`. `kind` is
`turn_start`, `turn_end`, `user`, `assistant`, `tool_start`, `tool_end`,
`run_start`, or `settled`. Messages are whole, never deltas. `text` is at most
48000 bytes; a longer tool result is cut and ends with `[truncated]`. Readers
skip a kind they do not know.

Activity version 2 adds runs. A run is one prompt: pi's `turn_start` and
`turn_end` mark one model round each, and `agent_end` can repeat inside one
prompt when pi retries; `agent_settled` comes once, when nothing more will
run.

- The first `agent_start` after a settle opens a run. Its id is
  `<first 8 characters of the session generation>-r<n>`, and every record
  from then until the run's `settled` record, inclusive, carries it as `run`.
  `agent_start` inside an open run opens nothing.
- `run_start` is written when the run opens.
- `settled` is written at `agent_settled`. Its `status` is `aborted` when the
  event reports an abort, else the outcome of the run's last
  `agent_before_settle` (`completed`, `aborted`, or `error`), else `error`.
- The `user` record of a request's own user message carries the request's
  `ref` as `id`.
- Past 4 MiB, the extension writes `run_start` after it replaces the file
  (a temporary file plus rename) with the records of the run before it, so a
  reader that has not read that run's `settled` record still finds it. A run
  never spans this replacement; past 16 MiB the extension truncates the file
  at any record.

The adapter tails the file from its end when it starts observing, reads it
only as a regular file in a plain `activity/` directory (never through a
symlink), skips lines that do not parse, carry another protocol, or name
another session, and never writes it. A bridge
without this file simply exports no activity. The stream is evidence of
nothing: it never decides a request outcome.

## `bridge.liveness`

```json
{
  "protocol": "amq:pi-bridge:v1",
  "live": true,
  "at": "2026-01-01T00:00:00.000Z",
  "pid": 4242,
  "surface": "tui",
  "session_generation": "3f0c...",
  "bridge_revision": 4,
  "session_id": "0199c0de-0000-7000-8000-000000000000",
  "activity": 2
}
```

The extension replaces the whole file (temporary file plus rename) every 2
seconds. Freshness is the file's modification time: a record older than 5
seconds is stale. The record is live only when it is fresh, parses, carries
this protocol, and has `live: true` and `pid > 0`. `session_generation` is the
current generation; the adapter addresses first-contact requests to it. Only the
owner (see [Ownership](#ownership)) writes the record. On shutdown it writes
`live: false`, so the adapter sees the bridge offline at once.
`surface` is the pi run mode (`tui`, `rpc`, `json`, or `print`).
`bridge_revision` is the extension's implementation revision. An extension
may also publish `upgrade`, its own remedy for an owner whose bridge is too
old (for example the command that updates the pi build that ships it). The
adapter shows it, bounded, in the refusal instead of this repository's install
line. The record may also carry `session_id` (see
[session identity](#bridge-revision-4-session-identity)); from revision 4 it must.
`activity` is the version of the activity records the extension writes; a
record without it writes version 1, which has no runs. The activity version is
separate from `bridge_revision`, which names the request rules.

## Bridge revision

The protocol string names the file formats; `bridge_revision` names the
extension rules the adapter relies on. Revision 3 is the rule set of every
section except [Bridge revision 4](#bridge-revision-4-session-identity): exact
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
  fix: load the extension that the adapter's `amq-remote` carries
  (`pi -e "$(amq-remote pi bridge-path)"`) and reload the pi session.
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
below 3, or one above the revision it advertises.

A rollout installs the extension, reloads every pi session, and restarts
every running `amq-remote` process. The revision gates only new
submissions: the adapter reads receipts and events already on disk from any
revision.

## Bridge revision 4: session identity

Revision 4 is revision 3 plus a required session identity in liveness.
Nothing in revision 3 changes, and the protocol string stays
`amq:pi-bridge:v1`.

A liveness record may carry `session_id`, and a revision-4 record must: pi's
own session id, from
`ctx.sessionManager.getSessionId()`. It stays the same across a reload and a
compaction, and it changes on a new session and on a fork. It matches
`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`, at most 128 characters. The
adapter reports it as the target's native session id, which a relay share
pins; without a live record carrying a valid `session_id` the adapter reports
none, and a share that needs one is refused. The reference extension in this
repository publishes it.

## Bridge revision 5: exact cancel

Revision 5 is revision 4 plus one seam: the adapter cancels one request it
published, and the extension stops it only if it is that request.

```json
{"protocol":"amq:pi-bridge:v1","ref":"amqr1_...","at":"2026-01-01T00:01:00Z"}
```

The adapter writes `cancels/<ref>.json` create-new, with the same steps as a
request, only while a live bridge advertises revision 5 or later, and never
rewrites it. A cancel already on disk is delivered. The adapter advertises
`cancel_request` only while the live record's `bridge_revision` is 5 or
higher.

The extension applies a cancel file with this `protocol` and `ref`:

- for a request with no receipt and no terminal event: a `refused` event with
  reason `cancelled` and no receipt, so the request never runs;
- for the request in flight: the native cancel of that request, after the
  extension has checked, in the same step, that the ref is the request it is
  running. The outcome is the request's usual terminal event, `cancelled`
  when the cancel took effect;
- for a request that already has a terminal event: nothing.

A cancel never reaches any other request or a local turn.

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
the adapter's 2-second receipt wait. It lists the directory only when the
directory changed, and at least every 5 minutes, so an idle poll does not grow
with the retained requests. For each request with neither a receipt
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
the request started, or `uncertain` when it did not. On `session_start`, once
it owns the mailbox, it appends `uncertain` to every receipt from another
generation that has no terminal event; ownership proves that generation's
bridge is gone, so a crash never leaves a ref running forever and never
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

## The acp-host wire

`amq-acp host` implements the extension side of this protocol for one ACP
agent, so `amq-remote` attaches to it as target kind `acp`. Everything above
applies with these differences:

- The directory is `<AM_ROOT>/agents/<handle>/extensions/acp-host/`, which
  the host shares with its own state (`host.lock`, `session.json`,
  `turns/`). The protocol string is `amq:acp-host:v1`.
- Liveness carries `surface: "acp"`, `bridge_revision` 5, `session_id` (the
  ACP `sessionId`, when it matches the session identity grammar), and
  `agent` (the agent's name and version from `initialize`). The host mints a
  new `session_generation` each time it starts and opens or loads its ACP
  session.
- The host is idle when no turn is in flight and its AMQ inbox holds no
  message. Only then does it run a request; otherwise it refuses `busy`. A
  request published while any turn is in flight is refused `busy` within a
  second, and at the latest when that turn ends, and never runs. Peer
  messages keep their order in the inbox; a request is never queued.
- The receipt and the `started` event are written immediately before the
  `session/prompt` bytes, so `started` needs no text matching. The request
  `text` is the prompt, verbatim. `completed` carries the turn's
  `agent_message_chunk` text after stop reason `end_turn`; stop reason
  `cancelled` is `cancelled`; every other stop reason, a JSON-RPC error, and
  an agent that exits are `failed`.
- On start the host appends `uncertain` to every receipt without a terminal
  event.
- The host ignores a request's `event`.
- Each `session/request_permission` writes a dialog request for Buzz (see
  [approvals from Buzz](amq-remote-approvals.md)) with the options the agent
  offered. The first of the owner's answer in Buzz and the terminal decides,
  and the agent receives the chosen option. A cancelled turn answers a
  pending approval `cancelled`; the terminal event closes it. An approval
  with no offered option is answered `cancelled`.
- A cancel file for the request in flight sends ACP `session/cancel`. The
  host checks the cancel file within a second, in the loop that runs that
  turn, so the cancel is for that turn only. It answers a pending approval
  `cancelled` and waits 30 seconds for the prompt response, and starts no
  other turn meanwhile. The `session/cancel` write runs on its own, so an
  agent that stopped reading its stdin does not hold the turn past those 30
  seconds; the host then records `failed` and exits.

## Embedding

A host application can load the reference extension unmodified and call
`registerAmqBridge(pi, options)` instead of its default export. It loads the
file that `amq-remote pi bridge-path` prints: the extension built into the
installed `amq-remote`, so the bridge and the adapter come from one release. Every option
is optional; without options the extension behaves as described above.

| Option | Default | Host value |
| --- | --- | --- |
| `identity(ctx)` | `AM_ROOT` and `AM_ME` | `{root, handle}` for the session, or `undefined` to stay inactive. `root` must be absolute; any other value is no identity. `ctx` is `undefined` before the session starts. The extension resolves the identity once per session, at its start, and keeps that result, `undefined` included, until the session ends. The same identity places the bridge directory, the wake marker, and the Buzz dialog. |
| `deliver.busy(ctx)` | `ctx.isIdle()` is false or `ctx.hasPendingMessages()` is true | `true` when a request would wait behind the host's own work. The extension then refuses it `busy`. A throw counts as busy. |
| `deliver.send(ctx, request, report)` | `sendUserMessage(text, { deliverAs: "followUp" })` with the correlation above | Hands `{ref, text, notAfter, origin}` to the host. `origin` is present only when the request carries a valid one. |
| `upgrade` | none | The liveness `upgrade` remedy. |

The extension claims the receipt immediately before it calls `send`, in the
same synchronous step, so delivery stays at-most-once. The host then owns the
request's correlation: it reports `{event: "started"}` when it admits the
request into a run, and then exactly one terminal outcome: `completed` with
`text`, `failed` or `uncertain` with `error`, `cancelled`, or `refused` with
reason `expired`. The extension writes these as the request's events, applies
no correlation and no start watchdog of its own to that request, and ignores
a report for a request that is no longer in flight. A terminal report of any
other shape settles the request `uncertain`, with an error that names what the
host sent. A throw from `send` after a terminal report changes nothing; after
`started` it settles the request `uncertain`, and before it `failed`. In
activity, the request's ref tags the first user message whose text is the
request's text, as without a host. So the host hands `request.text` to pi
verbatim: a changed text matches no user message, and the request's run is
mirrored as local work. The host must report a
terminal outcome: until it does, the request stays in flight and the
extension refuses every other request `busy`. `session_shutdown` closes it as
described above.

## Capabilities

v1 supports inspect and submit with `deliver_as: followUp` only, and exact
cancel through `cancels/` from bridge revision 5. Steering, question answers,
tool approvals, and terminal access are not part of the protocol; the adapter
refuses a steer submit before it writes anything. A tool approval is a
[dialog in Buzz](amq-remote-approvals.md).
