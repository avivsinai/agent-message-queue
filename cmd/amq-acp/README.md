# amq-acp

`amq-acp` is an [Agent Client Protocol](https://agentclientprotocol.com)
companion. As an ACP agent over stdio it delivers each prompt to one of three
destinations and holds the turn open until the answer arrives: an AMQ handle
as an ordinary message on a durable cockpit thread (mailbox mode), an
amq-remote target ([remote mode](#remote-mode)), or the session that
`amq-remote attach --self` bound ([binding mode](#binding-mode)).
[Host mode](#host-mode) runs the other direction: an ACP agent becomes an
AMQ participant. It is a separate binary on purpose: `amq` itself gains no
protocol server and no listening socket.

## What it speaks

ACP **version 2**. `initialize` answers `protocolVersion: 2`.

Implemented methods:

| Method | Behavior |
| --- | --- |
| `initialize` | Answers `protocolVersion: 2` and the minimum honest capability set. Unknown top-level params are rejected. |
| `session/new` | Returns a `sessionId`, `_meta.thread` (the durable AMQ cockpit thread for the session's channel), and `models` with exactly one model, the fixed destination. Requires a completed `initialize`. |
| `session/set_model` | Accepts only a model that `session/new` advertises in `models`: `amq:<AMQ_ACP_TO>`, `amq-remote:<target>` in remote mode, or `amq-remote:<name>` for a named binding in binding mode, which selects that binding for the session. This bridge runs no model; the destination answers with its own. Any other id is refused. |
| `session/prompt` | In mailbox mode, delivers the prompt text to `AMQ_ACP_TO` on the session's cockpit thread, then holds the turn open. The client receives `session/update` notifications as the turn progresses and the reply text as an `agent_message_chunk`; the final result is `stopReason: "end_turn"` with the reply in `_meta.amq`, or the typed refusal `stopReason: "refusal"` with `_meta.amq.state: "no_reply"` when the bounded wait expires. |
| `_session/steering` | Delivers owner steering on the session's cockpit thread, framed as untrusted task guidance. During an in-flight prompt it is AMQ `urgent` with the `buzz-steer` label and returns `outcome: "injected"`; while idle it is `normal` priority and returns `outcome: "startedNewTurn"`. A redelivered steer event returns `outcome: "duplicate"` with the original message id and delivers nothing new. The outcome names the delivery mode only: `_meta.amq` reports the prompt committed to the inbox, not drained or started, so neither outcome proves the peer acted. An in-turn steer refs the turn's prompt. A Nostr event id in `_meta` makes a redelivered steer idempotent. `initialize` advertises it as `_meta.steering.supported: true`, except in remote mode with a pinned target (binding mode: see below). |
| `session/cancel` | Ends the session's in-flight prompt turn: that `session/prompt` returns `stopReason: "cancelled"` with `_meta.amq.state: "cancelled"` and `reason: "session_cancelled"`. The queued AMQ prompt is not retracted and the peer is not notified; a later reply on the thread cannot answer a new prompt. With no turn in flight it is a no-op. |

Everything else returns JSON-RPC `-32601`. There is no `session/load`, no
`fs/*`, no `terminal/*`, and no tool calling. The v2 baseline block types are
accepted: `text` passes through and `resource_link` is rendered into the AMQ
body as a markdown link (`[title-or-name](uri)`). `promptCapabilities` are all
false, so `image`, `audio`, and embedded `resource` blocks are refused rather
than silently dropped.

## Turn semantics

- A prompt turn is correlated, not fire-and-forget. Only a message from the
  configured peer, on the prompt's thread, created after the prompt, whose
  `refs` name the prompt can answer the turn. `amq reply --id <prompt id>` sets
  those refs, and so does a reply to an in-turn steer. A stale reply, or a late
  answer to a cancelled prompt, never answers a later turn.
- The peer check is not authentication. Agents of the same user are not
  an isolation boundary ([SECURITY.md](../../SECURITY.md)), so any agent on
  the queue root can write a reply that answers the turn and is posted to the
  DM.
- A turn settles once: the first of reply, `session/cancel`, stream close
  and timeout decides it, and the others cannot overturn it.
- The turn is bounded. If no fresh reply arrives within
  `AMQ_ACP_TURN_TIMEOUT`, the result is the typed refusal
  `stopReason: "refusal"` with `_meta.amq.state: "no_reply"` and
  `_meta.amq.reason: "reply_timeout"` — never a fabricated success.
- The channel-to-thread mapping persists under
  `AMQ_ACP_STATE_DIR` (default `AM_ROOT/meta/acp`, path-guarded). A companion
  restart reopens the same cockpit thread instead of minting a new one.
- While the turn is open the bridge emits `session/update` notifications:
  `agent_thought_chunk` while waiting (including periodic heartbeats) and
  `agent_message_chunk` carrying the reply text when it arrives.
- Closing the ACP input stream ends in-flight turns immediately with
  `_meta.amq.reason: "client_disconnected"`. Queued AMQ messages are never
  retracted; the peer can still answer on the thread.

## Configuration

| Variable | Meaning |
| --- | --- |
| `AM_ROOT` | Required absolute queue root. |
| `AM_ME` | Sender handle. Required unless `AMQ_ACP_REMOTE_TARGET` is set. |
| `AMQ_ACP_TO` | Recipient handle for every prompt. Required unless `AMQ_ACP_REMOTE_TARGET` is set. |
| `AMQ_ACP_REMOTE_TARGET` | Remote mode: submit each prompt to this amq-remote target. See [Remote mode](#remote-mode). |
| `AMQ_ACP_REMOTE` | `binding`: follow the session bound by `amq-remote attach --self`. See [Binding mode](#binding-mode). |
| `AMQ_ACP_REMOTE_NATIVE_SESSION` | Required with `AMQ_ACP_REMOTE_TARGET`: the native session the owner shared. |
| `AM_BASE_ROOT` | Pinned base root; required whenever any pin variable is set. |
| `AM_SESSION` | Pinned session name. |
| `AM_ROOT_ID`, `AM_BASE_ROOT_ID` | Identity tokens authenticating the two roots. |
| `AMQ_ACP_STATE_DIR` | Durable channel/thread state directory (default `AM_ROOT/meta/acp`). |
| `AMQ_ACP_TURN_TIMEOUT` | Bounded reply wait per turn (default `10m`). |
| `AMQ_ACP_POLL_INTERVAL` | Reply poll interval (default `100ms`). |
| `AMQ_ACP_HEARTBEAT_INTERVAL` | `session/update` heartbeat interval (default `15s`). |

### Live settings

`~/.amq/acp.settings` is a JSON file that overrides the three durations
without a restart:

```json
{"turn_timeout": "30m", "poll_interval": "100ms", "heartbeat_interval": "15s"}
```

Each key is optional and uses the same positive Go duration as its
`AMQ_ACP_*` variable. The file is read at the start of each prompt, so an edit
applies from the next turn; a turn already running keeps its values. A missing
file means the startup values, so deleting it reverts. A refused file (bad
JSON, an unknown key, a bad or non-positive duration, over 64 KiB, a symlink or other
non-regular file) keeps the last good values and is logged once on stderr; it
never stops `amq-acp`.
Identity (root, handle, recipient, target, session pin, Buzz credentials) is
startup-only.

Every `AMQ_ACP_*` duration is fail-closed: an unparsable or non-positive value
refuses at startup and names the offending variable, so a typo can never
silently turn a bounded wait into an unbounded one.

Configuration is fail-closed. A missing `AM_ROOT`, a relative root, pin evidence
without an exact `AM_BASE_ROOT`, a root that contradicts the pinned base and
session, or an identity token that no longer names the same physical directory
all refuse with exit code 5 before any message is written.

```sh
# Replace the placeholder root with the absolute queue root on this machine.
AM_ROOT=/absolute/path/to/.agent-mail/collab AM_ME=cursor AMQ_ACP_TO=codex amq-acp
```

Pin those values in operator config or in a local Buzz harness copy. Chat and
prompt text must not pass `--root`, recipients, or argv.

`brew install avivsinai/tap/amq` puts `amq-acp` on `PATH`. A direct install
uses the matching `amq-acp_*_{linux,darwin}_{amd64,arm64}.tar.gz` asset. See
[INSTALL.md](../../INSTALL.md).

From a pinned AMQ shell, write the Buzz harness instead of editing JSON:

```sh
amq-acp install --to codex
```

That writes `custom_harnesses/amq_codex.json` with mode 0600, the absolute
`amq-acp` path, and `AM_ROOT`, `AM_BASE_ROOT`, `AM_SESSION`, `AM_ME`, and
`AMQ_ACP_TO`. It prints `In Buzz Desktop: New agent, select <label>`.
`--remote-target <target>` asks the endpoint which native session that target
is attached to and writes `AMQ_ACP_REMOTE_TARGET` plus
`AMQ_ACP_REMOTE_NATIVE_SESSION`. `AM_ME` is optional in that mode. A target id
such as `claude:98402` is kept in `AMQ_ACP_REMOTE_TARGET`; the file name
replaces `:` with `_`. If the endpoint is down or the target is unshared, the
command refuses: start `amq-remote up --root $AM_ROOT` first. `AM_BASE_ROOT`
and `AM_SESSION` are written only when this shell has them. `--remove` deletes
only the file this command wrote. A harness file this command did not write is
left unchanged.

Once per Mac, write the harness. Each session then gets its own Desktop agent:

```sh
amq-acp setup
amq-acp setup --session <name>
amq-acp setup --helper
```

`setup` writes `custom_harnesses/amq_remote.json` with mode 0600, the stable
`amq-acp` path, and env `AMQ_ACP_REMOTE=binding` only. It does not record a
root, a target, or a session pin. `setup --session <name>` writes
`AMQ <name>.agent.json` in the current directory (`--out` chooses another
path): a `buzz-agent-snapshot` version 1 named `AMQ: <name>`, runtime
`amq_remote`, model `amq-remote`, parallelism 1, `respondTo` `owner-only`,
and the system prompt line `AMQ Remote session: <name>`. That line pins the
agent to the named binding, so it serves that session only. A bare `setup`
writes `AMQ Remote.agent.json` with no pin: that agent drives the only
binding and, when several exist, answers with the list of sessions and the
agent to DM for each.
`setup --helper` writes `AMQ Remote helper.agent.json`: the agent `AMQ Remote
helper @ <machine>` with the system prompt line `AMQ Remote helper`, model
`amq-remote` and `respondTo` `owner-only`. Import it once per machine. The
helper serves no session; it asks Buzz Desktop to create each new session's
agent, so a new session needs only the Create agent dialog.
Import the file in Buzz Desktop: Agents, then + then Import, pick the file,
then Start, and turn on Auto-start in the agent's menu. Desktop caches its
harness list: if AMQ Remote is new and Desktop was open, click Settings,
then Agents, then Check again first (setup always prints this step). No
restart is needed. `/amq-remote` runs `setup --helper` or `setup --session`
itself when the session has no agent yet. A harness file this command did not
write is left unchanged.
An `amq_remote` agent runs at any parallelism, so an agent made from the
helper's draft (Desktop's default, 10) starts as is. Buzz starts that many
identical workers with one identity. Each turn decides through per-event file
locks, and one worker leads the agent's lifecycle (archive, unarchive,
"Connected again", the helper's control channel and drafts) through the lock
`agents/<pin or _helper>/<pubkey>.lock` in the state directory. When the
leader exits, another worker takes over within 30 s. Without advisory locks
(not macOS or Linux), parallelism must be 1.

## Buzz BYOH

This is a custom harness, not a Buzz preset. `amq-acp install` writes the
machine copy. The committed [`buzz-harness.json`](buzz-harness.json) has empty
`args` and no env: Buzz's default `BUZZ_ACP_AGENT_ARGS=acp` would be extra argv
and `amq-acp` would refuse.

- ACP pool workers must not drain AMQ mailboxes. `amq-acp` only writes
  `inbox/new`. A separate Edge owner drains with `amq drain`.
- In BYOH mode (`AMQ_ACP_TO` or `AMQ_ACP_REMOTE_TARGET`) `amq-acp` runs one
  worker: it refuses `BUZZ_ACP_AGENTS` other than `1`, so set parallelism 1.
  With `AMQ_ACP_REMOTE=binding` it accepts any parallelism and elects one
  lifecycle leader (see setup above). It always refuses `BUZZ_ACP_RESPOND_TO`
  other than `owner-only`, then unsets every `BUZZ_*` variable so a leaked
  nsec cannot enter AMQ messages. These variables are policy inputs, not a
  replacement for AMQ recipient configuration.
- When `_meta.nostr.eventId` (or `_meta.triggeringEventIds`) names one 64-hex
  Nostr event, that id is the idempotency key. A second prompt with the same
  id is a no-op. Two ids in one prompt are refused: one event, one AMQ job.
- `_meta.amq` reports independent facts: `committed`, `drained`, `started`,
  `completed`, and `egress` (`confirmed` or `uncertain`). Queued is not
  drained. Uncertain egress is not retried. The turn outcome adds `state`
  (`replied` or `no_reply`), `reason`, and the `reply` text when one arrived.

## Remote mode

With `AMQ_ACP_REMOTE_TARGET` set, a prompt drives one live native session
through the amq-remote endpoint that runs on `AM_ROOT` (`amq-remote up --root
"$AM_ROOT"`). No AMQ message is written. `AMQ_ACP_TO` must then be unset.

The share is one native session, not the target alias. Every command carries
`AMQ_ACP_REMOTE_NATIVE_SESSION`, and the endpoint socket refuses it with
`unshared` when the target is attached to any other session, such as a new
Claude conversation in the same process. The endpoint socket answers a local
`native` query with a target's current native session, so a sharing command
can pin it; the identity is not part of the session schema.

- `session/prompt` inspects the target, submits the text with `busy=reject`
  and `deliver=turn`, and holds the turn open until the request is terminal
  or uncertain. A completed request returns its native result as the agent
  message and `stopReason: "end_turn"`. Any other state, or a refusal such as
  `busy` or `endpoint_unreachable`, returns `stopReason: "refusal"` and an
  agent message that names the state and code.
- While the request runs, the native session's activity is mirrored into the
  ACP session, so Buzz Desktop's activity view shows it live: streamed text
  as `agent_message_chunk`, commands and tools as `tool_call` and
  `tool_call_update`, and plan updates as `plan`. amq-acp reads it from the
  endpoint socket's local `activity` stream for that exact request, under
  the native pin. The stream is read-only and carries nothing that submits,
  cancels or answers an approval. Codex text arrives as deltas merged per
  100 ms; Claude and pi text arrive one message at a time. The owner's own
  prompt is not repeated. When text was streamed, the final answer still
  posts to the DM but is not sent to the activity view a second time.
  Activity in the moment between admission and the start of the stream can
  be missing, and an endpoint without the stream leaves the turn as before.
- A submit whose outcome is unknown is reconciled by its exact request
  reference. When the record cannot be read, `_meta.remote.state` is
  `uncertain` and the message says not to resend.
- `session/cancel` sends a cancel for that exact request. `_meta.remote.cancel`
  holds the endpoint's disposition, or its refusal code. When the work keeps
  running, as on a Claude target that answers `unsupported`, the agent message
  says so. A cancel that settles the turn first stands, even when the request
  completes later.
- A Nostr event id maps to a fixed request id. A redelivered event follows its
  stored request, also after the endpoint reattached under a new epoch.
- With a pinned target, `_session/steering` is not available, and
  `initialize` reports `_meta.steering.supported: false`.
- `_meta.remote` reports `target`, `requestRef`, `state`, `code`, `reason`,
  `cancel`, and `truncated`.

### Binding mode

With `AMQ_ACP_REMOTE=binding`, the harness environment names no root, target
or pin. Each prompt reads the named binding that `amq-remote attach --self`
wrote (`~/.amq/remote/bindings/<name>.json`).

- A mailbox binding (the default) delivers the prompt as an AMQ message from
  `buzz` to the bound handle, then waits for a reply that refs it. A `status`
  reply is progress; any other reply is the final answer. The first delivery
  of an event claims one message id, and a redelivery reuses it and publishes
  only if the message is absent. Delivery never creates the handle's mailbox
  or inbox; it completes the other folders of an existing one. When the
  first delivery of an event is refused by the mailbox, the event ends as not
  delivered and a redelivery never publishes it; send the DM again. A refused
  redelivery, or any other publish error, is uncertain and points at
  `amq thread --id`. Progress shows
  "Delivered", then "Read by" (the handle's drained receipt), then the reply.
  Stop ends the wait and says the message stays in the inbox; it is never
  recalled. A final reply that
  arrives after the turn timed out or the client left is still posted to the
  DM, once, by a sweep at the next prompt and every 30 s while a stream is
  open, for up to 24 h. A reply after Stop is not posted.
- Follow-up DMs: a second `session/prompt` while a turn is open is still
  refused, but `initialize` advertises `_meta.steering.supported: true` in
  binding mode, so Buzz sends a follow-up as `_session/steering`. Once the
  turn's prompt is in the handle's inbox, the follow-up is delivered from
  `buzz` to the bound handle on the same thread, `urgent` with the
  `buzz-steer` label and refs to the prompt, and returns
  `outcome: "injected"`. A reply to it also answers the turn, which keeps waiting for
  the final reply. With no open mailbox turn, on a native binding, or once
  the event has its final answer, the steer is refused as in remote mode. A
  steer event's first delivery claims its message id and destination, so a
  redelivery returns `outcome: "duplicate"` and never reaches a later turn
  or another binding.
- A native binding (`attach --self --native`) submits there with that
  binding's native pin.
With no binding, the agent answers "Not connected. Run /amq-remote in a
Claude Code or Codex session." A request keeps the binding it started with;
a later attach moves only new prompts.

One Buzz agent serves one session. The agent's pin names the binding: the
system prompt line `AMQ Remote session: <name>` (new agents), or the model
`amq-remote:<name>` (agents made before the line existed). Both naming
different sessions refuse with "conflicting session pins", and so does a
`session/set_model` that names another binding. `session/new` advertises the
plain `amq-remote` model first, then one `amq-remote:<name>` per binding. An
unpinned agent drives the only binding, or answers with the list of
sessions. The system prompt line `AMQ Remote helper` makes the agent this
machine's helper: it never runs a prompt and answers with the list of
sessions and the agent to DM for each. `AMQ_ACP_TO`, `AMQ_ACP_REMOTE_TARGET`
and `AMQ_ACP_REMOTE_NATIVE_SESSION` are refused in this mode.

A pinned agent keeps its Buzz identity in step with its binding. On each
tick it rewrites its marker, `~/.amq/remote/acp/agents/<name>/<pubkey>.json`,
which `amq-remote attach` and `bindings` read to report the agent as
running (rewritten in the last 10 minutes), offline or missing. When the
binding is gone (`/amq-remote off`, or a session proven ended), the agent
archives itself (kind 9035, signed with its own key) and sets the status
"session ended"; when the same session binds that name again, it unarchives
itself (kind 9036) and clears the status. Archiving hides the agent from
pickers, mentions, member lists and the Agents library in Buzz Desktop; its
DM stays listed until the owner hides it.

### Posting answers into Buzz

buzz-acp sends ACP answer text only to the owner's activity view; a Buzz
agent's chat reply exists only if the agent publishes it. So amq-acp posts
every owner-facing text (a final answer, "Not connected", a busy or stop
notice) into the channel named by the prompt's `<context>` block with
`buzz messages send`, as the managed agent. The identity (`BUZZ_PRIVATE_KEY`,
`BUZZ_AUTH_TAG`, `BUZZ_RELAY_URL`) is kept in process memory for that child
only; the environment is still stripped and no AMQ message carries it.
`_meta.remote.posted` (or `_meta.amq.posted`) is `posted` or the error. The
binary is `AMQ_ACP_BUZZ_CLI`, else the CLI inside `Buzz.app` (under
`/Applications` or the absolute home's `Applications`, real directories only,
executable). There is no implicit `PATH` fallback: an unrelated `buzz` never receives
the key (`AMQ_ACP_BUZZ_CLI=buzz` still resolves through `PATH` by your choice).
The post budget (30 s) bounds how long `amq-acp` waits for the Buzz CLI. A
CLI that keeps running after its budget, for example a wrapper that
backgrounds the send, is outside the guarantee that no status text is posted
after the final answer; the bundled `Buzz.app` CLI completes its send before
it exits (observed in live use).
A bundle candidate is skipped if any path component is a symlink. The
installation and its parent directories are trusted against concurrent
replacement: pathname validation is not atomic with exec.

### Trust limits

In Buzz Desktop, the managed agent's identity and grant belong to Desktop,
and amq-acp never receives the key. Desktop signs a grant with no kind limit
and no expiry. Archiving the agent hides it and removes Desktop's local key;
it does not invalidate a copied key and grant. Only removing the agent's relay
access does that. Buzz's owner-only setting also admits the owner's other
agents. The strict per-kind, expiring path is `amq-remote` with a `relay`
manifest block.

## Limitations

- In mailbox mode, every prompt is delivered to the single handle in
  `AMQ_ACP_TO`; the ACP session id does not select a recipient.
- A timed-out turn is a refusal, not a retry. The peer's answer may still
  arrive later on the thread; read it with `amq thread` and re-prompt if the
  work must run.
- No audit copy is written to the sender's `outbox/sent`, unlike `amq send`.

## Host mode

`amq-acp host` runs the other direction: AMQ is the ACP client, and an ACP
agent such as Grok Build becomes an AMQ participant under its own handle.

```sh
amq coop exec --no-wake --me grok amq-acp -- host -- grok agent --no-leader stdio
```

The host spawns the agent after `--` without a shell, in `--cwd` (default:
the current directory), and opens one ACP session (protocol version 1). It
takes its inbox oldest first, one message at a time: it moves the message to
`cur`, writes a `drained` receipt, and runs the message as one
`session/prompt` turn. The reply goes to the sender on the same thread, with
`refs` naming the message, the kind `amq reply` would choose (`question`
becomes `answer`, `review_request` becomes `review_response`), and the labels
`acp-host` and `acp-stop:<stop reason>`. Its body is the turn's
`agent_message_chunk` text. The startup line on stderr names the handle and
the root, so check it before you send work.

Only a question, a review request, a todo, a brainstorm, or a message with no
kind runs a turn. An answer, a review response, a status, or a decision, and
any message labelled `acp-host`, is drained with a receipt and gets no reply,
so two hosts never answer each other forever. A message whose sender cannot
receive a reply in this root (a bridged `peer/handle`, an empty or invalid
handle) or that has no thread is drained the same way. A file that does not
parse moves to the DLQ with a `dlq` receipt, as `amq drain` does.

Every tool call the agent asks to run is rejected unless the agent's own
arguments grant autonomy, for example Grok's `--always-approve`. The host
advertises no file or terminal methods and never calls `authenticate`; an
agent that needs a login is a refusal with exit code 6, and you log in once
with the harness's own CLI. An agent that does not answer `initialize` and
`session/new` within 60 seconds is also exit 6. The agent's environment has
every `AM_*`, `AMQ_*` and `BUZZ_*` variable removed, so the agent does not
inherit the host's queue context or a Buzz identity. It runs as the same
user, so it can still name the root and the host's handle explicitly. This
is not an isolation boundary.

State lives in `<root>/agents/<me>/extensions/acp-host/`: `host.lock` (one
host per handle; a second one exits 6), `session.json`, and one journal
record per message under `turns/`.

`session.json` records the ACP session the host created, with the agent
executable name and the working directory. A restarted host resumes that
session with `session/load` when the agent advertises `loadSession` and the
agent and directory match, so the agent keeps the conversation. The history
the agent replays during the load never goes into a reply. When the agent
cannot load sessions, the record belongs to another agent or directory, or
the load fails, the host opens a new session and says so on stderr. The
startup line says whether the session is `new` or `resumed`.

The owner can drive the same ACP session from Buzz through `amq-remote`: a
manifest entry `{"kind":"acp","target":"grok","config":{"handle":"grok"}}`
attaches to the host through files under the same directory (see
[the acp-host wire](../../docs/pi-bridge-protocol.md#the-acp-host-wire)). A
remote request runs only while no turn is in flight and the inbox is empty,
and is otherwise refused `busy`. Its text goes to the agent verbatim, and the
answer comes back as the request's result. A tool call during a remote
request shows in Buzz as an approval the owner can reject; the host rejects
it anyway after five minutes, and approve from Buzz is not offered.
Cancelling the request from Buzz (`/cancel <ref>`) or with
`amq-remote cancel` sends ACP `session/cancel` for that turn only; a request
that has not run never runs.

`amq launch` starts a host from `.amq/launch.json` with adapter `acp` (see
[the launch API](../../docs/launch-api.md#participants)). An entry such as
`{"handle": "grok", "adapter": "acp", "resume_policy": "fresh", "command":
["grok", "agent", "--no-leader", "stdio"]}` runs the command above, with
wake disabled, under trust for the exact argv, environment and directory.
`amq setup` does not add it; write it by hand after `--check`.

`amq-acp host --check -- <agent command>` probes a harness before you use
it: it runs only `initialize`, with no AMQ root, handle, or session, prints
the agent's protocol version, info, `load_session`, and capabilities as
JSON, and exits 0. It exits 6 with the reason when the agent speaks another
protocol version, does not answer within 60 seconds, or exits.

```sh
amq-acp host --check -- grok agent --no-leader stdio
``` A turn whose prompt may have reached the
agent before a crash never runs again; on restart the host replies that it
restarted during the turn. A reply that can never be delivered is recorded
as `undeliverable` with the reason, and the host goes on to the next
message. A recipient whose mailbox is damaged (an unsafe or symlinked
mailbox, or a reply file name that holds another message id) stops the host
until the mailbox is repaired; `amq doctor` reports the damage. `settings.json` holds `turn_timeout` (default
`30m`), read at the start of each turn; a refused edit keeps the last good
value. On timeout or Ctrl-C the host sends `session/cancel` and waits 30
seconds for the turn to end.

The host never restarts the agent. When the agent exits, the host exits with
code 1; a turn in flight is answered with the agent's exit status. Ctrl-C,
SIGTERM, and SIGHUP (a closed terminal pane) stop the host the same way: it
closes the agent's stdin, waits 5 seconds, then kills the agent's process
group. A host killed with SIGKILL cannot do that; the agent then sees its
stdin close, and an agent that keeps running after stdin EOF is left behind. A message whose sender set cross-root reply routing (`reply_to` or
`reply_project`) runs no turn and gets a reply naming that limitation. The
host refuses to run on platforms without advisory file locks.

### agy over stream-json

`amq-acp host --protocol agy-stream -- agy [agy arguments]` makes agy, which
has no ACP agent mode, an AMQ participant through its headless stream-json
mode:

```sh
amq coop exec --no-wake --me agy amq-acp -- host --protocol agy-stream -- agy
```

The host starts agy itself with `--input-format stream-json --output-format
stream-json -p=`, so it refuses those flags, and `--conversation`, `-c`,
`--continue`, `--print`, `--prompt` and `-i`, in the agy arguments (exit 2).
Each message is one `user` line; the reply is that turn's `result.response`.
agy names its conversation in its `init` event before any turn, and
`session.json` records it. A restarted host starts agy with `--conversation
<id>`; when agy answers with another id (an unknown conversation runs under a
new one), the host says so and records the new id.

agy denies every tool call that needs a permission unless its own arguments
or its `permissions.allow` settings grant it, for example
`--dangerously-skip-permissions`; the host adds no permission flag. The reply
counts the denied calls (`denied_actions`). agy asks for no approval
headless, so a remote request never raises one.

Limits:

- agy has no cancel input. A timeout, Ctrl-C or an owner cancel sends agy
  SIGINT; agy ends the turn as interrupted and exits, and the next turn
  starts agy again on the same conversation.
- agy exiting between turns is not an error: the next turn starts it again.
  agy exiting during a turn ends the turn with its exit status, and the host
  exits.
- `--mode auto` is not available to every account; agy then continues in its
  default mode.
- `--check` starts agy, prints its `init` event as JSON, and stops it.
