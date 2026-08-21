# ADR: Companion bridge protocol

## Status

Accepted.

## Date

2026-08-20

## Context

[Two-host fleets](adr-two-host-fleets.md) freeze identity: host M and host G
each run local AMQ; cross-host mail is a companion, not Core. This ADR freezes
the `amq-bridge` wire, transport, and threat model before courier code.

The Grok Bot computer spike (`amq-hws`) is docs-only so far: no live G host
from the Mac. Public xAI material supports outbound HTTPS and rejects hosted
MCP to localhost. It does not prove git, inbound SSH, or process supervision.
That is enough to choose a transport. It is not enough to close the live-G
operational spike.

## Decision

### Companion, not Core

`amq-bridge` is a separate binary, like `amq-keepalive`. The `amq` binary
does not listen, dial, or interpret rendezvous URLs. Local apply reuses
existing Maildir `publishTmpNoReplace` on a stable transfer filename.

### Transport

v1 transport is **HTTPS store-and-forward**. Both nodes dial out. Host G
accepts no inbound connection. The rendezvous is an opaque blob store with
lease, retry, backoff, and bounded batches. It never reads AMQ handles or
Maildir state.

Not v1: git, Maildir sync, reverse tunnels, inbound SSH to G, remote drain,
or sockets inside `amq`.

Live G may change the rendezvous URL, idle timeout, and whether a courier
survives Bot-client close. It does not change this transport class.

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
- `payload` (exact AMQ message bytes)

Unknown fields and the following names are rejected: paths, roots, argv, env,
executable names, endpoints, and remote session selectors.

The signature covers canonical v1 envelope bytes. Canonicalization excludes the
`signature` field and the raw `payload` bytes, but includes `payload_sha256`.
The receiver verifies the signature before local Maildir apply. The
`key_generation` selects the local identity on the sender and the trusted
public key on the receiver.

The payload is an ordinary AMQ message. Project/job/session values inside it
are untrusted context, never routing keys.

### Receipts

Keep three layers distinct:

| State | Meaning |
| --- | --- |
| `transport_accepted` | The courier accepted the envelope for HTTPS. |
| `destination_maildir_committed` | `publishTmpNoReplace` committed `xfer-<transfer_id>.md`. |
| consumer-local drain/start/complete | Optional; may stay on the consuming host. |

ACK only after durable Maildir commit. Lost ACK replays the same
`(source_host, transfer_id, payload_sha256)` and must not create a second
message. Same key, different digest is conflict (`EEXIST` / explicit error).
Crash between commit and ACK is `uncertain` until a later identical replay
confirms the dest bytes.

### Authorization

The rendezvous is an untrusted HTTPS blob store. It does not authenticate a
host, and `--allow-source-host` is a routing allowlist, not authentication.
The receiving host verifies the Ed25519 signature against its local trusted
key, then maps `dest_alias` through that allowlist. Claimed handle, labels,
prompt text, and remote paths are not authority. All Grok Bots on G are one
host principal until a live test proves otherwise.

Each queue root has these bridge identity files:

- `<root>/bridge/host-id` (mode `0600`): the local host alias. It must match
  `--source-host` for push and the host component of `--receive-alias` for
  poll.
- `<root>/bridge/identity` (mode `0600`): the local key generation and
  Ed25519 private seed.
- `<root>/bridge/trusted/<source_host>` (mode `0600`): the trusted peer's key
  generation and Ed25519 public key.

Bootstrap writes `host-id`. `amq-bridge identity init` then writes `identity`
for that host. Copy only the public key record to the peer's
`trusted/<source_host>` file; never copy a private seed. A trusted generation
is selected locally and must match the signed envelope. Revocation is local
and immediate: delete the trusted file or replace it with a new trusted
generation before accepting more envelopes.

### Bot client on G

v1 Bot→AMQ is a fixed, audited local CLI wrapper. Not a general plugin. Not
hosted remote MCP (that requires a public URL and cannot target localhost).
The wrapper must not take prompt-controlled roots, argv, env, or endpoints.

Mac `Grok Bot.app` (this machine, 2026-08-20) is the operator UI, not host G:

- Bundle `com.anysphere.sand`, Team `DCNK4UB866`, Electron 0.20.0
- URL schemes `grokbot` and `sand`
- ATS allows localhost plus arbitrary loads; that is the Mac client talking
  outbound/local, not inbound SSH to G
- A `local-exec-daemon` process is part of the Mac app, not AMQ on G
- Support files include an encrypted gateway descriptor; do not treat that
  blob as a rendezvous URL or as proof of the cloud computer layout

Live `amq-hws` still requires a shell **on G** (`/workspace`, `~/.grok`,
courier vs routine, two Bot seats vs one host principal). The Mac app being
open does not close that spike.

## Consequences

- Courier code implements this envelope and HTTPS poll/push. It does not add
  listeners to `amq`.
- Mac node (`amq-baj.1`) can be built and tested against a fake rendezvous
  before live G exists.
- G node (`amq-baj.2`) still waits on live `amq-hws` for install layout and
  how Bot actually invokes the wrapper.
- Operators provision a public HTTPS rendezvous. AMQ does not ship a hosted
  relay as Core.
