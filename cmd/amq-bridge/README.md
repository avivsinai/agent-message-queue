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
- `bridge/trusted/<source_host>` (mode `0600`), the trusted peer generation and
  public key.

`scripts/amq-host-bootstrap.sh` writes `host-id`. Then initialize and export
the public record:

```sh
amq-bridge identity init --root "$AM_ROOT"
amq-bridge identity public --root "$AM_ROOT"
```

Copy only that public identity record to the peer's
`<root>/bridge/trusted/<source_host>` path. Never copy the private seed.
`--allow-source-host` is only an exact routing allowlist and does not
authenticate the peer. The receiver verifies the Ed25519 signature before
applying an envelope.

## Local file apply

This is the proven bidirectional hop. There is no public locker. Drop one
complete JSON `internal/bridge.Envelope` under the ignored local queue path
`<AMQ root>/bridge/drop/`, then run:

```sh
amq-bridge apply-file \
  --root "$AM_ROOT" \
  --file "$AM_ROOT/bridge/drop/envelope.json"
```

The command reads only that regular, non-symlink file. It loads the local
`bridge/host-id` and the trusted public key selected by the envelope's
`source_host`, verifies the Ed25519 signature and payload digest, requires the
`dest_alias` host to match the local host-id, and then calls the same
`ApplyEnvelope` Maildir path as the courier. It prints and durably records a
`destination_maildir_committed` receipt under `<AMQ root>/bridge/receipts/`.
Repeating the same file is an idempotent replay; the same transfer key with a
different payload is a conflict. Unsigned or forged envelopes, foreign
destinations, symlinks, and files outside `bridge/drop` are rejected. The
same command on the peer host applies the reverse hop. Run
`scripts/amq-bot-envelope-hop-probe.sh` to sign a drop file; set
`AMQ_BOT_ENVELOPE_HOP_THREAD` when the payload must keep an existing opaque
thread id.

## HTTPS courier

When an operator provisions a rendezvous, the courier dials out. AMQ does
not ship a hosted relay. The default outbound spool is:

```
<AMQ root>/bridge/outbox/<source-handle>/new/
```

Place complete AMQ message files there. `amq-bridge enqueue --dest-alias` writes
a sibling `.dest` sidecar next to the spool `.md`, and the courier stamps that
`dest_alias` on the wire (not `--dest-alias` on the courier if a sidecar exists).
After the rendezvous returns the exact `transport_accepted` receipt, the courier
archives the file in the sibling `sent/` directory and writes a typed receipt
under `<AMQ root>/bridge/receipts/`.

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
commit is rejected. Until a rendezvous exists, use apply-file; do not treat
this courier loop as the live hop.

## Host G (Grok computer)

Install AMQ and `amq-bridge` on G the same way as on the Mac. G is a normal
Linux AMQ host: its own root, its own agents, no inbound SSH. Durable queue
state belongs under a path that survives Bot client close (`/workspace` is
the proven layout). G update/reset and a second Bot seat on the same VM
remain untested.

1. Pin `AM_ROOT` / `AM_ME` in operator config, never in Bot chat.
2. Set `bridge/host-id` to the G host alias. It must match `--source-host` for
   push and the host component of `--receive-alias` for poll. Copy G's public
   identity record to the Mac's `bridge/trusted/grok` file, and copy the Mac's
   public identity record to G's `bridge/trusted/mac` file.
3. Apply inbound envelopes with `amq-bridge apply-file` on G. Reverse hops
   use the same command on the Mac. If a rendezvous exists, run the courier
   with G as `--source-host` and Mac aliases on `--allow-dest`. G does not
   listen.
4. Bot submit is config-only enqueue (`amq-bridge enqueue --config FILE
   --dest-alias host/agent`, stdin = one AMQ message). It writes a sibling
   `.dest` sidecar next to the spool `.md`. Prompt/chat must not pass
   `--root`, `--rendezvous`, `--me`, or `--spool` to `enqueue`.
5. Bot chat must invoke the fixed wrapper
   [`scripts/amq-bridge-bot-enqueue.sh`](../../scripts/amq-bridge-bot-enqueue.sh)
   instead of `amq-bridge enqueue` directly. Its argv is exactly
   `--dest-alias host/agent`; prompt content cannot add `--root`,
   `--rendezvous`, `--me`, `--spool`, or any extra argument. It reads the
   config path from `AMQ_BRIDGE_ENQUEUE_CONFIG` and refuses a missing,
   symlinked, or non-0600 config before stdin is ever read.
