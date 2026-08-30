# ADR: Companion bridge protocol

## Status

Accepted.

## Date

2026-08-20

## Context

[Two-host fleets](adr-two-host-fleets.md) freeze identity: host M and host G
each run local AMQ; cross-host mail is a companion, not Core. This ADR freezes
the `amq-bridge` wire, transport, and threat model.

Host G is a live Linux AMQ host. Durable queue state lives under a path that
survives Bot client close (`/workspace` is the proven layout). Hosted MCP to
localhost is rejected, so the Bot client is a fixed local CLI wrapper, not a
remote plugin. Git, inbound SSH, and process supervision are still not
transport.

## Decision

### Companion, not Core

`amq-bridge` is a separate binary, like `amq-keepalive`. The `amq` binary
does not listen, dial, or interpret rendezvous URLs. Local apply reuses
existing Maildir `publishTmpNoReplace` on a stable transfer filename.

### Transport

The wire unit is the signed envelope below. Local apply is the same
`ApplyEnvelope` path in every hop.

The supported G-Mac courier class is **G-initiated peer exchange**. Host G is
the only dialer: it starts a fixed, config-pinned peer-stdio session and the
Mac helper is the responder. The session is duplex, so envelopes and signed
outcomes can move in both directions, but the Mac does not initiate this
class. `amq` Core remains local and daemon-free; no socket is added to it.

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
an operator-provided rendezvous. It is not the live G-Mac hop or the live
architecture. The rendezvous is an opaque blob store with lease, retry,
backoff, and bounded batches; it never reads AMQ handles or Maildir state.
AMQ does not ship a hosted relay, and operators must not treat HTTPS
poll/push as the active peer exchange.

Not v1: git, Maildir sync, reverse tunnels, inbound SSH to G, remote drain,
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

The G-initiated peer exchange emits **Envelope v2**: one compact JSON object,
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
| `destination_rejected` | The authenticated destination emitted a terminal rejection, such as `alias_not_accepted` or `transfer_conflict`. |
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
are not authority. All Grok Bots on G are one host principal until a live test
proves otherwise.

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

### G-initiated exchange layout and stages

The peer-exchange WAL is separate from the HTTPS spool:

```text
tx/<peer>/{tmp,new,committed,rejected}
status-tx/<peer>/{tmp,new,returned}
rx/<peer>/{tmp,new,done,quarantine}
status-rx/<peer>/{tmp,new,done,quarantine}
drop/{tmp,new}
```

The G initiator and Mac responder exchange only hash-named envelope and
outcome objects. `OFFER`/`WANT` inventory is limited to those courier trees;
the courier never reads Maildir, `applied/`, or `apply-journal/`. `STORED`
means that the kind-specific `rx` or `status-rx` sink is durable. It does not
mean apply, source archive, or consumer drain.

### Implementer contract: amq-ad6 addenda

These eight addenda are normative and override older wording in this ADR or
the design source:

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
8. **Driver boundary.** There is no signed-bundle PR. PR 10 is SSH
   forced-command plus local-exec only. `drop/` uses hash-named publication
   into `drop/new/`.

### Bot client on G

v1 Bot→AMQ is a fixed, audited local CLI wrapper. Not a general plugin. Not
hosted remote MCP (that requires a public URL and cannot target localhost).
The wrapper must not take prompt-controlled roots, argv, env, or endpoints.

Mac `Grok Bot.app` is the operator UI, not host G:

- Bundle `com.anysphere.sand`, Team `DCNK4UB866`, Electron 0.20.0
- URL schemes `grokbot` and `sand`
- ATS allows localhost plus arbitrary loads; that is the Mac client talking
  outbound/local, not inbound SSH to G
- A `local-exec-daemon` process is part of the Mac app, not AMQ on G
- Support files include an encrypted gateway descriptor; do not treat that
  blob as a rendezvous URL or as proof of the cloud computer layout

The Mac app being open does not make this machine host G. G update/reset and
a second Bot seat on the same VM remain untested; treat G as one host
principal until a live test proves isolation.

## Consequences

- Courier code implements this envelope and G-initiated peer-stdio exchange;
  HTTPS poll/push remains optional. It does not add listeners to `amq`.
- `amq-bridge apply-file` is the manual/recovery path: it uses the same
  envelope and local apply rules without a public locker, but it is not the
  live G-Mac peer-exchange class.
- G is the only peer-exchange dialer, and destination apply remains the
  commit. Operators may provision a public HTTPS rendezvous, but AMQ does not
  ship a hosted relay as Core.
- G is a normal AMQ install. Pin `AM_ROOT` in operator config, never in Bot
  chat. Durable state belongs under a path that survives Bot client close.
