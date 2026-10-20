# amq.remote.link/1 contract fixtures

These files pin the wire bytes of [the amq.remote.link/1
protocol](../../docs/remote-link-protocol.md) between an `amq-remote`
endpoint and a linked server. A server implementation copies them byte for
byte and runs the same checks as `internal/remote/linkcontract` and
`internal/remote/jcs`. The frame schema is
[remote-link-v1.schema.json](../../schemas/remote-link-v1.schema.json).

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
