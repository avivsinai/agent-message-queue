# amq-acp

`amq-acp` is an ACP v2 agent over stdio. It queues a prompt to an AMQ
mailbox on a durable cockpit thread and holds the turn open until a reply
arrives. Remote mode sends the prompt to a named `amq-remote` target.
[`amq-acp host`](#host-mode) runs an ACP agent as an AMQ participant.

It runs no model, executes no tools, and reads no files on behalf of a
client. `amq` itself gains no protocol server or listening socket.

## What it speaks

ACP **version 2**. `initialize` answers `protocolVersion: 2`.

| Method | Behavior |
| --- | --- |
| `initialize` | Answers `protocolVersion: 2`. Unknown top-level params are rejected. |
| `session/new` | Returns a `sessionId` and the durable thread. |
| `session/set_model` | Accepts the sole advertised model: the mailbox recipient or `amq-remote:<target>`. |
| `session/prompt` | Queues a prompt on the cockpit thread and holds it open for a bounded reply. |
| `_session/steering` | Delivers steering as untrusted task guidance, urgent during a turn and normal while idle. |
| `session/cancel` | Ends the in-flight turn. The queued prompt is not retracted. |

Everything else returns JSON-RPC `-32601`. There is no `session/load`,
`fs/*`, `terminal/*`, or tool calling. A prompt carries text or resource
links only; image, audio and embedded resources are refused.

## Turn semantics

- Only a reply from the configured peer, in the prompt's thread, created
  after the prompt, whose refs name it, answers the turn.
- The peer check is not authentication; agents of the same user share the
  queue root and can write replies.
- A turn settles once: the first of reply, cancel, stream close and
  timeout decides it. A cancelled prompt's queued message is not retracted.
- The reply wait bounds at `AMQ_ACP_TURN_TIMEOUT`; expiry returns
  `stopReason: "refusal"` with `_meta.amq.state: "no_reply"` and reason
  `"reply_timeout"`, never a success.
- The channel-to-thread map persists in `AMQ_ACP_STATE_DIR` (default
  `AM_ROOT/meta/acp`), so the server resumes its thread after a restart.
- While a turn is open, the server sends `agent_thought_chunk` and
  `agent_message_chunk` notifications. Closing stdio ends turns with
  reason `"client_disconnected"`.

## Configuration

| Variable | Meaning |
| --- | --- |
| `AM_ROOT` | Absolute queue root. |
| `AM_ME` | Sender handle. |
| `AMQ_ACP_TO` | Recipient handle for every prompt. |
| `AMQ_ACP_REMOTE_TARGET` | Submit each prompt to an `amq-remote` target. |
| `AMQ_ACP_REMOTE_NATIVE_SESSION` | Native session shared by the owner. |
| `AM_BASE_ROOT` | Pinned base root. |
| `AM_SESSION` | Pinned session name. |
| `AM_ROOT_ID`, `AM_BASE_ROOT_ID` | Identity tokens authenticating those roots. |
| `AMQ_ACP_STATE_DIR` | Durable thread state (default `AM_ROOT/meta/acp`). |
| `AMQ_ACP_TURN_TIMEOUT` | Reply wait (default `10m`). |
| `AMQ_ACP_POLL_INTERVAL` | Reply poll interval (default `100ms`). |
| `AMQ_ACP_HEARTBEAT_INTERVAL` | Session heartbeat interval (default `15s`). |

A prompt must not gain identity from its text. Set these variables in the
server process's environment. Configuration is fail-closed: a missing or
relative root, an unauthenticated pin, or a root that contradicts its
session pin refuses with exit code 5 before a message is written.

`~/.amq/acp.settings` overrides the three durations at the next prompt.
Keys match the `AMQ_ACP_*` names above and hold positive Go durations.
Invalid JSON, unknown keys or non-positive durations keep the last good
values. A startup value remains until the next prompt.

```sh
# Replace the placeholder root with the absolute queue root on this machine.
AM_ROOT=/absolute/path/to/.agent-mail/collab AM_ME=cursor AMQ_ACP_TO=codex amq-acp
```

`brew install avivsinai/tap/amq` puts `amq-acp` on `PATH`. Direct installs
use the `amq-acp_*_{linux,darwin}_{amd64,arm64}.tar.gz` release asset; see
[INSTALL.md](../../INSTALL.md).

## Remote mode

Set `AMQ_ACP_REMOTE_TARGET` to `amq-remote:<target>`. A remote session
shares the target's native session; the owner confirms the share with a
passkey or an allowlist. A `session/prompt` posts into that session. The
endpoint's session id must match `AMQ_ACP_REMOTE_NATIVE_SESSION`.

A `session/cancel` attempts to cancel the remote prompt. A prompt records
its delivery but not that the remote agent has acted.

## Host mode

`amq-acp host` runs an ACP agent as the mailbox participant whose handle
is `AM_ME`. Each inbox message is one ACP turn; the answer becomes a reply
on the same thread.

```sh
AM_ROOT=/absolute/path/to/.agent-mail/collab AM_ME=assistant \
  amq-acp host -- my-agent-command
```

The `AM_ME` mailbox is used only for those turns. A steering message can post
mid-turn with the `buzz-steer` label. Host mode inherits the queue root's
access control: any writer on it can post to the mailbox. The agent
executed by `--` is not an access boundary.

Each prompt runs in `{cwd}/agents/{handle}` by default. `--cwd` overrides
the root. `--check` verifies that the process can start, without a turn.
