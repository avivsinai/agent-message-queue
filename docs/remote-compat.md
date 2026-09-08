# Remote compatibility manifest and seam register

## 1. Purpose

This is the compatibility manifest and seam register for the `amq-remote`
companion described in [the remote-control ADR](adr-remote-control.md). It
pins the exact tool versions the design was verified against and the exact
symbol, RPC method, or CLI command each harness seam depends on, so a version
bump or a harness upgrade has a single place to re-check. Every row cites the
verification report it came from. The reports (`seats/*.md`, `research/*.md`,
`review-verdict.md`, `amq-remote-design.html`) are the 2026-09-08 review
bundle kept outside this repository; a re-verification replaces the citation
with the new report's name.

**How to update**: re-verify every row in §2 whenever a pinned tool version
changes (a `brew upgrade`/release of Codex, Claude Code, Amit/pi, tmux,
macOS, or Go), and re-run the affected probe in `research/` before touching
the capability projection in §5. A version bump alone does not change a
capability's truth value; only a re-run probe does.

## 2. Pinned versions

| Tool | Version | Source of truth |
| --- | --- | --- |
| `amq` | 0.77.3 | `amq --version` |
| `amit` (pi core) | 0.1.23 (pi 0.85.1 @ d981de1229ef) | `amit --version` |
| `codex` | codex-cli 0.153.4 | `codex --version` |
| `claude` | 2.1.263 (Claude Code) | `claude --version` |
| `tmux` | 3.7c | `tmux -V` |
| macOS | 26.5.2 | `sw_vers -productVersion` |
| `go` | go1.27.1 darwin/arm64 | `go version` |

The design's own capability table pins slightly older point releases (Codex
0.153, Amit 0.1.x/pi 0.80, Claude Code 2.1) (source: amq-remote-design.html
§Capability per harness). The seams below were verified against the amit-pi
and probe reports' checkout versions, not necessarily this machine's current
`amit --version`/pi 0.85.1 — re-run the Amit seam checks (§3.2) if the pi
minor version changes materially, since `pi-inter-extension-bus.md` and
`amit-extension-seams.md` cite exact `types.ts` line numbers that shift
across pi releases.

## 3. Seam register

Each cell states the exact symbol/command/RPC method, or `unavailable`, plus
its evidence class: **delivered** (bytes left this process), **submitted**
(the harness accepted the payload for later admission), **admitted** (the
harness gave back a run identity), or **completed** (the harness reported a
terminal outcome for that run).

### 3.1 Codex

| Row | Mechanism | Evidence class | Citation |
| --- | --- | --- | --- |
| Submit entry | `turn/start` (fresh turn) or `thread/queue/add` (FIFO, applies when the thread goes idle) | admitted (`turn/start` returns `result.turn.id`); submitted (queue) | (source: research/r9-cc-codex-attachment.md §B.4; research/p1-codex-probe.md "Exact request shapes used") |
| Acceptance evidence | `turn/start` response `result.turn.id` (a `turnId`) | admitted | (source: research/p1-codex-probe.md "Exact request shapes used"; research/r9-cc-codex-attachment.md §B.4) |
| Native run identity | `turnId` bound to `threadId` | admitted | (source: research/p1-codex-probe.md results table phase 1) |
| Completion evidence | `turn/completed` notification (`status: completed\|failed\|interrupted`) | completed | (source: research/r6-codex-app-server-events.md §1 "Turn Lifecycle"; research/p1-codex-probe.md results table) |
| Exact cancellation gate | `turn/interrupt {threadId, turnId}` | completed (drives `turn/completed(interrupted)`) | (source: seats/harness-inject-surfaces.md §B.3 "Queue/steer" list; research/p1-codex-probe.md "Follow-up (`probe_busy.py`)" row — proven from a second, non-owning connection) |
| Steer | `turn/steer` | submitted | (source: seats/harness-inject-surfaces.md §B.3 "Queue/steer" list; research/r6-codex-app-server-events.md §3) |
| Approvals/questions | `ExecCommandApproval`, `ApplyPatchApproval`, `FileChangeRequestApproval`, `CommandExecutionRequestApproval`, `PermissionsRequestApproval`, `ToolRequestUserInput`, `McpServerElicitationRequest` (server-initiated JSON-RPC requests) | submitted (request), answered by client | (source: research/r6-codex-app-server-events.md §2; research/r9-cc-codex-attachment.md §B.3) — fanout to a non-owning client is **unverified**: the quota-blocked probe never triggered a real approval request (source: research/p1-codex-probe.md "Confound: account is quota-blocked") |
| Session-switch/reload epoch triggers | `thread/resume` (rehydrates full history), `thread/started`/`thread/status/changed` notifications | delivered | (source: research/p1-codex-probe.md results table phases 1-2; research/r6-codex-app-server-events.md §1) |
| Local draft access (must not submit) | `unavailable` — no draft/compose concept in the schema; the only staging primitive is `thread/queue/add`, which is a real queued submission, not a draft | n/a | (source: seats/harness-inject-surfaces.md §B.3; research/r6-codex-app-server-events.md §3 — no draft-shaped method found in the 155 `ClientRequest` methods) |
| Inspect/roster | `thread/read` (`includeTurns`), `thread/items/list`, `thread/turns/list`, `codex agents` | delivered (polling), strong typed schema | (source: research/r9-cc-codex-attachment.md §B.4; seats/harness-inject-surfaces.md §B.3) |
| Observation | Connecting and calling `thread/resume`, then reading the broadcast `item/*`/`turn/*`/`thread/status/changed` stream; a second, non-resuming client already receives `thread/started` before it ever resumes | delivered | (source: research/p1-codex-probe.md results table phase 1 note: "B received `thread/started` for A's thread before B ever called `thread/resume`") |

### 3.2 Amit / pi

| Row | Mechanism | Evidence class | Citation |
| --- | --- | --- | --- |
| Submit entry | `pi.sendUserMessage(text, {deliverAs: "followUp"})` via the AMQ doorbell spool → bridge extension | delivered (queues natively; not itself a receipt) | (source: seats/harness-inject-surfaces.md §C.2; seats/amit-extension-seams.md "Bottom line for the Buzz face" item 2) |
| Acceptance evidence | None native — `sendUserMessage` returns `void` | delivered only | (source: seats/harness-inject-surfaces.md §C.2 pi API citation "types.d.ts:903-905"; review-verdict.md "Confirmed by verification" bullet) |
| Native run identity | `unavailable` in stock pi; Amit is extension-only over npm pi with no AgentSession wrapper, so the remote extension itself owns the input boundary: it serializes remote admission (one in flight), treats the synchronous `input` event (`source: "extension"`) as admission, takes the user `AgentMessage` from `message_start` as the run token, and `turn_end`/`agent_end` as completion | admitted (extension-owned boundary), completed (`turn_end`) | (source: seats/amit-attachment-feasibility.md §1, §4) |
| Completion evidence | `agent_end`/`agent_settled` extension events, or the session JSONL's `message`/`custom` entries | completed (via shim correlation only) | (source: seats/harness-surfaces.md §1.1 event table; seats/amit-extension-seams.md §A.1) |
| Exact cancellation gate | `ctx.abort()` | completed, but only exact when a shim first confirms the bound run is still current; else `unsupported` | (source: seats/amit-extension-seams.md §C "Abort / interrupt"; amq-remote-design.html Amit adapter card "Cancel: exact only when the shim confirms the bound run is the current one") |
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
| Acceptance evidence | None documented at the field level; the doc's own language ("starts a new turn" / "reads the message between tool calls") is prose, not an RPC ack | delivered only | (source: research/r9-cc-codex-attachment.md §A.4 verdict table row "submit"; research/p2-cc-socket-probe.md "5-line conclusion" item 3) |
| Native run identity | `unavailable` — no request-id or turn-id concept on this channel | n/a | (source: research/r9-cc-codex-attachment.md §A.4 "No native concept of 'the result of request X'") |
| Completion evidence | Opt-in user-level `Stop` hook reporting `prompt_id`, `transcript_path`, `last_assistant_message`; or `notify_when_idle` one-shot idle/exit notice | completed (Stop hook, requires target's own settings.json), weak/heartbeat (notify_when_idle) | (source: research/r9-cc-codex-attachment.md §A.2 "Stop correlation", §A.4 "lookup/correlate to result" row) |
| Exact cancellation gate | `unavailable` — confirmed: "there is no user-level way to interrupt a running foreground turn without keystrokes" | n/a | (source: research/r9-cc-codex-attachment.md §A.4 "cancelExact" row; review-verdict.md "Confirmed by verification" bullet) |
| Steer | `unavailable` — delivery timing ("arrives between tool calls") is not steering semantics | n/a | (source: research/r9-cc-codex-attachment.md §C "Claude Code" verdict paragraph) |
| Approvals/questions | `unavailable` outside policy-gated Remote Control (disabled on this machine: `claude remote-control --help` → "Error: Remote Control is disabled by your organization's policy") | n/a | (source: seats/harness-inject-surfaces.md §A.3; research/r9-cc-codex-attachment.md §C) |
| Session-switch/reload epoch triggers | `unavailable` documented; `queue-operation` transcript entries mark queued-while-busy messages | delivered (observed in transcript only) | (source: seats/harness-surfaces.md §2.1 "queue-operation... queued user messages while busy") |
| Local draft access (must not submit) | `unavailable` — no draft/compose primitive found on the messaging socket or CLI surface | n/a | (source: research/p2-cc-socket-probe.md "What the official docs establish"; searched, no draft method found) |
| Inspect/roster | `claude agents --json` (`status`, `waitingFor`, `state`, `kind`, `pid`) | delivered, session-level polling only | (source: seats/harness-inject-surfaces.md §A.1 `claude agents --help`; research/r9-cc-codex-attachment.md §A.3) |
| Observation | Transcript JSONL tail (`~/.claude/projects/<slug>/<uuid>.jsonl`) plus `~/.claude/sessions/<pid>.json` process metadata (`messagingSocketPath`, `peerProtocol`, `peerFeatures`) | delivered | (source: seats/harness-surfaces.md §2.1, §2.2) |

## 4. Measured behaviors that constrain the adapters

- **Codex busy semantics (measured, not assumed)**: `turn/start` on a thread
  with an active turn does not error and does not queue a second turn — it
  returns the existing in-progress `Turn` object, and the second caller's own
  input text is silently discarded, never appearing as a thread item. An
  adapter must check thread status inside its admission boundary before
  calling `turn/start` blind, or use `thread/queue/add` when the caller opts
  into queueing. (source: research/p1-codex-probe.md "Conclusion" item 3)
- **Codex fanout (measured)**: every `ServerNotification` observed
  (`thread/started`, `thread/status/changed`, `turn/started`, `item/started`,
  `item/completed`, `account/rateLimits/updated`, `error`, `turn/completed`)
  reached both connected clients in real time, unconditionally — the
  app-server broadcasts to every attached control-socket client with no
  per-connection subscription gate. `turn/interrupt` from a non-owning client
  also succeeded and both clients received the resulting
  `turn/completed(interrupted)`. Approval-request fanout specifically was
  **not** exercised (the test account is quota-blocked, so no shell command
  ever ran and no `item/commandExecution/requestApproval` was ever sent).
  (source: research/p1-codex-probe.md "Conclusion" items 1, 2, 4)
- **Codex framing (measured)**: the unix-socket transport is a WebSocket
  control socket (HTTP/1.1 `Upgrade: websocket` handshake, RFC 6455), not raw
  newline-delimited JSON-RPC; only `--stdio`/`stdio://` is plain framing. The
  socket must bind under `$HOME` — binding under `/tmp` or `/private/tmp`
  failed even with the sandbox disabled. (source: research/p1-codex-probe.md
  "Setup facts discovered mid-probe" items 1-2)
- **Claude Code queue-at-tool-boundary (documented, not independently
  captured on the wire)**: a message delivered while the target session is
  busy is "read... between tool calls during an active turn, so a running
  tool is never interrupted." (source: research/r9-cc-codex-attachment.md
  §A.1, quoting code.claude.com/docs/en/cross-session-messaging)
- **Claude Code idle starts a turn (documented)**: "When the receiving
  session is idle, Claude Code starts a new turn with the message."
  (source: research/r9-cc-codex-attachment.md §A.1, same doc)
- **Claude Code has no interrupt (documented + corroborated)**: no
  documented interrupt/cancel RPC exists over the messaging socket, no hook
  aborts a running turn — "there is no user-level way to interrupt a running
  foreground turn without keystrokes." (source: research/r9-cc-codex-attachment.md
  §A.4 "cancelExact" row; review-verdict.md "Confirmed by verification")

## 5. Capability projection per harness (schema `amq.remote.session/1`)

Fields: `inspect`, `submit`, `cancel_request`, `answer_question`,
`approve_tool`, `steer`, `terminal`. Matches the design's capability table
(source: amq-remote-design.html §Capability per harness); every `false` below
carries a one-line reason instead of a bare boolean.

```json
{
  "schema": "amq.remote.session/1",
  "codex": {
    "inspect": true,
    "submit": true,
    "cancel_request": true,
    "answer_question": false,
    "approve_tool": false,
    "steer": true,
    "terminal": false
  },
  "amit": {
    "inspect": true,
    "submit": true,
    "cancel_request": false,
    "answer_question": false,
    "approve_tool": false,
    "steer": true,
    "terminal": false
  },
  "claude_code": {
    "inspect": true,
    "submit": true,
    "cancel_request": false,
    "answer_question": false,
    "approve_tool": false,
    "steer": false,
    "terminal": false
  }
}
```

Reasons for every `false` (source: as cited per row in §3, plus
amq-remote-design.html §Capability per harness and §c-caps callout):

- `codex.answer_question` / `codex.approve_tool`: the approval RPCs exist and
  are typed, but which connected client receives them when more than one is
  attached is unverified — the live probe's account is quota-blocked, so no
  approval request was ever emitted (source: research/p1-codex-probe.md
  "Confound: account is quota-blocked"; research/r9-cc-codex-attachment.md
  §B.3).
- `codex.terminal`: app-server has no PTY concept in-protocol (source:
  research/r9-cc-codex-attachment.md §B.4 "terminal" row).
- `amit.cancel_request`: `ctx.abort()` exists but is only an exact cancel
  when a shim first confirms the bound run is still current; that shim does
  not exist yet, so this ships `false` until it does (source:
  seats/amit-extension-seams.md §C; amq-remote-design.html Amit adapter
  card).
- `amit.answer_question` / `amit.approve_tool`: guardrails does not listen
  on `pi.events` today — wiring a remote decision into its
  `ctx.ui.select` await requires an Amit code change that has not landed
  (source: seats/pi-inter-extension-bus.md §1 "the gap is not the bus — it's
  that guardrails does not currently listen on it"; §2b).
- `amit.terminal`: no tmux server runs on this machine and no harness runs
  under tmux (source: review-verdict.md A5).
- `claude_code.cancel_request`: no user-level interrupt exists at all
  (source: research/r9-cc-codex-attachment.md §A.4 "cancelExact" row).
- `claude_code.answer_question` / `claude_code.approve_tool`: no documented
  way to answer a specific pending permission/question prompt from outside
  the session; approvals are local-terminal-only outside the policy-disabled
  Remote Control (source: research/r9-cc-codex-attachment.md §C "Claude
  Code").
- `claude_code.steer`: "arrives between tool calls" is delivery timing, not
  steering semantics — nothing mid-turn exists beyond that (source:
  research/r9-cc-codex-attachment.md §C).
- `claude_code.terminal`: `claude attach` only works on `--bg` sessions,
  which is a different kind of session than the one the user is typing into;
  no PTY/tmux binding is part of this feature (source:
  research/r9-cc-codex-attachment.md §A.4 "terminal" row / §C).

## 6. Open verifications

1. **Claude Code cross-session socket wire capture** — blocked. The probe
   session's own permission classifier denied inspecting the installed CLI
   binary and spawning a `claude --bg` target session, so no raw
   request/response bytes were captured; this needs a plain terminal outside
   that classifier. (source: research/p2-cc-socket-probe.md "Status: BLOCKED
   mid-task"; tracked as bead agent-message-queue-611.2, per
   amq-remote-design.html R0-03 row)
2. **Codex approval fanout** — quota-blocked. The test account's spend cap
   prevented the model from ever running a tool call, so no
   `item/commandExecution/requestApproval` was ever emitted in either probe
   pass; whether a non-owning connection receives (or can answer) another
   connection's approval request is unresolved. (source:
   research/p1-codex-probe.md "Confound: account is quota-blocked")
3. **Amit owning input boundary** — decided in principle, not built. Amit has
   no `AgentSession` wrapper, so the remote extension owns the boundary
   itself (serialized admission, `input` event as admission, `message_start`
   user message as run token). Whether `clearQueue()` can remove one queued
   remote request without dropping the human's follow-ups is the open
   question for exact cancel of a not-yet-started request; tracked under
   bead amit-m5pe. (source: seats/amit-attachment-feasibility.md §1, §2, §4)
