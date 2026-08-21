# ADR: Wake is a capability vector

## Status

Accepted.

## Date

2026-08-20

## Context

AMQ wake today is TTY inject. GUI seats (Hermes Desktop, Claude Desktop/Code,
ChatGPT.app) are real agents. Pretending they are a pty, or silently substituting
notify/prefill for inject, lies about delivery.

TTY inject remains defined by [wake operations](wake-operations.md). This ADR
does not change that path.

## Decision

Each seat advertises a vector:

| Field | Values |
| --- | --- |
| `activation` | `none` \| `launch` \| `foreground` |
| `delivery` | `none` \| `prefilled` \| `submitted` |
| `session` | `none` \| `new` \| `existing-exact` |
| `requires_human` | bool |
| `evidence` | `notifier_live` vs stronger (never `drained`) |

Callers request a minimum. Weaker capability is refused, not substituted. No
inject→notify, submit→prefill, or `existing-exact`→`new` downgrade.

App adapters pin bundle ID, Team ID, resolved executable, adapter version,
session id, process generation, and endpoint identity. Mismatch fails closed.
Unknown apps fail closed. Prompt text must not drive generic `osascript`.

App repair/restart is `operator_only` until identity and generation checks
match the TTY contract. `notifier_live` is not consumption.

## v1 seats (docs + local inspect, 2026-08-20)

| Seat | Vector AMQ may claim | Ship v1 adapter? |
| --- | --- | --- |
| TTY inject | current fail-closed inject | yes (existing) |
| Hermes Desktop | `submitted` + `existing-exact` only after live attach to the Desktop-owned gateway | **no** until spike |
| Claude Desktop Code | `prefilled` + `new` + `requires_human` (`claude://code/new`). Does not send. No existing-Code inject | optional human doorbell only |
| ChatGPT.app | none | **kill** |
| Grok Bot.app | none | **not a wake seat** (Mac operator UI for host G) |

Hermes HTTP `/v1/runs` and `hermes acp` stdio are other processes, not the GUI
seat. `claude-cli://` is the terminal handler, not GUI wake. This Mac's
ChatGPT.app is `com.openai.codex` / `codex://`; Computer Use cannot automate
ChatGPT or terminals.

Identity pins: Hermes `com.nousresearch.hermes` (not `.setup`); Claude Desktop
`com.anthropic.claudefordesktop` (Team `Q6L2SF6YDW`); ChatGPT
`com.openai.codex` (Team `2DC432GLL2`). Ad-hoc/unsigned Hermes Team IDs fail
closed.

### Local inspect (this Mac, 2026-08-20)

| App | Bundle / Team | Version | URL schemes | Notes |
| --- | --- | --- | --- | --- |
| Claude.app | `com.anthropic.claudefordesktop` / `Q6L2SF6YDW` | 1.32352.1 | `claude`, `msauth.com.anthropic.claudefordesktop` | Electron helpers share `com.anthropic.claudefordesktop.helper`. Nested `Claude iOS Sim.app` is `com.anthropic.claude.ios-sim`, not the Code seat. Separate `claude` CLI on PATH is TTY, not this GUI. |
| ChatGPT.app | `com.openai.codex` / `2DC432GLL2` | 26.814.41407 | `codex`, `http`, `https` | No `chatgpt://`. Nested Codex framework is `com.openai.codex.framework`. Kill for inject. |
| Hermes.app | — | — | — | Not in `/Applications`, not in `~/Applications`, not on PATH. Live Desktop gateway attach remains unproven. |
| Grok Bot.app | `com.anysphere.sand` / `DCNK4UB866` | 0.20.0 (CFBundleVersion 0.20.0) | `grokbot`, `sand` | Electron Mac client (Anysphere lineage). Not host G and not a GUI-inject seat. |

Do not write GUI wake adapters from this table. Hermes stays refused until a live Desktop-owned gateway attach. Claude stays optional human prefill. ChatGPT stays kill.

## Consequences

- `amq-64q.2` local inspect is complete. Hermes Desktop is not installed here,
  so Hermes stays refused until a live Desktop-owned gateway attach.
- `amq-64q.3` ships no GUI v1 adapters. TTY inject is unchanged. ChatGPT stays
  out. Claude prefill is optional and not shipped. GUI wake does not inherit
  TTY repair.
