# ADR: Remote control attaches to a running session

## Status

Accepted (sponsor direction, 2026-09-08). Implementation is tracked under
bead `agent-message-queue-611`.

## Date

2026-09-08

## Context

Coding harnesses (Codex, Amit on pi, Claude Code) run on a laptop under a
person's own terminal or app. Another client, a phone on Buzz or an agent on a
second host, wants to see what such a session does, give it work, follow that
work to its result, and cancel it. The first-party answers route through a
vendor relay, need an organization policy change, or require a specific
client. The open-source answers wrap the harness's PTY and break on every
release.

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
2. **Native seams only.** Observation uses the harness's own transcript,
   protocol, or extension API. Steering uses the harness's own steering API.
   No keystroke injection, no screen scraping, no competing process resumed
   on the same session file. Where a harness has no seam for an operation,
   the operation is advertised as unsupported.
3. **A request has an identity and a result; a terminal has a picture.**
   Every remote work item carries a caller-generated request id, an opaque
   target id, and a registration epoch. Its state is one of `received`,
   `dispatching`, `running`, `completed`, `failed`, `cancelled`, `rejected`,
   or `uncertain`. `uncertain` is a real state, never a fabricated outcome
   and never permission to dispatch again. Global idle is not completion.
4. **Exact cancellation.** Cancel names the original request; the native
   boundary compares the bound run before signalling. A cancel that arrives
   before its submit leaves a tombstone. A late cancel for one request never
   aborts another. A harness that offers only a session-wide abort does not
   advertise exact cancellation.
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
   CLI requests, AMQ-delivered requests, and Buzz-delivered requests end in
   the same handler. Live activity is a projection and never mutates.
8. **Exit codes follow the AMQ contract.** `0` success, `1` native work
   failed or cancelled, `2` usage, `3` not found, `4` timeout, `5` context
   mismatch, `6` action required (busy, unsupported, unshared, expired, or
   uncertain), `130` reader interrupted. JSON output never changes a code.

### Cross-host delivery

`amq-bridge` gains a courier class, `buzz-relay`. The existing signed
envelope is sealed with NIP-44 v2 to the peer host's body key, gift-wrapped,
and published as a stored event on the Buzz relay the operator already runs.
Both hosts dial outbound and authenticate with NIP-42. Receipt vocabulary is
unchanged: `transport_accepted` is the relay acknowledgement,
`destination_maildir_committed` is the receiver's apply. The bridge envelope
and its routing authority do not change.

### Buzz surfaces

The companion holds one owned body key per shared session or host. The human
owner attests the key with a NIP-OA `auth` tag signed from their own Buzz
identity. Activity is published as NIP-AO kind 24200 observer frames whose
payload is a genuine ACP JSON-RPC line, which Buzz Desktop renders unchanged.
Requests and results are a DM thread between the body and the owner;
reactions carry typed commands. No Buzz client change is required.

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
  thread returns the running turn and drops the new text, so the adapter
  checks status inside the admission boundary.
- Amit attaches through an extension and an owning-boundary shim in the Amit
  repository. Off-box export of Amit session content is gated by Amit's own
  privacy ruling.
- Claude Code advertises inspect and a weak-evidence submit only. It has no
  interrupt seam without keystrokes.
- Terminal viewing (a read-only tmux observer over an iroh stream) and native
  Buzz Desktop panels are deferred; the request contract does not depend on
  them.
- Known limitation: the Claude Code cross-session socket wire format is not
  published; the adapter pins what the installed binary accepts
  (`agent-message-queue-611.2`).
