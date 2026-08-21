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
`fs/*`, no `terminal/*`, and no tool calling. `promptCapabilities` are all
false, so only `text` content blocks are accepted; any other block type is
refused rather than silently dropped.

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
