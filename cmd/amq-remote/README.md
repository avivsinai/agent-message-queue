# amq-remote

`amq-remote` attaches to a harness session that is already running. Run
`inspect` on the target and use only the operations and evidence it
advertises. A declared harness does not imply `submit` or `cancel`. It is a
separate binary on purpose: `amq` itself gains no socket. Design invariants live in
[the remote-control ADR](../../docs/adr-remote-control.md). Pinned harness
seams live in [the compatibility manifest](../../docs/remote-compat.md). This
page is the operator reference for the binary.

## Install

Homebrew's `amq` formula does not install this binary. Download the matching
`amq-remote_*_{linux,darwin}_{amd64,arm64}.tar.gz` release asset and verify
its checksum. See [INSTALL.md](../../INSTALL.md). `amq upgrade` leaves this
binary unchanged; update it from the release asset.

`make build` also produces a local `amq-remote` next to `amq`.

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
| `claude` | `pid` (Claude Code process id). Optional `home` overrides the Claude home directory. | `Inspect` and `Submit` over the session's cross-session socket. `CancelRequest` and `Steer` are false. Unsupported on Windows. See [Claude Code](#claude-code). |
| `codex` | `socket` and `thread`. Optional `approve` advertises `ApproveTool`. | `Inspect`, `Submit`, and `CancelRequest`. `Steer` is false. |
| `amit` | `handle`. The Amit extension directory for that handle must already exist under the root. | `Inspect` and `Submit`. `CancelRequest` is false. Submit evidence is `submitted`. |
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
      "kind": "amit",
      "target": "amit-1",
      "config": {"handle": "amit-pi"}
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
`amq-remote share --session <session>`. `url` must be `wss://`; `ws://` is
accepted only for a loopback host. A target or session can be shared once.
`activity` is refused. `commands` needs `dm_channel_id`. The optional
`relay_self` pins the relay's NIP-11 `self` key (64 lowercase hex); without
it, `serve` reads the key from the relay's NIP-11 document and refuses a
redirect.

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

### Presence in Buzz Desktop

With `"presence": true` and a `"name"` (1 to 64 printable characters, no
path separator), `serve` publishes the body's Buzz profile and status so
the owner's Buzz Desktop can list it as an owned agent.

- Enroll the profile kind first with `amq-remote share --session <session>
  --enable buzz-profile`. The kind 0 profile carries the owner's one kind 0
  grant; without that grant no profile is published.
- The kind 10100 status is `online` while the endpoint has the target
  attached and `away` otherwise. A graceful shutdown publishes `offline`. A
  crash cannot, so `offline` is never a liveness claim.
- Desktop also needs the owner's own kind 30177 policy, with `d` set to the
  body's public key. The relay accepts an event only from the key that
  authenticated, so the owner's Buzz client must publish that policy, not
  `serve`. `serve` reads it and reports `policy_present` or
  `policy_missing`.

The name and status are clear text on the relay. Do not put a path or
prompt data in `name`.

### Owner DM commands

With `"commands": true`, `"dm_channel_id": "<channel>"` and
`"native_session_id": "<id>"`, the owner can operate the shared target from
the Buzz DM channel. `native_session_id` is the native session you approve
for sharing: the Codex thread id (the `thread` in `--discover` output) or
the Claude `sessionId` in `~/.claude/sessions/<pid>.json`. Commands run only while the target's attached session
has that id, so a different session under the same target is never shared
by inheritance. Enroll the DM kinds first with `amq-remote share --session
<session> --enable buzz-dm`; without them the surface stays closed and no
command runs.

| Owner sends | Result |
| --- | --- |
| Plain text | One submit to the target. The body replies with one result row and edits that row as the request changes. |
| `/inspect` | The target's session state. |
| `/status <ref>` | The state of a request that this channel submitted. |
| `/cancel <ref>`, or ❌ on a result row | Cancels that request. |

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
| `--codex-approve` | false | Advertise `approve_tool` for those Codex threads. |
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

### `share`

| Flag | Default | Meaning |
| --- | --- | --- |
| `--session ID` | empty | Shared session id. Required. |
| `--renew` | false | Reprint preimages for a fresh attestation window, for every kind. |
| `--days N` | `30` | Attestation window in days. Range `1..90`. An explicit `0` is exit 2. On `--renew`, an explicit `--days` that would not exceed the current window bounds is refused. |
| `--tag-file PATH` | empty | JSON file with one owner-signed tag: `kind`, `owner_pubkey`, `conditions`, `sig`. Enrolls that tag. |
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
as `serve` last wrote it: `auth_pending`, `authenticated` or `unavailable`,
with the last error. Any share that is not `authenticated` makes doctor exit
6. A commands share also reports its DM surface under `commands`:
`subscription_active` when open, or `closed: <reason>` or
`publish_pending: <reason>`. Any other value than `subscription_active`
makes doctor exit 6. A presence share reports `presence` (`online`,
`away`, or why nothing was published) and `discovery` (`policy_present`, or
`policy_missing` when the owner has not published the kind 30177 policy);
anything else than `online`, `away` and `policy_present` makes doctor exit
6.

`authenticated` means the relay accepted this body's AUTH. It does not prove
that the relay materialized the owner binding or that any viewer is ready.

## Claude Code

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
