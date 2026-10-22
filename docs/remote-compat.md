# Remote compatibility manifest and seam register

## 1. Purpose

This is the compatibility manifest and seam register for the `amq-remote`
companion described in [the remote-control ADR](adr-remote-control.md). It
records the exact symbol, RPC method, or CLI command each harness seam
depends on, and the evidence behind it. The evidence is not a claim about
current support on an unverified installation. Re-check the relevant rows
after upgrading a harness or runtime; a version bump alone does not change a
capability's truth value.

## 2. How to read the evidence

A row that depends on one harness build says which build it was observed on.
A row without a version describes a documented interface or a stable schema
method.

Sources are vendor documentation, vendor source at a pinned tag, the
harness's own CLI output (`--help`, `codex app-server generate-json-schema`),
the [Claude Code socket wire format](research/r0-03-cc-socket-wire-capture.md)
capture, and the AMQ Go symbol that enforces a row. Claims are graded:

- `[live]`: observed against a running harness.
- `[binary]`: derived from strings and symbols in the shipped binary.
- source-read: read from vendor source at the named tag, not observed live.
- **unverified**: inferred, not stated by the cited source or exercised.

## 3. Seam register

Each cell states the exact symbol, command, or RPC method, or `unavailable`,
plus its evidence class: **delivered** (bytes left this process),
**submitted** (the harness accepted the payload for later admission),
**admitted** (the harness gave back a run identity), or **completed** (the
harness reported a terminal outcome for that run).

### 3.1 Codex

Method names are from the app-server JSON schema that `codex app-server
generate-json-schema` writes; `thread/queue/*` appears only with
`--experimental`. The adapter is `internal/remote/codex`.

| Row | Mechanism | Evidence class | Citation |
| --- | --- | --- | --- |
| Submit entry | `turn/start` (fresh turn) or `thread/queue/add` (FIFO, applies when the thread goes idle) | admitted (see acceptance evidence); submitted (queue) | Schema; `Attachment.Submit` |
| Acceptance evidence | The `userMessage` item whose `clientId` matches the submit `clientUserMessageId`, **not** the `turn/start` `result.turn.id`. `[live]` `turn/start` on a busy thread joins the running turn and discards the caller's input, so the returned `turnId` cannot tell admitted input from a silent join; the `userMessage` echo survives reconnect via `thread/read`. | admitted | `Attachment.confirmRun` (the confirmation gate) |
| Native run identity | `turnId` bound to `threadId` | admitted | Schema |
| Completion evidence | `turn/completed` notification (`status: completed\|failed\|interrupted`) | completed | Schema |
| Exact cancellation gate | `turn/interrupt {threadId, turnId}`. `[live]` It works from a second connection that does not own the turn. | completed (drives `turn/completed(interrupted)`) | Schema; `Attachment.CancelExact` |
| Steer | `turn/steer` | submitted | Schema |
| Approvals/questions | `ExecCommandApproval`, `ApplyPatchApproval`, `FileChangeRequestApproval`, `CommandExecutionRequestApproval`, `PermissionsRequestApproval`, `ToolRequestUserInput`, `McpServerElicitationRequest` (server-initiated JSON-RPC requests). Every client on the thread receives the request, the first response wins, and `serverRequest/resolved` follows (source-read at `rust-v0.156.1`, `codex-rs/app-server/src/outgoing_message.rs:330-495`). AMQ answers only the decision approvals; a permissions, user-input or MCP elicitation request shows in the DM as "Answer in the terminal." with no one-tap answer | submitted (request), answered by client | Schema; `Attachment.onServerRequest` |
| Session-switch/reload epoch triggers | `thread/resume` (rehydrates full history), `thread/started` and `thread/status/changed` notifications | delivered | Schema |
| Local draft access (must not submit) | `unavailable`: the schema has no draft or compose method; the only staging primitive is `thread/queue/add`, which is a real queued submission, not a draft | n/a | Schema |
| Inspect/roster | `thread/read` (`includeTurns`), `thread/items/list`, `thread/turns/list`, `codex agents` | delivered (polling), strong typed schema | Schema; `codex agents --help` |
| Observation | Connecting and calling `thread/resume`, then reading the broadcast `item/*`, `turn/*` and `thread/status/changed` stream. `[live]` A second client that has not resumed already receives `thread/started` | delivered | `Attachment.onNotification` |

### 3.2 pi

pi citations name files in the public
[pi repository](https://github.com/earendil-works/pi) under
`packages/coding-agent/`. The bridge is a pi extension that reads request
files from the pi-bridge directory and calls the extension API; a claim marked
**unverified** is not stated in those files.

| Row | Mechanism | Evidence class | Citation |
| --- | --- | --- | --- |
| Submit entry | The bridge extension calls `pi.sendUserMessage(text, {deliverAs: "followUp"})` for each request file | delivered (queues natively; not itself a receipt) | (source: pi `src/core/extensions/types.ts` `sendUserMessage`; `docs/extensions.md` "Choose an integration point") |
| Acceptance evidence | None native — `sendUserMessage` returns `void` | delivered only | (source: pi `src/core/extensions/types.ts` `sendUserMessage`) |
| Native run identity | `unavailable` — the extension API returns no run id for a sent message. The bridge owns the input boundary best-effort: admission requires `ctx.isIdle()` and no `ctx.hasPendingMessages()` (otherwise the request is refused `busy`) and is serialized to one remote request in flight, but a local message submitted in the same instant can still race it; correlation is by exact `message_start` user-message text (duplicate texts are indistinguishable) plus the persisted entry id from `ctx.sessionManager.getEntries()`. **Unverified:** that pi reports a `sendUserMessage` failure only as an error event, never to the caller, and that the entry id appears only at `message_end` | submitted (extension-owned boundary; not the design's admitted, and `Known=false` is not proof of non-admission here), completed (`turn_end`) | (source: pi `src/core/extensions/types.ts` `isIdle`, `message_start`, `message_end`, `turn_end`, `getEntries`) |
| Completion evidence | `agent_end`/`agent_settled` extension events, or the session JSONL's `message`/`custom` entries | completed (via bridge correlation only) | (source: pi `docs/extensions.md` "Respect the runtime lifecycle"; `docs/session-format.md`) |
| Exact cancellation gate | `ctx.abort()` aborts the current agent operation; the extension context has no clear-queue or dequeue-by-item method (`hasPendingMessages()` only). The bridge exposes no cancel request, so the adapter answers `unsupported`. **Unverified:** that `ctx.abort()` keeps queued follow-ups | `unsupported` | (source: pi `src/core/extensions/types.ts` `abort`, `hasPendingMessages`) |
| Steer | `pi.sendUserMessage(text, {deliverAs: "steer"})`, or the `steer` RPC command | submitted | (source: pi `src/core/extensions/types.ts` `sendUserMessage`; `docs/rpc-commands.md` "steer") |
| Approvals/questions | `ctx.ui.select(...)` is an in-process await and `pi.events` is an in-process bus between extensions. Stock pi raises no tool approval that an extension can answer from outside, so the reference bridge (revision 3) has no decision seam. Bridge revision 4 adds one for an extension that holds a tool call for an external decision: `interaction` event lines and adapter-written `answers/` files (see [the pi bridge protocol](pi-bridge-protocol.md), "Bridge revision 4: tool approval"). Questions have no seam | approvals: revision 4 only; questions: n/a | (source: pi `src/core/extensions/types.ts` `select`, `events`; `docs/extensions.md` "Choose an integration point") |
| Native session identity | `ctx.sessionManager.getSessionId()`: the session header id, kept when a session file is loaded again (reload, resume; compaction only appends entries) and new on `newSession` and on a branched (forked) session. A bridge of revision 4 publishes it as `session_id` in `bridge.liveness`; the adapter reports it as the native session id a relay share pins | delivered (revision 4 only) | (source: pi `src/core/extensions/types.ts` `sessionManager`; `src/core/session-manager.ts` `ReadonlySessionManager`, `getSessionId`, `newSession`) |
| Session-switch/reload epoch triggers | `session_before_switch`, `session_before_fork`, `session_before_compact`, `session_shutdown`, `session_before_tree` extension events | delivered | (source: pi `src/core/extensions/types.ts` event declarations) |
| Local draft access (must not submit) | `ctx.ui.getEditorText()` reads the input editor text; the bridge does not read it. The `clear_queue` RPC command returns already-queued text, not an unsent draft | n/a | (source: pi `src/core/extensions/types.ts` `getEditorText`; `docs/rpc-commands.md` "clear_queue") |
| Inspect/roster | RPC `get_state`, `get_tree`, `get_messages`, `get_session_stats`, and `get_entries` with a `since` entry-id cursor | delivered | (source: pi `docs/rpc-commands.md`) |
| Observation | Session JSONL append-only files under `~/.pi/agent/sessions/` plus the `pi.events` in-process bus for extensions in the same runtime; RPC mode is a child process over stdin/stdout, with no socket or server mode | delivered (a file tail is lossy for streaming deltas) | (source: pi `docs/sessions.md`, `docs/session-format.md`, `docs/rpc.md`) |

### 3.3 Claude Code

"Capture" is the [Claude Code socket wire format](research/r0-03-cc-socket-wire-capture.md),
observed on Claude Code 2.1.278. The adapter is `internal/remote/claude`.

| Row | Mechanism | Evidence class | Citation |
| --- | --- | --- | --- |
| Submit entry | Cross-session messaging socket (`messagingSocketPath` in `~/.claude/sessions/<pid>.json`), optional `{"type":"auth","token":"..."}` first line on macOS/Linux | delivered, not admitted | Capture §1, §2 |
| Wire protocol | Newline-delimited JSON over the UDS: auth line `{"type":"auth","token":"<target peerToken>"}` (token from `~/.claude/sessions/<pid>.<sha256(resolve(sockPath))>.key`), then one frame `{msgV:1,msg_id:<uuid>,type:"user",message:{role:"user",content:"<cross-session-message XML envelope>"},priority:"next",from:"<sender addr>"}`. Envelope: `<cross-session-message from="…" from-session="…" from-name="…">\n<body>\n</cross-session-message>`, byte-exact, because the receiver re-serializes and compares. 1 MiB line cap. No per-frame ack on the happy path; malformed frames are dropped silently. `[live]` A hand-crafted frame was delivered to a `claude --bg` target and rendered as `› Message from @…: …` | delivered | Capture §1-§4 |
| Acceptance evidence | `[live]` The target's JSONL transcript gains a `type: "queue-operation"` entry, then a `type: "user"` entry carrying the exact envelope plus the harness wrapper ("Another Claude session sent a message: …") and `origin: {kind: "peer", msg_id: <frame msg_id>}`. A target that is mid-turn absorbs the frame instead as a `type: "attachment"` entry with `attachment.type: "queued_command"` and the same `attachment.origin.msg_id`. The adapter correlates on that `msg_id` in both shapes | submitted | Capture §3.2; `parseTranscriptLine` |
| Busy/queue and dedup behavior | `[live]` Frames sent while the target is mid-turn are parked and queued, rendered as `› Message from @…` banners at the next turn boundary, and processed in one batched turn; duplicate bodies within 30 s are dropped with a visible in-session notice `Dropped a peer message from @…: identical to the previous message from this sender.` `[binary]` The queue cap is 50 (`maxQueuedPeerMessages`; overflow drops as `queue-full`); overflow is not live-exercised | delivered | Capture §5 |
| Inbound policy | `crossSessionInbound` setting: `accept`, `hold` or `refuse`; policy > user > repo, and a repo may only tighten; an invalid value fails closed to `hold`; an explicit value always wins. `[binary]` Unset is mode-parity gating, not unconditional delivery: it auto-delivers only when the sender's permission-mode class matches the target's, holds a mismatched sender, and holds a sender asserting no class while the target bypasses prompts. AMQ envelopes omit `from-mode`, so unattended sends need the user-level value **`accept`**. The listener is gated by the `CLAUDE_CODE_HARBOR_KITE` env, else flag `tengu_harbor_kite` (default on; fails closed on sockets-dir vetting) | delivered | Capture §5a |
| Native run identity | `unavailable`: the channel has no request id or turn id | n/a | Capture §3 |
| Completion evidence | Opt-in user-level `Stop` hook reporting `prompt_id`, `transcript_path`, `last_assistant_message` | completed (requires the target's own settings.json) | [Claude Code hooks](https://code.claude.com/docs/en/hooks) "Stop" |
| Exact cancellation gate | `unavailable`: no documented user-level way interrupts a running foreground turn without keystrokes | n/a | Claude Code hooks and CLI documentation |
| Steer | `unavailable`: delivery timing ("arrives between tool calls") is not steering semantics | n/a | Capture §5 |
| Approvals | Opt-in `PermissionRequest` command hook (`amq-remote claude install-approval-hook`). Its stdin carries `session_id`, `prompt_id`, `tool_name` and `tool_input`, but no `tool_use_id`. AMQ's hook prints `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"…"}}}` for the owner's ❌, and `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}` only when the checks in [remote approvals](amq-remote-approvals.md) pass. No output leaves the terminal dialog to decide. The allow path is not live-verified. `[live]` on 2.1.283: the hook's `prompt_id` equals the `promptId` of the peer delivery line; a terminal reject sends SIGTERM to the hook, then SIGKILL about 1 s later; a terminal approve sends no signal, and the call's `tool_result` line (with `promptId` and `tool_use_id`) is the only trace; parallel decision hooks race and the first decision wins. Questions stay `unavailable` | delivered (live probe; DM path not live-verified) | [Claude Code hooks](https://code.claude.com/docs/en/hooks) "PermissionRequest" |
| Session-switch/reload epoch triggers | `unavailable` as a documented signal; `queue-operation` transcript entries mark messages queued while busy | delivered (observed in transcript only) | Capture §3.2 |
| Local draft access (must not submit) | `unavailable`: no draft or compose primitive on the messaging socket or CLI surface | n/a | Capture §3 |
| Inspect/roster | `claude agents --json` (`status`, `waitingFor`, `state`, `kind`, `pid`) | delivered, session-level polling only | `claude agents --help` |
| Observation | Transcript JSONL tail (`~/.claude/projects/<slug>/<uuid>.jsonl`) plus `~/.claude/sessions/<pid>.json` process metadata (`messagingSocketPath`, `peerProtocol`, `peerFeatures`) | delivered | Capture §1, §3.2; `transcriptPath` |

A request bound to one session (a Buzz share or a link) is refused with
`session_changed` when the registry names another session for the pid,
checked at dispatch and again right before the socket write. A frame names no
destination session and its token binds only the pid and the socket path, so
a session switch after that last check, on the same socket, is not detected.

### 3.4 ACP host

The adapter is kind `acp` in `internal/remote/pi`, over the bridge seam of
[the pi bridge protocol](pi-bridge-protocol.md) ("The acp-host wire"). The
extension side is `amq-acp host` (`internal/acp/host`), the only ACP client
of its agent. ACP facts are from agentclientprotocol.com.

| Row | Mechanism | Evidence class | Citation |
| --- | --- | --- | --- |
| Submit entry | The host writes the receipt, then sends one `session/prompt` with the request text | delivered | `Host.runRequest` |
| Acceptance evidence | None native: `session/prompt` returns only at the end of the turn | submitted | ACP `/protocol/prompt-turn` |
| Native run identity | `unavailable`: ACP returns no run id at prompt time | n/a | ACP `/protocol/prompt-turn` |
| Completion evidence | The `session/prompt` response `stopReason` | completed | ACP `/protocol/prompt-turn`; `requestOutcome` |
| Exact cancellation gate | The adapter writes `cancels/<ref>.json` (bridge revision 5). The host sends `session/cancel`, which is session-wide, only after checking that the ref is its prompt in flight; it is the agent's only client and keeps one prompt in flight, so the cancel stops that request alone. A ref that has not run is refused `cancelled` | completed (`stopReason: cancelled`) | ACP `/protocol/prompt-turn`; `Host.prompt`, `Attachment.CancelExact` |
| Steer | `unavailable` | n/a | - |
| Approvals | `session/request_permission` during a request's turn becomes an `interaction` line with `presence: local_only`; the owner can reject from the remote face, and the host's policy rejects at expiry. Not live-verified against a real harness | approvals: reject only | ACP `/protocol/tool-calls`; `Host.askOwner` |
| Native session identity | The ACP `sessionId`, published as `session_id` in `bridge.liveness` | delivered | `Host.writeLiveness` |
| Session-switch epoch triggers | A host start mints a new `session_generation` | delivered | `Host.startBridge` |
| Local draft access (must not submit) | n/a: the host has no editor | n/a | - |

## 4. Measured adapter constraints

These observations constrain the adapters and remain part of the compatibility
contract:

- **Codex busy admission:** `turn/start` on an active thread returns the
  existing turn and silently discards the new caller input. The adapter must
  check status inside its admission boundary or use `thread/queue/add` when
  queueing is explicitly requested. A returned `turnId` alone is not proof
  that the caller's input was admitted; the matching `userMessage` item is.
  (`[live]`; `Attachment.Submit`, `Attachment.confirmRun`)
- **Codex transport and fanout:** the Unix socket is a WebSocket control
  socket; only `--stdio`/`stdio://` uses plain framing. Observed on codex-cli
  0.160: `codex app-server daemon start` makes the default control socket
  `~/.codex/app-server-control/app-server-control.sock` a symlink to the
  daemon's socket at `/private/tmp/codex-daemon-<uid>/<hash>`. Discovery
  follows that symlink only to a socket the current user owns and keeps the
  symlink path in the attach config. Re-check this layout after a Codex
  upgrade. `[live]` Server notifications and non-owning `turn/interrupt` fan
  out to connected clients. Approval-request fanout is source-read at
  `rust-v0.156.1`, not observed live (§5).
  (`DefaultControlSocket`, `checkControlSocket` in `internal/remote/codex`)
- **Codex approvals reviewer:** observed on codex-cli 0.160, a thread's
  `approvalsReviewer` (`user`, `auto_review`, or the legacy
  `guardian_subagent`) comes from `approvals_reviewer` in
  `~/.codex/config.toml` when the thread starts. An automatic reviewer
  answers approval requests itself, so few or none reach a remote client.
  `thread/resume` returns the thread's reviewer and
  `thread/settings/updated` reports a change; the adapter publishes an
  automatic one as the session's `approval_reviewer`, and doctor reports it
  for a target with `approve`. A `-c approvals_reviewer=user` on a resumed
  TUI does not change the reviewer of turns submitted remotely; a thread
  started with it does. An `approvalsReviewer` override on `turn/start`
  applies to that turn and every later one, the terminal's included, so the
  adapter never sends one.
  (`[live]`; schema; `Attach`, `autoReviewer`)
- **Claude Code delivery:** a message delivered to a busy session is read at
  a tool boundary and does not interrupt a running tool; an idle session
  starts a new turn. No documented user-level interrupt exists over the
  messaging socket, so delivery is not steering or exact cancellation.
  (Capture §5)

## 5. Capability projection per harness (schema `amq.remote.session/1`)

Fields: `inspect`, `submit`, `cancel_request`, `answer_question`,
`approve_tool`, `steer`, `terminal`. Each non-true value has a reason in the
seam rows above. The runtime schema uses Boolean negotiated capabilities,
with `terminal` as its fixed v1 `unavailable` enum. `unverified` is a
documentary evidence label and is not a schema value; `unavailable` is a
valid value for the runtime `terminal` enum.

| Harness | inspect | submit | cancel_request | answer_question | approve_tool | steer | terminal |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Codex | true | true | true | false | opt-in | false | unavailable |
| pi | true | true | bridge revision 5 | false | bridge revision 4 | false | unavailable |
| ACP host | true | true | true | false | reject only | false | unavailable |
| Claude Code | true | submitted | false | false | opt-in, reject only | false | unavailable |

The v1 endpoint masks `steer` to false, even where an attachment declares it
(`Endpoint.sessionProjection` in `internal/remote/core`). Claude Code
`submit` is graded **submitted**: after a socket send, the target's
transcript gains the `type: "user"` JSONL entry carrying the exact envelope
(preceded by a `queue-operation` entry). The harness accepted the payload,
but the channel has no admission receipt and no run identity, so a submit
never reaches `admitted`. The void-returning `sendUserMessage` seam is pi's,
not Claude Code's.

Reasons for every `false` (source: as cited per row in §3):

- `codex.approve_tool` is opt-in per target (`"approve": true`). Source-read
  at `rust-v0.156.1`: the app-server sends each approval request to every
  client on the thread, applies the first response, drops later ones with
  only a log line, and then sends `serverRequest/resolved` to every client
  (`codex-rs/app-server/src/outgoing_message.rs:330-495`,
  `thread_lifecycle.rs:864-885`). The adapter answers
  approvals of runs it submitted and, for a turn it did not submit (one
  started in the Codex TUI), decision approvals on the relay share pinned to
  the thread. It clears an approval on `serverRequest/resolved` or when its
  turn completes, so a late answer is refused. A remote approve is offered
  only for a plain command that the prompt shows whole; file changes and
  network, permission or write-root grants take a remote reject only. Not
  live-verified against a running Codex terminal.
- `codex.answer_question`: user-input requests are not projected.
- `codex.terminal`: the app-server protocol has no PTY concept.
- `pi.cancel_request`: the reference bridge (revision 3) has no cancel
  request, and the adapter advertises it only for a live bridge of revision
  5. `ctx.abort()` aborts the current operation, not one request, and the
  extension context has no dequeue primitive for a queued request that has
  not started (source: §3.2 "Exact cancellation gate").
- `pi.approve_tool` is true only while the live bridge advertises
  `bridge_revision` 4 or higher. That extension raises each approval under a
  request it owns as an `interaction` line and applies the first decision
  from a local face or from the adapter's `answers/` file, bound to the
  interaction id, manifest hash and expiry. A remote approve is offered only
  for `presence: remote` and a prompt the owner sees whole; otherwise the
  remote is offered reject only. The reference bridge is revision 3 and
  advertises no approvals. Not live-verified against a revision-4 extension.
- `pi.answer_question`: the bridge has no external decision seam for the
  in-process `ctx.ui.select` await (source: §3.2 "Approvals/questions").
- `pi.terminal`: no terminal binding is part of this adapter contract.
- `acp.approve_tool` is advertised (the host is bridge revision 4), with a
  remote reject only: every approval is `presence: local_only` until a live
  capture shows that a harness puts the whole call in `rawInput`.
- `acp.answer_question`: ACP has no question seam the host serves.
- `acp.terminal`: no terminal binding is part of this adapter contract.
- `claude_code.cancel_request`: no user-level interrupt exists at all
  (§3.3 "Exact cancellation gate").
- `claude_code.approve_tool`: opt-in through the manifest `approve` and the
  `PermissionRequest` hook, and only for a tool call of a request that a
  relay share submitted as its own turn. Buzz blocks any call; it allows
  only a Bash call shown whole, with the owner's signed ✅ verified by the
  hook against the owner key pinned on its command line. Not live-verified
  through the Buzz DM. The adapter config `observe_approvals` (without
  `approve`) shows the pending approval as the request's interaction with
  `remote_answer` false: `approve_tool` stays false, a remote answer is
  refused `unsupported`, and the hook, pinned by an observe-only pin, raises
  the request and applies no answer file. The terminal decides. One adapter
  entry has one mode: with both `approve` and `observe_approvals`, `approve`
  wins.
- Discovery (`up --discover`) lists a live pi chat under the root as
  target `pi:<handle>` with its manifest entry, from the handle's
  `bridge.liveness`, and prints the chat's pi `session_id` as
  `native_session_id=` when the bridge publishes one. `attach --self`
  matches the liveness `pid` against its own process ancestry to find the
  chat it runs in, refuses when two chats report the same pid, and pins that
  chat's `session_id`.
- `claude_code.answer_question`: no documented way answers a pending
  question prompt from outside the session.
- `claude_code.steer`: "arrives between tool calls" is delivery timing, not
  steering semantics; nothing mid-turn exists beyond that (§3.3 "Steer").
- `claude_code.terminal`: `claude attach` works only on `--bg` sessions,
  which is a different kind of session than the one the user is typing into;
  no PTY or tmux binding is part of this feature.
