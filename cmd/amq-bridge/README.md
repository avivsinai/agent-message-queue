# amq-bridge

`amq-bridge` is the companion cross-host courier. It is separate from the
local `amq` binary and does not open a listener or synchronise Maildirs.

The signed envelope and local Maildir apply path are
[the bridge protocol ADR](../../docs/adr-bridge-protocol.md). Every hop uses
the same `ApplyEnvelope` commit. Install the binary from the matching
`amq-bridge_*_{linux,darwin}_{amd64,arm64}.tar.gz` release asset; Homebrew
does not install it. See [INSTALL.md](../../INSTALL.md).

## Identity initialization

Initialize one Ed25519 bridge identity for each host before applying or
sending envelopes. The root contains:

- `bridge/host-id` (mode `0600`), the local host alias;
- `bridge/identity` (mode `0600`), the active generation and private seed; and
- `bridge/trusted/<source_host>/<generation>` (mode `0600`), the trusted
  peer's public key for the named generation.

`scripts/amq-host-bootstrap.sh` writes `host-id`. Then initialize and export
the public record:

```sh
amq-bridge identity init --root "$AM_ROOT"
amq-bridge identity public --root "$AM_ROOT"
```

Copy only that public identity record to the peer's
`<root>/bridge/trusted/<source_host>/<generation>` path. Never copy the
private seed. Keep the current and immediately previous trusted generations
during rotation; remove an old generation only after no in-flight object names
it. The overlap is bounded to two generations.
`--allow-source-host` is only an exact routing allowlist and does not
authenticate the peer. The receiver verifies the Ed25519 signature before
applying an envelope.

## Manual file apply (recovery)

This is a manual/recovery path, not the live G-Mac peer-exchange class. There
is no public locker. Publish one complete JSON `internal/bridge.Envelope` as a
hash-named regular file under the ignored local queue path
`<AMQ root>/bridge/drop/new/<object_sha256>.envelope`, then run:

```sh
amq-bridge apply-file \
  --root "$AM_ROOT" \
  --file "$AM_ROOT/bridge/drop/new/<object_sha256>.envelope"
```

The command reads only that regular, non-symlink file. It loads the local
`bridge/host-id` and the trusted public key at
`trusted/<source_host>/<key_generation>`, verifies the v2 Ed25519 signature
and payload digest, requires the `dest_alias` host to match the local host-id,
and then calls the same `ApplyEnvelope` Maildir path as the courier. It prints
and durably records a `destination_maildir_committed` receipt under
`<AMQ root>/bridge/receipts/`. Repeating the same file is an idempotent
replay; the same transfer key with a different payload is a conflict.
The command consumes only files directly under `bridge/drop/new/`.
`bridge/drop/tmp/`, `*.part` files, symlinks, unsigned or forged envelopes,
foreign destinations, and files outside `bridge/drop/new/` are rejected. The
same recovery command can run on either host. Run
`scripts/amq-bot-envelope-hop-probe.sh` to sign a drop file; set
`AMQ_BOT_ENVELOPE_HOP_THREAD` when the payload must keep an existing opaque
thread id.

## G-initiated peer exchange

For G-Mac traffic, this is the live courier class. Host G is the only dialer:
it starts a fixed, config-pinned `amq-bridge peer-stdio` session and the Mac
helper responds. The session is duplex, so G-to-Mac envelopes and
Mac-to-G outcomes can move in one session, but the Mac does not initiate the
class. `amq` itself remains local and daemon-free.

Envelope v2 is the emitted peer-exchange format. The exchange moves exact,
hash-named object files, not re-serialized AMQ messages:

```text
tx/<peer>/{tmp,new,committed,rejected}
status-tx/<peer>/{tmp,new,returned}
rx/<peer>/{tmp,new,done,quarantine}
status-rx/<peer>/{tmp,new,done,quarantine}
```

The receiver writes an envelope to `rx/<peer>/tmp`, fsyncs it, and
no-replace renames it to `rx/<peer>/new/<object_sha256>.envelope`. That
durable destination rename is `transport_accepted`; it does not apply the
payload or retire the source `tx` object. The source retires `tx` only after a
verified signed `destination_maildir_committed` or `destination_rejected`
outcome. Outcomes use `status-tx/<peer>/new` and may move to
`status-tx/<peer>/returned` after transfer. Peer exchange never creates or
uses `sent/`; that directory remains HTTPS-only.

`OFFER`/`WANT` inventory is limited to the `tx`, `status-tx`, `rx`, and
`status-rx` trees. The courier never reads Maildir, `applied/`, or
`apply-journal/`. `STORED` means that the kind-specific destination sink is
durable; it is not apply, source archive, or consumer drain.

## Optional HTTPS courier

When an operator provisions a rendezvous, this implemented optional courier
class can dial out. It is not the live G-Mac hop or the live architecture.
AMQ does not ship a hosted relay. Its outbound spool is:

```
<AMQ root>/bridge/outbox/<source-handle>/new/
```

Place complete AMQ message files there. `amq-bridge enqueue --dest-alias` writes
a sibling `.dest` sidecar next to the spool `.md`, and the courier stamps that
`dest_alias` on the wire (not `--dest-alias` on the courier if a sidecar exists).
After the rendezvous returns the exact `transport_accepted` receipt, this
HTTPS-only spool may archive the file in the sibling `sent/` directory and
write a typed receipt under `<AMQ root>/bridge/receipts/`. This legacy HTTPS
archive rule does not apply to peer-exchange `tx` objects, which wait for a
terminal destination outcome.

One bounded bidirectional cycle on a host uses a **local** receive alias and
a **remote** send alias. Do not poll a foreign dest alias into this root:

```sh
amq-bridge --root "$AM_ROOT" \
  --rendezvous https://relay.example \
  --source-host mac \
  --source-handle codex \
  --dest-alias grok/claude \
  --receive-alias mac/codex \
  --allow-dest grok/claude,mac/codex \
  --allow-source-host grok \
  --mode both --once
```

The rendezvous contract is:

- `POST /v1/transfers` with one `internal/bridge.Envelope`; the response is
  `{"receipt":{"stage":"transport_accepted",...}}`.
- `GET /v1/transfers?dest_alias=<alias>&limit=<n>` with an `envelopes` array.
- `POST /v1/transfers/<transfer_id>/ack` after local apply; the response must
  carry `destination_maildir_committed`.

The receiver allowlist is exact. A polled envelope for another alias, an
unknown envelope field, a digest conflict, or an ACK before local Maildir
commit is rejected. Until a rendezvous exists, use peer exchange or
apply-file; do not treat this optional HTTPS loop as the live hop.

## Host G (Grok computer)

Install AMQ and `amq-bridge` on G the same way as on the Mac. G is a normal
Linux AMQ host: its own root, its own agents, no inbound SSH. Durable queue
state belongs under a path that survives Bot client close (`/workspace` is
the proven layout). G update/reset and a second Bot seat on the same VM
remain untested.

1. Pin `AM_ROOT` / `AM_ME` in operator config, never in Bot chat.
2. Set `bridge/host-id` to the G host alias. It must match the local source
   host and the host component of any receiver-owned alias. Copy G's public
   identity record to the Mac's `bridge/trusted/grok/<generation>` path, and
   copy the Mac's public identity record to G's
   `bridge/trusted/mac/<generation>` path.
3. Start the G-initiated peer-stdio exchange with the fixed Mac helper. G is
   the dialer and the Mac is the responder; the session transfers both
   directions' envelopes and outcomes. Apply inbound envelopes locally after
   they reach `rx/<peer>/new`. G does not listen.
4. Live peer exchange uses one config-only submit-intent record per AMQ
   message (`amq-bridge enqueue --config FILE --dest-alias host/agent`, stdin
   = one AMQ message). The signer emits Envelope v2 from that intent; it does
   not create a sibling `.dest` sidecar or use the HTTPS spool. `.dest`
   sidecars are HTTPS-only. Prompt/chat must not pass `--root`, `--rendezvous`,
   `--me`, or `--spool` to `enqueue`.
5. Bot chat must invoke the fixed wrapper
   [`scripts/amq-bridge-bot-enqueue.sh`](../../scripts/amq-bridge-bot-enqueue.sh)
   instead of `amq-bridge enqueue` directly. Its argv is exactly
   `--dest-alias host/agent`; prompt content cannot add `--root`,
   `--rendezvous`, `--me`, `--spool`, or any extra argument. It reads the
   config path from `AMQ_BRIDGE_ENQUEUE_CONFIG` and refuses a missing,
   symlinked, or non-0600 config before stdin is ever read.
