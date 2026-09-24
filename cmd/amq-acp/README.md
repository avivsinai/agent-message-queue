# amq-acp

`amq-acp` is a preview [Agent Client Protocol](https://agentclientprotocol.com)
companion. It speaks ACP over stdio and turns each prompt into an ordinary AMQ
message on a durable cockpit thread, then holds the turn open until a fresh
reply is observed on that thread. It is a separate binary on purpose: `amq`
itself gains no protocol server and no listening socket.

## What it speaks

ACP **version 2**. `initialize` answers `protocolVersion: 2`.

Implemented methods:

| Method | Behavior |
| --- | --- |
| `initialize` | Answers `protocolVersion: 2` and the minimum honest capability set. Unknown top-level params are rejected. |
| `session/new` | Returns a `sessionId`, `_meta.thread` (the durable AMQ cockpit thread for the session's channel), and `models` with exactly one model, the fixed destination. Requires a completed `initialize`. |
| `session/set_model` | Accepts only a model that `session/new` advertises in `models`: `amq:<AMQ_ACP_TO>`, `amq-remote:<target>` in remote mode, or `amq-remote:<name>` for a named binding in binding mode, which selects that binding for the session. This bridge runs no model; the destination answers with its own. Any other id is refused. |
| `session/prompt` | Delivers the prompt text to `AMQ_ACP_TO` on the session's cockpit thread, then holds the turn open. The client receives `session/update` notifications as the turn progresses and the reply text as an `agent_message_chunk`; the final result is `stopReason: "end_turn"` with the reply in `_meta.amq`, or the typed refusal `stopReason: "refusal"` with `_meta.amq.state: "no_reply"` when the bounded wait expires. |
| `_session/steering` | Delivers owner steering on the session's cockpit thread, framed as untrusted task guidance. During an in-flight prompt it is AMQ `urgent` with the `buzz-steer` label and returns `outcome: "injected"`; while idle it is `normal` priority and returns `outcome: "startedNewTurn"`. A redelivered steer event returns `outcome: "duplicate"` with the original message id and delivers nothing new. The outcome names the delivery mode only: `_meta.amq` reports the prompt committed to the inbox, not drained or started, so neither outcome proves the peer acted. An in-turn steer refs the turn's prompt. A Nostr event id in `_meta` makes a redelivered steer idempotent. `initialize` advertises it as `_meta.steering.supported: true`. |
| `session/cancel` | Ends the session's in-flight prompt turn: that `session/prompt` returns `stopReason: "cancelled"` with `_meta.amq.state: "cancelled"` and `reason: "session_cancelled"`. The queued AMQ prompt is not retracted and the peer is not notified; a later reply on the thread cannot answer a new prompt. With no turn in flight it is a no-op. |

Everything else returns JSON-RPC `-32601`. There is no `session/load`, no
`fs/*`, no `terminal/*`, and no tool calling. The v2 baseline block types are
accepted: `text` passes through and `resource_link` is rendered into the AMQ
body as a markdown link (`[title-or-name](uri)`). `promptCapabilities` are all
false, so `image`, `audio`, and embedded `resource` blocks are refused rather
than silently dropped.

## Live turns, threads, and honesty

- A prompt turn is **correlated, not fire-and-forget**. Only a message from the
  configured peer, on the prompt's thread, created after the prompt, whose
  `refs` name the prompt can answer the turn. `amq reply --id <prompt id>` sets
  those refs, and so does a reply to an in-turn steer. A stale reply, or a late
  answer to a cancelled prompt, never answers a later turn.
- A turn settles **once**: the first of reply, `session/cancel`, stream close
  and timeout decides it, and the others cannot overturn it.
- The turn is **bounded**. If no fresh reply arrives within
  `AMQ_ACP_TURN_TIMEOUT`, the result is the typed refusal
  `stopReason: "refusal"` with `_meta.amq.state: "no_reply"` and
  `_meta.amq.reason: "reply_timeout"` — never a fabricated success.
- The channel-to-thread mapping **persists** under
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
```

`setup` writes `custom_harnesses/amq_remote.json` with mode 0600, the stable
`amq-acp` path, and env `AMQ_ACP_REMOTE=binding` only. It does not record a
root, a target, or a session pin. `setup --session <name>` writes
`AMQ <name>.agent.json` in the current directory (`--out` chooses another
path): a `buzz-agent-snapshot` version 1 named `AMQ: <name>`, runtime
`amq_remote`, model `amq-remote:<name>`, parallelism 1, and `respondTo`
`owner-only`. That model selects the named binding, so the agent serves that
session only. A bare `setup` writes `AMQ Remote.agent.json` with model
`amq-remote`, which drives the only binding and refuses when several exist.
Import the file in Buzz Desktop: Agents, then + then Import, pick the file,
then Start. Then run `/amq-remote` in the session. A harness file this command
did not write is left unchanged.

## Buzz BYOH

This is a custom harness, not a Buzz preset. `amq-acp install` writes the
machine copy. The committed [`buzz-harness.json`](buzz-harness.json) has empty
`args` and no env: Buzz's default `BUZZ_ACP_AGENT_ARGS=acp` would be extra argv
and `amq-acp` would refuse.

- ACP pool workers must not drain AMQ mailboxes. `amq-acp` only writes
  `inbox/new`. A separate Edge owner drains with `amq drain`.
- `amq-acp` refuses `BUZZ_ACP_AGENTS` other than `1` and `BUZZ_ACP_RESPOND_TO`
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
- `_session/steering` is not available, and `initialize` reports
  `_meta.steering.supported: false`.
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
  only if the message is absent. Progress shows "Delivered", then "Read by"
  (the handle's drained receipt), then the reply. Stop ends the wait and says
  the message stays in the inbox; it is never recalled.
- A native binding (`attach --self --native`) submits there with that
  binding's native pin.
With no binding, the agent answers "Not connected. Run /amq-remote in a
Claude Code or Codex session." A request keeps the binding it started with;
a later attach moves only new prompts. `session/new` advertises one model per
named binding, `amq-remote:<name>`, and `session/set_model` selects which
binding the ACP session drives, so each Buzz agent serves exactly one session.
With no selection it uses the only binding, or refuses when there are several.
With no binding it advertises the plain `amq-remote` model. `AMQ_ACP_TO`, `AMQ_ACP_REMOTE_TARGET` and
`AMQ_ACP_REMOTE_NATIVE_SESSION` are refused in this mode.

### Posting answers into Buzz

buzz-acp sends ACP answer text only to the owner's activity view; a Buzz
agent's chat reply exists only if the agent publishes it. So amq-acp posts
every owner-facing text (a final answer, "Not connected", a busy or stop
notice) into the channel named by the prompt's `<context>` block with
`buzz messages send`, as the managed agent. The identity (`BUZZ_PRIVATE_KEY`,
`BUZZ_AUTH_TAG`, `BUZZ_RELAY_URL`) is kept in process memory for that child
only; the environment is still stripped and no AMQ message carries it.
`_meta.remote.posted` (or `_meta.amq.posted`) is `posted` or the error. The
binary is `AMQ_ACP_BUZZ_CLI`, else `buzz` on `PATH`, else the one bundled
with Buzz Desktop.

### Trust limits

In Buzz Desktop, the managed agent's identity and grant belong to Desktop,
and amq-acp never receives the key. Desktop signs a grant with no kind limit
and no expiry. Archiving the agent hides it and removes Desktop's local key;
it does not invalidate a copied key and grant. Only removing the agent's relay
access does that. Buzz's owner-only setting also admits the owner's other
agents. The strict per-kind, expiring path is `amq-remote` with a `relay`
manifest block.

## Limitations

- Every prompt is delivered to the single handle in `AMQ_ACP_TO`; the ACP
  session id does not select a recipient.
- A timed-out turn is a refusal, not a retry. The peer's answer may still
  arrive later on the thread; read it with `amq thread` and re-prompt if the
  work must run.
- No audit copy is written to the sender's `outbox/sent`, unlike `amq send`.
