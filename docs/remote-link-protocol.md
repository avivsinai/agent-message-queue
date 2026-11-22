# amq.remote.link/1 protocol

`amq.remote.link/1` is the WebSocket an `amq-remote` endpoint dials to a
linked server, a server the user signs tasks in. The machine dials the
server; the server never connects in. Operator commands are in
[the amq-remote reference](../cmd/amq-remote/README.md#link).

The frame schema is [remote-link-v1.schema.json](../schemas/remote-link-v1.schema.json).
The golden [contract fixtures](../testdata/link/README.md) pin the bytes: a
server implementation copies them and runs the same checks as
`internal/remote/linkcontract` and `internal/remote/jcs`.

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
  acknowledged again. Another digest is a conflict: the server never
  overwrites, and answers `error{code: conflict}`. The endpoint then marks that
  revision terminal: no resend, one log line, counted in `link status`.

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
| `busy` | The harness is busy and `input.busy` is `reject`. The reply is `{"outcome": {"op": "request.submit", "code": "busy"}}`. The server may send the same signed document again; wait at least 2 seconds between sends. It is admitted when the session is free, while the document's `issued_at` is within 60 s of the machine's clock and before its `not_after`. A document for the same request id with another input or `not_after` does not run and is answered `request_conflict` |

`busy` and `request_conflict` arrive as an outcome reply, not as an error
reply, and `busy` leaves a busy record.

`interaction.respond` and `session.events` from a link are always refused
with `unsupported`. A request whose reply would exceed the frame limit is
answered `too_large`, never with an oversize frame.

A signed submit on a binding whose `consent` is `local` that passes every
check is held on the machine, not admitted. The reply is
`{"outcome": {"op": "request.submit", "code": "pending_local", "message"}}`,
and `hello.pending_local` lists the digest (`"sha256:"` + hex of the signed
document bytes) while the task waits. The person confirms with
`amq-remote link confirm ID`, ID being the first 8 hex digits of the request
id; the local passkey signs SHA-256(`"amq.remote.link/1\0confirm\0"` +
digest). After a confirmation the request has a record and revisions like
any other. A task whose deadline passes, or that a restart drops, has no
record and no revision; the next hello omits its digest.

## Session grant

The owner can sign one grant for one shared binding. Until the grant ends,
the server sends tasks to that session with no signature per task, while
the harness reports that the session asks before a tool acts. A signed
submit stays valid on every binding at all times. Nothing changes in
`hello`, `bindings/1` or a binding's `capabilities`: each grant request is a
new body, which an older machine answers `unsupported`.

### The grant document

Schema `amq.remote.grant/1`, signed as a consent document is: its JCS
bytes, the link's passkey, WebAuthn get, challenge = SHA-256 of the bytes,
user presence and verification. Every key is required and no other is read:

| Key | Meaning |
| --- | --- |
| `schema` | `amq.remote.grant/1` |
| `server_id`, `store_id` | As in a consent |
| `grant_id` | A lowercase UUID the server chooses |
| `binding`, `target_id`, `epoch`, `native_session_id`, `labels` | The binding as the machine published it |
| `grant_generation` | The value `grant_info` gave for this binding |
| `issued_at`, `not_after` | RFC 3339; `not_after - issued_at` is above 0 and at most `max_lifetime_s` |
| `input` | The one input class the grant covers: `{"busy": "reject", "deliver": "turn", "min_evidence": "submitted" or "admitted"}` |

The machine keeps at most one grant per binding of a link; a newer accepted
grant replaces the older one.

### Requests

- `amq.remote.link.grant_info/1` `{schema, binding}` answers
  `{"grant_info": {binding, available, reason?, grant_generation?,
  max_lifetime_s?, max_text_bytes?, now}}`. `available` false carries
  `reason`: `consent_local`, `mode_unreadable` (the harness has not
  reported a mode that asks) or `harness_unsupported`. `available` true
  carries `grant_generation`, `max_lifetime_s` (86400) and `max_text_bytes`.
  `now` is the machine's clock, for `issued_at` and a command's `not_after`.
- `amq.remote.link.signed_grant/1` has the keys of a signed submit over a
  grant document. The machine verifies it, stores it, shows the owner a
  desktop notice and answers `{"grant": {digest, grant_id, binding,
  not_after}}`, `digest` being `"sha256:"` + hex of the document bytes.
  `issued_at` must be within 60 s of the machine's clock. The stored grant
  presented again gets the same reply and changes nothing. The server sends
  tasks under a grant only after this reply.
- `amq.remote.link.granted_submit/1` `{schema, grant_digest, command}`.
  `command` is a plain `request.submit` with the grant's input class written
  out; its `not_after` is after the machine's `now`, at most 3 minutes
  ahead and not after the grant's. A busy session answers the outcome
  `busy`; the same command may be sent again, at least 2 seconds apart,
  until its `not_after`, and every send passes every check again. A target
  that is offline answers `busy` and keeps no record. Revisions and cancel
  are as for a signed request.
- `amq.remote.link.grant_revoke/1` `{schema, binding}` (the binding must be
  this link's). Granted tasks read after this frame are refused; the machine
  writes the new generation and answers `{"revoked": {binding,
  grant_generation}}`. A task already handed to the harness is not recalled.

### What the machine checks at each granted task

1. The body decodes strictly and `command` is a valid `request.submit`.
2. A grant with this digest is stored for the command's binding.
3. The grant's generation is the binding's now, `now` is before the grant's
   `not_after`, and its signing key is still accepted.
4. The binding is shared with the link; its target, epoch, labels and native
   session equal the grant's and the binding's now.
5. The share's consent is `passkey`. A granted task is never held for a
   local confirmation.
6. The command has the grant's input class and its `not_after` bounds; the
   text is at most `max_text_bytes`.
7. The harness reports now that the session asks before a tool acts. On
   Codex this is the thread's own settings as the app-server reported them
   on the attachment's connection: `approvalPolicy` `untrusted` or
   `on-request`, `approvalsReviewer` `user`, sandbox `readOnly` or
   `workspaceWrite`. A granted `turn/start` carries no override.
8. The request is admitted as a link request, with the grant's native
   session and signing key; its record keeps the grant's digest.

Right before the hand-over to the harness (also for a busy request sent
again) the machine checks 3 and 5 again, and the adapter checks 7. A
granted request stopped there ends `rejected` with code `consent_invalid`.
A snapshot has no message: the reason (`grant_revoked: ...`,
`grant_expired: ...`, `grant_needs_signature: ...`) is the message of the
outcome of `request.get`.

### How a grant ends

- Its `not_after`.
- A new generation: a random value per binding of a link, never a counter
  and never reused. The machine mints one when the share is written (attach,
  attach again, a change of consent or tools), when the share is removed
  (detach), when the owner revokes (`amq-remote link grant revoke`), when the
  server sends `grant_revoke`, when the link's key list changes, and when
  the stored value is missing or unreadable.
- A newer grant accepted for the same binding.
- A new epoch or another native session behind the binding.

The machine shows a desktop notice when it accepts a grant and when a grant
ends by revoke or expiry; `amq-remote link status` lists the stored grants.
A restore of the machine's state from a backup can bring back a grant that
has not expired.

### Codes

Error replies, before admission (nothing was sent):

| Code | From | Meaning | What the server does |
| --- | --- | --- | --- |
| `unsupported` | any grant request | An older machine | Signatures per task |
| `consent_invalid` | `signed_grant`, `granted_submit` | Signature, passkey, WebAuthn profile, strict decoding, server or store id, an input class a grant may not hold, a command out of its bounds | Fix it; for a grant, sign again |
| `clock_skew` | `signed_grant` | `issued_at` is more than 60 s ahead of the machine's clock | Read `now` from `grant_info`; sign again |
| `grant_stale` | `signed_grant` | `issued_at` is more than 60 s behind the machine's clock, and the digest is not the stored grant's | Sign again |
| `grant_too_long` | `signed_grant` | The lifetime is not in (0, `max_lifetime_s`] | Sign a shorter grant |
| `grant_unsupported` | `signed_grant`, `granted_submit` | The binding takes no grant now; the message starts with the reason | End the grant; signatures per task |
| `grant_unknown` | `granted_submit` | No grant with this digest is stored for the binding | End the grant; sign again |
| `grant_revoked`, `grant_expired` | both | The grant ended (above) | End the grant; ask `grant_info`; sign again when the owner wants |
| `grant_needs_signature` | `granted_submit` | The session does not ask now, its mode cannot be read, or the text is longer than `max_text_bytes` | Send this task as a signed submit with a new `request_id`; the grant stays |
| `unshared`, `stale_epoch`, `session_changed`, `expired` | all | As for a consent | Read the binding again |
| `busy` with `retry_after_ms` | all | The connection's queue is full; nothing was read | Send the same frame again |

`grant_*` codes are link reply codes only, never the code of a request
record or a revision. Outcome replies to `granted_submit` are as for a
signed submit: no code (admitted), `busy`, `request_conflict` (the request
id is in use with another input, `not_after`, signature or grant),
`storage_full`, `attachment_lost`.

## Link info

Before linking, the endpoint reads the server's public link info: `server_id`,
`link_url`, optional `tool_profiles` and optional `default_tool_profile`.
`tool_profiles` lists the profiles the link grants; the default names the
profile a new share takes when `attach --tools` is omitted.

A server that states `default_tool_profile` accepts `binding` in a `tools`
request. A `call` request carries `binding` for every server. The endpoint
sends the binding of the calling session only while that binding is one of
the link's shares; a caller without a share sends none and gets the
server's read tools. `call_get` and the consent operations name no session.

## Tool calls

A call that ended without a result is `{"status": "error", "error": {"code"}}`
with one of `rejected`, `expired`, `unknown`, `refused`, `tool_error`,
`conflict` (the same idempotency key with another tool or arguments) and
`not_allowed` (the tool is not in the link's catalog). `busy`, with
`retry_after_ms`, means "try again later" and is never final: the endpoint
sends the same call or `call_get` again.

The endpoint sends `call_id = "c_" + idempotency_key`, a client-scoped alias:
the server echoes that id in every `call_reply` and answers `call_get` for it,
resolved by the calling subject, the link's device and the key, for 24 hours
after the call is final; after that a `call_get` answers `error.code` `unknown`.
A write that waits for the owner answers `pending_approval` with its
`review_url`.

## Endpoint state

A link lives in exactly one AMQ root, normally the machine root
`~/.amq/remote/root`; its state is never copied or moved, and one link name
in two roots is refused. Per link root, under `<root>/extensions/remote/link/`:

| Path | Content |
| --- | --- |
| `store_id` | The store identity, minted once. |
| `<name>/device.key` | The Ed25519 device key, mode 0600, never overwritten. |
| `<name>/link.json` | The pinned server id, URL, signing origin, and the tool profiles the server grants. |
| `<name>/consent_keys.json` | Consent public keys only. |
| `<name>/grants.json` | Per shared binding, its grant generation and the one grant accepted for it, mode 0600; changed under `grants.json.lock`. |
| `<name>/status.json` | The link's last recorded status, which `link status` shows. |
| `retired/<sink>` | Sinks of removed or revoked links. |
