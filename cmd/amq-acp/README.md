# amq-acp

`amq-acp` is a preview [Agent Client Protocol](https://agentclientprotocol.com)
companion. It speaks ACP over stdio and turns each prompt into an ordinary AMQ
message. It is a separate binary on purpose: `amq` itself gains no protocol
server and no listening socket.

## What it speaks

ACP **version 1 only**. `initialize` always answers `protocolVersion: 1`, even
when the client asks for a newer version, and it answers in v1's own response
shape (`agentCapabilities` and `agentInfo`, not v2's `capabilities` and `info`).
A client that needs v2 should disconnect.

Implemented methods:

| Method | Behavior |
| --- | --- |
| `initialize` | Answers `protocolVersion: 1` and the minimum honest capability set. Unknown top-level params are rejected. |
| `session/new` | Returns a `sessionId`. Requires a completed `initialize`. |
| `session/prompt` | Delivers the prompt text to `AMQ_ACP_TO` and returns `stopReason: "end_turn"`. |
| `session/cancel` | Acknowledged. Prompt turns complete synchronously, so nothing is ever in flight to abort. |

Everything else returns JSON-RPC `-32601`. There is no `session/load`, no
`fs/*`, no `terminal/*`, and no tool calling. The v1 baseline block types are
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

Configuration is fail-closed. A missing `AM_ROOT`, a relative root, pin evidence
without an exact `AM_BASE_ROOT`, a root that contradicts the pinned base and
session, or an identity token that no longer names the same physical directory
all refuse with exit code 5 before any message is written.

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

- ACP pool workers must not drain AMQ mailboxes. `amq-acp` only writes
  `inbox/new`. A separate Edge owner drains with `amq drain`.
- `amq-acp` refuses `BUZZ_ACP_AGENTS` other than `1` and `BUZZ_ACP_RESPOND_TO`
  other than `owner-only`, then unsets every `BUZZ_*` variable so a leaked
  nsec cannot enter AMQ messages. The gate waits for the harness to hold a
  Buzz NIP-PL kind:30350 lease with a quota-1 deployment-identity profile
  (block/buzz#5667); this companion does not invent a lease, it gates inbound
  on that lease once the profile ships.
- When `_meta.nostr.eventId` (or `_meta.triggeringEventIds`) names one 64-hex
  Nostr event, that id is the idempotency key. A second prompt with the same
  id is a no-op. Two ids in one prompt are refused: one event, one AMQ job.
- `_meta.amq` reports independent facts: `committed`, `drained`, `started`,
  `completed`, and `egress` (`confirmed` or `uncertain`). Queued is not
  drained. Uncertain egress is not retried.

## Limitations

- A prompt is **queued**, not consumed. `session/prompt` reports
  `state: "queued_to_inbox"` in `_meta.amq`. Use `amq receipts` for proof that
  the recipient actually drained it.
- Replies do not flow back to the ACP client. There are no `session/update`
  notifications, so the peer's answer must be read with `amq drain`, `amq read`,
  or `amq thread`.
- Every prompt is delivered to the single handle in `AMQ_ACP_TO`; the ACP
  session id does not select a recipient.
- No audit copy is written to the sender's `outbox/sent`, unlike `amq send`.
