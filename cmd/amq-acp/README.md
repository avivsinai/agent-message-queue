# amq-acp

`amq-acp` is an [Agent Client Protocol](https://agentclientprotocol.com) v2
bridge. It speaks ACP over stdio and turns prompts and steering into messages
on a durable AMQ cockpit thread. It is a separate binary on purpose: `amq`
itself gains no protocol server and no listening socket.

## What it speaks

ACP **version 2**. `initialize` answers `protocolVersion: 2` and advertises
`_meta.steering.supported: true`. Prompt turns stay open while the bridge polls
the pinned AMQ thread for a reply. It emits standard `session/update` thought
and message chunks, then returns the reply with `stopReason: "end_turn"`.

Implemented methods:

| Method | Behavior |
| --- | --- |
| `initialize` | Answers `protocolVersion: 2`, advertises steering, and rejects unknown top-level params. |
| `session/new` | Returns a `sessionId` plus the channel/thread mapping. `_meta.channelId` (including nested `channelId` forms) selects the durable cockpit thread. |
| `session/prompt` | Delivers a normal-priority prompt to `AMQ_ACP_TO`, waits for a fresh reply on the same thread, emits updates, and returns the reply. A bounded wait returns `stopReason: "refusal"` with `_meta.amq.state: "no_reply"`. |
| `_session/steering` | Delivers on the same thread. During a prompt it uses AMQ urgent priority and the `buzz-steer` label, returning `injected`; while idle it uses normal priority and returns `startedNewTurn`. |
| `session/cancel` | Cancels an in-flight AMQ reply wait and returns a typed `cancelled` stop result. |

Everything else returns JSON-RPC `-32601`. There is no `session/load`, no
`fs/*`, no `terminal/*`, and no tool calling. The baseline block types are
accepted: `text` passes through and `resource_link` is rendered into the AMQ
body as a markdown link (`[title-or-name](uri)`). `promptCapabilities` are all
false, so `image`, `audio`, and embedded `resource` blocks are refused rather
than silently dropped.

## Configuration

| Variable | Meaning |
| --- | --- |
| `AM_ROOT` | Required absolute queue root. |
| `AM_ME` | Required sender handle. |
| `AMQ_ACP_TO` | Required recipient handle for every prompt. |
| `AM_BASE_ROOT` | Pinned base root; required whenever any pin variable is set. |
| `AM_SESSION` | Pinned session name. |
| `AM_ROOT_ID`, `AM_BASE_ROOT_ID` | Identity tokens authenticating the two roots. |
| `AMQ_ACP_STATE_DIR` | Optional durable state directory. It must remain under `AM_ROOT`; default is `AM_ROOT/meta/acp`. |
| `AMQ_ACP_TURN_TIMEOUT` | Maximum wait for a reply. Default `10m`. |
| `AMQ_ACP_IDLE_TIMEOUT` | In-memory runtime idle expiry. Default `15m`; channel/thread state remains durable. |
| `AMQ_ACP_POLL_INTERVAL` | Reply polling interval. Default `100ms`. |
| `AMQ_ACP_HEARTBEAT_INTERVAL` | `agent_thought_chunk` heartbeat interval. Default `15s`. |

Configuration is fail-closed. A missing `AM_ROOT`, a relative root, pin evidence
without an exact `AM_BASE_ROOT`, a root that contradicts the pinned base and
session, an out-of-root state directory, or an identity token that no longer
names the same physical directory all refuse with exit code 5 before any
message is written.

```sh
AM_ROOT="$AM_ROOT" AM_ME=cursor AMQ_ACP_TO=codex amq-acp
```

Pin those values in operator config or in a local Buzz harness copy. Chat and
prompt text must not pass `--root`, recipients, or argv.

Install the binary from the matching `amq-acp_*_{linux,darwin}_{amd64,arm64}.tar.gz`
release asset; Homebrew does not install it. See [INSTALL.md](../../INSTALL.md).

## Buzz BYOH

This is a Tier-3 custom harness, not a Buzz preset. Copy
[`buzz-harness.json`](buzz-harness.json) to Buzz Desktop
`custom_harnesses/amq_acp.json`. Then add `env` on **that machine copy** with
`AM_ROOT`, `AM_ME`, and `AMQ_ACP_TO`. The committed example has empty `args` and
no env: Buzz's default `BUZZ_ACP_AGENT_ARGS=acp` would be extra argv and
`amq-acp` would refuse.

ACP pool workers must not drain AMQ mailboxes. `amq-acp` writes `inbox/new` and
scans only the pinned thread for a reply. A separate Edge owner or recipient
agent drains the inbox.

## Limitations

- AMQ delivery is durable queueing, not proof of consumption. The bridge waits
  for a fresh reply from `AMQ_ACP_TO`; it does not claim success on queueing
  alone.
- The first fresh reply on the cockpit thread completes the ACP turn. Send the
  substantive answer as the first reply; an interim acknowledgement such as
  `on it` ends the turn and later messages are unsolicited.
- Every prompt is delivered to the single handle in `AMQ_ACP_TO`. The ACP
  channel metadata selects the thread, not a different recipient.
- No audit copy is written to the sender's `outbox/sent`, unlike `amq send`.
- A malformed or missing channel mapping is refused. The bridge never guesses
  an external queue root or an unpinned identity.
