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
- `bridge/trusted/<source_host>` (mode `0600`), the trusted peer's public
  key for its active generation (a single file, not a per-generation
  directory — the reader is `bridge.TrustedPath`).

`scripts/amq-host-bootstrap.sh` writes `host-id`. Then initialize and export
the public record:

```sh
amq-bridge identity init --root "$AM_ROOT"
amq-bridge identity public --root "$AM_ROOT"
```

Copy only that public identity record to the peer — or, better, let the
provisioning command do it. On the destination root, pipe the source's
public record into:

```sh
amq-bridge trust add --root "$AM_ROOT" --host <source_host>
```

It accepts the one-line `identity public` output (piped or pasted), the
same record saved to a file via `--from <file>`, or the two-line
`generation <g>` / `public <hex>` key-file form, and writes
`<root>/bridge/trusted/<source_host>` through `bridge.WriteTrusted`.
Never copy the private seed. The trusted file holds ONE active generation
per source host (a single file, not a per-generation directory — the
reader is `bridge.TrustedPath`); during rotation re-run `trust add` with
the new generation's record, which overwrites the file atomically. Remove
an old generation's trust only after no in-flight object names it. The
overlap is bounded to two generations.
`--allow-source-host` is only an exact routing allowlist and does not
authenticate the peer. The receiver verifies the Ed25519 signature before
applying an envelope.

## Manual file apply (recovery)

This is a manual/recovery path, not the peer-exchange class. There is no public
locker. Publish one complete JSON `internal/bridge.Envelope` as a
hash-named regular file under the ignored local queue path
`<AMQ root>/bridge/drop/new/<object_sha256>.envelope`, then run:

```sh
amq-bridge apply-file \
  --root "$AM_ROOT" \
  --file "$AM_ROOT/bridge/drop/new/<object_sha256>.envelope"
```

The command reads only that regular, non-symlink file. It loads the local
`bridge/host-id` and the trusted public key at
`trusted/<source_host>` (the generation is taken from the envelope's
`key_generation` field and must match the trusted record), verifies the v2
Ed25519 signature
and payload digest, requires the `dest_alias` host to match the local host-id,
and then routes through the transfer ledger (docs/adr-bridge-protocol.md,
Addendum 4) into the same `ApplyEnvelope` Maildir path the courier uses. It
prints and durably records a `destination_maildir_committed` receipt under
`<AMQ root>/bridge/receipts/`. Repeating the same file is an idempotent
replay; the same transfer key with a different payload is a conflict
(`transfer %s refused: transfer_conflict (committed result for this key
belongs to a different payload)`); a transfer with unknown ledger history is
refused as `uncertain` without a receipt.
The command consumes only files directly under `bridge/drop/new/`.
`bridge/drop/tmp/`, `*.part` files, symlinks, unsigned or forged envelopes,
foreign destinations, and files outside `bridge/drop/new/` are rejected. The
same recovery command can run on either host. Run
`scripts/amq-bot-envelope-hop-probe.sh` to sign a drop file; set
`AMQ_BOT_ENVELOPE_HOP_THREAD` when the payload must keep an existing opaque
thread id.

## Peer exchange

The peer-stdio courier is a config-pinned, duplex session between two hosts.
The configured initiator dials; the peer responds. `amq` itself remains local
and daemon-free.

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

For a config-only peer submit, enqueue one AMQ message on stdin:

```sh
amq-bridge enqueue --config FILE --dest-alias host-b/agent
```

The signer emits Envelope v2 from the config intent. It does not create a
`.dest` sidecar or use the HTTPS spool. Prompt-driven callers must not pass
`--root`, `--rendezvous`, `--me`, `--spool`, or any extra argument. Use
[`scripts/amq-bridge-bot-enqueue.sh`](../../scripts/amq-bridge-bot-enqueue.sh)
for a fixed-argv wrapper; it reads `AMQ_BRIDGE_ENQUEUE_CONFIG` and refuses a
missing, symlinked, or non-0600 config before reading stdin.

## Optional HTTPS courier

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
# `relay.example` is an operator-provided placeholder rendezvous URL.
amq-bridge --root "$AM_ROOT" \
  --rendezvous https://relay.example \
  --source-host host-a \
  --source-handle codex \
  --dest-alias host-b/claude \
  --receive-alias host-a/codex \
  --allow-dest host-b/claude,host-a/codex \
  --allow-source-host host-b \
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
