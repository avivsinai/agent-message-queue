# Remote compatibility manifest and seam register

## 1. Purpose

This is the compatibility manifest and seam register for the `amq-remote`
companion described in [the remote-control ADR](adr-remote-control.md). It
records pinned compatibility evidence and the exact symbol, RPC method, or
CLI command each harness seam depends on. The pinned evidence is not a claim
about current support on an unverified installation. Re-run the relevant
compatibility checks after upgrading a harness or runtime; a version bump
alone does not change a capability's truth value.

Citation paths in the seam tables refer to a point-in-time research bundle
that is not included in this repository. They identify the evidence behind a
row; they are not required files for ordinary onboarding.

## 2. Pinned compatibility evidence

| Tool | Version | Source of truth |
| --- | --- | --- |
| `amq` | 0.77.3 | `amq --version` |
| `amit` (pi core) | 0.1.23 (pi 0.85.1 @ d981de1229ef) | `amit --version` |
| `codex` | codex-cli 0.154.0 | `codex --version` |
| `claude` | 2.1.278 (Claude Code) | `claude --version` |
| `tmux` | 3.7c | `tmux -V` |
| macOS | 26.5.2 | `sw_vers -productVersion` |
| `go` | go1.27.1 darwin/arm64 | `go version` |

These rows are pinned evidence, not a support matrix for whatever versions
are installed today. The design's capability table used slightly older point
releases (Codex 0.154, Amit 0.1.x/pi 0.80, Claude Code 2.1). Re-run the Amit
seam checks if the pi minor version changes materially because the cited
extension symbols can move between releases.

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
| Approvals/questions | `ExecCommandApproval`, `ApplyPatchApproval`, `FileChangeRequestApproval`, `CommandExecutionRequestApproval`, `PermissionsRequestApproval`, `ToolRequestUserInput`, `McpServerElicitationRequest` (server-initiated JSON-RPC requests) | submitted (request), answered by client | (source: research/r6-codex-app-server-events.md §2; research/r9-cc-codex-attachment.md §B.3) — fanout to a non-owning client is **unverified** |
| Session-switch/reload epoch triggers | `thread/resume` (rehydrates full history), `thread/started`/`thread/status/changed` notifications | delivered | (source: research/p1-codex-probe.md results table phases 1-2; research/r6-codex-app-server-events.md §1) |
| Local draft access (must not submit) | `unavailable` — no draft/compose concept in the schema; the only staging primitive is `thread/queue/add`, which is a real queued submission, not a draft | n/a | (source: seats/harness-inject-surfaces.md §B.3; research/r6-codex-app-server-events.md §3 — no draft-shaped method found in the 155 `ClientRequest` methods) |
| Inspect/roster | `thread/read` (`includeTurns`), `thread/items/list`, `thread/turns/list`, `codex agents` | delivered (polling), strong typed schema | (source: research/r9-cc-codex-attachment.md §B.4; seats/harness-inject-surfaces.md §B.3) |
| Observation | Connecting and calling `thread/resume`, then reading the broadcast `item/*`/`turn/*`/`thread/status/changed` stream; a second, non-resuming client already receives `thread/started` before it ever resumes | delivered | (source: research/p1-codex-probe.md results table phase 1 note: "B received `thread/started` for A's thread before B ever called `thread/resume`") |

### 3.2 Amit / pi

| Row | Mechanism | Evidence class | Citation |
| --- | --- | --- | --- |
| Submit entry | `pi.sendUserMessage(text, {deliverAs: "followUp"})` via the AMQ doorbell spool → bridge extension | delivered (queues natively; not itself a receipt) | (source: seats/harness-inject-surfaces.md §C.2; seats/amit-extension-seams.md "Bottom line for the Buzz face" item 2) |
| Acceptance evidence | None native — `sendUserMessage` returns `void` | delivered only | (source: seats/harness-inject-surfaces.md §C.2 pi API citation "types.d.ts:903-905"; review-verdict.md "Confirmed by verification" bullet) |
| Native run identity | `unavailable` in stock pi; Amit is extension-only over npm pi with no AgentSession wrapper. The remote extension owns the input boundary best-effort: the `isIdle()` precheck is **advisory only** (pi swallows `sendUserMessage` rejections into `emitError`, so a race between precheck and submit loses the request silently, never as a refusal); admission is serialized to one remote request in flight; correlation is by **exact** `message_start` user-message text (duplicate texts are indistinguishable; queues are untyped strings) plus a `getEntries()` tail-diff for the persisted entry id, which becomes visible **only at persist time (`message_end`)** | submitted (extension-owned boundary; not the design's admitted, and `Known=false` is not proof of non-admission here), completed (`turn_end`) | (source: seats/amit-attachment-feasibility.md §1, §A of the amit-pi review) |
| Completion evidence | `agent_end`/`agent_settled` extension events, or the session JSONL's `message`/`custom` entries | completed (via shim correlation only) | (source: seats/harness-surfaces.md §1.1 event table; seats/amit-extension-seams.md §A.1) |
| Exact cancellation gate | `ctx.abort()` stops the current run only and keeps queued follow-ups; `clearQueue()` is not on the extension context and pi has no dequeue-by-item primitive | completed for the current bound run; `unsupported` for a queued-not-started request | (source: seats/amit-attachment-feasibility.md §2) |
| Steer | `pi.sendUserMessage(text, {deliverAs: "steer"})`, or the dedicated `steer` RPC command | submitted | (source: seats/harness-surfaces.md §1.1; amq-remote-design.html capability table row "steer") |
| Approvals/questions | Guardrails' `ctx.ui.select(...)` await, race-able via `pi.events` (`amit:approval-decision`) once guardrails adds a listener — not wired today | submitted, requires an Amit code change | (source: seats/pi-inter-extension-bus.md §2b, "Verdict"; seats/amit-extension-seams.md §A.7 "the one real gap") |
| Session-switch/reload epoch triggers | `session_before_switch`, `session_before_fork`, `session_before_compact`, `session_shutdown`, `session_before_tree` extension events | delivered | (source: seats/amit-extension-seams.md §A.1 "Lifecycle" row) |
| Local draft access (must not submit) | `unavailable` — no draft concept found; `clear_queue` returns already-queued text (the Esc UX primitive), not an unsent draft | n/a | (source: seats/harness-surfaces.md §1.1 "abort, clear_queue... are also commands") |
| Inspect/roster | Extension snapshot (`get_state`, `get_tree`, `get_messages`, `get_session_stats`) plus session JSONL tail with `get_entries since` durable cursor | delivered | (source: seats/harness-surfaces.md §1.1 "Second client attaching"; §D table) |
| Observation | Session JSONL append-only tail (`~/.pi/agent/sessions/...jsonl`) plus `pi.events` in-process bus for extensions in the same runtime; no socket/server mode exists in stock pi RPC | delivered (file tail is lossy for streaming deltas, effort, background-task state) | (source: seats/harness-surfaces.md §1.1 "no socket/server mode"; seats/amit-extension-seams.md §D table "Conclusion") |

### 3.3 Claude Code

| Row | Mechanism | Evidence class | Citation |
| --- | --- | --- | --- |
| Submit entry | Cross-session messaging socket (`CLAUDE_CODE_MESSAGING_SOCKET`), optional `{"type":"auth","token":"..."}` first line on macOS/Linux | delivered, not admitted | (source: research/p2-cc-socket-probe.md "Socket location and binding", "Auth line"; research/r9-cc-codex-attachment.md §A.1) |
| Wire protocol (verified end-to-end, v2.1.278) | Newline-delimited JSON over the UDS: auth line `{"type":"auth","token":"<target peerToken>"}` (token from `~/.claude/sessions/<pid>.<sha256(resolve(sockPath))>.key`), then one frame `{msgV:1,msg_id:<uuid>,type:"user",message:{role:"user",content:"<cross-session-message XML envelope>"},priority:"next",from:"<sender addr>"}`. Envelope: `<cross-session-message from="…" from-session="…" from-name="…">\n<body>\n</cross-session-message>` — byte-exact round-trip required by the receiver's re-serialize-and-compare parse. 1 MiB line cap. No per-frame ack on the happy path (fire-and-forget with internal `msg_id` receipt tracking); malformed frames are dropped silently. Hand-crafted frame delivered to a live `claude --bg` target and rendered in its transcript as `› Message from @amq-probe: …` | delivered + verified (live capture) | (source: research/r0-03-cc-socket-wire-capture.md — full frame shapes, ingress guard limits, adapter implications; binary symbols `Jht`/`Pe`/`pQe`/`TG`/`Gar`/`kXr`) |
| Acceptance evidence | None documented at the field level; the doc's own language ("starts a new turn" / "reads the message between tool calls") is prose, not an RPC ack | delivered only | (source: research/r9-cc-codex-attachment.md §A.4 verdict table row "submit"; research/p2-cc-socket-probe.md "5-line conclusion" item 3) |
| Busy/queue + dedup behavior | `[live]` Frames sent while the target is mid-turn are parked and queued, rendered as `› Message from @…` banners at the next turn boundary, and processed in one batched turn; duplicate bodies within 30s are dropped with a visible in-session notice `Dropped a peer message from @…: identical to the previous message from this sender.` `[binary]` Queue cap is 50 (`maxQueuedPeerMessages`; overflow → `queue-full` drop) — cap value from the binary, overflow itself not live-exercised | delivered + verified (idle/busy/dedup live; cap binary) | (source: research/r0-03-cc-socket-wire-capture.md §5 — busy/parked/dedup probes against a `claude --bg` target) |
| Inbound policy | `crossSessionInbound` setting exists: enum `accept` (default) / `hold` / `refuse`; policy > user > repo, repo may only tighten; invalid value fails closed to `hold`; extra holds on permission-mode mismatch. Default (unset) delivers inbound messages — required user-level value: none. Listener gate: `CLAUDE_CODE_HARBOR_KITE` env → flag `tengu_harbor_kite`, default on (fail-closed on sockets-dir vetting) | delivered + verified (binary; default-accept confirmed live) | (source: research/r0-03-cc-socket-wire-capture.md §5a) |
| Native run identity | `unavailable` — no request-id or turn-id concept on this channel | n/a | (source: research/r9-cc-codex-attachment.md §A.4 "No native concept of 'the result of request X'") |
| Completion evidence | Opt-in user-level `Stop` hook reporting `prompt_id`, `transcript_path`, `last_assistant_message`; or `notify_when_idle` one-shot idle/exit notice | completed (Stop hook, requires target's own settings.json), weak/heartbeat (notify_when_idle) | (source: research/r9-cc-codex-attachment.md §A.2 "Stop correlation", §A.4 "lookup/correlate to result" row) |
| Exact cancellation gate | `unavailable` — confirmed: "there is no user-level way to interrupt a running foreground turn without keystrokes" | n/a | (source: research/r9-cc-codex-attachment.md §A.4 "cancelExact" row; review-verdict.md "Confirmed by verification" bullet) |
| Steer | `unavailable` — delivery timing ("arrives between tool calls") is not steering semantics | n/a | (source: research/r9-cc-codex-attachment.md §C "Claude Code" verdict paragraph) |
| Approvals/questions | `unavailable` outside policy-gated Remote Control (disabled on this machine: `claude remote-control --help` → "Error: Remote Control is disabled by your organization's policy") | n/a | (source: seats/harness-inject-surfaces.md §A.3; research/r9-cc-codex-attachment.md §C) |
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
  socket; only `--stdio`/`stdio://` uses plain framing. The socket was
  observed to require a path under `$HOME`; treat that as pinned evidence to
  re-check on upgrades, not as a timeless platform rule. Server notifications
  and non-owning `turn/interrupt` were observed to fan out to connected
  clients, while approval-request fanout remains unverified.
  (source: research/p1-codex-probe.md)
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
| Codex | true | true | true | false | false | false | unavailable |
| Amit/pi | true | true | true (current bound run only) | false | false | false | unavailable |
| Claude Code | true | unverified | false | false | false | false | unavailable |

The v1 endpoint masks `steer` to false at the D1 gate even where an
attachment declares it (`internal/remote/codex/attachment.go:293`,
`internal/remote/core/endpoint.go:1055-1056`). Claude Code `submit` remains
unverified because its cross-session socket has no admission receipt
(verification reference: `agent-message-queue-611.2`); a delivered submit is
not an admitted one. The void-returning
`sendUserMessage` seam is pi/Amit's, not Claude Code's.

Reasons for every `false` (source: as cited per row in §3, plus
amq-remote-design.html §Capability per harness):

- `codex.answer_question` / `codex.approve_tool`: the approval RPCs exist and
  are typed, but fanout to a non-owning client is unverified (source:
  research/r9-cc-codex-attachment.md §B.3).
- `codex.terminal`: app-server has no PTY concept in-protocol (source:
  research/r9-cc-codex-attachment.md §B.4 "terminal" row).
- `amit.cancel_request` is `true` for the current bound run only:
  `ctx.abort()` stops that run and keeps queued follow-ups; a queued
  remote request that has not started cannot be cancelled because pi has no
  dequeue primitive, and the adapter answers `unsupported` for it (source:
  seats/amit-attachment-feasibility.md §2).
- `amit.answer_question` / `amit.approve_tool`: the extension contract has no
  external decision seam for the `ctx.ui.select` await (source:
  seats/pi-inter-extension-bus.md §1; §2b).
- `amit.terminal`: no terminal binding is part of this adapter contract.
- `claude_code.cancel_request`: no user-level interrupt exists at all
  (source: research/r9-cc-codex-attachment.md §A.4 "cancelExact" row).
- `claude_code.answer_question` / `claude_code.approve_tool`: no documented
  way to answer a specific pending permission/question prompt from outside
  the session (source: research/r9-cc-codex-attachment.md §C "Claude Code").
- `claude_code.steer`: "arrives between tool calls" is delivery timing, not
  steering semantics — nothing mid-turn exists beyond that (source:
  research/r9-cc-codex-attachment.md §C).
- `claude_code.terminal`: `claude attach` only works on `--bg` sessions,
  which is a different kind of session than the one the user is typing into;
  no PTY/tmux binding is part of this feature (source:
  research/r9-cc-codex-attachment.md §A.4 "terminal" row / §C).
