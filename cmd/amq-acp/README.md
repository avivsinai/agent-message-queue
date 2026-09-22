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
| `session/new` | Returns a `sessionId` and `_meta.thread`, the durable AMQ cockpit thread for the session's channel. Requires a completed `initialize`. |
| `session/prompt` | Delivers the prompt text to `AMQ_ACP_TO` on the session's cockpit thread, then holds the turn open. The client receives `session/update` notifications as the turn progresses and the reply text as an `agent_message_chunk`; the final result is `stopReason: "end_turn"` with the reply in `_meta.amq`, or the typed refusal `stopReason: "refusal"` with `_meta.amq.state: "no_reply"` when the bounded wait expires. |
| `_session/steering` | Delivers owner steering on the session's cockpit thread, framed as untrusted task guidance. During an in-flight prompt it is AMQ `urgent` with the `buzz-steer` label and returns `outcome: "injected"`; while idle it is `normal` priority and returns `outcome: "startedNewTurn"`. The outcome names the delivery mode only: `_meta.amq` reports the prompt committed to the inbox, not drained or started, so neither outcome proves the peer acted. An in-turn steer refs the turn's prompt. A Nostr event id in `_meta` makes a redelivered steer idempotent. `initialize` advertises it as `_meta.steering.supported: true`. |
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
| `AM_ME` | Required sender handle. |
| `AMQ_ACP_TO` | Required recipient handle for every prompt. |
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

Install the binary from the matching `amq-acp_*_{linux,darwin}_{amd64,arm64}.tar.gz`
release asset; Homebrew does not install it. See [INSTALL.md](../../INSTALL.md).

## Buzz BYOH

This is a custom harness, not a Buzz preset. Copy
[`buzz-harness.json`](buzz-harness.json) to Buzz Desktop
`custom_harnesses/amq_acp.json`. Then add `env` on **that machine copy** with
`AM_ROOT`, `AM_ME`, and `AMQ_ACP_TO`. The committed example has empty `args` and
no env: Buzz's default `BUZZ_ACP_AGENT_ARGS=acp` would be extra argv and
`amq-acp` would refuse.

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

## Limitations

- Every prompt is delivered to the single handle in `AMQ_ACP_TO`; the ACP
  session id does not select a recipient.
- A timed-out turn is a refusal, not a retry. The peer's answer may still
  arrive later on the thread; read it with `amq thread` and re-prompt if the
  work must run.
- No audit copy is written to the sender's `outbox/sent`, unlike `amq send`.
