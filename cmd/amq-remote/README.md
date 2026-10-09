# amq-remote

`amq-remote` attaches to a harness session that is already running. Run
`inspect` on the target and use only the operations and evidence it
advertises. A declared harness does not imply `submit` or `cancel`. It is a
separate binary on purpose: `amq` itself gains no socket. Design invariants live in
[the remote-control ADR](../../docs/adr-remote-control.md). Pinned harness
seams live in [the compatibility manifest](../../docs/remote-compat.md). This
page is the operator reference for the binary.

## Install

Homebrew's `amq` formula installs `amq-remote` and the owner signer
`amq-owner-sign`. Each also has a
`<name>_*_{linux,darwin}_{amd64,arm64}.tar.gz` release asset; verify its
checksum. See [INSTALL.md](../../INSTALL.md). `amq upgrade` leaves these
binaries unchanged; update them with `brew upgrade amq` or from the release
asset.

`make build` also produces a local `amq-remote` and `amq-owner-sign` next to
`amq`.

## Where state lives

The endpoint owns `<root>/extensions/remote/`. `amq cleanup` does not remove
extension data. The IPC socket is `<root>/extensions/remote/endpoint.sock`.
Adapter declarations are `<root>/extensions/remote/manifest.json` unless
`--manifest` points somewhere else.

`--root` is required for every command that opens that directory. It defaults
to `AM_ROOT` and must be absolute. A missing or relative root is exit 2.

## Commands

```text
amq-remote serve
amq-remote up
amq-remote sessions
amq-remote inspect TARGET
amq-remote submit TARGET
amq-remote status REQUEST_REF
amq-remote wait REQUEST_REF
amq-remote cancel REQUEST_REF
amq-remote requests
amq-remote share --session ID
amq-remote attach --self [--native | --link NAME]
amq-remote link add NAME URL --code CODE
amq-remote link keys add NAME --code CODE
amq-remote link remove NAME
amq-remote link status
amq-remote doctor
amq-remote version
```

`serve` runs the endpoint in the foreground. `up` supervises `serve`: it
respawns a crashed child with backoff and stops when the child exits 0. A
child exit 2 is a usage error and is not respawned. A second `up` for the
same root exits 6 (`endpoint_already_running`) while the first supervisor
holds the lifetime lock.

`sessions`, `inspect`, `submit`, `status`, `wait`, `cancel`, and `doctor`
talk to a running endpoint. `status` also reads the local sender spool when
the endpoint has no record yet. `requests` does not use that socket: it reads
the local request store and the sender spool directly, including while the
endpoint is down.
`share` mints or enrolls a session body key and does not start the endpoint.
`version` prints `amq-remote <version>`.

Client commands and `doctor` accept `--json`. JSON goes to stdout and
diagnostics to stderr. `serve`, `up`, and `share` do not emit a JSON result.

## Manifest

`manifest.json` is read when `serve` starts. An absent file is an empty
adapter list, which is legal. `schema_version` is `1`. `layer` is `remote`.
Each adapter has a unique `target` (the id you pass to `inspect` and
`submit`), a `kind`, and an optional `config` object.

| Kind | Required `config` | What `inspect` advertises on this tree |
| --- | --- | --- |
| `claude` | `pid` (Claude Code process id). Optional `home` overrides the Claude home directory. Optional `approve` advertises `ApproveTool`: the Buzz DM can deny a tool call through the PermissionRequest hook, and allow one when the hook pins the owner (see [Approvals from the Buzz DM](#approvals-from-the-buzz-dm)). | `Inspect` and `Submit` over the session's cross-session socket. Submit evidence is `submitted`, so a relay share for Claude sets `min_evidence` `submitted`. `CancelRequest` and `Steer` are false. Unsupported on Windows. See [Claude Code](#claude-code). |
| `codex` | `socket` and `thread`. Optional `approve` advertises `ApproveTool`. | `Inspect`, `Submit`, and `CancelRequest`. `Steer` is false. |
| `pi` | `handle`; optional `upgrade_hint`, the remedy shown when the live bridge is too old and publishes none. The pi-bridge extension directory for that handle, `agents/<handle>/extensions/pi-bridge/` under the root, must already exist. | `Inspect` and `Submit`. `ApproveTool` only while the live bridge advertises `bridge_revision` 4 or higher; the reference bridge is revision 3. `CancelRequest` is false. Submit evidence is `submitted`. |
| `fake` | none | Test double. `epoch` is accepted only for this kind. |

`epoch` on any kind other than `fake` is exit 2. A duplicate `target` is
exit 2. An unknown kind is refused at startup; `serve` continues with the
adapters that attached, writes `refusals.json`, and prints the refusal.

```json
{
  "schema_version": 1,
  "layer": "remote",
  "adapters": [
    {
      "kind": "claude",
      "target": "claude-main",
      "config": {"pid": 12345}
    },
    {
      "kind": "codex",
      "target": "cx-1",
      "config": {"socket": "/absolute/path/codex.sock", "thread": "THREAD", "approve": false}
    },
    {
      "kind": "pi",
      "target": "pi-1",
      "config": {"handle": "pi-agent"}
    }
  ]
}
```

Two serve flags append to this file's adapter list instead of editing it.
`--fake` adds kind `fake`, target `fake`. `--codex-socket PATH` attaches the
daemon's loaded threads, or only `--codex-thread ID` when that flag is set.
Those sugar targets look like `codex:` plus the thread id with hyphens
removed. `--codex-approve` sets `approve` on those sugar entries. A target
id that appears both in the file and in the flags is exit 2.

`serve --discover` and `up --discover` list running sessions and exit. They
attach nothing and write nothing, so they work while an endpoint already
owns the root. Each line is the kind, the target, a display name, and a
manifest entry to paste:

```text
claude   claude:12345                             main                 {"config":{"pid":12345},"kind":"claude","target":"claude:12345"}
codex    codex:0198a1b2c3d47e5f8a9b0c1d2e3f4a5b   -                    {"config":{"socket":"...","thread":"0198a1b2-..."},"kind":"codex","target":"codex:0198a1b2c3d47e5f8a9b0c1d2e3f4a5b"}
```

Claude sessions come from `~/.claude/sessions`: interactive, with a live
pid and a messaging socket. Codex threads come from the app-server daemon at
`~/.codex/app-server-control/app-server-control.sock`, or from
`--codex-socket` when set. A discovered session is shared only after you
paste its entry into the manifest: discovery is never consent.

When `serve` starts, it prints one line per attached target with its
capabilities and `observed_at`. Under `up`, those lines come from the child
`up` started.

## Relay

A manifest with `"schema_version": 2` may carry one `relay` object. Without
it, `serve` makes no relay connection. An older binary refuses a version 2
file instead of ignoring the object.

```json
{
  "schema_version": 2,
  "layer": "remote",
  "adapters": [{"kind": "codex", "target": "codex-work", "config": {"socket": "...", "thread": "..."}}],
  "relay": {
    "url": "wss://relay.example",
    "shares": [{"target": "codex-work", "session": "work", "owner_pubkey": "<64 lowercase hex>"}]
  }
}
```

Each share binds one declared target to the body key enrolled under
`amq-remote share --session <session>`. `share --target --relay` writes this
block (see [Strict share in one owner step](#strict-share-in-one-owner-step));
do not edit it by hand. The target must already be an adapter in the
manifest. `url` must be `wss://`; `ws://` is accepted only for a loopback
host. A target or session can be shared once.
`commands` needs `dm_channel_id` and `native_session_id`; `activity` needs
`native_session_id`. The optional `relay_self` pins the relay's NIP-11
`self` key (64 lowercase hex); without it, `serve` reads the key from the
relay's NIP-11 document and refuses a redirect.

`serve` keeps one connection per share. It answers the relay's NIP-42
challenge with a kind 22242 event signed by the body key, carrying exactly
one enrolled NIP-OA tag: the owner's kind 1059 grant. The connection counts
as authenticated only after the relay's positive OK for that event. On each
connect, and every minute while connected, `serve` re-reads the enrolled
generation, so a renewal is picked up. The connection closes at the grant's
signed expiry. An enrolled owner that differs from `owner_pubkey` stops
authentication. The first body `serve` loads stays pinned: a different body
enrolled under the same owner is refused until `serve` restarts. A lost
connection reconnects with backoff of 1 to 30 seconds. Local IPC and AMQ delivery keep working while the relay is down.

`serve` never mints, renews or enrolls a key. Use `amq-remote share` for
that.

What the relay checks: at NIP-42 AUTH, a Buzz relay verifies the owner
signature on the NIP-OA tag and refuses self-attestation. Relays that include
block/buzz#7004 also refuse a grant whose `created_at` bounds have passed;
older relays accept a signed grant until the owner or the body key is removed
from the relay. No relay checks `kind=` conditions at connection admission,
so the per-kind limit is enforced by `amq-remote` itself, not by the relay.
Treat a body key and its tags as one credential: to revoke it, remove the
owner's relay membership or ban the body key on the relay. A socket that is
already open stays open until it reconnects.

### Strict share in one owner step

The strict relay path uses per-kind, expiring owner grants and works while
Buzz Desktop is off. The owner signs every grant in one run of
`amq-owner-sign`, a separate binary that keeps the owner key in its own
process. `amq` and `amq-remote` never read the owner key.

```bash
amq-remote share --session work --enable buzz-dm > share.txt
amq-owner-sign --share share.txt --out bundle.json --relay wss://relay.example
amq-remote share --session work --bundle bundle.json --target codex-work \
  --relay wss://relay.example --dm-channel <channel> --native-session <id>
amq-remote up
```

1. `share` mints the body key and prints one preimage per kind for a new
   window. Add `--enable buzz-profile` for [presence](#presence-in-buzz-desktop).
2. `amq-owner-sign` checks each printed preimage against the body and
   conditions. The signer shows what it signs: the session, the body pubkey,
   and each kind with its expiry date. Check the body pubkey against the
   share output before you confirm by typing its first 8 characters. Only
   then does it read the owner key (`nsec1...` or 64 hex) from the terminal
   without echo. It signs every grant, writes one bundle, mode 0600, that
   holds only the public signed tags, and then publishes the owner's kind
   30177 policy for the body on the relay. If the publish fails, the bundle
   is already written; run it again with the same flags.
3. `share --bundle` checks every tag the same way `--tag-file` does and
   enrolls the whole bundle, or nothing. With `--target` and `--relay` it
   writes the relay block in the same locked step. It copies the owner
   public key from the tags. `--dm-channel` and `--native-session` turn on
   [owner DM commands](#owner-dm-commands); they need the buzz-dm grants in
   the bundle. `--presence <name>` turns on
   [presence](#presence-in-buzz-desktop).

The owner can open the DM only after the bundle is enrolled. Then run
`share --session work --target codex-work --relay wss://relay.example
--dm-channel <channel> --native-session <id>` without `--bundle`. It writes
the binding under the enrolled grants: nothing to sign, and `--dry-run`
shows the bind without writing it. `--presence` and `--min-evidence` work
the same way.

Renewal is the same owner step: `share --session work --renew > share.txt`,
one `amq-owner-sign` run, and one `share --bundle` with the same `--target`
and `--relay`. With `--target`, a bundle signed by a different owner is
refused before anything is enrolled. Without `--target`, `share --bundle`
enrolls the bundle and leaves the manifest unchanged. The policy event is
addressed by the body key, so publishing it again replaces it.

The body cannot open a DM channel. Its grants cover the kinds in
`ShareKinds` plus the opt-in surfaces, and none creates a channel. The owner
opens the one-to-one DM with the body from the Buzz client and passes its
channel id with `--dm-channel`.

### Presence in Buzz Desktop

With `share --presence <name>` (1 to 64 printable characters, no path
separator), which writes `"presence": true` and `"name"`, `serve` publishes the body's Buzz profile and status so
the owner's Buzz Desktop can list it as an owned agent.

- Enroll the profile kind first with `amq-remote share --session <session>
  --enable buzz-profile`. The kind 0 profile carries the owner's one kind 0
  grant; without that grant no profile is published.
- The kind 10100 status is `online` while the endpoint has the target
  attached and `away` otherwise. A graceful shutdown publishes `offline`. A
  crash cannot, so `offline` is never a liveness claim.
- Desktop also needs the owner's own kind 30177 policy, with `d` set to the
  body's public key. The relay accepts an event only from the key that
  authenticated, so the owner publishes that policy, not `serve`:
  `amq-owner-sign` does it in the owner step. `serve` reads it and reports
  `policy_present` or `policy_missing`.

The name and status are clear text on the relay. Do not put a path or
prompt data in `name`.

### Activity in Buzz Desktop

With `"activity": true` and `"native_session_id": "<id>"`, `serve` exports
the pinned session's turns, messages and tool calls to the owner as NIP-44
encrypted kind 24200 frames, which the Buzz Desktop agent session panel
renders. For Codex the id is the thread id. A Codex turn streams live,
whether it started in the Codex TUI or from Buzz: the user prompt, the
assistant text as it is written, each command with its output so far, and
the plan. Deltas for one item are merged into one frame per 100 ms, so a
fast stream stays well under the 100 frames per second cap. Approvals are
not shown. For Claude the id is the registry `sessionId`, and the activity
comes from the transcript the adapter already parses, one message at a
time; a prompt sent from Buzz shows as the text the owner typed. Export
runs only while the target's attached native
session is the pinned one, and stops the moment it is not. Activity carries
no control: nothing in it can submit, cancel or approve. A frame the relay
may or may not have accepted is never sent again. Codex and Claude targets
export activity; pi targets do not.

### Owner DM commands

With `"commands": true`, `"dm_channel_id": "<channel>"` and
`"native_session_id": "<id>"`, the owner can operate the shared target from
the Buzz DM channel. `native_session_id` is the native session you approve
for sharing: the Codex thread id (the `thread` in `--discover` output), the
Claude `sessionId` in `~/.claude/sessions/<pid>.json`, or the pi session id
that a bridge of revision 4 or later publishes as `session_id` in
`bridge.liveness`. Commands run only while the target's attached session
has that id, so a different session under the same target is never shared
by inheritance. Enroll the DM kinds first with `amq-remote share --session
<session> --enable buzz-dm`; without them the surface stays closed and no
command runs. `share --dm-channel <channel> --native-session <id>` writes
these three fields.

Owner commands submit with evidence floor `admitted` by default. A target
whose adapter proves only delivery, such as pi or Claude (`submitted`),
refuses those submits. `"min_evidence": "submitted"` on the share, written by `share
--min-evidence submitted`, accepts the weaker evidence. Until the request
ends, the result row says so: once the request runs, that it reached the
session and its start is not proven; before that, only that the share
accepts submitted evidence. An approval message that stops taking a remote
answer is edited to say so, and a reaction on it then answers nothing.

| Owner sends | Result |
| --- | --- |
| Plain text | One submit to the target. The body replies with one result row and edits that row as the request changes. |
| `/inspect` | The target's session state. |
| `/status <ref>` | The state of a request that this channel submitted. |
| `/cancel <ref>`, or ❌ on a result row | Cancels that request. |
| ✅ or ❌ on an approval message | Approves or rejects that pending approval. ✅ is offered only for a command the message shows whole; a file change, a network or permission grant, or a shortened command is approved in the terminal. The first answer, in Buzz or in the terminal, wins, and the message is edited with the outcome. A Codex permission, input or MCP elicitation request shows with no reaction to take: answer it in the terminal. Codex targets with `approve`, and pi targets whose bridge is revision 4 or later. Claude targets with `approve` and the PermissionRequest hook; ✅ only when the hook pins the owner. |
| `yes` or `no` typed in the DM (also `y`, `n`, `approve`, `reject`, ✅, ❌) | Answers a pending approval as ✅ or ❌ does. In the approval message's thread it answers that approval; in the main DM it answers the target's pending approval once the owner could have seen it. A Claude approval takes a typed yes only in its thread; a yes in the main DM is not sent, and the reply says how to allow. |

With `"mention_channels": ["<channel>", ...]` (at most 16, commands
required), an owner message in one of those channels that mentions the body
submits a plain prompt too. A leading `nostr:npub1…` mention is dropped from
the prompt. The result row goes to the DM channel, never to the mentioning
channel. Slash commands work only in the DM.

The surface opens only when the relay's own key signs the channel's NIP-29
membership (kind 39002) as exactly the owner and the body, and its metadata
(kind 39000) as private and of type `dm`. `serve` reads the membership and
the native session again every minute and closes the surface on the first
read that no longer verifies. These are stored relay snapshots, not a read
of current membership, so a change can show late; no bound on the delay is
claimed.

The body signs a kind 9 or 40003 event only when the enrolled generation has
the owner's grant for that kind. It attaches that grant as the event's
NIP-OA tag. The relay does not enforce these grants; `serve` does, and it
checks them again before it sends owed output. Each owner event is claimed
once and its outcome recorded, so a redelivered DM never runs again, even
after a busy rejection. Output owed from an earlier channel or body is kept,
never redirected.

Privacy: Buzz DM content is not end-to-end encrypted. The relay operator can
read the prompts and the result rows. Do not share a session whose prompts or
results the relay operator must not see.

## Link

A link connects this root to a server the user signs tasks in (for example a
web assistant). The machine dials the server; the server never connects in.
Protocol: `amq.remote.link/1` ([contract](../../testdata/link/README.md)).

1. In the server, create a link code and a consent passkey. It shows a code
   and the passkey fingerprint.
2. `amq-remote link add NAME URL --code CODE` mints this root's device key,
   redeems the code, and asks for the fingerprint. Only a typed match pins the
   consent key; the server can never add one. The manifest gains a `links`
   entry (`schema_version` 2).
3. In the session to share, run `amq-remote attach --self --link NAME`
   (`--consent passkey` by default, or `local`; `--tools read` by default).
   The share names the session's binding.
4. `serve` (under `up`) keeps the link up. A revision counts as published only
   after the server acknowledged that it committed it; until then the store
   still owes it and offers it again. A revision the server holds with another
   digest is a conflict: never sent again, counted in `link status`.
5. To sign with another passkey (a second device), create it in the server
   and run `amq-remote link keys add NAME --code CODE`, typing its
   fingerprint. The server can remove a key, never add one.
6. `amq-remote link status` shows each link's socket, generation, owed
   revisions, conflicts and retired sinks. `amq-remote link remove NAME`
   retires the link's sink and deletes its device key; the records it created
   settle without network.

State, per root under `<root>/extensions/remote/link/`: `store_id` (minted
once), `<name>/device.key` (Ed25519, 0600, never overwritten),
`<name>/link.json` (the pinned server id, URL and signing origin),
`<name>/consent_keys.json` (public keys only), `<name>/status.json`, and
`retired/<sink>`. A server that closes with 4010 revokes the device: the
endpoint retires the sink and deletes the key. Close 1012 (server restart)
reconnects after 0-60 s; 45 s without a ping reconnects too.

A session shared with a link never gains answering authority: the server sees
the agent's prompts and cannot answer them.

## Flags

Flags may appear before or after the positional argument.

### Common

| Flag | Default | Commands |
| --- | --- | --- |
| `--root DIR` | `AM_ROOT` | all except `version` |
| `--json` | false | `sessions`, `inspect`, `submit`, `status`, `wait`, `cancel`, `requests`, `doctor` |

### `serve` and `up`

`up` forwards these to the `serve` child and adds its own flags below.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--me HANDLE` | `remote` | Endpoint mailbox handle registered in the root. |
| `--fake` | false | Append the fake runtime as target `fake`. |
| `--codex-socket PATH` | empty | Unix socket of a running Codex app-server. |
| `--codex-thread ID` | empty | With `--codex-socket`, attach only this thread. |
| `--codex-approve` | false | Answer approvals of runs this endpoint submitted to those Codex threads from remote clients. |
| `--manifest PATH` | `<root>/extensions/remote/manifest.json` | Adapter manifest path. |
| `--discover` | false | List candidates and exit. |
| `--poll DURATION` | `500ms` | Import and reconciliation interval. |

### `up` only

| Flag | Default | Meaning |
| --- | --- | --- |
| `--registry PATH` | keepalive companion registry | Registry file for this supervisor. |
| `--max-restarts N` | `0` | Respawns before giving up. `0` means unlimited. |
| `--self PATH` | this executable | `amq-remote` binary used to spawn `serve`. |

`up` takes no positional arguments. A flag `serve` does not define, other
than `--self`, `--registry`, and `--max-restarts`, is exit 2.

### `submit TARGET`

Exactly one of `--text`, `--text-file`, or `--stdin` is required. An empty
prompt is exit 2.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--text STRING` | empty | Prompt text. |
| `--text-file PATH` | empty | Read the prompt from a file. |
| `--stdin` | false | Read the prompt from stdin. |
| `--busy reject\|queue` | `reject` | Accepted values. `queue` is disabled in v1: the endpoint returns `unsupported` (exit 6) before any durable write. |
| `--deliver turn\|steer` | `turn` | Accepted values. `steer` is disabled in v1: the endpoint returns `unsupported` (exit 6) before any durable write. |
| `--request-id UUID` | new UUID | Retry identity. A repeat reconciles instead of submitting twice. |
| `--admit-within DURATION` | `2m` | Latest admission time relative to now. Must be inside `(0, 24h]`. |
| `--min-evidence admitted\|submitted` | omitted | Minimum submit evidence. Omitted keeps legacy admission. |
| `--epoch EPOCH` | live inspect | Previously verified epoch. Required to enqueue while the endpoint is down. |

`submit` does not spool until it has an epoch. With no `--epoch` it inspects
the live target first, and a failed inspect writes nothing. With a previously
verified `--epoch`, an unreachable endpoint still exits 0 and returns the
spool receipt. Restart dispatches that envelope only while its admission
window is still open; a closed window expires it without dispatch.

### `status`, `wait`, `cancel`

Each takes one `REQUEST_REF`. `status` also accepts a bare request id that is
still in the sender spool.

| Flag | Command | Default | Meaning |
| --- | --- | --- | --- |
| `--timeout DURATION` | `wait` | `0` | Give up after this long. The work continues. `0` means no limit. |
| `--limit N` | `requests` | `20` | Most recent request-store records to show. |

`wait` interrupted by SIGINT exits 130. The request keeps running.

### `attach`, `detach`

`amq-remote attach --self` binds this session's Buzz agent (amq-acp in binding
mode) to this session's AMQ handle, `AM_ME` under `AM_ROOT`. Each Buzz DM then
arrives as an AMQ message from `buzz`, and the handle's `amq reply` is the
answer; a `kind: status` reply is progress. No endpoint, hook, or wake is
needed; noticing the message is the handle owner's business. A session that
is not on AMQ is refused and pointed at `--native`.

`amq-remote attach --self --native` binds the exact native session instead. Typing it is the sharing choice, so
the session is found exactly and never guessed from a discovery list. Claude
is found by the command's process ancestry. Codex is found by
`CODEX_THREAD_ID` among the threads loaded in the app-server daemon. Attach
registers the target in the running endpoint without a restart and adds it
to `manifest.json`. When no endpoint runs, it starts `amq-remote up` in the
background (log: `up.log` in the state directory). Claude needs the Stop hook
on this path.

Each attach writes a **named** binding, `~/.amq/remote/bindings/<name>.json`
(default `<handle>-<project>`, or the native target; `--name` overrides), and
removes any other binding for the same session. Several sessions can be
connected at once. Each has its own Buzz agent, whose ACP model
`amq-remote:<name>` selects that binding (`amq-acp setup --session <name>`
writes its Import file).

`amq-remote detach` needs one scope: `--self` removes the binding that names
this session, `--name <name>` removes the binding of that name, and `--all`
removes every binding. The session and the endpoint keep running.

A defaulted name that another session already holds is refused, naming
`--name <unique>` as the remedy; attaching the same session again is
idempotent. An explicit `--name` replaces the binding of that name.

### `share`

| Flag | Default | Meaning |
| --- | --- | --- |
| `--session ID` | empty | Shared session id. Required. |
| `--renew` | false | Reprint preimages for a fresh attestation window, for every kind. |
| `--days N` | `30` | Attestation window in days. Range `1..90`. An explicit `0` is exit 2. On `--renew`, an explicit `--days` that would not exceed the current window bounds is refused. |
| `--tag-file PATH` | empty | JSON file with one owner-signed tag: `kind`, `owner_pubkey`, `conditions`, `sig`. Enrolls that tag. | 
| `--bundle PATH` | empty | JSON array of those tags, one file for the whole window. Enrolls every tag. Not combined with `--tag-file`. |
| `--target ID` | empty | Adapter target written into the manifest relay block. Set with `--relay`. |
| `--relay URL` | empty | Relay `wss://` URL written into the manifest relay block. Set with `--target`. The owner public key is copied from the enrolled tags. |
| `--dm-channel ID` | empty | The owner's Buzz DM channel id. Set with `--native-session` and `--target`. Writes `dm_channel_id`, `native_session_id` and `"commands": true` into the share. Refused unless the tags hold the buzz-dm kinds. |
| `--native-session ID` | empty | The native session id approved for sharing. Set with `--dm-channel`. |
| `--min-evidence CLASS` | empty | Submit evidence floor for owner commands: `admitted` (the default when unset) or `submitted`. Writes `min_evidence` into the share. Set with `--target`. |
| `--enable SURFACE` | empty | Request the kinds of `buzz-dm` (9, 40003) or `buzz-profile` (0) in the new window. Repeatable. |
| `--dry-run` | false | Print what a real run would do. Writes nothing. |

## Exit codes

The usage banner names `0`, `1`, `2`, `3`, `4`, `6`, and `130`. The mapper
in `internal/remote/protocol` returns those codes. It does not return 5.

| Exit | Meaning |
| --- | --- |
| 0 | Success. A `submit` or `status` snapshot that is still running is also 0. An unreachable endpoint is 0 only after `submit` has spooled an envelope. |
| 1 | The snapshot is `failed` or `cancelled`, or the spool envelope is `failed` or `expired`. An error that is not a typed refusal is 1 when the command did not choose an exit code. |
| 2 | Usage. Includes a bad flag, a relative or missing root, manifest validation, and a duplicate target. |
| 3 | `not_found`. |
| 4 | `wait` reached `--timeout`, or `wait` is looking at a snapshot that is not `completed`, `failed`, `cancelled`, `rejected`, or `uncertain`. |
| 6 | Action required. Refusal codes: `busy`, `unsupported`, `unshared`, `expired`, `stale_epoch`, `request_conflict`, `storage_full`, `attachment_lost`, `result_expired`, `already_resolved`, `endpoint_already_running`, `endpoint_unreachable`, `store_closed`, `draining`. A snapshot in `rejected` or `uncertain` is also 6. `doctor` uses 6 when the state directory is missing or the endpoint is not reachable. `share` returns 6 for symlink, key-loading, and mint failures, and that explicit code is kept. |
| 130 | `wait` was interrupted. The request keeps running. |

## Doctor

`amq-remote doctor --root <absolute-root>` reports the state directory, the
socket path, whether the endpoint answered `session.list`, record counts by
state, and any persisted adapter refusals. No state directory yet means the
endpoint has never been started: the report says to run `amq-remote serve`
once. `amq-remote up` starts that same `serve` child.

With a relay configured, doctor also reports each share's connection state
as `serve` last wrote it (`auth_pending`, `authenticated` or `unavailable`),
its DM surface under `commands`, and its `presence` and `discovery`.

A Codex target with `approve` whose thread runs Codex's automatic approvals
reviewer (`approvals_reviewer` `auto_review` or `guardian_subagent`) is
listed under `approval_reviewer`: that reviewer answers the thread's
approvals before a remote client sees them. It is the owner's setting, so
doctor reports it and does not fail. A new thread started with
`approvals_reviewer = "user"` sends its approvals to Buzz.

`failing` lists each broken boundary with a subject, the detail, and a
remedy. Doctor exits 6 exactly when `failing` is not empty.

| Boundary | Fails when |
| --- | --- |
| `endpoint` | No state directory yet, or the endpoint does not answer `session.list`. |
| `amq_route` | The endpoint handle (`--me`, default `remote`) is not listed in the root's `config.json`, so other agents cannot route to it. |
| `registration` | An adapter was refused at startup, or `refusals.json` is unreadable. |
| `native_capability` | A Claude target is attached but `~/.claude/settings.json` has no AMQ Stop hook, or a Claude target with `approve` has no AMQ PermissionRequest hook, so Buzz cannot block its tool calls. Doctor reads only that file; when it sets `disableAllHooks`, doctor reports `claude_stop_hook` or `claude_approval_hook` instead, because project or managed settings decide the effective state. |
| `body_key` | A share's body key or enrolled generation cannot be used. |
| `tag_expiry` | A share's enrolled grants have expired. |
| `relay_auth` | A share is not `authenticated`. |
| `dm_surface` | A commands share's DM surface is not `subscription_active`. |
| `publication` | Owed DM output could not be sent yet. |
| `presence` | A presence share has not published `online` or `away`. |
| `activity` | An activity share is not `exporting`. |
| `discovery` | The owner has not published the kind 30177 policy for the body. |

`authenticated` means the relay accepted this body's AUTH. It does not prove
that the relay materialized the owner binding or that any viewer is ready.

`publication_refused` is advice, not a failure: it names each owed DM
output whose last attempt the relay refused, with the relay's reason, the
number of refused attempts and the next try. A refusal describes one
attempt, so the output stays owed and is retried with a backoff (30 seconds,
doubling to 10 minutes). Order is kept per request: while a request's
earlier output is owed, its later outputs wait; other requests' outputs
still go out. Do not send the message again from Buzz: that starts a new
request.

`stale_harness` is advice, not a failure: it is reported under `notes` and
never makes doctor exit non-zero. A different identity can also be a
byte-identical copy or another binary with the same name, so the note asks
the operator to decide, naming the running process and the installed path.
A process whose executable identity cannot be read (lsof unavailable, a
2-second probe timeout, or a first text record that is not a readable
amq-acp path) is *unknown*, not current: it never counts as fine, and a
single `stale_harness` note names every affected pid with the reason, so a
stale process never just vanishes from the report. The whole probe runs
under one 3-second total deadline (ps included) with a 500ms WaitDelay, so
a stalled child cannot extend it.

Submit writes one frame to the target session's cross-session socket. The
socket answers nothing, so the ladder comes from the session's transcript:
the entry that delivers the frame is `submitted`, the next assistant entry
in that turn is admitted, and a Stop event ends the run with the turn's last
assistant text as the result. The Stop event needs the hook:

```text
amq-remote claude install-stop-hook     # adds a Stop hook to ~/.claude/settings.json
amq-remote claude uninstall-stop-hook   # removes only that hook
```

Without the hook a request is admitted but never completes. Install edits
`~/.claude/settings.json` in place and keeps every other byte. Uninstall
removes only the AMQ hook and leaves any empty `Stop` or `hooks` container
behind. The hook command itself always exits 0, so a broken bridge never
blocks Claude Code. After an endpoint restart, a request that was in flight
reads `uncertain`.

### Approvals from the Buzz DM

A Claude target with `"approve": true`, shared by a relay share with
commands and `native_session_id`, shows a tool approval of a request that
the share submitted in the owner's DM. ❌ on it denies that one call. ✅
allows it, but only when the hook pins the owner's public key. It needs one
more hook:

```text
amq-remote claude install-approval-hook [--owner <hex|npub>] [--root <dir>] [--session <name>]   # adds a PermissionRequest hook to ~/.claude/settings.json
amq-remote claude uninstall-approval-hook                                                     # removes only that hook
```

Without `--owner` the installer pins the one owner of the relay shares in
the manifest (`--root` or `AM_ROOT`, or `--manifest`). With no owner found
it installs a reject-only hook. With an owner it pins that owner's one
relay share with a DM channel (or the `--session` one), as the manifest and
the enrolled credentials name it at install time, on the hook's command
line: `... claude permission-hook --wait 600 --owner '<64 hex>' --root
'<AMQ root>' --session '<name>' --relay '<relay URL>' --body '<body
pubkey>' --channel '<DM channel>' --target '<target>'`. Claude Code asks
before Claude edits its own settings file, so Claude cannot change the pin
on its own. The hook never reads the manifest: the root and session only
locate the enrolled body secret it signs in with, whose pubkey must be the
pinned body. A pin that misses any flag is reject-only. Run the installer
again after the share's relay, channel or body changes.

The hook has no matcher, a 600 second wait, and a 630 second timeout. It
exits at once with no output, so the terminal dialog decides, unless every
check passes: the session has a pin that a live `amq-remote serve` wrote for
that share, and the call belongs to a prompt that an AMQ request delivered
as its own turn. A request that Claude absorbed into a running turn gets no
DM approval. The hook never exits 2. It denies for the owner's ❌ on that
exact call, and the turn goes on.

Any same-user process can write the answer file, so an allow in it proves
nothing by itself. The hook prints allow only when all of these hold:

- its command line pins an owner and a whole share;
- the call is a Bash call with only `command`, `description`, `timeout` and
  `run_in_background`, shown whole: nothing hidden, shortened, or removed
  for display;
- the answer carries the owner's kind 7 reaction ✅, or the owner's kind 9
  typed approve (`yes`, `y`, `approve` or ✅) in the share's DM channel,
  whose id and BIP-340 signature verify under the pinned key, dated from 30
  seconds before the request to its deadline;
- the reaction's last `e` tag, or one of the typed reply's `reply`, `root`
  or unmarked `e` tags, is the approval message, whose id and
  signature verify, which the share's body key signed in the share's DM
  channel, and whose content is the approval text for the hook's own
  rendering of the call under a request of the share's target: the
  command preview, then `Interaction: <id>` and `Action: <sha256 of the
  call>`;
- the hook's own read of the pinned relay, signed in with the share's
  body key as `amq-remote serve` is, each read to the end of stored events
  within 10 seconds, finds every way a Buzz client changes or hides the
  message harmless: each edit (kind 40003, by the body or by the owner,
  whom Buzz lets edit an agent's message) shows the same call with only
  another answer line or outcome, no deletion (kind 5, or the Buzz-native
  kind 9005) names the message or one of its edits, the body sent no
  deletion ever, and the owner sent no deletion since the message, with or
  without an `h` tag. The relay hides a deleted edit, AMQ never deletes,
  and a deletion's time is what its signer claims;
- the approval is still open when the hook checks, both before and after
  it verifies: its deadline has not passed, Claude has not ended the hook,
  and nothing else closed it. One allow applies once.

On a missing pin, any failed check, an error, or a timeout the hook prints
no allow. The endpoint runs the same verification before it passes a ✅ to
the hook. When it fails, nothing is answered and the DM says why:
"The approval message was altered after it was posted; check the
terminal." for an edit that changed the call or a deletion, and "Could not
verify the approval: <reason>. React again to retry." otherwise (a relay
error or timeout, for example). A later ✅ or a ❌ still works. When the
endpoint's check passes and the hook's own check then fails, the hook
records that it refused exactly that proof: the answer never applies, the
DM gives the same reply with the hook's reason, and a later ❌ or a new ✅
replaces it. The hook allows a proof only when no refusal of it is
recorded. While one answer to an approval is checked, a second one waits
for it.

The DM offers ✅ only when the call can pass these checks, the
installed hook pins the share's owner, and the approval is bound to one
tool call in the transcript; otherwise it offers ❌ only.

The pin holds only while Claude cannot change settings or run commands
without a prompt. Each of these defeats it: a click on "allow Claude to edit
.claude for this session", `bypassPermissions` mode, or a `permissions.allow`
rule that allows every Bash command. `amq-remote doctor` reports the pinned
owner and share and warns on the last two in `~/.claude/settings.json`.

The relay is trusted to return the full edit and deletion history of the
message. A same-user process that can replace the installed `amq-remote`
binary, or edit the Claude settings with the owner's consent, defeats this
protection.

The message shows the call whole, or not at all: a call that may contain
a secret anywhere shows "Command hidden: it may contain a secret. Check the
terminal.", and a call too long to show whole shows "Command too long:
check the terminal." ❌ still blocks either; ✅ is never offered. The first
answer wins, in Buzz or in the terminal. When the hook stops before it
records that its decision was written whole, the message says the answer
may not have reached the terminal (`delivery_unknown`). A terminal reject
stops the hook at once.
A terminal approve does not signal the hook, so the endpoint detects it
from the call's `tool_result` in the transcript and edits the message to
say it was answered outside Buzz. When two identical calls are waiting in
one turn, the approval cannot be bound to one of them, ✅ is not offered,
and the first `tool_result` among them closes it. Another PermissionRequest
hook that decides can answer first; AMQ reports only what its own hook
printed.
