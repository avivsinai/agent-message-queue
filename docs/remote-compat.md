# Remote compatibility manifest and seam register

## 1. Purpose

This is the compatibility manifest and seam register for the `amq-remote`
companion described in [the remote-control ADR](adr-remote-control.md). It
records pinned compatibility evidence and the exact symbol, RPC method, or
CLI command each harness seam depends on. The pinned evidence is not a claim
about current support on an unverified installation. Re-run the relevant
compatibility checks after upgrading a harness or runtime; a version bump
alone does not change a capability's truth value.

Citation paths in the seam tables identify the evidence behind a row. Paths under
`docs/research/` are included in this repository as point-in-time research records
(see the framing note at the top of each such document); other paths refer to a
point-in-time research bundle that is not included. They are not required files for
ordinary onboarding.

## 2. Pinned compatibility evidence

| Tool | Version | Source of truth |
| --- | --- | --- |
| `amq` | 0.77.3 | `amq --version` |
| `pi` | 0.85.1 | `pi --version` |
| `codex` | codex-cli 0.154.0 | `codex --version` |
| `claude` | 2.1.278 (Claude Code) | `claude --version` |
| `tmux` | 3.7c | `tmux -V` |
| macOS | 26.5.2 | `sw_vers -productVersion` |
| `go` | go1.27.1 darwin/arm64 | `go version` |

These rows are pinned evidence, not a support matrix for whatever versions
are installed today. The design's capability table used slightly older point
releases (Codex 0.154, pi 0.80, Claude Code 2.1). Re-run the pi seam checks
if the pi minor version changes materially because the cited extension
symbols can move between releases.

## 3. Seam register

Each cell states the exact symbol/command/RPC method, or `unavailable`, plus
its evidence class: **delivered** (bytes left this process), **submitted**
(the harness accepted the payload for later admission), **admitted** (the
harness gave back a run identity), or **completed** (the harness reported a
terminal outcome for that run).

### 3.1 Codex

| Row | Mechanism | Evidence class | Citation |
| --- | --- | --- | --- |
| Submit entry | `turn/start` (fresh turn) or `thread/queue/add` (FIFO, applies when the thread goes idle) | admitted (see acceptance evidence); submitted (queue) | (source: research/r9-cc-codex-attachment.md §B.4; research/p1-codex-probe.md "Exact request shapes used") |
| Acceptance evidence | The `userMessage` item whose `clientId` matches the submit `clientUserMessageId` — **not** the `turn/start` `result.turn.id`. `turn/start` on a busy thread silently joins the running turn and discards the caller's input, so the returned `turnId` cannot distinguish our-input-admitted from silently-joined; the `userMessage` echo survives reconnect via `thread/read`. | admitted | (source: research/p1-codex-probe.md tight-timing result "B's input never appears as an item"; the confirmation gate, internal/remote/codex/attachment.go) |
| Native run identity | `turnId` bound to `threadId` | admitted | (source: research/p1-codex-probe.md results table phase 1) |
| Completion evidence | `turn/completed` notification (`status: completed\|failed\|interrupted`) | completed | (source: research/r6-codex-app-server-events.md §1 "Turn Lifecycle"; research/p1-codex-probe.md results table) |
| Exact cancellation gate | `turn/interrupt {threadId, turnId}` | completed (drives `turn/completed(interrupted)`) | (source: seats/harness-inject-surfaces.md §B.3 "Queue/steer" list; research/p1-codex-probe.md "Follow-up (`probe_busy.py`)" row — proven from a second, non-owning connection) |
| Steer | `turn/steer` | submitted | (source: seats/harness-inject-surfaces.md §B.3 "Queue/steer" list; research/r6-codex-app-server-events.md §3) |
| Approvals/questions | `ExecCommandApproval`, `ApplyPatchApproval`, `FileChangeRequestApproval`, `CommandExecutionRequestApproval`, `PermissionsRequestApproval`, `ToolRequestUserInput`, `McpServerElicitationRequest` (server-initiated JSON-RPC requests) | submitted (request), answered by client | (source: research/r6-codex-app-server-events.md §2; research/r9-cc-codex-attachment.md §B.3) — every client on the thread receives the request, the first response wins, and `serverRequest/resolved` follows (source-read at `rust-v0.156.1`, `app-server/src/outgoing_message.rs:330-495`; not live-verified). AMQ answers only the decision approvals; a permissions, user-input or MCP elicitation request shows in the DM as "Answer in the terminal." with no one-tap answer |
| Session-switch/reload epoch triggers | `thread/resume` (rehydrates full history), `thread/started`/`thread/status/changed` notifications | delivered | (source: research/p1-codex-probe.md results table phases 1-2; research/r6-codex-app-server-events.md §1) |
| Local draft access (must not submit) | `unavailable` — no draft/compose concept in the schema; the only staging primitive is `thread/queue/add`, which is a real queued submission, not a draft | n/a | (source: seats/harness-inject-surfaces.md §B.3; research/r6-codex-app-server-events.md §3 — no draft-shaped method found in the 155 `ClientRequest` methods) |
| Inspect/roster | `thread/read` (`includeTurns`), `thread/items/list`, `thread/turns/list`, `codex agents` | delivered (polling), strong typed schema | (source: research/r9-cc-codex-attachment.md §B.4; seats/harness-inject-surfaces.md §B.3) |
| Observation | Connecting and calling `thread/resume`, then reading the broadcast `item/*`/`turn/*`/`thread/status/changed` stream; a second, non-resuming client already receives `thread/started` before it ever resumes | delivered | (source: research/p1-codex-probe.md results table phase 1 note: "B received `thread/started` for A's thread before B ever called `thread/resume`") |

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
| Approvals/questions | `ctx.ui.select(...)` is an in-process await and `pi.events` is an in-process bus between extensions. Stock pi raises no tool approval that an extension can answer from outside, so the reference bridge (revision 3) has no decision seam. Bridge revision 4 adds one for an extension that holds a tool call for an external decision: `interaction` event lines and adapter-written `answers/` files (see the pi bridge protocol, "Bridge revision 4: tool approval"). Questions have no seam | approvals: revision 4 only; questions: n/a | (source: pi `src/core/extensions/types.ts` `select`, `events`; `docs/extensions.md` "Choose an integration point") |
| Native session identity | `ctx.sessionManager.getSessionId()`: the session header id, kept when a session file is loaded again (reload, resume; compaction only appends entries) and new on `newSession` and on a branched (forked) session. A bridge of revision 4 publishes it as `session_id` in `bridge.liveness`; the adapter reports it as the native session id a relay share pins | delivered (revision 4 only) | (source: pi `src/core/extensions/types.ts` `sessionManager`; `src/core/session-manager.ts` `ReadonlySessionManager`, `getSessionId`, `newSession`) |
| Session-switch/reload epoch triggers | `session_before_switch`, `session_before_fork`, `session_before_compact`, `session_shutdown`, `session_before_tree` extension events | delivered | (source: pi `src/core/extensions/types.ts` event declarations) |
| Local draft access (must not submit) | `ctx.ui.getEditorText()` reads the input editor text; the bridge does not read it. The `clear_queue` RPC command returns already-queued text, not an unsent draft | n/a | (source: pi `src/core/extensions/types.ts` `getEditorText`; `docs/rpc-commands.md` "clear_queue") |
| Inspect/roster | RPC `get_state`, `get_tree`, `get_messages`, `get_session_stats`, and `get_entries` with a `since` entry-id cursor | delivered | (source: pi `docs/rpc-commands.md`) |
| Observation | Session JSONL append-only files under `~/.pi/agent/sessions/` plus the `pi.events` in-process bus for extensions in the same runtime; RPC mode is a child process over stdin/stdout, with no socket or server mode | delivered (a file tail is lossy for streaming deltas) | (source: pi `docs/sessions.md`, `docs/session-format.md`, `docs/rpc.md`) |

### 3.3 Claude Code

| Row | Mechanism | Evidence class | Citation |
| --- | --- | --- | --- |
| Submit entry | Cross-session messaging socket (`CLAUDE_CODE_MESSAGING_SOCKET`), optional `{"type":"auth","token":"..."}` first line on macOS/Linux | delivered, not admitted | (source: research/p2-cc-socket-probe.md "Socket location and binding", "Auth line"; research/r9-cc-codex-attachment.md §A.1) |
| Wire protocol (captured end-to-end, v2.1.278) | Newline-delimited JSON over the UDS: auth line `{"type":"auth","token":"<target peerToken>"}` (token from `~/.claude/sessions/<pid>.<sha256(resolve(sockPath))>.key`), then one frame `{msgV:1,msg_id:<uuid>,type:"user",message:{role:"user",content:"<cross-session-message XML envelope>"},priority:"next",from:"<sender addr>"}`. Envelope: `<cross-session-message from="…" from-session="…" from-name="…">\n<body>\n</cross-session-message>` — byte-exact round-trip required by the receiver's re-serialize-and-compare parse. 1 MiB line cap. No per-frame ack on the happy path (fire-and-forget with internal `msg_id` receipt tracking); malformed frames are dropped silently. Hand-crafted frame delivered to a live `claude --bg` target and rendered in its transcript as `› Message from @amq-probe: …` | delivered (live capture) | (source: research/r0-03-cc-socket-wire-capture.md — full frame shapes, ingress guard limits, adapter implications; binary symbols `Jht`/`Pe`/`pQe`/`TG`/`Gar`/`kXr`) |
| Acceptance evidence | `[live]` field-level: the target's JSONL transcript gains a `type: "queue-operation"` entry then a `type: "user"` entry carrying the exact envelope plus the harness wrapper ("Another Claude session sent a message: …") and `origin: {kind: "peer", msg_id: <frame msg_id>}`. A target that is mid-turn absorbs the frame instead as a `type: "attachment"` entry with `attachment.type: "queued_command"` and the same `attachment.origin.msg_id` (`[live]`, v2.1.278). The adapter correlates on that msg_id in both shapes. TUI banner `› Message from @amq-probe: …` confirmed in the same probe | submitted | (source: research/r0-03-cc-socket-wire-capture.md §3.2/§4; research/r9-cc-codex-attachment.md §A.4 verdict table row "submit") |
| Busy/queue + dedup behavior | `[live]` Frames sent while the target is mid-turn are parked and queued, rendered as `› Message from @…` banners at the next turn boundary, and processed in one batched turn; duplicate bodies within 30s are dropped with a visible in-session notice `Dropped a peer message from @…: identical to the previous message from this sender.` `[binary]` Queue cap is 50 (`maxQueuedPeerMessages`; overflow → `queue-full` drop) — cap value from the binary, overflow itself not live-exercised | delivered (idle/busy/dedup live; cap binary) | (source: research/r0-03-cc-socket-wire-capture.md §5 — busy/parked/dedup probes against a `claude --bg` target) |
| Inbound policy | `crossSessionInbound` setting: enum `accept` / `hold` / `refuse`; policy > user > repo, repo may only tighten; invalid value fails closed to `hold`; an explicit value always wins. `[binary]` Unset (default) is mode-parity gating, not unconditional delivery: auto-delivers only when the sender's permission-mode class matches the target's; a mismatched sender is held; a sender asserting no class is held while the target bypasses prompts. Since AMQ envelopes omit `from-mode`, the required user-level value for unattended sends is **`accept`**. Listener gate: `CLAUDE_CODE_HARBOR_KITE` env → flag `tengu_harbor_kite`, default on (fail-closed on sockets-dir vetting) | delivered (binary: schema + hold taxonomy; parity default not live-exercised) | (source: research/r0-03-cc-socket-wire-capture.md §5a — full schema string quoted) |
| Native run identity | `unavailable` — no request-id or turn-id concept on this channel | n/a | (source: research/r9-cc-codex-attachment.md §A.4 "No native concept of 'the result of request X'") |
| Completion evidence | Opt-in user-level `Stop` hook reporting `prompt_id`, `transcript_path`, `last_assistant_message`; or `notify_when_idle` one-shot idle/exit notice | completed (Stop hook, requires target's own settings.json), weak/heartbeat (notify_when_idle) | (source: research/r9-cc-codex-attachment.md §A.2 "Stop correlation", §A.4 "lookup/correlate to result" row) |
| Exact cancellation gate | `unavailable` — confirmed: "there is no user-level way to interrupt a running foreground turn without keystrokes" | n/a | (source: research/r9-cc-codex-attachment.md §A.4 "cancelExact" row; review-verdict.md "Confirmed by verification" bullet) |
| Steer | `unavailable` — delivery timing ("arrives between tool calls") is not steering semantics | n/a | (source: research/r9-cc-codex-attachment.md §C "Claude Code" verdict paragraph) |
| Approvals | Opt-in `PermissionRequest` command hook (`amq-remote claude install-approval-hook`). Its stdin carries `session_id`, `prompt_id`, `tool_name` and `tool_input`, but no `tool_use_id`. AMQ's hook prints `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"…"}}}` for the owner's ❌, and `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}` only when its command line pin (owner, relay, body, DM channel, target) verifies the owner's signed ✅ on that share's approval message that shows the whole call, and its own read of the pinned relay finds no edit (by the body or the owner) that changed the call and no deletion (kind 5 or 9005) (the answer file alone is not owner authorization). No output leaves the terminal dialog to decide. The allow path is not live-verified. `[live]` 2.1.283: the hook's `prompt_id` equals the `promptId` of the peer delivery line; a terminal reject sends SIGTERM to the hook, then SIGKILL about 1 s later; a terminal approve sends no signal, and the call's `tool_result` line (with `promptId` and `tool_use_id`) is the only trace; parallel decision hooks race and the first decision wins. Questions stay `unavailable`. | delivered (live probe; DM path not live-verified) | (source: https://code.claude.com/docs/en/hooks "PermissionRequest") |
| Session-switch/reload epoch triggers | `unavailable` documented; `queue-operation` transcript entries mark queued-while-busy messages | delivered (observed in transcript only) | (source: seats/harness-surfaces.md §2.1 "queue-operation... queued user messages while busy") |
| Local draft access (must not submit) | `unavailable` — no draft/compose primitive found on the messaging socket or CLI surface | n/a | (source: research/p2-cc-socket-probe.md "What the official docs establish"; searched, no draft method found) |
| Inspect/roster | `claude agents --json` (`status`, `waitingFor`, `state`, `kind`, `pid`) | delivered, session-level polling only | (source: seats/harness-inject-surfaces.md §A.1 `claude agents --help`; research/r9-cc-codex-attachment.md §A.3) |
| Observation | Transcript JSONL tail (`~/.claude/projects/<slug>/<uuid>.jsonl`) plus `~/.claude/sessions/<pid>.json` process metadata (`messagingSocketPath`, `peerProtocol`, `peerFeatures`) | delivered | (source: seats/harness-surfaces.md §2.1, §2.2) |

## 4. Measured adapter constraints

These observations constrain the adapters and remain part of the compatibility
contract:

- **Codex busy admission:** `turn/start` on an active thread returns the
  existing turn and silently discards the new caller input. The adapter must
  check status inside its admission boundary or use `thread/queue/add` when
  queueing is explicitly requested. A returned `turnId` alone is not proof
  that the caller's input was admitted; the matching `userMessage` item is.
  (source: research/p1-codex-probe.md; internal/remote/codex/attachment.go)
- **Codex transport and fanout:** the Unix socket is a WebSocket control
  socket; only `--stdio`/`stdio://` uses plain framing. In codex-cli 0.160,
  `codex app-server daemon start` makes the default control socket
  `~/.codex/app-server-control/app-server-control.sock` a symlink to the
  daemon's socket at `/private/tmp/codex-daemon-<uid>/<hash>`. Discovery
  follows that symlink only to a socket the current user owns and keeps the
  symlink path in the attach config. Treat this layout as pinned evidence to
  re-check on upgrades, not as a timeless platform rule. Server notifications
  and non-owning `turn/interrupt` were observed to fan out to connected
  clients. Approval-request fanout is source-read at `rust-v0.156.1`, not
  observed live (§5).
  (source: research/p1-codex-probe.md; live `ls -la ~/.codex/app-server-control`
  with codex-cli 0.160.0; internal/remote/codex/discover.go)
- **Codex approvals reviewer:** in codex-cli 0.160, a thread's
  `approvalsReviewer` (`user`, `auto_review`, or the legacy
  `guardian_subagent`) comes from `approvals_reviewer` in
  `~/.codex/config.toml` when the thread starts. An automatic reviewer
  answers approval requests itself, so few or none reach a remote client.
  `thread/resume` returns the thread's reviewer and
  `thread/settings/updated` reports a change; the adapter publishes an
  automatic one as the session's `approval_reviewer`, and doctor reports it
  for a target with `approve`. A `-c approvals_reviewer=user` on a resumed
  TUI did not change the reviewer of turns submitted remotely; a thread
  started with it did. An `approvalsReviewer` override on `turn/start`
  applies to that turn and every later one, the terminal's included, so the
  adapter never sends one.
  (source: live test 2026-10-06 with codex-cli 0.160.0; `codex app-server
  generate-json-schema` of codex-cli 0.160.1; internal/remote/codex/attachment.go)
- **Claude Code delivery:** a message delivered to a busy session is read at
  a tool boundary and does not interrupt a running tool; an idle session
  starts a new turn. No documented user-level interrupt exists over the
  messaging socket, so delivery is not steering or exact cancellation.
  (source: research/r9-cc-codex-attachment.md §A.1, §A.4)

## 5. Capability projection per harness (schema `amq.remote.session/1`)

Fields: `inspect`, `submit`, `cancel_request`, `answer_question`,
`approve_tool`, `steer`, `terminal`. Matches the design's capability table
(source: amq-remote-design.html §Capability per harness); each non-true value
has a reason in the seam rows below. The runtime schema uses Boolean negotiated
capabilities, with `terminal` as its fixed v1 `unavailable` enum. `unverified`
is a documentary evidence label and is not a schema value; `unavailable` is a
valid value for the runtime `terminal` enum.

| Harness | inspect | submit | cancel_request | answer_question | approve_tool | steer | terminal |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Codex | true | true | true | false | opt-in | false | unavailable |
| pi | true | true | false | false | bridge revision 4 | false | unavailable |
| Claude Code | true | submitted | false | false | opt-in, reject only | false | unavailable |

The v1 endpoint masks `steer` to false at the D1 gate even where an
attachment declares it (`internal/remote/codex/attachment.go:293`,
`internal/remote/core/endpoint.go:1055-1056`). Claude Code `submit` is graded **submitted**: after a socket send, the target's transcript gains the `type: "user"`
JSONL entry carrying the exact envelope (preceded by a `queue-operation` entry) —
the harness accepted the payload, but the channel has no admission receipt and no
run identity, so a submit never reaches `admitted`. The void-returning
`sendUserMessage` seam is pi's, not Claude Code's.

Reasons for every `false` (source: as cited per row in §3, plus
amq-remote-design.html §Capability per harness):

- `codex.approve_tool` is opt-in per target (`"approve": true`). At
  `rust-v0.156.1` the app-server sends each approval request to every client
  on the thread, applies the first response, drops later ones with only a
  log line, and then sends `serverRequest/resolved` to every client (source:
  `codex-rs/app-server/src/outgoing_message.rs:330-495`,
  `thread_lifecycle.rs:864-885`). The adapter answers only approvals of runs
  it submitted and clears an approval on `serverRequest/resolved`, so a late
  answer is refused. A remote approve is offered only for a plain command
  that the prompt shows whole; file changes and network, permission or
  write-root grants take a remote reject only. Not live-verified against a
  running Codex terminal.
- `codex.answer_question`: user-input requests are not projected.
- `codex.terminal`: app-server has no PTY concept in-protocol (source:
  research/r9-cc-codex-attachment.md §B.4 "terminal" row).
- `pi.cancel_request`: the bridge has no cancel request. `ctx.abort()`
  aborts the current operation, not one request, and the extension context
  has no dequeue primitive for a queued request that has not started
  (source: §3.2 "Exact cancellation gate").
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
- `claude_code.cancel_request`: no user-level interrupt exists at all
  (source: research/r9-cc-codex-attachment.md §A.4 "cancelExact" row).
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
  `native_session_id=` when the bridge publishes one.
- `claude_code.answer_question`: no documented way to answer a pending
  question prompt from outside the session (source:
  research/r9-cc-codex-attachment.md §C "Claude Code").
- `claude_code.steer`: "arrives between tool calls" is delivery timing, not
  steering semantics — nothing mid-turn exists beyond that (source:
  research/r9-cc-codex-attachment.md §C).
- `claude_code.terminal`: `claude attach` only works on `--bg` sessions,
  which is a different kind of session than the one the user is typing into;
  no PTY/tmux binding is part of this feature (source:
  research/r9-cc-codex-attachment.md §A.4 "terminal" row / §C).
