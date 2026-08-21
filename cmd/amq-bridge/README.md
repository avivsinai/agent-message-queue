# amq-bridge

`amq-bridge` is the companion HTTPS courier. It is separate from the local
`amq` binary and does not open a listener or synchronise Maildirs.

The default outbound spool is:

```
<AMQ root>/bridge/outbox/<source-handle>/new/
```

Place complete AMQ message files there. `amq-bridge enqueue --dest-alias` writes
a sibling `.dest` sidecar next to the spool `.md`, and the courier stamps that
`dest_alias` on the wire (not `--dest-alias` on the courier if a sidecar exists).
After the rendezvous returns the exact `transport_accepted` receipt, the courier
archives the file in the sibling `sent/` directory and writes a typed receipt
under `<AMQ root>/bridge/receipts/`.

## Identity initialization

Initialize one Ed25519 bridge identity for each host before starting the
courier. The root contains:

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
`<root>/bridge/trusted/<source_host>` path. Never copy the private seed. The
rendezvous is an untrusted blob store; `--allow-source-host` is only an exact
routing allowlist and does not authenticate the peer. The courier verifies the
Ed25519 signature before applying a polled envelope.

One bounded bidirectional cycle on the Mac uses a **local** receive alias and
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
commit is rejected.

## Host G (Grok computer)

Install AMQ and `amq-bridge` on G the same way as on the Mac. G is a normal
AMQ host: its own root, its own agents, outbound HTTPS only.

1. Put durable state under a directory that survives computer *update* and
   *restart* (prove this on live G; `/workspace` vs `~/.grok` vs `/tmp` is an
   `amq-hws` measurement, not a guess).
2. Pin `AM_ROOT` / `AM_ME` in operator config, never in Bot chat.
3. Set `bridge/host-id` to the G host alias. It must match `--source-host` for
   push and the host component of `--receive-alias` for poll. Copy G's public
   identity record to the Mac's `bridge/trusted/grok` file, and copy the Mac's
   public identity record to G's `bridge/trusted/mac` file.
4. Run the same courier with G as `--source-host` and Mac aliases on
   `--allow-dest`. G does not listen.
5. Bot submit is config-only enqueue (`amq-bridge enqueue --config FILE
   --dest-alias host/agent`, stdin = one AMQ message). It writes a sibling
   `.dest` sidecar next to the spool `.md`. Prompt/chat must not pass
   `--root`, `--rendezvous`, `--me`, or `--spool` to `enqueue`.
6. Bot chat must invoke the fixed wrapper
   [`scripts/amq-bridge-bot-enqueue.sh`](../../scripts/amq-bridge-bot-enqueue.sh)
   instead of `amq-bridge enqueue` directly. Its argv is exactly
   `--dest-alias host/agent`; prompt content cannot add `--root`,
   `--rendezvous`, `--me`, `--spool`, or any extra argument. It reads the
   config path from `AMQ_BRIDGE_ENQUEUE_CONFIG` and refuses a missing,
   symlinked, or non-0600 config before stdin is ever read.

Live G layout and Bot-client load path remain `amq-hws`. This README does not
close that spike.
