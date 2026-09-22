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
| `claude` | `pid` (Claude Code process id). Optional `home` overrides the Claude home directory. | `Inspect` only. `Submit` and `CancelRequest` are false. |
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

`--discover` lists discovered candidates as `<kind> <target>` and exits
before attaching anything.

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
`commands` and `activity` are refused: this binary authenticates on the
relay and does nothing more yet.

`serve` keeps one connection per share. It answers the relay's NIP-42
challenge with a kind 22242 event signed by the body key, carrying exactly
one enrolled NIP-OA tag: the owner's kind 1059 grant. The connection counts
as authenticated only after the relay's positive OK for that event. On each
connect `serve` re-reads the enrolled generation, so a renewal is picked up.
An expired grant, or an enrolled owner that differs from `owner_pubkey`,
stops authentication. A lost connection reconnects with backoff of 1 to 30
seconds. Local IPC and AMQ delivery keep working while the relay is down.

`serve` never mints, renews or enrolls a key. Use `amq-remote share` for
that.

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
| `--dry-run` | false | Print what a real run would do. Writes nothing. |

## Exit codes

The usage banner names `0`, `1`, `2`, `3`, `4`, `5`, `6`, and `130`. The
mapper in `internal/remote/protocol` returns the codes below. It does not
return 5; the banner's "context mismatch" line is not a path in that mapper.

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
6. `authenticated` means the relay accepted this body's AUTH. It does not
prove that the relay materialized the owner binding or that any viewer
is ready.
