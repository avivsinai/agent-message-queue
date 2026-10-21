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

Peer exchange carries [transfer record v3](#transfer-record-v3).
`amq-bridge sync --stdio` runs the initiator side and
`amq-bridge listen --stdio` the responder side. The initiator offers records
from its `bridge/tx/<peer>/` outbox; the receiver files each wanted record into
the destination Maildir and answers with a signed `STORED` receipt.

The manually operated **`amq-bridge apply-file`** path remains available for
recovery and file-based exchange. It verifies one complete envelope file
under `bridge/drop/` and uses the same local apply and receipt rules; it is not a remote drain or a Maildir synchronizer.

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

Keep the layers distinct:

| State | Meaning |
| --- | --- |
| `transport_accepted` | The HTTPS rendezvous accepted a pushed envelope. `PushOnce` then moves the spool item to `sent/`. It is not delivery evidence. |
| `destination_maildir_committed` | `ApplyEnvelope` committed `xfer-<source_host>-<transfer_id>.md` into the destination Maildir. The HTTPS poller (`PollOnce`) writes this receipt under `bridge/receipts/` and then acknowledges the rendezvous item (`ackEnvelope`); `amq-bridge apply-file` writes the same receipt. |
| `STORED` | A v3 receiver filed the record into its Maildir. The sender verifies the signed receipt and only then removes the record from `bridge/tx/<peer>/`. |
| `destination_rejected` | Reserved and not emitted. A terminal destination refusal (for example `transfer_conflict`) is reported to the operator through `PollResult.Refused` on every poll; see [Transfer ledger](#transfer-ledger). |
| consumer-local drain/start/complete | Optional; may stay on the consuming host. |

No destination outcome retires an envelope v2 object on the source host:
`sent/` holds what the rendezvous accepted, and a refused envelope stays in the
rendezvous queue. Lost outcomes replay the same transfer and must not create a
second message. A same-key, different-digest replay is `transfer_conflict` and
binds the received envelope and payload digests in the rejection outcome.

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
- `<root>/bridge/trusted/<source_host>` (mode `0600`): the trusted peer's
  Ed25519 public key for its single active generation (one flat file per
  source host; the reader is `bridge.TrustedPath`).

Bootstrap writes `host-id`. `amq-bridge identity init` then writes `identity`
for that host. Provision the peer's trust with
`amq-bridge trust add --host <source_host>` on the destination, fed the
peer's public key record (never a private seed). Rotation re-runs the same
command with `--replace`, which renames the new record over the file
atomically and refuses a generation downgrade. Generations are unsigned
decimal integers as `identity init` mints them and are compared
numerically (9 → 10 accepted, 10 → 9 refused); non-decimal labels have no
defined order and `--replace` refuses them rather than guessing. The file
holds one active
generation; envelopes naming an older generation are refused once it is
replaced. Verify the generation named in each envelope.
Do not use a per-generation `trusted/<source_host>/<generation>` directory
layout for v2.

### Wire and implementation constraints

The following constraints are normative:

1. **Maildir publication.** Apply writes to `tmp` and no-replace renames to
   `new/xfer-<source>-<transfer>.md`.
2. **Bounds.** Envelope v2 limits are `bridge.MaxPayloadBytes` and
   `bridge.MaxObjectBytes`; v3 offer pages are limited by
   `xferv3.MaxOfferRecords` and `xferv3.MaxOfferPageBytes`.
3. **Apply lock.** Acquire the per-transfer lock (see
   [Transfer ledger](#transfer-ledger)) before ledger, ACL, or journal work.
4. **Conflict binding.** A `transfer_conflict` binds the currently received
   envelope and payload digests. Receipts are
   `bridge/receipts/<transfer_id>__<stage>.json`.
5. **Durable cursor.** A v3 peer persists its offer cursor per peer.
6. **Signer binding.** The local `source_host` signs. From, Id, and Thread are
   bound from the AMQ payload.
7. **Driver boundary.** There is no signed-bundle transport. `amq-bridge
   apply-file` reads one complete envelope file under `bridge/drop/`.

### Local client wrapper

v1 client-to-AMQ access is a fixed, audited local CLI wrapper. It is not a
general plugin or hosted remote MCP.
The wrapper must not take prompt-controlled roots, argv, env, or endpoints.

An operator UI on one host is not the peer host and does not become an AMQ
host principal merely because it is open. Multiple seats on one host remain
one host principal unless a separate, verified identity boundary is provided.

### Transfer ledger

Destination apply (courier `PollOnce` and `amq-bridge apply-file`) runs under
a per-transfer lock at
`bridge/transfer-ledger/<session>/locks/<source>-<transfer>.lock`, scoped by
the destination-alias session both callers derive identically. It is a `flock`
on Unix and `LockFileEx` over byte range [0,1) on Windows (blocking,
exclusive, one shared helper).

For each `(source_host, transfer_id)` an append-only JSONL file
`bridge/transfer-ledger/<session>/<source>-<transfer>.jsonl` (mode 0600)
records the states `prepared` then `committed` or `rejected`, plus
`uncertain`. The first `prepared` record binds the payload digest
immutably. A different digest under the same key is `transfer_conflict`,
observed in the outcome; it never overwrites the binding or a committed
winner. The `prepared` intent is durable, file and directory chain, before
`ApplyEnvelope` runs. Directory durability uses `fsync` on Unix and a
directory flush on Windows; where a filesystem rejects the flush as
unsupported, durability degrades to the file-level fsync.

A crash before the `committed` append recovers from durable publication
evidence: a digest-matching artifact retained in `inbox/new`, `inbox/cur`, or
a DLQ envelope wrapping the original bytes promotes the record to `committed`
(replayed) without re-applying. Promotion carries the artifact's path as
`committed_path`; a replayed receipt for a consumer-drained transfer reports
the surface (new, cur, or DLQ) where the delivery was verified.

**Retryable non-delivery.** An in-process apply failure, in the process that
wrote `prepared`, is a retryable `rejected` record naming the failure. A retry
first consults publication evidence and promotes on a match; with no evidence
it re-arms a fresh durable `prepared` intent and re-applies. A retry whose
intent append fails does not apply.

**Apply is not all-or-nothing.** A `*fsq.CommittedDurabilityError` means the
rename into `inbox/new` succeeded but the destination directory sync failed.
The publication is a fact and its durability is unproven. The ledger records
`published_durability_unknown` with the error's `FinalPath`. That transfer is
never re-applied, and the destination receipt and ACK are withheld until a
later call verifies digest-matching evidence and the durability of the
evidence's carrier directory.

**Uncertain history.** An `os.ErrExist` whose existing bytes cannot be read is
post-publication ambiguity: `uncertain`, never retryable, never a definitive
conflict. A collision whose destination provably holds matching bytes is a
committed delivery even when tmp cleanup fails (classified as
`CommittedDurabilityError` at the fsq boundary). A known non-delivery is never
reclassified as `uncertain`; only unknown history (no evidence, no in-process
knowledge) is. `uncertain` refuses both the destination receipt and the ACK.

**Terminal rejection.** `os.ErrExist` on a fresh key (same-name artifact with
different bytes) records a terminal `rejected` (`transfer_conflict`) and
reports the same outcome on every later attempt.

**Torn tail.** An unparseable tail line is a torn append over an intact
prefix. The last valid record governs, and a torn tail after a terminal record
cannot un-terminal it. A torn read with no valid record, or a conflicting
digest over a non-terminal prefix, is fail-closed `uncertain`. When recovery
promotes `committed` over a torn tail, a bare newline first terminates the
fragment and the record lands on its own line.

**Courier batching.** A refused transfer (`uncertain` history, terminal
rejection, or conflict) is skipped, not batch-fatal. Envelopes behind it still
apply and ACK. The refusal appears in `PollResult.Refused`, which the CLI
serializes to the operator on every run with the ledger's unresolved-transfer
diagnostics.

**Conflicted-transfer retirement.** No receiver emits `destination_rejected`
and no source consumes it. The refused envelope stays in the receiver's
rendezvous queue and is re-reported on every poll. Removing the source-side
spool copy does not retire the posted remote item, because the protocol has no
source-deletion notification. Recovery is a rendezvous-operator action:
clear the conflicting envelope or replace it with a corrected payload. AMQ
provides no command for it.

### Transfer record v3

A v3 transfer is a sibling type in `internal/bridge/xferv3`; package `bridge`
does not import it, and Core `amq` stays socket-free. Envelope v2,
`apply-file`, the HTTPS spool, and the v2 transfer ledger stay.

The v3 record, id, signature, receipt, offer page, seed-to-X25519 derivation,
and Noise IK handshake are the layouts in
`internal/bridge/xferv3/testdata/vectors.json`. That file is the byte
authority. `TestVectors` in `internal/bridge/xferv3` fails if any vector
drifts; it runs with the package tests in CI.

The record is eight length-prefixed fields in this order: `amq-xfer-v3`,
source host, handle, dest alias (`host/agent`), source message id, thread id,
`payload_size` (8-byte big-endian uint64), and `payload_sha256` (32 raw
bytes). `id` is lowercase unpadded standard base32 of SHA-256 over those
bytes. The Ed25519 signature is over length-prefixed `amq-env-v3` and the
52-character id text. The signature sits beside the record, so a re-sign
keeps the id.

A receipt is length-prefixed `amq-receipt-v1`, receiver host, sender host,
id text, and `STORED`. An offer page is `amq-offer-v3`, a page index, a
count of at most 32, then that many record-plus-signature pairs. A disk
bundle is the record, the signature, and the payload.

v3 host and handle names are 1 to 63 bytes from `[a-z0-9._-]`, and are never
`.` or `..`. v2 `ParseAlias` and `validateBridgeIdentifier` stay on the
stricter grammar and refuse a dot.

The X25519 static is HKDF-SHA256 of the 32-byte Ed25519 seed, salt `amq-v3`,
info `amq-v3-x25519-static`. It is never stored. The Noise session is
`Noise_IK_25519_ChaChaPoly_BLAKE2s` with prologue `amq-v3`, using
`github.com/flynn/noise` at the version pinned in `go.mod`.

The v3 outbox is `bridge/tx/<peer>/<id>`. The sender removes a record only
after it verifies the receiver's signed `STORED` receipt for that id. The v2
ledger stays beside the v3 ledger. An operator drains every v2 outbox before a
host offers v3 records; nothing enforces this.

v3 ledger states `intent` and `published` live in
`bridge/ledger/v3/<id[:2]>/<id>` as checksummed `O_APPEND` records. Filing
stages under `bridge/rx/<peer>/<id>`, writes `intent`, renames into
`inbox/new/<id>.md`, then writes `published`. A pre-admission `NO <reason>`
writes no record. A torn tail is truncated when the staged copy is still
present; if it is gone the next fact is `published`. Directory and file
sync use `fsync` on Linux and `F_FULLFSYNC` on Darwin and fail closed. The v2
ledger is read-only beside this one.

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
