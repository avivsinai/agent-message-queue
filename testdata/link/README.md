# amq.remote.link/1 contract fixtures

These files pin the wire contract between an `amq-remote` endpoint and a
linked server. A server implementation copies them byte for byte and runs
the same checks as `internal/remote/linkcontract` and `internal/remote/jcs`.
The frame schema is `schemas/remote-link-v1.schema.json`.

| Path | Content |
| --- | --- |
| `jcs/vectors.json` | RFC 8785 (JCS) vectors: input JSON text, canonical bytes (`canonical_hex`), or `refused: true` |
| `consent/document.json` | An `amq.remote.consent/2` document, exactly the bytes that were signed (its JCS form, no trailing newline) |
| `consent/assertion.json` | A software authenticator's ES256 WebAuthn assertion over it, the public key, and the passkey fingerprint |
| `device.json` | The endpoint's Ed25519 device key (SPKI) and the creator host derived from it |
| `frames/NN-name.json` | One golden frame per body: `direction`, `body_def` (a `$defs` name in the schema), `frame` |

The fixtures use example names only (`srv_example`, `sign.example.test`).

## Encodings

- Binary values are base64url without padding (RFC 4648 section 5): keys,
  signatures, nonces, credential ids, `document_b64`, `authenticator_data`,
  `client_data_json`.
- Public keys are SPKI DER: P-256 for consent keys (`alg` -7, ES256, DER
  signature), Ed25519 for device keys.
- JCS uses the integer profile: integers within +-(2^53-1) only. A fraction,
  an exponent, a larger integer, a duplicate key, a lone surrogate or
  trailing data is refused.

## Envelope

`{"schema": "amq.remote.link/1", "id"?, "re"?, "gen"?, "body"}`

- `id` names a frame; `re` answers the frame with that id. `hello` has both:
  it answers `challenge` and `welcome` answers it.
- `gen` is absent on `challenge` and `hello`. `welcome` assigns it, and every
  later frame in both directions carries it. A frame from an older
  generation is dropped.
- Notifications (`bindings`, `key_revoked`) carry an `id` and get no reply.
- The envelope ignores unknown fields. Every body is strict: an unknown or
  duplicate key is refused. A new body version is a new schema string.

## Signatures and derived values

- `hello.signature`: Ed25519 by the device key over
  `"amq.remote.link/1\0hello\0" + server_id + "\0" + nonce + "\0" + store_id`,
  with `server_id` and `nonce` from the challenge.
- Consent: the WebAuthn challenge is SHA-256 of the document bytes. The
  signature covers `authenticatorData || SHA-256(clientDataJSON)`, with
  `clientDataJSON` hashed as the raw bytes received, never re-serialized.
  `authenticatorData` is `SHA-256(rp_id) || flags || counter`; UP and UV are
  set, BE equals `backup_eligible`.
- Fingerprint: base32 (RFC 4648 alphabet, no padding) of
  `SHA-256(JCS({credential_id, spki, alg, rp_id, origin}))`, the first 20
  characters, shown as five groups of four.
- Creator host: `"link-"` + the first 16 hex digits of SHA-256(device key SPKI DER).
- Revision digest: `"sha256:"` + hex SHA-256 of the JCS form of `snapshot`.
  The acknowledgement's `ok` is that digest.

## Test keys

No private key is committed. The keys are derived from public seed strings:

- Consent key (P-256): scalar = (SHA-256(`"amq.remote.link/1 test consent key"`) mod (n-1)) + 1.
- Device key (Ed25519): seed = SHA-256(`"amq.remote.link/1 test device key"`).
- Credential id: the first 16 bytes of SHA-256(`"amq.remote.link/1 test credential"`).
- Challenge nonce: base64url of SHA-256(`"amq.remote.link/1 test nonce"`).

## Refusal codes

An error reply is `{"error": {"code", "message"?, "retry_after_ms"?}}`.
Besides AMQ's existing codes (`busy`, `expired`, `stale_epoch`, `unshared`,
`invalid`, `request_conflict`, `not_found`, ...), a signed submit is refused
before admission with one of:

| Code | Meaning |
| --- | --- |
| `consent_invalid` | The passkey is unknown or revoked, the WebAuthn profile fails, or the document does not decode strictly |
| `binding_changed` | The server, store, binding, target, epoch or labels differ from the machine's own values |
| `session_changed` | The native session behind the binding is another one |
| `clock_skew` | `issued_at` or `not_after` is outside the allowed window on the machine's clock |

## Rewriting

The files are golden. `AMQ_LINK_FIXTURES_WRITE=1 go test ./internal/remote/linkcontract -run TestGenerateFixtures`
rewrites them; the ES256 signature is randomized, so every rewrite changes
`assertion.json` and `04-signed_submit.json`, and every mirror must copy the
files again.
