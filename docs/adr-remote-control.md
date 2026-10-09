# ADR: Remote control attaches to a running session

## Status

Accepted.

## Context

Coding harnesses (Codex, pi, Claude Code) run on a laptop under a
person's own terminal or app. Another client, a phone on Buzz or an agent on a
second host, wants to see what such a session does, give it work, follow that
work to its result, and cancel it. The first-party answers route through a
vendor relay, need an organization policy change, or require a specific
client. The open-source answers own the harness process or wrap its PTY,
and they cannot reach a session the user already started.

AMQ already owns durable local delivery, receipts, a bridge envelope, a wake
capability vector, and a session guard. It does not own execution, and its
core has no daemon and no socket.

## Decision

A companion binary, `amq-remote`, attaches to a session that is already
running and exposes it to another client. The companion follows these
invariants.

1. **Attach, never replace.** The harness keeps its launch parent, its
   terminal, its conversation store, and its own permissions. The companion
   is not a wrapper, a PTY proxy, a scheduler, or a restart supervisor.
   Removing the companion leaves the local session intact.
2. **Each operation names its medium.** The operations are Send (a prompt
   in), Answer (the result out), Watch (live events or the screen), Decide
   (approve or reject a pending tool call), and Stop (cancel the running
   turn). A medium is how the companion touches the session:
   - **message**: the agent reads an AMQ message and writes its own reply.
     It is cooperative, not a weaker native medium.
   - **native**: the harness's own transcript, protocol, hook, or extension
     API. It is preferred wherever it exists.
   - **terminal**: the screen of the harness's terminal and single keys.
     It is opt-in per shared session. It never carries Send. A key is sent
     once, before it expires, and only while the prompt region that a
     per-harness rule extracts still matches the capture the owner saw.
     A harness with no rule has no terminal Decide.

   Fallback goes only from native to terminal, and a surface always shows
   which medium an operation uses. No competing process is resumed on the
   same session file. An operation that no medium supports is advertised as
   unsupported.
3. **A request has an identity and a result; a terminal has a picture.**
   Every remote work item carries a caller-generated request id, an opaque
   target id, and a registration epoch. Its state is one of `received`,
   `dispatching`, `running`, `completed`, `failed`, `cancelled`, `rejected`,
   or `uncertain`. `uncertain` is a real state, never a fabricated outcome
   and never permission to dispatch again. Global idle is not completion.
   The terminal medium has no request identity, so a terminal Stop leaves
   the record `uncertain` until native evidence arrives.
4. **Exact cancellation.** Cancel names the original request; the native
   boundary compares the bound run before signalling. A cancel that arrives
   before its submit leaves a tombstone. A late cancel for one request never
   aborts another. A harness that offers only a session-wide abort does not
   advertise exact cancellation. Ownership extends to every remote carrier
   and every read path: a request that a remote source submitted (a Buzz
   share or a link) is read and cancelled only by that source, and the AMQ
   mailbox and the local socket are refused. A link sees only the requests
   it created and the sessions shared with it, with other sources' request
   and interaction references removed; it never answers an interaction and
   never reads session events. A link creates no tombstones: its cancel of
   an absent request is refused, and its busy request ends as an ordinary
   refused record that compacts and is never admitted again. A link holds
   at most four open requests.
5. **Capabilities are observed, not inferred.** Each attachment publishes a
   projection (`inspect`, `submit`, `cancel_request`, `answer_question`,
   `approve_tool`, `steer`, `terminal`). Values come from the installed
   runtime. Weaker capability is refused, not substituted, as in
   [the wake capability-vector ADR](adr-wake-capability-vector.md).
6. **One writer, atomic records.** Request state lives in one atomic JSON
   record per request under the companion's own state directory, written
   with the repository's durable-write primitives. One endpoint process per
   user per root holds a process lock; a second exits with a diagnostic.
7. **AMQ carries durable intent and evidence.** Commands and results travel
   as ordinary AMQ messages to a dedicated handle in the same root. Local
   CLI requests, AMQ-delivered requests, Buzz-delivered requests, and
   link-delivered requests (a server the machine itself dialed) end in the
   same handler. Live activity is a projection and never mutates.
8. **Exit codes follow the AMQ contract.** `0` success, `1` native work
   failed or cancelled, `2` usage, `3` not found, `4` timeout, `6` action
   required (busy, unsupported, unshared, expired, or
   uncertain), `130` reader interrupted. JSON output never changes a code.

### Settled contract details

- **A submit window is explicit and enforced natively.** `not_after` defaults
  to two minutes and is checked at the native admission boundary, not only on
  arrival. A request whose window closed while it waited is `expired`, which
  is action-required, never a silent dispatch.
- **Steering is influence, not ownership.** Remote steering is a separate
  operation that names the target request's ref and the turn it expects to
  be running. It never takes ownership of another caller's run and never
  rebinds that run's identity; if the expected turn is no longer current the
  operation is refused. `deliver=steer` stays disabled in v1 (below) until
  that ownership model is implemented.
- **`capabilities.submit` stays boolean.** Per-mode differences are published
  as a typed evidence projection rather than by splintering the capability
  into one flag per mode, and a caller states the minimum evidence class it
  will accept. A caller that needs `admitted` is refused by an adapter that
  can only prove `submitted`, instead of being silently given the weaker
  guarantee — the same "weaker capability is refused, not substituted" rule
  as invariant 5.
- **`busy=queue` is refused in v1.** Queuing needs a per-runtime reservation
  covering dispatching, unresolved and unacknowledged work, plus a
  `not_after` check at native admission for the queued item. Until both
  exist, a busy target is refused with `busy` (action-required) rather than
  admitting work whose ordering and expiry we cannot honour.
- **A decision uses the medium of its evidence.** The owner answers the
  exact interaction that was shown: a native approval by its interaction id,
  a terminal approval by the capture it was shown with. The harness applies
  the first answer it receives, from any side. Each interaction ends with one
  recorded resolution: `answered` (the answer the companion delivered),
  `answered_elsewhere` (the harness resolved it without that answer),
  `run_ended`, or `delivery_unknown` (the companion began to deliver an
  answer and cannot tell whether it arrived). The companion never reports an answer as applied when the
  harness resolved the interaction first. Only the owner's Buzz share that
  submitted the request may answer its interactions; the AMQ mailbox and the
  local socket are refused, so the asking agent cannot answer itself.
- **A link submit runs only from verified signed bytes.** The command a link
  carries is the one decoded from the bytes the owner signed, never a second
  copy beside them. The record keeps the signed native session, and core
  compares it with the attachment's native session at native admission, just
  before the handoff. A mismatch or an unknown session is refused
  `session_changed` and nothing runs.
- **A revision is published when its sink acknowledged it.** A carrier counts
  a revision as published only after the sink confirms that it committed that
  revision or a newer one. Until then the revision stays owed and the
  reconcile sweep offers it again.
- **Activity divergence is acceptable for a cache, never for execution
  truth.** The live activity projection may lag or differ from the harness's
  own view; it is a convenience. Admission, cancellation and completion are
  decided from native evidence and the durable record alone, and a
  divergence in the projection never authorises a dispatch or a cancel.

### Carriers

The default carrier is the AMQ mailbox. `/amq-remote` binds this session's
AMQ handle. A Buzz direct message arrives as an AMQ message from `buzz`, and
the agent's `amq reply` is the answer that `amq-acp` posts into that
DM. No wake and no Stop hook are required.

`--native` drives the exact native session through `amq-remote`. Claude needs
the Stop hook on that path.

One Buzz agent serves one session. A named binding selects it: the ACP model
id is `amq-remote:<name>`.

The relay below — `share`, a body key, and per-kind grants — is an advanced
path. It is not how a session is connected. The relay enforces the owner
signature and, when it implements NIP-OA time bounds, the grant expiry; it
does not enforce `kind=` conditions at admission, so per-kind limits are the
endpoint's own restraint.

The Desktop grant is broad and does not expire. Archiving the agent in
Desktop does not invalidate a copied key and grant; only removing its relay
access does. An owner-only audience also admits the owner's other agents.

### Advanced relay

`amq-bridge` gains a courier class, `buzz-relay`. The existing signed
envelope is sealed with NIP-44 v2 to the peer host's body key, gift-wrapped,
and published as a stored event on the Buzz relay the operator already runs.
Both hosts dial outbound and authenticate with NIP-42. Receipt vocabulary is
unchanged: `transport_accepted` is the relay acknowledgement,
`destination_maildir_committed` is the receiver's apply. The bridge envelope
and its routing authority do not change.

### Buzz surfaces

On the advanced relay, the companion holds one owned body key per shared
session or host. The human owner attests the key with a NIP-OA `auth` tag
signed from their own Buzz identity. Activity is published as NIP-AO kind 24200 observer frames.
Assistant text is an `acp_read` frame whose payload is a genuine ACP
`session/update` line, with projection provenance in that line's extension
metadata. `session_resolved`, `turn_started`, and `turn_completed` use the
Desktop lifecycle payloads. Buzz Desktop renders those kinds unchanged.
Requests and results are a DM thread between the body and the owner;
reactions carry typed commands. A pending approval is its own message in
that thread: ✅ on it approves and ❌ rejects, and the message is edited with
the resolution. No Buzz client change is required.

## Kill-list amendments

Unchanged: `amq` gains no socket or listener; AMQ never holds a human's Buzz
nsec; prompt text never selects an executable, root, argv, or environment.

Clarified: a companion may hold a body key that the human owner attests. The
body key signs the companion's own events only. The companion runs as a
separate process beside `amq`, as `amq-keepalive` and `amq-bridge` do.

## Consequences

- Codex is the strongest first adapter: the shared app-server daemon fans
  every notification out to every connection, `turn/start` returns a turn
  id, and `turn/interrupt` works from a second client. `turn/start` on a busy
  thread silently joins the running turn and drops the new text, so the
  adapter checks status inside the admission boundary and, for an idle
  `turn/start`, admits only after our own `userMessage` item (keyed by
  `clientUserMessageId`) confirms the text landed; an unconfirmed turn is
  refused, never admitted.
- pi attaches through the pi-bridge, a pi extension that owns the input
  boundary inside the pi process and exchanges request, receipt, and event
  files with the adapter. Because pi's `sendUserMessage` cannot report its
  own rejection, the pi adapter's evidence class is `submitted`, never
  `admitted`, and it leaves a record `uncertain` rather than `rejected` when
  it retains nothing.
- Claude Code has no supported exact-turn interrupt for an independently
  running interactive session. The mailbox carrier does not use one: the
  agent's reply is the answer. Native submit into Claude is live-verified;
  its completion needs the Stop hook; cancel is unsupported. See the
  [compatibility manifest](remote-compat.md). A session-wide abort is not
  exact cancellation.
- The request contract does not include native Buzz Desktop panels. Terminal
  Watch and Decide exist only where a harness advertises them (invariant 5).
- The Claude Code cross-session socket wire format is not published upstream;
  [the compatibility manifest](remote-compat.md) pins the observed format.
