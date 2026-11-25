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
and `capabilities`. `approve_tool` and `answer_question` are false for ordinary link commands.
A server queries signed-answer capability separately with `answer_info`. `hello` carries the bindings and
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
| `busy` | The harness is busy and `input.busy` is `reject`. The reply is `{"outcome": {"op": "request.submit", "code": "busy"}}`. The server may send the same signed document again; wait at least 2 seconds between sends. When nothing was sent to the harness for it, it is admitted when the session is free, while the document's `issued_at` is within 60 s of the machine's clock and before its `not_after`. A request sent again after it ended rejected gets the code and reason it ended with. A document for the same request id with another input or `not_after` does not run and is answered `request_conflict` |

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

## Signed dialog answers

An owner can attach a passkey share with `--approvals link` to make eligible
permission dialogs visible on the running task’s card. The card contains the
tool, its complete decision text and its one-time choices in native order.
A server enables those choices when the binding reports `available: true`
and the interaction carries `remote_answer: true`. An absent flag means
false. Each distinct approval requires a passkey assertion for the selected
dialog and exact choice. The terminal and Buzz can still answer first.

Send `amq.remote.link.answer_info/1` with `binding`. The reply carries
`answer_info` with the binding, `available` and `now` (the machine’s RFC 3339
clock). An unavailable binding can give the reason `approvals_terminal`,
`consent_local` or `harness_unsupported`. An unshared binding returns
`unshared`. An older endpoint can return `unsupported` for either frame.

A signing server constructs the interaction digest from the request’s
published approval. Take exactly `interaction_id`, `kind`, `options` in
their published order, `prompt`, and each of `approve_option` and
`reject_option` that is present. Hash the JCS bytes of that object with
SHA-256 and prefix the lowercase hex with `sha256:`. Copy the chosen role’s
label verbatim into `option`. Availability and other snapshot fields stay
outside this digest.

The signed document is `amq.remote.answer/1`. It has exactly these keys.

| Signed content | Fields |
| --- | --- |
| Schema and link | `schema`, `server_id`, `store_id` |
| Published binding | `binding`, `target_id`, `epoch`, `native_session_id`, `labels` |
| Request and choice | `request_ref`, `interaction_id`, `interaction_digest`, `option` |
| RFC 3339 timestamps | `issued_at`, `not_after` |

Use the same WebAuthn assertion as for a signed submit, with SHA-256 of the
exact JCS document bytes as the challenge. Send the assertion fields and
`document_b64` under the frame schema `amq.remote.link.signed_answer/1`.
The machine checks the signature against its current enrolled keys, decodes
the document strictly, matches server, store and live binding, then checks
time. Issuance must be within 60 seconds of the machine’s clock, lifetime
must be positive and at most 120 seconds, and the machine’s clock must be
strictly before expiry. Each send undergoes all checks again.

The verification-stage errors in order are `consent_invalid` for malformed
signed content, signature or link identity, `unshared`, `stale_epoch`,
`session_changed`, then the time checks (`consent_invalid` for malformed
timestamps, `clock_skew`, `consent_invalid` for invalid lifetime,
`expired`). The endpoint then checks whether the share accepts answers
(`answer_unsupported`), the submitting link owns the running request on this
binding (`not_found`), the dialog awaits an answer (`already_resolved`),
the interaction digest still matches (`dialog_changed`) and the label names
an eligible one-time choice (`invalid`).

The success reply is `{"answer":{"request_ref":"…","interaction_id":"…","state":"recorded"}}`.
It confirms that the machine durably wrote the choice. After a lost reply
or an `internal` error during the write, read the task revisions for the
outcome. A repeated send while the answer file exists gets `already_resolved`.
If the harness adapter rejects the choice and removes that file, the card
can become actionable again and the same still-valid document can be sent
again. The final revision reports `answered` when the adapter takes the
link’s answer or `answered_elsewhere` when the terminal or Buzz wins.
A session grant still requires a separate signature for each approval. The signing
projection, document, assertion frame and replies are published as
[golden fixtures](../testdata/link/README.md).

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

The machine keeps at most one grant per binding of a link. A grant replaces
the stored one only when its `issued_at` is later; any other digest than
the stored grant's is refused `grant_stale`, so a replaced grant stays
retired.

### Requests

- `amq.remote.link.grant_info/1` `{schema, binding}` answers
  `{"grant_info": {binding, available, reason?, grant_generation?,
  max_lifetime_s?, max_text_bytes?, now}}`. `available` false carries
  `reason`: `consent_local`, `mode_unreadable` (the harness has not
  reported a mode that asks), `harness_unsupported` or
  `state_in_workspace` (the session can write the machine's state
  directory with no dialog: its working directory, a writable root of its
  sandbox, a runtime workspace root of the thread, or `/tmp` or `$TMPDIR`
  (this machine's value) of a `workspaceWrite` sandbox that does not
  exclude them, holds that directory or lies inside it). A server shows an
  unknown reason as not available. `available` true
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
  until its `not_after`, and every send passes every check again. On pi a
  busy answer can spend the request id, because the pi bridge refuses a
  busy request after the request was published: a server sends a busy task
  to pi again with a new `request_id` under the same grant. A granted
  request is never held as a deferred record: a harness that cannot read
  the session's mode while its attachment is offline answers
  `grant_needs_signature`, and a target that is offline at the hand-over
  answers `busy` with no record. Revisions and cancel are as for a signed
  request.
- `amq.remote.link.grant_revoke/1` `{schema, binding}` (the binding must be
  this link's). Granted tasks read after this frame are refused; the machine
  writes the new generation and answers `{"revoked": {binding,
  grant_generation}}`. When that write fails it answers `storage_full`, and
  the binding's grant stays refused until the server sends the revoke again
  or the machine writes the generation on its own. A task already handed to
  the harness is not recalled.

### What the machine checks at each granted task

1. The body decodes strictly and `command` is a valid `request.submit`.
2. A grant with this digest is stored for the command's binding.
3. The grant's generation is the binding's now, `now` is before the grant's
   `not_after`, and the link's key list is the one the grant was accepted
   under.
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
   `workspaceWrite`. A granted `turn/start` carries no override. pi has no
   approval mode; on pi the session asks while the live pi bridge, of
   bridge revision 6 or later, publishes `gate: true` in the `tui` surface:
   the bridge then asks the person at pi's terminal before every tool call
   of the granted task except pi's own `read`, `grep`, `find` and `ls`, and
   in the person's own runs until the person starts a new run after the
   granted run settled (see
   [the pi guard](pi-bridge-protocol.md#bridge-revision-6-the-guard-of-granted-tasks)).
   A pi session in another mode, a bridge with no gate, or no fresh
   liveness answers `grant_info` not available with reason
   `mode_unreadable`.
8. The request is admitted as a link request, with the grant's native
   session and signing key; its record keeps the grant's digest.

Right before the hand-over to the harness (also for a busy request sent
again) the machine checks 3 and 5 again, and the adapter checks 7 and, on
the settings of that dispatch, that the session cannot write the machine's
state directory with no dialog. A granted request stopped there ends
`rejected` with code `consent_invalid`. A snapshot has no message: the
reason (`grant_revoked: ...`, `grant_expired: ...`,
`grant_needs_signature: ...`, `grant_unsupported: state_in_workspace: ...`)
is the message of the outcome of `request.get`.

### Claude Code: the task at the prompt

Claude Code shows a session's permission mode only to a hook, at a prompt.
So a Claude Code session takes a grant only when AMQ's prompt hook
(`UserPromptSubmit`, `amq-remote claude install-prompt-hook`) is installed,
and the machine gives a granted task's text at the prompt, not on the
socket:

- Check 7 reads the latest prompt report of the session: the hook's session
  id is the session's, and its mode is `default`, `acceptEdits` or
  `dontAsk`. The report lives in the endpoint's memory only. A restart, a
  replaced attachment, another session, or a granted task that ended
  untaken or with its answer withheld drops it, and the next prompt in the
  session brings it back; until then `grant_info` answers
  `mode_unreadable` and a granted task `grant_needs_signature`. The state directory check reads the
  session's working directory; Claude Code reports no additional working
  directory, so a directory the person added to the session is not seen.
- `max_text_bytes` is 8192: Claude Code gives a hook's added context to the
  model whole only below 10,000 characters.
- A session in a turn answers `busy`; a session whose state cannot be read
  answers `grant_needs_signature`.
- The machine sends the session a stub, `AMQ granted task <msg id>`, as a
  cross-session message. The task's text stays in the request record. The
  stub raises a prompt; the hook reports its session id, its mode and the
  stub's id on the endpoint's local socket, and makes no decision.
- Under its lock, the endpoint checks the request's `not_after`, epoch and
  native session, checks 3 and 5 again, and checks that the mode of that
  same prompt asks and that the session still cannot write the machine's
  state. It then records durably that it gave the text and answers with
  it. The hook prints it as the prompt's added context, after
  one line that names the request and the text's SHA-256. The text is given
  at most once per request, also across a restart. The read of the grant
  state at that call is the hand-over: a revoke, a key change or an expiry
  applied before it refuses the text; one applied after it does not recall
  the task. A refusal ends the request `rejected` (for example
  `consent_invalid` with `grant_needs_signature: plan`, or with
  `grant_unsupported: state_in_workspace: ...` as before the send), and
  the model has only the stub.
- The request completes only when the session's transcript shows the
  hook's context with exactly the text the endpoint gave, and the stub was
  the only prompt of its turn. Claude Code can give queued prompts (another
  stub, a signed task, the person's own prompt) to the model as one turn;
  then, or when the context is not whole, the request ends `failed` with no
  result text, and the message of `request.get` is `the answer cannot be
  attributed to this task; the task may have run`. A later event or a
  restart does not bring the answer back.
- A stub whose text no hook took by the request's `not_after` ends
  `rejected` `expired` with the message `the session did not take the task
  before its deadline`, also after a restart and with no attachment.

The endpoint's local socket is a boundary between OS users, not a proof
that the caller is the session's hook: another process of the same user can
take the text of a pending request and so make that request fail. It cannot
put text into the model. An older endpoint, or one that does not serve the
attachment the hook's locator names, gives no text, and the hook prints
nothing.

### How a grant ends

- Its `not_after`. Expiry is kept: inside one run the machine measures the
  life a grant has left on a clock that does not go back, and when it first
  sees a grant expired it clears it from its stored state. A system clock
  set back across a restart, before the grant was seen expired, extends it
  by that amount; a clock set back after that brings nothing back once the
  end is written (see below).
- A new generation: a random value per binding of a link, never a counter
  and never reused. The machine mints one when the share is written (attach,
  attach again, a change of consent or tools), when the share is removed
  (detach), when the owner revokes (`amq-remote link grant revoke`), when the
  server sends `grant_revoke`, and when the stored value is missing or
  unreadable.
- A change of the link's key list. The machine stores with each grant the
  key list it was accepted under; a grant is valid only while the link's
  key list is that list. So a key that is added, dropped or revoked ends
  every grant of the link at once (`grant_revoked`). The end is permanent:
  the machine clears the stored grant when it sees the change, and a key
  list that later returns to its earlier value does not bring the grant
  back. The generation does not change; the owner signs a new grant.
- A newer grant accepted for the same binding.
- A new epoch or another native session behind the binding, at the first
  granted task that meets the change and whose otherwise valid signed grant
  names the old binding. The machine clears that grant and shows an end
  notice with the reason "session changed". A binding that is temporarily
  absent ends nothing. Until that task, `amq-remote link status` shows the
  stored grant with whether its session is attached; if it is not, it serves
  no task until the session is back.
- The session can write the machine's state directory with no dialog: the
  binding then takes no grant (`state_in_workspace`). A link in the machine
  root (the default) can grant; a link kept inside a project cannot grant to
  that project's sessions. A link's state is never moved: to grant there,
  remove the link (`amq-remote link remove NAME`) and add it again in the
  machine root (`amq-remote link add` without `--root`), or keep a signature
  per task.

An end is kept on disk: the machine keeps, per binding, the `issued_at` of
the newest grant it accepted in the binding's generation, and a cleared
grant leaves that value in place. A grant that ended, and every grant
signed before it, is refused when it is presented again: in the run that saw
the end with the end's own code, and after a restart with `grant_stale`,
also when the wall clock was set back. A new generation drops the value,
since a grant names its generation; a grant of an earlier generation is
refused `grant_revoked`.

The machine writes an end that it saw (a key-list change, an expiry) at its
next tick after it saw it. When the machine stops before that write, or the
write fails, the stored grant stays: the run that saw the end refuses it,
and after a restart in that state the grant is live again only if what
ended it no longer holds (the key list is back at its earlier value, or the
clock was set back before its expiry), until the next check ends it again.
An end by a new generation (a share write, a detach, a revoke) is not in
this limit: the generation is written before the answer.

The machine shows a desktop notice when it accepts a grant and when a grant
ends by revoke, expiry or their session changing; `amq-remote link status` lists the stored grants.
A restore of the machine's state from a backup can bring back a grant that
has not expired.

### Codes

Error replies, before admission (nothing was sent):

| Code | From | Meaning | What the server does |
| --- | --- | --- | --- |
| `unsupported` | any grant request | An older machine | Signatures per task |
| `consent_invalid` | `signed_grant`, `granted_submit` | Signature, passkey, WebAuthn profile, strict decoding, server or store id, an input class a grant may not hold, a command out of its bounds | Fix it; for a grant, sign again |
| `clock_skew` | `signed_grant` | `issued_at` is more than 60 s ahead of the machine's clock | Read `now` from `grant_info`; sign again |
| `grant_stale` | `signed_grant` | `issued_at` is more than 60 s behind the machine's clock, or not later than that of the newest grant accepted for the binding (the message names that time), and the digest is not the stored grant's | Sign again. When the named time is ahead of the machine's clock, a new grant is accepted after the clock passes it, or after a revoke of the binding's grant |
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
`storage_full`, `attachment_lost`. A request sent again after it ended
rejected gets the code and reason it ended with, for example
`consent_invalid` with a reason that starts with `grant_needs_signature` or
`grant_unsupported`.

The reason a grant ended depends on what the machine saw. In the run of the
machine that saw a grant end, every later answer for that grant keeps its
reason, also after the grant is cleared from the stored state. After a
restart, a grant cleared before it answers `granted_submit` with
`grant_unknown` and `signed_grant` with `grant_stale`, and a hand-over
reason reads `grant_revoked: the grant is
no longer stored`. The server does the same in each case: it signs again.

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
| `<name>/grants.json` | Per shared binding, its grant generation, the one grant accepted for it and the `issued_at` of the newest grant accepted in that generation, mode 0600; changed under `grants.json.lock`. |
| `<name>/status.json` | The link's last recorded status, which `link status` shows. |
| `retired/<sink>` | Sinks of removed or revoked links. |
