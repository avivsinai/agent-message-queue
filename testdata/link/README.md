# amq.remote.link/1 contract fixtures

These files pin the wire contract between an `amq-remote` endpoint and a
linked server. A server implementation copies them byte for byte and runs
the same checks as `internal/remote/linkcontract` and `internal/remote/jcs`.
The frame schema is `schemas/remote-link-v1.schema.json`.

| Path | Content |
| --- | --- |
| `jcs/vectors.json` | RFC 8785 (JCS) vectors: input JSON text, canonical bytes (`canonical_hex`), or `refused: true` |
| `consent/document.json` | An `amq.remote.consent/2` document, exactly the bytes that were signed (its JCS form, no trailing newline) |
| `consent/assertion.json` | A software authenticator's ES256 (`alg` -7) WebAuthn assertion over it, the public key, and the passkey fingerprint |
| `consent/assertion_ed25519.json` | The same document signed by an Ed25519 (`alg` -8) passkey |
| `consent/refused/*.json` | Documents a verifier refuses with `consent_invalid`, as `document_b64` |
| `device.json` | The endpoint's Ed25519 device key (SPKI) and the creator host derived from it |
| `frames/NN-name.json` | One golden frame per body: `direction`, `body_def` (a `$defs` name in the schema), `frame` |

The fixtures use example names only (`srv_example`, `sign.example.test`).
A server's parity test takes `rp_id` and `origin` from
`consent/assertion.json`, never from its production configuration.

## Encodings

- Binary values are base64url without padding (RFC 4648 section 5), never
  empty: keys, signatures, nonces, credential ids, `document_b64`,
  `authenticator_data`, `client_data_json`.
- Public keys are SPKI DER: P-256 (`alg` -7, ES256, DER signature) or
  Ed25519 (`alg` -8) for consent keys, Ed25519 for device keys.
- JCS uses an integer profile: integers within +-(2^53-1) only, at most 64
  nested objects and arrays. A fraction, an exponent or a larger integer is
  refused, never formatted, as are duplicate keys, lone surrogates and
  trailing data. A canonicalizer that formats floats (for example Python's
  `rfc8785` package) must add these refusals.

## Envelope

`{"schema": "amq.remote.link/1", "id"?, "re"?, "gen"?, "body"}`

- `id` names a frame; `re` answers the frame with that id. `hello` has both:
  it answers `challenge`, and `welcome` answers it.
- `gen` is absent on `challenge` and `hello`. `welcome` assigns it, and every
  later frame in both directions carries it. A frame from an older
  generation is dropped.
- Notifications (`bindings`, `key_revoked`) carry an `id` and get no reply.
- The envelope ignores unknown fields. Every body is strict: an unknown or
  duplicate key is refused. A new body version is a new schema string.

## Transport

- The endpoint dials the server: `wss://`, or `ws://` to a loopback host.
  It answers the challenge only for the `server_id` it pinned at linking.
- A frame is at most 4 MiB. Both sides refuse a larger one before buffering.
- Liveness is WebSocket ping and pong. The server pings every 20 s; the
  endpoint reconnects after 45 s without a ping, after the machine wakes, and
  after close 1012 (server restart) with a random delay of 0-60 s. Other
  reconnects back off with jitter.
- Close 4010 is a permanent revoke: the endpoint retires the link's sink and
  deletes its device key, so it can never dial again.
- Commands for one request run in arrival order. A reader never waits on a
  command or a tool call; when its queue is full it answers `busy` with
  `retry_after_ms`, and the sender keeps the work.
- An acknowledgement (`{"ok": digest}`) is sent only after the receiver
  committed the revision. Until then the endpoint still owes the revision
  and offers it again on a later sweep (at most once per 30 s per offer, and
  again on every new connection). The same revision with the same digest is
  acknowledged again; another digest is a conflict.

## Bindings

A binding is one AMQ binding shared with the link, as the machine sees it
now: `binding`, `target_id`, `epoch`, `native_session_id`, `labels`
(`device`, `session`, `harness`, `project`), `harness`, `display_name`,
`attachment`, `consent` (`passkey` or `local`), `tools` (the tool profile)
and `capabilities`. `approve_tool` and `answer_question` are always false: a
link never answers the agent's prompts. `hello` carries the bindings and
`pending_local` (digests of documents waiting for a local confirmation);
`amq.remote.link.bindings/1` pushes them again when they change, and
`session.list` and `session.inspect` answer with them. A consent document's
binding, target, epoch, native session and labels must equal these values.

## Signatures and derived values

- `hello.signature`: Ed25519 by the device key over
  `"amq.remote.link/1\0hello\0" + server_id + "\0" + nonce + "\0" + store_id`,
  with `server_id` and `nonce` from the challenge.
- Consent: the WebAuthn challenge is SHA-256 of the document bytes. The
  signature covers `authenticatorData || SHA-256(clientDataJSON)`, with
  `clientDataJSON` hashed as the raw bytes received, never re-serialized.
  `authenticatorData` is `SHA-256(rp_id) || flags || counter`; UP and UV are
  set, BE equals `backup_eligible`. There is no counter check.
- Fingerprint: base32 (RFC 4648 alphabet, no padding) of
  `SHA-256(JCS({credential_id, spki, alg, rp_id, origin}))`, the first 20
  characters, shown as five groups of four.
- Creator host: `"link-"` + the first 16 hex digits of SHA-256(device key SPKI DER).
- Revision digest: `"sha256:"` + hex SHA-256 of the JCS form of `snapshot`.
  The acknowledgement's `ok` is that digest.

## Refusals

An error reply is `{"error": {"code", "message"?, "retry_after_ms"?}}`. A
signed submit is refused before admission with one of:

| Code | Meaning |
| --- | --- |
| `consent_invalid` | The passkey is unknown or revoked, the WebAuthn profile fails, the document is for another server or store, or it does not decode strictly (`consent/refused/`) |
| `stale_epoch` | The binding's target, epoch or labels moved on: read the binding again and sign again |
| `session_changed` | The native session behind the binding is another one |
| `clock_skew` | `issued_at` is more than 60 s from the machine's clock |
| `expired` | The consent's `not_after` has passed |
| `unshared` | The binding is not shared with this link |
| `busy` | The harness is busy and `input.busy` is `reject`. No `retry_after_ms`: sending again is a new request and a new signature |

`interaction.respond` and `session.events` from a link are always refused
with `unsupported`. A tool call that ended without a result is
`{"status": "error", "error": {"code": "rejected" | "expired" | "unknown" | "refused"}}`.

## Test keys

No private key is committed. The keys are derived from public seed strings:

- Consent key, ES256 (P-256): scalar = (SHA-256(`"amq.remote.link/1 test consent key"`) mod (n-1)) + 1;
  credential id = the first 16 bytes of SHA-256(`"amq.remote.link/1 test credential"`).
- Consent key, Ed25519: seed = SHA-256(`"amq.remote.link/1 test consent key ed25519"`);
  credential id = the first 16 bytes of SHA-256(`"amq.remote.link/1 test credential ed25519"`).
- Device key (Ed25519): seed = SHA-256(`"amq.remote.link/1 test device key"`).
- Challenge nonce: base64url of SHA-256(`"amq.remote.link/1 test nonce"`).

## Rewriting

The files are golden. `AMQ_LINK_FIXTURES_WRITE=1 go test ./internal/remote/linkcontract -run TestGenerateFixtures`
rewrites them; the ES256 signature is randomized, so every rewrite changes
`consent/assertion.json` and the signed-submit frame, and every mirror must
copy the files again.
