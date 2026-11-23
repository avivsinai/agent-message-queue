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

A running endpoint runs the binary it started with. After an upgrade, stop
its `amq-remote up` (Ctrl-C, or SIGTERM to its pid) and run `amq-remote up`
again. A command that needs the upgraded endpoint, such as `link add`, says
"restart amq-remote up first". `amq-remote doctor` notes an endpoint that runs
another version than the installed binary, with the command that restarts it.

`make build` also produces a local `amq-remote` and `amq-owner-sign` next to
`amq`.

## Where state lives

The endpoint owns `<root>/extensions/remote/`. `amq cleanup` does not remove
extension data. The IPC socket is `<root>/extensions/remote/endpoint.sock`.
Adapter declarations are `<root>/extensions/remote/manifest.json` unless
`--manifest` points somewhere else.

`--root` is required for every command that opens that directory, except the
link commands. It defaults to `AM_ROOT` and must be absolute. A missing or
relative root is exit 2. A link command without `--root` finds its link in the
machine root, `~/.amq/remote/root`, or in `AM_ROOT`.

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
amq-remote requests release REQUEST_REF [--yes]
amq-remote share --session ID
amq-remote machine request [--valid-for DURATION] [--name NAME] [--agent PUBKEY]
amq-remote machine enroll --grant FILE
amq-remote attach --self [--native | --link NAME]
amq-remote detach --self | --name NAME | --all
amq-remote bindings
amq-remote dialogs [--days N] [--json]
amq-remote channel reset NAME
amq-remote mirror on|off|status --self | --name NAME
amq-remote link add NAME URL --code CODE
amq-remote link keys add NAME --code CODE
amq-remote link local-key NAME
amq-remote link confirm ID
amq-remote link remove NAME
amq-remote link status
amq-remote link tools [--link NAME]
amq-remote link call TOOL --args JSON [--link NAME]
amq-remote mcp --link NAME
amq-remote service install|uninstall|status [--root ROOT]
amq-remote doctor [--fix] [--root ROOT]
amq-remote version
amq-remote claude install-stop-hook | uninstall-stop-hook
amq-remote claude install-approval-hook | uninstall-approval-hook
amq-remote claude install-prompt-hook | uninstall-prompt-hook
amq-remote pi bridge-path
amq-remote statusline [--session ID] [--plain]
```

`serve` runs the endpoint in the foreground. `up` supervises `serve`: it
respawns a crashed child with backoff and stops when the child exits 0. A
child exit 6 (action required: its configuration is refused, or another
endpoint holds the root) is not respawned; any other exit, a crash included,
is. A refusal that may pass, such as `storage_full`, `store_closed` or
`draining`, makes `serve` exit 1. A second `up` for the
same root exits 6 (`endpoint_already_running`) while the first supervisor
holds the lifetime lock.

`sessions`, `inspect`, `submit`, `status`, `wait`, `cancel`,
`requests release`, and `doctor` talk to a running endpoint. `status` also reads the local sender spool when
the endpoint has no record yet. `requests` does not use that socket: it reads
the local request store and the sender spool directly, including while the
endpoint is down.
`share` mints or enrolls a session body key and does not start the endpoint.
`machine request` makes the machine agent's key under
`~/.amq/remote/machine/<agent pubkey>/`, or reuses the one key there (pass
`--agent` when there is more than one), and prints a grant request for
`amq-owner-sign --machine FILE --out FILE --relay URL`, which the owner runs in
their own terminal. `--valid-for` is 1h to 8760h (one year); the default is 2160h (90 days).
`machine enroll --grant FILE` checks the signed grant against the agent key
and stores it with the relay URL the owner named when signing; it prints the
agent key, the owner key, the relay and the end date. The
grant lets the agent key act as the owner's Buzz agent for every event kind,
and it ends at the printed date. See [Machine agent](#machine-agent).
`version` prints `amq-remote <version>`. The `claude` commands install and
remove the Claude Code hooks (see [Claude Code](#claude-code)); the usage
banner does not list them.

`--json` prints the result as JSON on stdout, with diagnostics on stderr. The
commands that take it are listed under [Common flags](#common). `serve` and
`up` accept `--json` and print no JSON result. `share` has no `--json`.

## Status line

`amq-remote statusline` prints one status-line segment for a session bound
to Buzz: `📡 buzz`, or `📡 buzz (mirror off)` while the session's mirror is
off. It prints nothing for an unknown session, an ended or detached binding,
or any error, and always exits 0, so a status line never shows an error. It
reads only the binding files: no network, no process scan, no lock, no write.
`--plain` prints `buzz` without the emoji.

The session is `session_id` from the Claude Code status-line JSON on stdin,
else the file stem of its `transcript_path`; `--session ID` names it for any
other harness that runs a command in its status line. The binding of that
session is the one whose recorded session id matches.

AMQ never edits the user's `statusLine` setting. Add one line to your own
Claude Code status-line script, where `$input` holds the JSON it read from
stdin and `$out` is the line it prints:

```bash
seg=$(amq-remote statusline <<<"$input" 2>/dev/null); [ -n "$seg" ] && out+=" | $seg"
```

Elsewhere: `amq-remote statusline --session <id>`.

## Manifest

`manifest.json` is read when `serve` starts; a running `serve` also follows
its relay block (see below). An absent file is an empty
adapter list, which is legal. `schema_version` is `1`. `layer` is `remote`.
Each adapter has a unique `target` (the id you pass to `inspect` and
`submit`), a `kind`, and an optional `config` object.

| Kind | Required `config` | What `inspect` advertises on this tree |
| --- | --- | --- |
| `claude` | `pid` (Claude Code process id). Optional `home` overrides the Claude home directory. Dialogs reach Buzz through the PermissionRequest hook (see [Approvals from the Buzz channel](#approvals-from-the-buzz-channel)). | `Inspect` and `Submit` over the session's cross-session socket. Submit evidence is `submitted`, so a relay share for Claude sets `min_evidence` `submitted`. `CancelRequest` and `Steer` are false. Unsupported on Windows. See [Claude Code](#claude-code). |
| `codex` | `socket` and `thread`. | `Inspect`, `Submit`, and `CancelRequest`. `Steer` is false. |
| `pi` | `handle`; optional `upgrade_hint`, the remedy shown when the live bridge is too old and publishes none. The pi-bridge extension directory for that handle, `agents/<handle>/extensions/pi-bridge/` under the root, must already exist. | `Inspect` and `Submit`. `CancelRequest` only while the live bridge advertises `bridge_revision` 5 or higher. Submit evidence is `submitted`. |
| `acp` | `handle`: the `AM_ME` of a running `amq-acp host`, whose directory `agents/<handle>/extensions/acp-host/` under the root must already exist. | `Inspect` and `Submit` over the host's bridge seam (protocol `amq:acp-host:v1`). `CancelRequest`: `amq-remote cancel` or `/cancel` stops the request's ACP turn, or keeps a request that has not run from running. Submit evidence is `submitted`, so a relay share sets `min_evidence` `submitted`. See [the acp-host wire](../../docs/pi-bridge-protocol.md#the-acp-host-wire). |
| `mailbox` | `handle`: an AMQ handle in serve's own root whose `agents/<handle>/inbox` already exists; the adapter never creates it. Optional `reply_timeout` (default `10m`, at most `24h`). The handle must not be `buzz`, `amq-remote` or serve's `--me`, and `--me` must not be `buzz` or `amq-remote`. Unknown config fields are refused. | `Inspect`, `Submit` and `CancelRequest`. Each request is one AMQ message to the handle: from `buzz` on thread `cockpit/<channel>` with the label `nostr:<event>` for a Buzz DM, else from `amq-remote` on thread `remote/<creator host>`. The handle answers with `amq reply --id <id>`; when the endpoint next looks, the newest of its replies that is not `--kind status` and has a body is the request's result; later replies stay unread in the carrier inbox. With no such reply within `reply_timeout` the request fails, and the message stays in the inbox. A cancel after delivery ends the request as delivered and no longer waited for. Several requests can be open at once; each is its own message, answered by its own id, so a second message never waits for the first or comes back busy. The handle cannot ask the owner a question through the share: a mailbox has no interaction seam. Submit evidence is `submitted`. See [Strict share in one owner step](#strict-share-in-one-owner-step). |
| `fake` | none | Test double, linked into test builds only; a release binary refuses it as an unknown kind. `epoch` is accepted only for this kind. |

`epoch` on any kind other than `fake` is exit 6. A duplicate `target` is
exit 6. An unknown kind is refused at startup; `serve` continues with the
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
      "config": {"socket": "/absolute/path/codex.sock", "thread": "THREAD"}
    },
    {
      "kind": "pi",
      "target": "pi-1",
      "config": {"handle": "pi-agent"}
    },
    {
      "kind": "acp",
      "target": "grok",
      "config": {"handle": "grok"}
    }
  ]
}
```

Two serve flags append to this file's adapter list instead of editing it.
`--fake` adds kind `fake`, target `fake` (test builds only). `--codex-socket PATH` attaches the
daemon's loaded threads, or only `--codex-thread ID` when that flag is set.
Those sugar targets look like `codex:` plus the thread id with hyphens
removed. A target id that appears both in the file and in the flags is
exit 6.

`serve --discover` and `up --discover` list running sessions and exit. They
attach nothing and write nothing, so they work while an endpoint already
owns the root. Each line is the kind, the target, a display name, and a
manifest entry to paste:

```text
claude   claude:12345                             main                 {"config":{"pid":12345},"kind":"claude","target":"claude:12345"}
codex    codex:0198a1b2c3d47e5f8a9b0c1d2e3f4a5b   -                    {"config":{"socket":"...","thread":"0198a1b2-..."},"kind":"codex","target":"codex:0198a1b2c3d47e5f8a9b0c1d2e3f4a5b"}
```

Claude sessions come from `~/.claude/sessions`: interactive, with a live
pid and a messaging socket. A session run with `CLAUDE_CONFIG_DIR` set is
supported: attach from inside it records that absolute directory as the
entry's `config_dir`, and the endpoint reads the session's data there. Codex threads come from the app-server daemon at
`~/.codex/app-server-control/app-server-control.sock`, or from
`--codex-socket` when set. A discovered session is shared only after you
paste its entry into the manifest: discovery is never consent.

When `serve` starts, it prints one line per attached target with its
capabilities and `observed_at`. Under `up`, those lines come from the child
`up` started.

## Relay

A manifest with `"schema_version": 2` may carry one `relay` object. Without
it, `serve` makes no relay connection. A binary without relay support
refuses a `schema_version` 2 file instead of ignoring the object.

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

`serve` keeps one authenticated connection per share, signed in with the
share's body key and the owner's grant until the grant's signed expiry. It
picks up a renewal without a restart. The first body
`serve` loads stays pinned: a different body enrolled under the same owner
is refused until `serve` restarts or the share is removed and added again.
A lost connection reconnects with backoff. Local IPC and AMQ delivery keep
working while the relay is down. The authentication, reconnect, and DM
surface rules are in [the relay connection reference](../../docs/amq-remote-relay.md).

A running `serve` follows the relay block. When the manifest bytes change,
it applies the shares as a restart would:
- A share added starts, with the same enrollment checks, pins and surfaces
  as at startup. A share whose target or enrolled body a running share
  still holds is refused whole until that share is removed or `serve`
  restarts.
- A share removed stops. The other shares are not touched.
- A share whose entry changed in any field keeps its running values. `serve`
  logs once that you must restart `serve`, or remove and re-add the share,
  to adopt the change.
- A changed `url` or `relay_self` needs a restart while a share runs.
- A manifest that does not load keeps every running share and is reported
  once.
- A share surface that was refused at setup is set up again on the next
  manifest change.

`serve` never mints, renews or enrolls a key. Use `amq-remote share` for
that.

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

To share an AMQ handle instead of a harness session, add a `mailbox`
target for it to the manifest and bind the share with `--target <target>
--native-session <handle> --min-evidence submitted`. The owner's DM then
reaches the handle as one AMQ message, and the handle's `amq reply` is the
result row. Buzz Desktop does not need to run.

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
the plan. Approvals are not shown. For Claude the id is the registry `sessionId`, and the activity
comes from the transcript the adapter already parses, one message at a
time; a prompt sent from Buzz shows as the text the owner typed. Export
runs only while the target's attached native
session is the pinned one, and stops the moment it is not. Activity carries
no control: nothing in it can submit, cancel or approve. A frame the relay
may or may not have accepted is never sent again. Codex, Claude and pi targets
export activity. For pi the bridge writes the session's turns, whole messages
and tool calls to `activity/<session_id>.jsonl`; a pi bridge without that file
exports none.

The endpoint socket also serves a local, read-only `activity` stream of one
request while it runs, under the request's native pin. `amq-acp` remote mode
reads it to show the turn to its ACP client while it runs (see
[the amq-acp companion](../amq-acp/README.md#remote-mode)).

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
accepts submitted evidence.

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

The surface opens only while the relay shows the channel as a private DM
whose members are exactly the owner and the body, and closes when that no
longer holds; a membership change can show late. A redelivered DM never runs
again, even after a busy rejection; the busy row says to send the message
again. The exact checks are in [the relay
connection reference](../../docs/amq-remote-relay.md#owner-dm-surface).

Privacy: Buzz DM content is not end-to-end encrypted. The relay operator can
read the prompts and the result rows. Do not share a session whose prompts or
results the relay operator must not see.

## Machine agent

One machine agent per machine runs inside `amq-remote serve`.
It has its own key, a grant the owner signs, and connects
directly to the relay. The owner reads and writes in the Buzz app. No Buzz
desktop app is needed. For each session that attaches it makes a private
channel, adds the owner as an admin and mentions them, and posts that
session's approval dialogs and its mirror. An owner's message in that
channel reaches the session. Until the session answers, the channel shows
the agent as working (a Buzz typing pulse every 3 seconds, for at most
30 minutes).

Set it up once per machine:

1. `amq-remote machine request > request.txt` makes the agent key and
   prints the grant request.
2. The owner runs `amq-owner-sign --machine request.txt --out grant.json
   --relay URL` in their own terminal. The owner key never reaches
   `amq-remote`.
3. `amq-remote machine enroll --grant grant.json` stores the grant.

`amq-remote up`, or the login service that `amq-remote service install`
writes (see [Restart after a reboot](#restart-after-a-reboot)), then runs the
agent: every `serve` follows the enrolled key while it runs and starts the
agent when exactly one key with a valid grant is enrolled. When several
`serve` processes run, one runs the agent and another takes over when it
stops.

The grant lets the agent key act as the owner's Buzz agent for every event
kind until the date that `machine request --valid-for` asks for: 90 days by
default, at most one year. To renew, run `amq-remote machine request` again
(it reuses the key), have the owner sign it, and run `amq-remote machine
enroll --grant FILE`. A running `serve` adopts a renewed grant for the same
key, relay and owner with no restart.

The machine agent keeps a record of each relay write for three days after
its outcome is known. A write with no known outcome stays until it is
known; `amq-remote doctor` notes one that is older than a day.

The machine agent reads each channel's relay-signed state once a minute. A
channel that the owner archived, made public, or left, or that the machine
agent no longer owns, is suspended: nothing is sent there and no message is
taken from it, and `amq-remote bindings`, `attach` and `doctor` say why.
The agent never undoes the owner's change; when the state is good again the
channel works again, and turns that ended meanwhile are not posted.
`amq-remote channel reset NAME` makes a new channel.

One machine agent serves a machine. Two enrolled keys are refused; remove
the directory of the one you do not use.

The key that creates a session channel owns it. Another key of the same
owner has no membership and cannot read or archive it. Before replacing a
key, run `amq-remote detach --all` while `serve` runs with the old key. The
agent archives the channels within about a minute; confirm in Buzz that
they are archived before you replace the key. `amq-remote
doctor` names channels the earlier key's records show as unarchived. The
owner is an admin of each session channel and can archive it in Buzz.

`serve` writes what the agent does to
`~/.amq/remote/machine/<agent pubkey>/status.json`: `state` (`serving`,
`waiting` or `refused`), the refusal `code` and its `line`, which ends with
the next step, `relay_url`, `relay_state`, `relay_error`, `grant_not_after`,
`not_authenticated_since` (present while the relay connection is not
authenticated), `pid` and `updated` (when the file was last written). It is a snapshot that
`attach`, `bindings` and `doctor` read, not a setting. The codes are
`identity_ambiguous`, `no_authority`, `authority_expired`,
`relay_self_unknown`, `relay_self_changed` and `auth_refused`.
A relay that does not answer is `waiting`, with the cause in `line`; the
agent starts when the relay answers.

Limits:

- An image in an owner's message is not downloaded: the session gets an
  `[image not attached: ...]` line for it.
- A channel the owner archives is not unarchived: when that session attaches
  again, it gets a new channel.

## Link

A link connects this machine to a server the user signs tasks in (for example
a web assistant). The machine dials the server; the server never connects in.
Protocol: [`amq.remote.link/1`](../../docs/remote-link-protocol.md).

1. In the server, create a link code and a consent passkey. It shows a code
   and the passkey fingerprint.
2. `amq-remote link add NAME URL --code CODE` mints the device key in the
   machine root, `~/.amq/remote/root`, unless `--root` names another root
   (`AM_ROOT` does not choose it). It redeems the code and asks for the
   fingerprint. Only a typed match pins the consent key; the server can never
   add one. The manifest gains a `links` entry (`schema_version` 2), and the
   endpoint for that root starts when none runs. It prints the commands that
   share a session and use the server's tools (`link tools`, `mcp --link`).
   A server's optional `default_tool_profile` applies when `--tools` is
   omitted (otherwise `read`). `link tools`, `link call` and `mcp --link`
   name the calling native binding when it is shared; an unshared caller
   receives the server's read tools.
3. In the session to share, from any AMQ root or none, run
   `amq-remote attach --self --link NAME` (`--consent passkey` by default, or
   `local`; `--tools read` by default; the server's `default_tool_profile`
   takes precedence when it is set). The share names the session's
   binding and runs through the endpoint of the link's root. The share adds
   no channel to a session that is connected through its AMQ mailbox. One
   link in the machine root serves every session on the machine: a pi session
   in another AMQ root is registered with its own root and is named
   `<handle> · <project>/<session>`. When the server lists
   the tool profiles it grants in its link info, `--tools` must be one of them.
   The endpoint reads the server's link info again each time the link connects,
   so a change on the server needs no restart.
4. `serve` (under `up`) keeps the link up and offers each request revision
   until the server acknowledges it. A revision the server holds with another
   digest is a conflict: never sent again, counted in `link status`.
5. To sign with another passkey (a second device), create it in the server
   and run `amq-remote link keys add NAME --code CODE`, typing its
   fingerprint. The server can remove a key, never add one.
6. A share with `--consent local` waits for you on this machine too: the
   signed task is held until you run `amq-remote link confirm ID` in a
   terminal, read the exact text on the page it opens, and confirm with
   Touch ID. That needs the link's local passkey, offered by `link add` when
   it runs in a terminal, or created later by `amq-remote link local-key NAME`
   while the endpoint runs. The endpoint serves both pages on one loopback
   port it records for the root, and the passkey is bound to that address: a
   look-alike page on another port cannot confirm. When that record is lost,
   the endpoint records a new port, and `link local-key NAME` creates the
   passkey again for the new address; a record it cannot read stops local
   confirmation until it can. A held
   task is dropped at its deadline (10 minutes) or on restart, never resent.
7. `amq-remote link status` shows each link's socket, generation, owed
   revisions, conflicts and retired sinks, and for each share whether the
   server sees it now: `shared`, or why not (`session changed`, `not attached
   now`, `no such binding`, or `ended`) with the step that fixes it. A share
   is announced only while its session is the one attached. The endpoint
   follows a session change in a live process and a proved pi resume. A
   native Claude session resumed in a new process needs
   `attach --self --link NAME` again, which keeps the share's binding name.
   `amq-remote detach --name <name>` drops an entry that has no binding.
   `amq-remote link remove NAME` retires the link's sink and deletes its device key; the records it created
   settle without network. The server keeps the machine until you unlink it
   on the server's connections page. `detach` removes the share's entry, and
   a detached session never comes back by itself.

### Restart after a reboot

Nothing restarts the endpoint after a reboot unless a login service does.
`amq-remote service install` writes one that runs `amq-remote up --root ROOT`
at login (`ROOT` defaults to the machine root) and starts it now, replacing a
running one. It reports started only after it saw the service run; a loaded job
that does not run is started one time, and if it still does not run the command
exits 6 with the state and the command to start it.

- macOS: the launchd user agent
  `~/Library/LaunchAgents/io.github.avivsinai.amq-remote.plist`, logging to
  `ROOT/extensions/remote/up.log` (see [Endpoint log](#endpoint-log)). Over SSH, with no login session to start it
  in, install writes it and it starts at the next login.
- Linux: the systemd user unit
  `$XDG_CONFIG_HOME/systemd/user/amq-remote.service` (default `~/.config`),
  logging to the journal (`journalctl --user -u amq-remote`). A user unit
  runs while you are logged in; for a machine that must serve its links
  before anyone logs in, run `loginctl enable-linger` once.

There is one login service per user. Installing it for another root replaces
the old root's service, and install says so.

The service runs the `amq-remote` found on `PATH` when it is this binary, so a
Homebrew upgrade keeps it working; otherwise install warns which binary the
service runs. A temporary build (`go run`) is refused. The service restarts
`up` only when `up` fails, and `up` keeps its single-instance lock: a manual
`up` for the same root while the service runs exits 6, and while a manual `up`
runs, the service retries every 10 s and takes over when it stops.

`amq-remote service status` reports the root the installed unit runs and its
state: `current` (the unit install would write now), `stale` (another
`amq-remote` binary: install again), `other_root`, or `missing`; and whether
its process runs now. `amq-remote service uninstall` stops the service and
removes its unit; when the stop fails (no login session to reach), it still
removes the unit and says that the running endpoint stops at logout. Neither
command changes a unit it did not write. `doctor` names `service install` when
the root has a link and no current service for it. It also notes a running
endpoint that reports another version than the installed binary (or none), with
`service install` as the command that restarts it when a login service runs
this root, and a restart of `amq-remote up` otherwise. `--fix` does not restart
the endpoint.

A link lives in exactly one root, normally the machine root; its state is
never copied or moved. One link name in two roots is refused. The state
files are listed in [the protocol's endpoint state](../../docs/remote-link-protocol.md#endpoint-state).
When the server revokes the device, the endpoint retires the sink and
deletes the device key.

A session shared with a link never gains answering authority: the server sees
the agent's prompts and cannot answer them. A tool permission prompt is
answered in the session's terminal, and a task that needs one waits there;
`attach --self --link` says so (JSON `"approvals": "terminal"`).

## Flags

Flags may appear before or after the positional argument.
`amq-remote COMMAND --help` prints the flags and exits 0 for the client
commands, `requests`, `attach`, `detach`, `bindings`, `dialogs`, `channel reset`,
`mirror`, the `link` commands, `mcp`, `service`, and `doctor`. `serve --help`,
`up --help`, and `claude install-approval-hook --help` print the flags and
exit 2.
`share --help`, `version --help`, and the other `claude` commands print no
flag list; the `share` flags are in the table below.

### Common

| Flag | Default | Commands |
| --- | --- | --- |
| `--root DIR` | `AM_ROOT`; link commands and `mcp`: the link's root; `service`: the machine root | every command except `dialogs`, `version`, `pi bridge-path`, `claude install-stop-hook`, `claude uninstall-stop-hook`, `claude uninstall-approval-hook`, `claude install-prompt-hook`, and `claude uninstall-prompt-hook` |
| `--json` | false | `sessions`, `inspect`, `submit`, `status`, `wait`, `cancel`, `requests`, `requests release`, `attach`, `detach`, `bindings`, `dialogs`, `channel reset`, `mirror`, the `link` commands, `service`, `doctor`, and `mcp` (errors only; `mcp` always speaks JSON-RPC on stdout) |

### `serve` and `up`

`up` forwards these to the `serve` child and adds its own flags below.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--me HANDLE` | `remote` | Endpoint mailbox handle registered in the root. |
| `--fake` | false | Append the fake runtime as target `fake` (test builds only; a release binary refuses it). |
| `--codex-socket PATH` | empty | Unix socket of a running Codex app-server. |
| `--codex-thread ID` | empty | With `--codex-socket`, attach only this thread. |
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

### Endpoint log

When the output of `up` and its `serve` child is `up.log` in the state
directory (the login service on macOS, or an endpoint that attach starts),
each line starts with the UTC time. A link logs one line when its state
changes (online, or a new failure), not one per redial. Before a write would
take `up.log` past 1 MiB, its content moves to `up.log.1` and `up.log` starts
empty, so the log keeps at most 2 MiB. Other destinations, such as a terminal
or the systemd journal, get the lines unchanged.

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
| `--min-evidence admitted\|submitted` | omitted | Minimum submit evidence. Omitted sets no floor: any evidence is accepted. |
| `--epoch EPOCH` | live inspect | Previously verified epoch. Required to enqueue while the endpoint is down. |

`submit` does not spool until it has an epoch. With no `--epoch` it inspects
the live target first, and a failed inspect writes nothing. With a previously
verified `--epoch`, an unreachable endpoint still exits 0 and returns the
spool receipt. Restart dispatches that envelope only while its admission
window is still open; a closed window expires it without dispatch. Only an
unreachable endpoint queues a submit. A refusal (`busy`, `draining`,
`storage_full` or any other code, exit 6) settles the envelope, and nothing
sends it later: to run the prompt, submit it again.

### `status`, `wait`, `cancel`, `requests`

`status`, `wait`, and `cancel` each take one `REQUEST_REF`. `status` also
accepts a bare request id that is still in the sender spool.

| Flag | Command | Default | Meaning |
| --- | --- | --- | --- |
| `--timeout DURATION` | `wait` | `0` | Give up after this long. The work continues. `0` means no limit. |
| `--limit N` | `requests` | `20` | Most recent request-store records to show. |

`wait` interrupted by SIGINT exits 130. The request keeps running.

### `requests release`

An `uncertain` request keeps its target busy: every new submit to that
target is refused `busy` (exit 6), also after a restart. A Claude Code
request becomes `uncertain` (code `attachment_lost`) when no turn of it
started within its window: its frame never showed in the session, or the
session showed it and then moved on, for example to a new prompt or
`/clear`. Either can still run later and must not mix with a newer run. `cancel` cannot end it when the harness has no interrupt.
`requests release REQUEST_REF` is the owner's way out on this machine. It
ends the record `failed` with code `released_by_owner`, and the target
admits new work again. It stops nothing in the session: before it acts it
prints a warning that the request's input may still arrive and run there as
a turn that no request owns. The adapter forgets the run first, so such a
late turn never completes the released request or a newer one, and later
native evidence never reopens the record.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--yes` | false | Release without asking. Required when stdin is not a terminal; with a terminal, the command asks once. |

The command goes through the running endpoint, like `cancel`; with no
endpoint it exits 6 (`endpoint_unreachable`) and changes nothing. A request
that is not `uncertain` is refused with exit 6 and stays as it is, and so is
one whose target is not attached. Only the Claude Code adapter can forget a
run, so only Claude Code targets support it; Codex, pi and every other
adapter refuse it with `unsupported`. It acts on a request of any origin, a Buzz or link
request included. Buzz has no release command; `/cancel` stays as it is. A
released Buzz request's result row reads `failed (released_by_owner)` and
says that the prompt may still run in the session with no reply. A
successful release exits 0.

### `attach`, `detach`, `bindings`, `channel reset`

| Flag | Command | Default | Meaning |
| --- | --- | --- | --- |
| `--self` | `attach`, `detach` | false | `attach`: bind the session this command runs in. `detach`: remove the binding that names this session. |
| `--native` | `attach` | false | Drive the exact native session through the endpoint, not the AMQ mailbox. |
| `--link NAME` | `attach` | empty | Share this session with linked server `NAME`. Implies `--native`. |
| `--consent passkey\|local` | `attach` | `passkey` | With `--link`: `local` also holds each signed task for a confirmation on this machine. |
| `--tools PROFILE` | `attach` | server default, else `read` | With `--link`: the server tool profile agents in this session may call. Defaults to the server's `default_tool_profile`, else `read`. Empty for none. |
| `--me HANDLE` | `attach` | `AM_ME` | AMQ handle of this session. |
| `--name NAME` | `attach`, `detach` | see below | `attach`: the binding name. `detach`: remove only the binding with this name. |
| `--all` | `detach` | false | Remove every binding. |

`amq-remote attach --self` binds this session's AMQ handle, `AM_ME` under
`AM_ROOT`, to the machine agent. Each owner message in the session's channel
then arrives as an AMQ message from `buzz`, and the handle's `amq reply` is the
answer; a `kind: status` reply is progress. No endpoint, hook, or wake is
needed; noticing the message is the handle owner's business. A session that
is not on AMQ is refused and pointed at `--native`.

For a Codex mailbox session, attach first checks whether the app-server daemon
has its thread loaded. Otherwise it records the nearest invoking ancestor whose
kernel executable is Codex, with its process start identity checked against the
current ancestry. An ancestor with an `app-server` argument cannot prove one
session. This supports a TUI with an embedded app-server. If
neither path proves the session is live, its binding stays unknown.

For a mailbox session in any other harness, such as pi, started under `amq
coop exec` with a live `amq wake`, attach reads the owner that the wake
recorded in `agents/<handle>/.wake.lock` for this root and handle: the
process `coop exec` became, with its pid, start time and boot id. Attach
records that process as the holder only when it still has that identity and
attach runs under it, checked parent by parent. The binding is then live
while that process runs and ended when it exits. A harness that `coop exec`
runs directly, as pi does, is itself the holder; a wrapper that forks the
harness is the holder instead, and the binding follows the wrapper's
lifetime. The lock is trusted as written by the same user, like the binding
files: attach refuses a lock owned by another user or writable by others,
but nothing proves that `coop exec` wrote it. The lock records no pid
namespace, so this proof works on macOS only. On Linux, and when the lock
records no owner (for example an `--inject-cmd` or interrupt-key wake, a wake
after `amq wake repair`, or a wake that could not record its process), the
binding stays unknown.

`amq-remote attach --self --native` binds the exact native session instead. Typing it is the sharing choice, so
the session is found exactly and never guessed from a discovery list. (A
resumed session may get that same share back by itself; see below.) Claude
is found by the command's process ancestry. A pi chat is found the
same way: its tools run under the pi process whose pid the chat's bridge
publishes in `bridge.liveness`, with the chat's session id. Codex is found by
`CODEX_THREAD_ID` among the threads loaded in the app-server daemon. Attach
registers the target in the running endpoint without a restart and adds it
to `manifest.json`. When no endpoint runs, it starts `amq-remote up` in the
background (log: `up.log` in the state directory; see
[Endpoint log](#endpoint-log)). Claude needs the
[Stop hook](#claude-code) on this path: without it a task never finishes, so
attach of a Claude session (`--native` or `--link`) whose
`~/.claude/settings.json` has no AMQ Stop hook exits 6 and names
`amq-remote claude install-stop-hook`. When that file sets
`disableAllHooks`, attach goes ahead and warns (JSON `warning`), because a
project or managed setting decides whether the hook runs. Attach never edits
`~/.claude/settings.json` itself.

Each attach writes a named binding, `~/.amq/remote/bindings/<name>.json`
(default `<handle>-<project>-<session>` for a root
`<project>/.agent-mail/<session>`; for a native session `<kind>-<project>`,
from the session's own working directory, or the native target when that
directory is unknown or the session is shared with a link; `--name`
overrides),
and removes any other binding for the same session. A session keeps the name
it already holds; when another session holds its default name, it takes the
first free `<name>-2`, `<name>-3`, and so on, and never replaces the other
session's binding. Under `attach --self --link`, when the name pins the same
root and target and the target now runs a new native session, the name moves
to it. The bindings live
in `$AMQ_REMOTE_BINDING`'s directory when that is set; its missing directories
are created 0700 below an existing ancestor that has no symlink on its path,
is owned by you, and is not group- or world-writable. Several sessions can be
connected at once. The machine agent inside `amq-remote serve` makes a private
channel for each live session, adds the owner and mentions them, and runs
each owner message in that channel in that session only. Attach writes only
the binding and holds no Buzz key.

A session connected through its AMQ mailbox and also shared with a link has
one channel, the one of its mailbox binding; `amq-remote bindings` shows
`channel with <name>` on the other binding.

When a shared session's process ends and the same session comes back in a
new process (for example a resumed conversation), its share comes back
without a new `/amq-remote`, and never after `/amq-remote off`:

- A pi session (mailbox or native): the machine agent binds it again and
  restores its channel within about 35 s when the pi bridge of the same root
  and handle (`bridge.liveness`) names the session id the ended binding
  recorded.
- A Claude mailbox seat on macOS whose wake owner is the Claude process
  itself (a direct `amq coop exec claude`): the machine agent binds it again
  and restores its channel within about 35 s when that process's Claude
  session registry entry (`sessions/<pid>.json` under the binding's Claude
  config dir, not written before the process started) names the recorded
  session id and transcript.
- Any other Claude session, mailbox or native, and every Claude session on
  Linux: the approval hook binds it again on the session's first approval
  dialog, from the Claude process that runs the hook, and the machine agent
  then restores its channel.
- A Codex session is not restored, because Codex publishes no record of a
  resumed thread; run `$amq-remote` in it again.

The machine agent looks at sessions that ended within the last 7 days.

Attach tells the owner only what is still needed. It prints `Connected:
<label>`, the session's label (`<handle> · <project>/<session> @
<machine>` for an AMQ seat, so two seats of one harness in one session have
distinct channels; `<harness> · <directory> @ <machine>` for a native
session), then one line from the [machine agent](#machine-agent)'s enrolled
key, its marker under `~/.amq/remote/acp/agents/_machine/`, its status file
and its channel record. While the machine agent runs and serves the session,
and the session is proven live, attach waits up to 15 s for the channel to
become active. The line is the first that applies:

- the channel is suspended: why, that nothing is sent there until that
  changes in Buzz, and `amq-remote channel reset <name>` for a new channel;
- the machine agent serves and the channel is active: open that channel in
  Buzz;
- the machine agent serves and the channel is being made:
  `AMQ Remote is making the channel; it appears in Buzz within a minute.`;
- the machine agent does not serve: `The machine agent runs inside
  amq-remote serve and is not running: <line>`, where the line is its status
  line, else the refusal of a grant that does not load, else
  `run amq-remote up.`;
- the session is not proven live and has no channel: the machine agent makes
  a channel only for a session proven live, with the liveness evidence (JSON
  `unproven`);
- the channel is uncertain: Buzz did not confirm the create; AMQ keeps
  looking for it and never makes a second one, and `amq-remote channel reset
  <name>` gives it up;
- the channel failed: the error, and that AMQ tries again in 10 minutes, or,
  after the third refusal, `amq-remote channel reset <name>`;
- there is no machine agent: the [one-time setup](#machine-agent).

A Codex thread is proven live while its app-server daemon has it loaded, as
the machine agent judges it. When the thread runs Codex's automatic approvals
reviewer, attach adds `Buzz gets no Codex approvals: Codex's automatic reviewer
answers them (approvals_reviewer = <reviewer>).` and `--json` reports
`approval_reviewer`; it is the owner's setting, and AMQ does not change it.
`amq launch` prints the same line for a Codex daemon seat.

`--json` reports `name`, `label`, `reused` (this session already held the
name, as a binding or a tombstone, so its channel comes back), `machine`
(`running`, `offline` or `missing`) and, whenever there is a machine agent,
`machine_name`, `machine_refusal` (the status code while `serve` refuses it)
and, while it does not serve, `machine_line` (the line above). An enrolled
machine key with no marker yet reads `machine` `offline`. It also reports
`channel` (`active`, `creating`, `uncertain`, `failed` or `suspended`),
`channel_name`, and, when uncertain, failed or suspended, `channel_error` and
`channel_retry` (AMQ creates again by itself). For a Claude session it reports `approval_hook`, the AMQ approval hook in
the session's own settings file: `ok`, `missing`, `stale` (not the form the
installer writes now), `disabled` (the file sets `disableAllHooks`) or
`unreadable`. The binding records the session's Claude `config_dir`, so
doctor checks that file. A link share prints its own lines; a link has no
Buzz agent.

`amq-remote channel reset <name>` gives up the machine agent's channel of
session `<name>`. It is the only way to allow a new create after one that
Buzz never confirmed. The machine agent applies it on its next scan: the old
record is retired, a channel that create made is still looked for and
archived, and a new channel is made. A lost channel may be left over in
Buzz; the owner can archive it there.

`amq-remote bindings` (`/amq-remote status`) prunes the sessions proven
ended, then lists the machine agent and each binding with its label, `live`
or `unknown`, its name and its channel, with `amq-remote detach --name
<name>` for the unknown ones. The machine agent is listed as `Machine agent:
<name> (running in amq-remote serve)`, or `(not running: <line>)` with its
status line. A session with a permission dialog not yet resolved shows it at
the end of its line, for example `dialog open 13m (Bash)`: the phase, the time
since it opened and the tool. `--json` reports `bindings` (with `channel`,
`channel_name`, and `dialogs`: each permission dialog not yet resolved, with
its `id`, `tool`, `phase` (`open` or `answered`), `posted` (the machine agent
posted it) and `opened_at`), `pruned` and `machine`.

`amq-remote detach` needs one scope: `--self` removes the binding that names
this session, `--name <name>` removes the binding of that name, and `--all`
removes every binding. The session and the endpoint keep running. A session
the machine agent serves stops taking messages in its channel at once, and
the machine agent archives the channel; attaching the same session again
brings the channel back. Detach of a native session also removes its target's
entry from `manifest.json`; a running endpoint keeps the target until it
restarts. At its next start, `serve` removes the entry of a Claude session
whose process ended.

A defaulted name that another session holds, or held before it was detached
or ended, is never taken: the session gets the next free `<name>-N`.
Attaching the same session again keeps its name. An explicit `--name`
replaces the binding of that name.

### Dialog history

`amq-remote dialogs` shows the last 14 days of resolved approval dialogs,
grouped by session and ranked by time held. Each row shows the count, total,
median and longest wait, where the dialogs were answered, and the three
tools with the most time held. Use `--days N` to change the window or
`--json` to see every tool and the durations in seconds.

Each history line holds the dialog ID, binding, harness, tool name, opening
and resolution times, outcome and answer source. Dialog text, tool inputs
and option labels are never recorded in history. A dialog appears about an
hour after resolution, or when the machine agent removes a departed session's
dialogs. With the request already gone, the resolution counts toward the
session's dialog count only. The dialogs directory retains up to two files of
1 MiB each, `history.jsonl` and `history.1.jsonl`.

### `link`, `mcp`

| Flag | Command | Default | Meaning |
| --- | --- | --- | --- |
| `--code CODE` | `link add`, `link keys add` | empty | The single-use code the server shows. |
| `--device-name NAME` | `link add` | the host name | This machine's name as the server shows it. |
| `--link NAME` | `link confirm`, `link tools`, `link call`, `mcp` | the only link | The link to use. |
| `--args JSON` | `link call` | `{}` | The tool's arguments as a JSON object. |
| `--idempotency-key KEY` | `link call` | a new key, printed first | The call's durable key. Run the call again with the same key to resume after a lost answer. |
| `--wait` | `link call` | false | Wait for the call's final state. |

### `share`

| Flag | Default | Meaning |
| --- | --- | --- |
| `--root DIR` | `AM_ROOT` | AMQ root whose state directory holds the session's keys. |
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
| `--presence NAME` | empty | Display name for the body's Buzz profile and status; turns on [presence](#presence-in-buzz-desktop). Set with `--target`. |
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
| 6 | Action required. Refusal codes: `busy`, `unsupported`, `unshared`, `expired`, `stale_epoch`, `request_conflict`, `storage_full`, `attachment_lost`, `result_expired`, `already_resolved`, `endpoint_already_running`, `endpoint_unreachable`, `store_closed`, `draining`. A snapshot in `rejected` or `uncertain` is also 6. `doctor` uses 6 when any link fails (see Doctor). `share` returns 6 for symlink, key-loading, and mint failures, and that explicit code is kept. |
| 130 | `wait` was interrupted. The request keeps running. |

## Doctor

`amq-remote doctor --root <absolute-root>` checks every link between the
owner and the sessions of this machine; without `--root` or `AM_ROOT` it
checks the machine root. It runs these checks with or without an endpoint
state directory:

- The Claude hooks in each settings file the machine's sessions read: the
  one the environment names (`$CLAUDE_CONFIG_DIR/settings.json`, else
  `~/.claude/settings.json`), `~/.claude/settings.json` when it is another
  file, and the file of each `config_dir` a manifest entry or the binding
  of a live session records. Each needs the approval hook and the wake
  dialog guard in the form their installers write now, and the Stop hook
  when native mode is in use. While no binding and no machine agent exists,
  nothing can reach Buzz yet, so a missing approval hook or guard is a
  note. The prompt hook is the owner's choice: a file without it gets the
  note "the AMQ prompt hook (UserPromptSubmit) is not installed, so a
  Claude Code session takes no link session grant and each link task needs
  a signature; to install it: amq-remote claude install-prompt-hook", and
  an installed one is checked for its form (`prompt` and `prompt_form` in
  the `hooks` row). Doctor runs the installed
  approval command through `sh -c` with `AMQ_HOOK_PROBE=1`, an empty stdin
  and `AMQ_REMOTE_BINDING` set to an existing binding, so its gate opens;
  the receiver then prints its version and binds and posts nothing. A
  receiver with a payload on stdin is a real dialog and ignores
  `AMQ_HOOK_PROBE`. A command
  that prints nothing did not reach amq-remote (`probe` `closed`). A file
  that sets `disableAllHooks` is a note.
- The machine agent inside `amq-remote serve`: doctor reads its enrolled key
  and status file, fails with the status line while `serve` does not run it
  or refuses it, fails when its relay has not been authenticated for more
  than a minute, and notes a grant that ends within 14 days, with the renew
  commands. A machine with no machine agent gets a note with the
  [one-time setup](#machine-agent). The `machine_agent` row adds `agent`
  and `owner` (the first 8 characters of each key), `relay`, `relay_state`,
  `grant_end` and `refusal`.
- The channel of each binding the machine agent serves is `active`.
- Each mailbox binding: its handle is registered, and `amq wake check`
  reports a live wake on the current image.

No state directory means the endpoint has never been started. That fails
only when a native binding names this root; a mailbox binding needs no
endpoint. `amq-remote up` starts the same `serve` child. With a state
directory, doctor also reports the socket path, whether the endpoint
answered `session.list`, record counts by state, and any persisted adapter
refusals.

With a relay configured, doctor also reports each share's connection state
as `serve` last wrote it (`auth_pending`, `authenticated` or `unavailable`),
its DM surface under `commands`, and its `presence` and `discovery`.

A Codex target whose thread runs Codex's automatic approvals
reviewer (`approvals_reviewer` `auto_review` or `guardian_subagent`) is
listed under `approval_reviewer`: that reviewer answers the thread's
approvals before a remote client sees them. It is the owner's setting, so
doctor reports it and does not fail. A new thread started with
`approvals_reviewer = "user"` sends its approvals to Buzz.

`failing` lists each broken link with a subject, the detail, and a remedy;
`notes` lists advice in the same shape. Doctor exits 6 exactly when
`failing` is not empty. The text output prints one line per entry,
`FAIL <link> <subject>: <detail>; fix: <remedy>`, then each note as `NOTE`,
then each reported value as `INFO <key>: <value>` (such as
`approval_reviewer`), then `OK: no failing link` when nothing fails.
`--json` prints the report with its rows: `hooks`, `machine_agent`,
`channels`, `left_channels` and `mailboxes`. `left_channels` lists the
agent key, label and channel ID for each channel recorded as unarchived
under an earlier key. For a Claude target of the endpoint, the
`native_capability` rows and the `claude_stop_hook` and
`claude_approval_hook` keys stay beside the `hooks` rows for clients that
read them.

### Repair

`amq-remote doctor --fix` repairs what it can before it checks, and prints
one `FIXED <link> <subject>: <what>` line per change (`fixed` in JSON). Each
step is idempotent: a second run changes nothing.

- In every settings file the hook check reads (the one the environment names
  only when its directory exists), it installs or migrates what the check
  flags: the approval hook, the Stop hook (where native mode needs it, or to
  migrate an installed one), the prompt hook (only in a file where the owner
  installed it with `install-prompt-hook`, never because another hook is
  installed there) and the wake dialog guard. It never installs a
  hook the owner removed with its uninstall command. A repair keeps the
  installed binary unless it is the versioned target of the stable path it
  would write. The hooks run the `amq-remote` and `amq` on PATH when those
  are the installed binaries, so an upgrade keeps them working.
- While no login service is installed, it installs the one that runs
  `amq-remote up` for the machine root.

An approval or Stop hook that `--fix` installs or keeps records the owner's
opt-in (see the approval hook below). `--fix` never restarts amq-acp or a wake or deletes a request record.

| Link | Fails when |
| --- | --- |
| `hooks` | A settings file lacks the approval hook or the wake dialog guard, holds one in a stale form, cannot be read, or its approval command does not reach amq-remote. It also fails without the Stop hook when a native binding or an endpoint manifest entry exists; on a mailbox-only machine that is a note. A missing prompt hook is always a note; a prompt hook in a stale form fails. The subject is the settings file; the remedy names its `CLAUDE_CONFIG_DIR`. |
| `machine_agent` | The machine agent does not serve, or its relay has not been authenticated for more than a minute. |
| `channel` | A binding's channel is `uncertain` or `failed`. |
| `mailbox` | A mailbox binding's handle is unregistered, or its wake is not live or runs another image. |
| `stale_harness` | A running amq-acp runs an executable that an upgrade deleted. |
| `endpoint` | No state directory while a native binding names this root, or the endpoint does not answer `session.list`. |
| `amq_route` | The endpoint handle (`--me`, default `remote`) is not listed in the root's `config.json`, so other agents cannot route to it. |
| `registration` | An adapter was refused at startup, or `refusals.json` is unreadable. |
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
number of refused attempts and the next try. The output stays owed and is
retried with a backoff, in order per request. Do not send the message again
from Buzz: that starts a new request.

`stale_harness` is advice when the executable still exists: it is reported
under `notes`. It names each running amq-acp process whose executable
differs from the installed one, with the installed path, and asks the
operator to decide, since a different identity can also be a
byte-identical copy. A process whose executable an upgrade deleted runs a
version no file holds, so it fails. A process whose executable cannot be
read within the probe deadline is reported as unknown, never as current.

## Claude Code

Submit writes one frame to the target session's cross-session socket. The
socket answers nothing, so progress comes from the session's transcript:
the entry that delivers the frame is `submitted`, the next assistant entry
in that turn is admitted, and a Stop event ends the run with the turn's final
text as the result: the text Claude Code gives the hook, or, from an older
Claude Code without it, the last assistant text in the transcript. The Stop
event needs the hook:

```text
amq-remote claude install-stop-hook     # adds a Stop hook to ~/.claude/settings.json
amq-remote claude uninstall-stop-hook   # removes only that hook
```

Without the hook a request is admitted but never completes. Install edits
`~/.claude/settings.json` (`$CLAUDE_CONFIG_DIR/settings.json` when that is
set; the same applies to every settings path below) in place and keeps every
other byte. Every AMQ writer of a settings or hooks file (these installers,
`doctor --fix`, the wake dialog guard, the keepalive SessionStart hook and
the machine agent) takes one lock per file under `~/.amq/locks/` and reads
the file again before it replaces it; when another program changed it in
between, the edit is applied again to what that program wrote. A command
waits at most 10 seconds for the lock and then names the lock file; the
machine agent waits at most 50 ms and checks a busy file again on its next
tick. Uninstall
removes only the AMQ hook and leaves any empty `Stop` or `hooks` container
behind. The hook command itself always exits 0, so a broken bridge never
blocks Claude Code. After an endpoint restart, a request that was in flight
reads `uncertain`.

### Session grants and the prompt hook

A Claude Code session shows its permission mode only in a hook's input, at
a prompt. AMQ's prompt hook reads it there:

```text
amq-remote claude install-prompt-hook     # adds a UserPromptSubmit hook to ~/.claude/settings.json
amq-remote claude uninstall-prompt-hook   # removes only that hook
amq-remote claude prompt-hook             # the receiver the installed hook runs
```

The hook runs at each prompt of each Claude Code session on the machine,
but its command starts `amq-remote` only when an attachment or a binding
exists, as the Stop hook's command does. The receiver decides nothing: it
reports the session id and the `permission_mode` of its input to each
endpoint that serves the session, and prints the text that endpoint
answers as the prompt's added context. Its timeout is 10 seconds, and it
always exits 0.

With the hook installed, a session whose mode asks before risky actions
(`default`, `acceptEdits` or `dontAsk`) takes a link session grant: a
granted task from the linked server runs in that session with no signature
per task. A session in `plan`, `auto` or `bypassPermissions`, and every
session on a machine without the hook, takes no grant: each link task needs
a signature, as it does with no grant. Nothing changes for a signed task.

The hook is the owner's own choice. `install-prompt-hook` records the
owner's opt-in for that settings file; no other hook's opt-in installs it.
`doctor --fix` puts it back only in a file where the owner installed it,
and never after `uninstall-prompt-hook`; the machine agent posts about it
only for such a file.

### Approvals from the Buzz channel

Every dialog a connected session raises in its terminal also shows in the
session's Buzz channel: a command, a file edit, a network or permission
grant, an MCP call, a question. The message carries the dialog text and its
options as the harness shows them, so the owner reads what the terminal
shows. ✅ takes the first option, ❌ the deny option, a reply `1`, `2`, ...
takes that option, `yes` and `no` work as ✅ and ❌, and a dialog that asks for
text takes any reply. The first answer wins, in Buzz or in the terminal, and
the message is edited with the outcome. A Claude Code question with one
single-select question shows its choices; a reply with the number, or with
other text, is the answer. A question with more than one part, a multi-select
question and a plan approval show their text and are answered in the terminal.

Once the Claude hook is installed, every Claude Code session on the machine
(terminal, desktop app Code tab, IDE) mirrors its dialogs to Buzz, with or
without `/amq-remote`, each in its own channel. The first dialog of a session
that is not connected binds it as `attach --self` does: the mailbox binding
of an AMQ session (`AM_ROOT` and `AM_ME` set), whose channel then reaches its
inbox, else the native binding of `--native`, whose channel takes messages
once `/amq-remote --native` runs in the session. The machine agent makes the
channel as for any session. `/amq-remote off` stops this for that session
until `/amq-remote` connects it again.
Install it once:

```text
amq-remote claude install-approval-hook     # adds a PermissionRequest hook to ~/.claude/settings.json
amq-remote claude uninstall-approval-hook   # removes only that hook
```

The hook runs the `amq-remote` on PATH when that is this binary. Install
also records, under `~/.amq/remote/hooks/`, that the owner wants this hook
in this settings file; `install-stop-hook`, `install-prompt-hook` and
`doctor --fix` record the same for theirs, and the uninstall commands
replace it with the owner's removal.

Claude Code applies a PermissionRequest decision only when every
PermissionRequest hook has ended. A hook of your own that allows or denies
calls therefore has no effect while AMQ's hook waits for your answer;
`amq-remote doctor` and the installer note such a hook. AMQ changes no hook
that is not its own.

The machine agent watches the settings file of each Claude session it
serves (the `config_dir` its binding records); a file that did not change
costs one stat per tick. It never writes the file: only the install
commands and `doctor --fix` change it. When a changed file has lost the
approval hook, the agent posts once per version of the file, across
restarts of the agent, in the session's channel: "Approvals of this session
stay in the terminal: the AMQ approval hook left <path> at <mtime>. Run
`amq-remote doctor --fix`." A native session that lost the Stop hook gets
the same kind of line for its turns, and one that lost the prompt hook,
in a file where the owner installed it, gets one for its link tasks. A hook
that is present in an older
form (for example the approval hook with the 10-minute timeout) gets one
post per version of the file: "The AMQ approval hook in <path> is an older
form (changed <mtime>). Run `amq-remote doctor --fix`." After
`uninstall-approval-hook` (or `uninstall-stop-hook` or
`uninstall-prompt-hook`) it never posts about that hook in that file.

### Requests to the owner

When a connected mailbox session sends a question, ruling, task or review
request to `user`, its Buzz channel mentions the owner and shows the request
and its default or reply time, if set. Reply in the thread of that post to
send the agent an AMQ reply from `user`. The agent can keep working while
the request is open.

The post gains one outcome line: `→ Answered (Buzz)` for a thread reply,
`→ Answered` for another AMQ answer from the owner, `→ Closed by <handle>`
when the requester closes it, or `→ Expired` after 14 days.

Reactions do not answer these requests. Each post belongs to the requesting
session's channel. There is no machine-wide channel, and requests from other
projects or sessions stay in their own scope. Requests from disconnected
sessions or native bindings stay on the AMQ board.

### Turns in the session's channel

The machine agent's channel of a Claude, Codex or pi session also shows each
turn of the session, wherever it started: the terminal, the desktop app, a
Buzz message, a peer session or a background task. One message shows one
turn. It appears when the model first answers and shows `… running` with the
tools used so far. While the turn runs it is updated at most every 30 s
while what it shows changes (new tools or an interrupt; elapsed time alone is
no change), at most 10 times plus one to show an interrupt, and it shows the
answer as soon as the turn ends:

```text
▶ run the tests and fix the failure
✓ 4 tools (Bash ×2, Read, Edit) · 1m12s
<the turn's final answer>
```

The first line is what started the turn: the first line of a typed prompt (at
most 200 characters, harness markup left out), a slash command's name,
`image`, `pasted text`, `message from a teammate`, `Buzz`, `AMQ message`,
`message from another session`, `background task finished`, `scheduled
task`, `shell command`, or `continued`. A turn that answered a Buzz message
(the message reached the running turn, or the session replied to it during
the turn) shows `Buzz` in place of `AMQ message` or `background task
finished`, and keeps that label. The second line is how
the turn ended (`✓`; `✗ interrupted`, `unfinished`, `error` or `refused`;
`■ ended` when the session ended or was cleared), the tools it used by name
and count, and how long it ran. Tool input never leaves the machine. The
answer is all the text of the turn's last message; one longer than 3,500
bytes is cut and says so. A turn that answered Buzz shows the first two
lines only: the answer is the carrier's own post in the same channel. A shell
command typed with `!` shows only
when the model answers it, and never with the command itself; local slash
commands and a subagent's own steps are not shown. A background task's
notification after a turn starts a new turn. When a Stop hook makes the
session go on after a turn, that turn's message shows it running again and
is edited while what it shows changes, at most every 30 s. After 10 edits
it no longer shows the turn running, and from the 11th edit on each wait
doubles, up to 10 minutes; the newest answer is shown at the end of the
wait. The mirror queues at most five changes besides those of a message
that shows its turn running; past that, the oldest waiting change to an
ended message is dropped, and that message keeps the earlier answer it
shows.

The machine agent reads the session's transcript, which Claude Code names in
its session registry and in each approval hook; attach, and the approval hook
when it connects a session, record it in the binding. A session whose transcript cannot be read gets one
message, "This session's activity is not available here". Turns that ended
before the machine agent first saw the session are not shown, and after a
long stop the newest turn is shown with the number of turns left out. Each
turn is posted at most once; a post whose outcome is unknown is not posted
again.

An owner message in the channel that is exactly `mirror off` or `mirror on`
(case ignored) switches this for that session and is not delivered to it.
The switch counts only as its signed event, fetched from the relay and
signed by the owner's key; with no verified event it does not apply, and the
channel gets one line saying why. A switch signed before the decision that
set the mirror as it is does not apply.
`amq-remote mirror on|off|status --self` (or `--name NAME`) is the same
switch from the session's terminal, and always applies. While it is off, turns that end are not
shown later. A turn that runs when the mirror is turned off ends its message
with `■ ended (mirror off)`, and that message is not updated again.

Codex needs no hook: Codex approvals reach Buzz only for a session on the
Codex app-server (`codex app-server daemon start`, then `codex --remote
unix://`), and a plain `codex` TUI session answers them in the terminal. Managed Codex seats use a running shared daemon without any Codex setting. A hook
never blocks a dialog it cannot show: the terminal decides. [Approvals from Buzz](../../docs/amq-remote-approvals.md)
describes the request, answer and resolution files.
