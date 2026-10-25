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
"restart amq-remote up first".

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
amq-remote share --session ID
amq-remote attach --self [--native | --link NAME]
amq-remote detach --self | --name NAME | --all
amq-remote bindings
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
amq-remote doctor
amq-remote version
amq-remote claude install-stop-hook | uninstall-stop-hook
amq-remote claude install-approval-hook | uninstall-approval-hook
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
`version` prints `amq-remote <version>`. The `claude` commands install and
remove the Claude Code hooks (see [Claude Code](#claude-code)); the usage
banner does not list them.

`--json` prints the result as JSON on stdout, with diagnostics on stderr. The
commands that take it are listed under [Common flags](#common). `serve` and
`up` accept `--json` and print no JSON result. `share` has no `--json`.

## Manifest

`manifest.json` is read when `serve` starts; a running `serve` also follows
its relay block (see below). An absent file is an empty
adapter list, which is legal. `schema_version` is `1`. `layer` is `remote`.
Each adapter has a unique `target` (the id you pass to `inspect` and
`submit`), a `kind`, and an optional `config` object.

| Kind | Required `config` | What `inspect` advertises on this tree |
| --- | --- | --- |
| `claude` | `pid` (Claude Code process id). Optional `home` overrides the Claude home directory. Optional `approve` advertises `ApproveTool`: the Buzz DM can deny a tool call through the PermissionRequest hook, and allow one when the hook pins the owner (see [Approvals from the Buzz DM](#approvals-from-the-buzz-dm)). | `Inspect` and `Submit` over the session's cross-session socket. Submit evidence is `submitted`, so a relay share for Claude sets `min_evidence` `submitted`. `CancelRequest` and `Steer` are false. Unsupported on Windows. See [Claude Code](#claude-code). |
| `codex` | `socket` and `thread`. Optional `approve` advertises `ApproveTool`. | `Inspect`, `Submit`, and `CancelRequest`. `Steer` is false. |
| `pi` | `handle`; optional `upgrade_hint`, the remedy shown when the live bridge is too old and publishes none. The pi-bridge extension directory for that handle, `agents/<handle>/extensions/pi-bridge/` under the root, must already exist. | `Inspect` and `Submit`. `ApproveTool` only while the live bridge advertises `bridge_revision` 4 or higher; the reference bridge is revision 3. `CancelRequest` only while the live bridge advertises `bridge_revision` 5 or higher. Submit evidence is `submitted`. |
| `acp` | `handle`: the `AM_ME` of a running `amq-acp host`, whose directory `agents/<handle>/extensions/acp-host/` under the root must already exist. | `Inspect` and `Submit` over the host's bridge seam (protocol `amq:acp-host:v1`). `ApproveTool`, reject only: the owner can reject a tool call from Buzz; the host's policy rejects at expiry. `CancelRequest`: `amq-remote cancel` or `/cancel` stops the request's ACP turn, or keeps a request that has not run from running. Submit evidence is `submitted`, so a relay share sets `min_evidence` `submitted`. See [the acp-host wire](../../docs/pi-bridge-protocol.md#the-acp-host-wire). |
| `mailbox` | `handle`: an AMQ handle in serve's own root whose `agents/<handle>/inbox` already exists; the adapter never creates it. Optional `reply_timeout` (default `10m`, at most `24h`). The handle must not be `buzz`, `amq-remote` or serve's `--me`, and `--me` must not be `buzz` or `amq-remote`. Unknown config fields are refused. | `Inspect`, `Submit` and `CancelRequest`. Each request is one AMQ message to the handle: from `buzz` on thread `cockpit/<channel>` with the label `nostr:<event>` for a Buzz DM, else from `amq-remote` on thread `remote/<creator host>`. The handle answers with `amq reply --id <id>`; its first reply that is not `--kind status` is the request's result. With no such reply within `reply_timeout` the request fails, and the message stays in the inbox. A cancel after delivery ends the request as delivered and no longer waited for. One request runs at a time, so the handle ends its request before it asks the owner a question. Submit evidence is `submitted`. See [Strict share in one owner step](#strict-share-in-one-owner-step). |
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
- A share removed stops. Its approval pins go, and the other shares are not
  touched.
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
reads it to show a Buzz-started turn live in Desktop (see
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

A Codex target with `approve` also shows approvals of turns started in the
Codex TUI, on the share whose `native_session_id` is the thread. Each has
its own DM message, outside any request's thread. ✅ and ❌, or `yes` and
`no` typed in that message's thread, answer it as above, only from that
share and for one hour; then the message says to answer in the terminal.
A permission, input or MCP elicitation request of such a turn is not shown.
After an endpoint restart, the app-server sends a still-pending approval
again, and its DM message answers it again. An approval that closed while
the endpoint was down keeps its old text, and a reaction on it is not sent.

With `"mention_channels": ["<channel>", ...]` (at most 16, commands
required), an owner message in one of those channels that mentions the body
submits a plain prompt too. A leading `nostr:npub1…` mention is dropped from
the prompt. The result row goes to the DM channel, never to the mentioning
channel. Slash commands work only in the DM.

The surface opens only while the relay shows the channel as a private DM
whose members are exactly the owner and the body, and closes when that no
longer holds; a membership change can show late. A redelivered DM never runs
again, even after a busy rejection. The exact checks are in [the relay
connection reference](../../docs/amq-remote-relay.md#owner-dm-surface).

Privacy: Buzz DM content is not end-to-end encrypted. The relay operator can
read the prompts and the result rows. Do not share a session whose prompts or
results the relay operator must not see.

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
3. In the session to share, from any AMQ root or none, run
   `amq-remote attach --self --link NAME` (`--consent passkey` by default, or
   `local`; `--tools read` by default). The share names the session's
   binding and runs through the endpoint of the link's root. When the server
   lists the tool profiles it grants in its link info, `--tools` must be one
   of them. A pi session is driven only by the endpoint of its own AMQ root,
   so its link must live there: link that root with `--root`.
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
   look-alike page on another port cannot confirm. A held
   task is dropped at its deadline (10 minutes) or on restart, never resent.
7. `amq-remote link status` shows each link's socket, generation, owed
   revisions, conflicts and retired sinks, and for each share whether the
   server sees it now: `shared`, or why not (`session changed`, `not attached
   now`, `no such binding`) with the step that fixes it. A share is announced
   only while its session is the one attached; after a session change (a pi
   `/new`, a reload), run `attach --self --link NAME` again in the new
   session, which keeps the share's binding name. `amq-remote link remove NAME`
   retires the link's sink and deletes its device key; the records it created
   settle without network. The server keeps the machine until you unlink it
   on the server's connections page.

### Restart after a reboot

Nothing restarts the endpoint after a reboot unless a login service does.
`amq-remote service install` writes one that runs `amq-remote up --root ROOT`
at login (`ROOT` defaults to the machine root) and starts it now, replacing a
running one:

- macOS: the launchd user agent
  `~/Library/LaunchAgents/io.github.avivsinai.amq-remote.plist`, logging to
  `ROOT/extensions/remote/up.log`. Over SSH, with no login session to start it
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
the root has a link and no current service for it.

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
commands, `requests`, `attach`, `detach`, `bindings`, the `link` commands,
`mcp`, `service`, and `doctor`. `serve --help`, `up --help`, and
`claude install-approval-hook --help` print the flags and exit 2.
`share --help`, `version --help`, and the other `claude` commands print no
flag list; the `share` flags are in the table below.

### Common

| Flag | Default | Commands |
| --- | --- | --- |
| `--root DIR` | `AM_ROOT`; link commands and `mcp`: the link's root; `service`: the machine root | every command except `version`, `claude install-stop-hook`, `claude uninstall-stop-hook`, and `claude uninstall-approval-hook` |
| `--json` | false | `sessions`, `inspect`, `submit`, `status`, `wait`, `cancel`, `requests`, `attach`, `detach`, `bindings`, the `link` commands, `service`, `doctor`, and `mcp` (errors only; `mcp` always speaks JSON-RPC on stdout) |

### `serve` and `up`

`up` forwards these to the `serve` child and adds its own flags below.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--me HANDLE` | `remote` | Endpoint mailbox handle registered in the root. |
| `--fake` | false | Append the fake runtime as target `fake`. |
| `--codex-socket PATH` | empty | Unix socket of a running Codex app-server. |
| `--codex-thread ID` | empty | With `--codex-socket`, attach only this thread. |
| `--codex-approve` | false | Answer approvals of those Codex threads from remote clients: runs this endpoint submitted, and turns started in the Codex TUI on a thread a relay share pins. |
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
| `--min-evidence admitted\|submitted` | omitted | Minimum submit evidence. Omitted sets no floor: any evidence is accepted. |
| `--epoch EPOCH` | live inspect | Previously verified epoch. Required to enqueue while the endpoint is down. |

`submit` does not spool until it has an epoch. With no `--epoch` it inspects
the live target first, and a failed inspect writes nothing. With a previously
verified `--epoch`, an unreachable endpoint still exits 0 and returns the
spool receipt. Restart dispatches that envelope only while its admission
window is still open; a closed window expires it without dispatch.

### `status`, `wait`, `cancel`, `requests`

`status`, `wait`, and `cancel` each take one `REQUEST_REF`. `status` also
accepts a bare request id that is still in the sender spool.

| Flag | Command | Default | Meaning |
| --- | --- | --- | --- |
| `--timeout DURATION` | `wait` | `0` | Give up after this long. The work continues. `0` means no limit. |
| `--limit N` | `requests` | `20` | Most recent request-store records to show. |

`wait` interrupted by SIGINT exits 130. The request keeps running.

### `attach`, `detach`, `bindings`

| Flag | Command | Default | Meaning |
| --- | --- | --- | --- |
| `--self` | `attach`, `detach` | false | `attach`: bind the session this command runs in. `detach`: remove the binding that names this session. |
| `--native` | `attach` | false | Drive the exact native session through the endpoint, not the AMQ mailbox. |
| `--link NAME` | `attach` | empty | Share this session with linked server `NAME`. Implies `--native`. |
| `--consent passkey\|local` | `attach` | `passkey` | With `--link`: `local` also holds each signed task for a confirmation on this machine. |
| `--tools PROFILE` | `attach` | `read` | With `--link`: the server tool profile agents in this session may call. Empty for none. |
| `--me HANDLE` | `attach` | `AM_ME` | AMQ handle of this session. |
| `--name NAME` | `attach`, `detach` | see below | `attach`: the binding name. `detach`: remove only the binding with this name. |
| `--all` | `detach` | false | Remove every binding. |

`amq-remote attach --self` binds this session's Buzz agent (amq-acp in binding
mode) to this session's AMQ handle, `AM_ME` under `AM_ROOT`. Each Buzz DM then
arrives as an AMQ message from `buzz`, and the handle's `amq reply` is the
answer; a `kind: status` reply is progress. No endpoint, hook, or wake is
needed; noticing the message is the handle owner's business. A session that
is not on AMQ is refused and pointed at `--native`.

`amq-remote attach --self --native` binds the exact native session instead. Typing it is the sharing choice, so
the session is found exactly and never guessed from a discovery list. Claude
is found by the command's process ancestry. A pi chat is found the
same way: its tools run under the pi process whose pid the chat's bridge
publishes in `bridge.liveness`, with the chat's session id. Codex is found by
`CODEX_THREAD_ID` among the threads loaded in the app-server daemon. Attach
registers the target in the running endpoint without a restart and adds it
to `manifest.json`. When no endpoint runs, it starts `amq-remote up` in the
background (log: `up.log` in the state directory). Claude needs the
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
connected at once. Each has its own Buzz agent, pinned to its binding by the
system prompt line `AMQ Remote session: <name>`, which `amq-acp setup
--session <name>` writes into its import file. An agent may instead pin with
the model `amq-remote:<name>`.

Attach tells the owner only what is still needed. It prints `Connected:
<label>`, the session's label (`<harness> · <project>/<session> @
<machine>`), then one line from the agent's markers, which amq-acp rewrites
under `~/.amq/remote/acp/agents/<name>/` while the agent runs:

- the agent ran in the last 10 minutes: `DM "<agent>" in Buzz.`;
- it has markers but none that recent: start it in Buzz Desktop, or, if it
  was deleted, run `amq-acp setup --session <name>` and import the file;
- it has none, the helper agent runs with its control channel set up, and
  the session is proven live: the helper opens Buzz Desktop's Create agent
  dialog for it;
- it has none otherwise: run `amq-acp setup --session <name>` once and
  import the file. With no helper on this machine, attach also suggests
  importing the helper once (`amq-acp setup --helper`), so that later
  sessions need only the Create agent dialog.

`--json` reports `name`, `label`, `reused` (this session already held the
name, as a binding or a tombstone, so its agent comes back), `agent` and
`helper` (`running`, `offline` or `missing`), `draft` (the helper opens the
Create agent dialog), and `agent_name` (the marker's display name, else the
label, else `AMQ: <name>`) whenever there is an agent. A link share prints
its own lines; a link has no Buzz agent.

`amq-remote bindings` (`/amq-remote status`) prunes the sessions proven
ended, then lists each binding with its label, `live` or `unknown`, its
agent's state and its name, with `amq-remote detach --name <name>` for the
unknown ones. It names, once, the agents of removed bindings that carry no
label: those agents never archive themselves, so delete them in Buzz Desktop
and hide their DMs. The text output records them as named;
`--json` lists them without recording. `--json` reports `bindings`, `pruned`,
`helper` and `legacy_agents`.

`amq-remote detach` needs one scope: `--self` removes the binding that names
this session, `--name <name>` removes the binding of that name, and `--all`
removes every binding. The session and the endpoint keep running. The
session's agent then archives itself, and it unarchives when the same session
attaches again.

A defaulted name that another session holds, or held before it was detached
or ended, is never taken: the session gets the next free `<name>-N`.
Attaching the same session again keeps its name. An explicit `--name`
replaces the binding of that name.

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
number of refused attempts and the next try. The output stays owed and is
retried with a backoff, in order per request. Do not send the message again
from Buzz: that starts a new request.

`stale_harness` is advice, not a failure: it is reported under `notes` and
never makes doctor exit non-zero. It names each running amq-acp process
whose executable differs from the installed one, with the installed path, and
asks the operator to decide, since a different identity can also be a
byte-identical copy. A process whose executable cannot be read within the
probe deadline is reported as unknown, never as current.

## Claude Code

Submit writes one frame to the target session's cross-session socket. The
socket answers nothing, so progress comes from the session's transcript:
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
amq-remote claude install-approval-hook [--owner <hex|npub>] [--root <dir>] [--session <name>] [--manifest <path>]   # adds a PermissionRequest hook to ~/.claude/settings.json
amq-remote claude uninstall-approval-hook                                                                         # removes only that hook
```

Without `--owner` the installer pins the one owner of the relay shares in
the manifest (`--root` or `AM_ROOT`, or `--manifest`). With no owner found
it installs a reject-only hook. With an owner it pins that owner's one relay
share with a DM channel (or the `--session` one) on the hook's command line.
Run the installer again after the share's relay, channel or body changes.

For a Claude session that a Buzz Desktop agent drives with `--native` (no
relay share), pin that agent's DM instead:

```text
amq-remote claude install-approval-hook --desktop --owner <hex|npub> --body <agent hex|npub> --channel <DM channel id>
```

The agent shows this command with its values under each deny-only approval
in the DM; check the values the installer prints against it. The hook then allows a call on
the owner's ✅ only with the agent's signed grant; an approval from another
agent or channel can only deny. See
[Claude Code approvals from Buzz](../../docs/amq-remote-approvals.md#buzz-desktop).

The DM offers ✅ only for a Bash call it shows whole, when the installed hook
pins the share's owner and the approval is bound to one tool call; otherwise
it offers ❌ only. A call that may contain a secret shows "Command hidden: it
may contain a secret. Check the terminal.", and a call too long to show
whole shows "Command too long: check the terminal." The first answer wins, in
Buzz or in the terminal, and the message is edited with the outcome. A
request that Claude absorbed into a running turn gets no DM approval. When an
approval cannot be verified, nothing is answered and the DM says why.

The pin holds only while Claude cannot change settings or run commands
without a prompt. Each of these defeats it: a click on "allow Claude to edit
.claude for this session", `bypassPermissions` mode, or a `permissions.allow`
rule that allows every Bash command. `amq-remote doctor` reports the pinned
owner and share and warns on the last two in `~/.claude/settings.json`.

The checks the hook and the endpoint run before an allow are in
[Claude Code approvals from Buzz](../../docs/amq-remote-approvals.md).
