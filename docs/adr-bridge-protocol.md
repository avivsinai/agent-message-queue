# ADR: Companion bridge protocol

## Status

Accepted.

## Context

[Two-host fleets](adr-two-host-fleets.md) define identity: each host runs local
AMQ; cross-host mail is a companion, not Core. This ADR defines
the `amq-bridge` wire, transport, and threat model.

Each participating host keeps its queue state in a durable local path. A
companion or client wrapper is not a hosted plugin and must not turn Git,
inbound SSH, or process supervision into transport.

## Decision

### Companion, not Core

`amq-bridge` is a separate binary, like `amq-keepalive`. The `amq` binary
does not listen, dial, or interpret rendezvous URLs. Local apply reuses
existing Maildir `publishTmpNoReplace` on a stable transfer filename.

### Transport

The wire unit is the signed envelope below. Local apply is the same
`ApplyEnvelope` path in every hop.

The supported peer courier class is **initiator-driven peer exchange**. The
initiator is the only dialer: it starts a fixed, config-pinned peer-stdio
session and the responder answers. The session is duplex, so envelopes and
signed outcomes can move in both directions, but the responder does not
initiate this class. `amq` Core remains local and daemon-free; no socket is
added to it.

The initiator offers objects from `tx/<peer>/new` and
`status-tx/<peer>/new`. A receiver writes exact object bytes to a private
`tmp` file, fsyncs the file, and no-replace renames the file into
`rx/<peer>/new/<object_sha256>.envelope` (or the corresponding
`status-rx` path for an outcome). That durable rename is
`transport_accepted`. It is transport acceptance only: apply-watch later
verifies and applies the envelope, and the source does not retire its `tx`
object at this stage.

The manually operated **`amq-bridge apply-file`** path remains available for
recovery and file-based exchange. It verifies a complete hash-named regular
file published under `drop/new/` and uses the same local apply and receipt
rules; it is not a remote drain or a Maildir synchronizer.

HTTPS store-and-forward remains implemented as an optional courier class for
an operator-provided rendezvous. It is not the peer-stdio architecture. The
rendezvous is an opaque blob store with lease, retry,
backoff, and bounded batches; it never reads AMQ handles or Maildir state.
AMQ does not ship a hosted relay, and operators must not treat HTTPS
poll/push as the active peer exchange.

Not v1: git, Maildir sync, reverse tunnels, inbound SSH to the initiator, remote drain,
or sockets inside `amq`.

### Envelope

The wire unit is a versioned envelope. Required fields:

- `version`
- `transfer_id`
- `source_host` (Ed25519-authenticated host principal, not a claimed string)
- `source_handle` (attribution only)
- `dest_alias` (receiver-owned `<host>/<agent>`)
- `source_message_id`
- `thread_id` (opaque correlation)
- `payload_sha256`
- `key_generation`
- `signature` (hex encoding of a 64-byte Ed25519 signature)
- `payload_b64` (unpadded standard-base64 encoding of exact AMQ message bytes)

Unknown fields and the following names are rejected: paths, roots, argv, env,
executable names, endpoints, and remote session selectors.

The peer exchange emits **Envelope v2**: one compact JSON object,
fixed key order, unknown fields refused, and `payload_b64` decoded with raw
standard base64 (no padding or whitespace). The signature preimage is the
length-prefixed v2 field contract, not a re-serialized payload. The receiver
hashes the exact envelope file bytes for `envelope_sha256`, recomputes the
decoded `payload_sha256` and `transfer_id`, and verifies the signature before
any claimed identifier can become a path. Version 1 is not emitted by this
class.

The payload is an ordinary AMQ message. Project/job/session values inside it
are untrusted context, never routing keys.

### Receipts

Keep three layers distinct:

| State | Meaning |
| --- | --- |
| `transport_accepted` | The destination durably renamed the exact envelope bytes into `rx/<peer>/new/<object_sha256>.envelope`. It does not retire the source `tx` object. |
| `destination_maildir_committed` | `ApplyEnvelope` committed `xfer-<source_host>-<transfer_id>.md` into the destination Maildir. |
| `destination_rejected` | Named for outcome-table completeness only: NOT emitted by this protocol version. A terminal destination refusal (e.g. `transfer_conflict`) is reported to the operator via `PollResult.Refused` on every poll; recovery is a rendezvous-operator action (see Addendum 4's conflicted-transfer retirement). |
| consumer-local drain/start/complete | Optional; may stay on the consuming host. |

The source retires `tx/<peer>/new` only after a verified signed
`destination_maildir_committed` or `destination_rejected` outcome. A
`transport_accepted` outcome is diagnostic and never archives the source
object. Lost outcomes replay the same transfer and must not create a second
message. A same-key, different-digest replay is `transfer_conflict` and binds
the received envelope and payload digests in the rejection outcome.

Outcome objects travel through `status-tx/<peer>/new` and may move to
`status-tx/<peer>/returned` after a successful peer transfer. `sent/` is not
used by peer exchange; it remains an HTTPS-only archive.

### Authorization

For the optional HTTPS class, the rendezvous is an untrusted blob store. It
does not authenticate a host, and `--allow-source-host` is a routing allowlist,
not authentication. For peer exchange, the fixed helper configuration and
the Ed25519 signature provide the host binding. The receiving host verifies
the signature against its local trusted generation, then maps `dest_alias`
through its allowlist. Claimed handle, labels, prompt text, and remote paths
are not authority. Multiple seats on one host are one host principal until a
separate verified identity boundary proves otherwise.

Each queue root has these bridge identity files:

- `<root>/bridge/host-id` (mode `0600`): the local host alias. It must match
  `--source-host` for push and the host component of `--receive-alias` for
  poll.
- `<root>/bridge/identity` (mode `0600`): the local key generation and
  Ed25519 private seed.
- `<root>/bridge/trusted/<source_host>/<generation>` (mode `0600`): the
  trusted peer's Ed25519 public key for the named generation.

Bootstrap writes `host-id`. `amq-bridge identity init` then writes `identity`
for that host. Copy only the public key record to the peer's
`trusted/<source_host>/<generation>` path; never copy a private seed. During
rotation, keep the current and immediately previous generations as a bounded
two-generation overlap. Verify the generation named in each envelope. Remove
an old generation only after doctor reports zero in-flight objects naming it.
Do not accept a third generation as an implicit overlap or use a flat
`trusted/<source_host>` path for v2.

### Peer exchange layout and stages

The peer-exchange WAL is separate from the HTTPS spool:

```text
tx/<peer>/{tmp,new,committed,rejected}
status-tx/<peer>/{tmp,new,returned}
rx/<peer>/{tmp,new,done,quarantine}
status-rx/<peer>/{tmp,new,done,quarantine}
drop/{tmp,new}
```

The initiator and responder exchange only hash-named envelope and outcome
objects. `OFFER`/`WANT` inventory is limited to those courier trees;
the courier never reads Maildir, `applied/`, or `apply-journal/`. `STORED`
means that the kind-specific `rx` or `status-rx` sink is durable. It does not
mean apply, source archive, or consumer drain.

### Wire and implementation constraints

The following constraints are normative:

1. **Hash-named publish.** Create with `O_EXCL` in `tmp`, write, fsync, hash
   with SHA-256, and no-replace rename to `<64hex>.<kind>`, then fsync the
   directory. Maildir publication is `tmp` to no-replace
   `new/xfer-<source>-<transfer>.md`. Hash before the final filename.
2. **Bounds.** `max_payload_bytes=8MiB`, `max_object_bytes=12MiB`,
   `max_offered_bytes_per_session=16MiB`, and `max_offer_count=64`.
3. **Apply lock.** Acquire `flock` on
   `apply-locks/<source>/<transfer>.lock` before ledger, ACL, or journal
   work. `O_EXCL` is the compare-and-set for terminal `applied` only.
4. **Conflict binding.** A `transfer_conflict` binds the currently received
   envelope and payload digests. The source finds `tx` by
   `envelope_sha256`. Receipts are
   `bridge/receipts/<peer>/<envelope_sha256>.outcome`.
5. **Unique exchange.** OFFER and WANT are unique. PUT equals OFFER. Send
   one PUT per WANT and then `PUT_END`; missing PUT aborts.
6. **Durable cursor.** Persist the offer cursor. Process outcomes first, and
   advance the cursor on `STORED` or `not-WANT`.
7. **Signer binding.** The local `source_host` signs. Bind From, Id, and
   Thread (and To when present) from the AMQ payload. Resume the same digest;
   a different digest is `submit/failed`.
8. **Driver boundary.** There is no signed-bundle transport. A forced-command
   driver, when used, is local-exec only. `drop/` uses hash-named publication
   into `drop/new/`.

### Local client wrapper

v1 client-to-AMQ access is a fixed, audited local CLI wrapper. It is not a
general plugin or hosted remote MCP.
The wrapper must not take prompt-controlled roots, argv, env, or endpoints.

An operator UI on one host is not the peer host and does not become an AMQ
host principal merely because it is open. Multiple seats on one host remain
one host principal unless a separate, verified identity boundary is provided.

## Consequences

- Courier code implements this envelope and initiator-driven peer-stdio exchange;
  HTTPS poll/push remains optional. It does not add listeners to `amq`.
- `amq-bridge apply-file` is the manual/recovery path: it uses the same
  envelope and local apply rules without a public locker, but it is not the
  live peer-exchange class.
- The configured initiator is the only peer-exchange dialer, and destination apply remains the
  commit. Operators may provision a public HTTPS rendezvous, but AMQ does not
  ship a hosted relay as Core.
- Each peer is a normal AMQ install. Pin `AM_ROOT` in operator config, never in
  prompt text. Durable state belongs under a path that survives client close.

### Addendum 4 — transfer ledger (611.14, amended per review-827-r1)

Applies to the destination-apply path (courier `PollOnce` and
`amq-bridge apply-file`). Where Addendum 3 names an apply-lock path, this
addendum supersedes it: the per-transfer lock lives at
`bridge/transfer-ledger/<session>/locks/<source>-<transfer>.lock` (a `flock`
on Unix, `LockFileEx` over byte range [0,1) on Windows — blocking, exclusive,
both twins from the same helper), scoped by the destination-alias session
both callers derive identically.

**Ledger.** For each `(source_host, transfer_id)` an append-only JSONL file
`bridge/transfer-ledger/<session>/<source>-<transfer>.jsonl` (mode 0600)
records states `prepared` → `committed` | `rejected`, plus `uncertain`. The
first `prepared` record binds the transfer's payload digest immutably; a
different digest under the same key is `transfer_conflict` observed in the
outcome and never overwrites the binding or a committed winner. The intent
(`prepared`) is durable — file and directory chain — before `ApplyEnvelope`
runs. A crash before the `committed` append recovers from durable publication
evidence: a digest-matching artifact retained in `inbox/new`, `inbox/cur`, or
a DLQ envelope wrapping the original bytes promotes the record to `committed`
(replayed) without re-applying.

**Proven non-delivery is retryable (review-827-r1 P0, amended per
review-827-r2 P0).** An in-process apply failure — in the same process that
just wrote `prepared` — is recorded as a retryable `rejected` record (typed
`retryable` flag) naming the failure (`apply failed (retryable): ...`).
ApplyEnvelope is NOT all-or-nothing: a `*fsq.CommittedDurabilityError` means
the rename into `inbox/new` SUCCEEDED but the destination directory sync
failed — the publication is a FACT, yet its durability is UNPROVEN (a crash
can lose the unsynced rename). The ledger records the distinct
`published_durability_unknown` state with the error's `FinalPath`: never
re-applied (that would duplicate once the consumer drains new → cur), and
the destination receipt and ACK are WITHHELD until a later call verifies
digest-matching evidence AND the durability of that evidence's actual
carrier directory. Two further non-delivery classes are explicit:
an `os.ErrExist` whose existing bytes cannot be read is post-publication
ambiguity (`uncertain`, never retryable, never a definitive conflict), and a
collision whose destination PROVABLY already holds matching bytes is a
committed delivery even when the tmp cleanup fails (classified as
`CommittedDurabilityError` at the fsq boundary). Every other in-process
failure is, in this process's knowledge, a pre-publication proven
non-delivery. A retry of a retryable
record first consults durable publication evidence (new/cur/DLQ, same rule
as crash recovery) and promotes on a match; only with no evidence does it
re-arm a fresh durable `prepared` intent and re-apply — so a crash during the
retry resolves exactly like the first attempt, and a retry whose intent
append fails does not apply at all. A known non-delivery is never
reclassified as `uncertain`; only unknown history (no evidence, no in-process
knowledge) is `uncertain`, and `uncertain` refuses both the destination
receipt and the ACK.

**Terminal rejection is one answer.** `os.ErrExist` on a fresh key
(same-name artifact with different bytes) records a terminal `rejected`
(`transfer_conflict`) on the first refusal and reports the same terminal
outcome on every later attempt.

**Torn tail (review-827-r1 P1c, amended per review-827-r2).** An unparseable
tail line of the ledger file is a torn append over an intact prefix: the last
valid record governs the disposition, and a torn tail after a terminal record
cannot un-terminal it. A torn read with no valid record (or a conflicting
digest over a non-terminal prefix) remains fail-closed `uncertain`. When
crash recovery promotes a committed record over a torn tail, the promotion is
framed durably: a bare newline first terminates the torn fragment, then the
committed record lands on its own parseable line — appended JSON fused into
an unterminated fragment would be discarded by the reader with the artifact
it names.

**Courier batching (review-827-r1 P1b, amended per review-827-r2 P1).** A
refused transfer — `uncertain` history, terminal rejection, or conflict — is
skipped, not batch-fatal: the poll loop continues, envelopes behind it still
apply and ACK, and the refusal is reported in `PollResult.Refused`, which the
CLI serializes to the operator on every run alongside the ledger's
unresolved-transfer diagnostics.

**Conflicted-transfer retirement (review-827-r2 P1, corrected per codex
r2-r2 finding 4).** The `destination_rejected` row in the outcome table names
a stage this protocol does NOT implement: no receiver code emits it and no
source code consumes it, and a receiver-side redelivery loop cannot be
closed from the wire alone. The refused envelope STAYS in the receiver's
rendezvous queue and is re-reported on every poll (steady-state signal, not
an error). Removing the local source-side spool copy does NOT retire the
posted remote item — this protocol has no source-deletion notification. The
recovery is a rendezvous-operator action (clearing the conflicting envelope
from the rendezvous queue, or replacing it with a corrected payload), which
this wave does not automate and does not provide a command for; that is the
actual operator-managed recovery limit of this protocol version.

**Recovered receipts.** Evidence-promoted commits carry the retained
artifact's path as `committed_path` (a replayed receipt for a consumer-drained
transfer reports the drain surface — new/cur/DLQ — where the delivery was
verified).
