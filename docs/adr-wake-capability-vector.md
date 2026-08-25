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
| Claude Desktop Code | `prefilled` + `new` + `requires_human` (`claude://code/new`). Does not send. No existing-Code inject. **Shipped** behind the registration capability gate (`internal/keepalive/adapter/claudedesktop.go`): registered in `DefaultRegistry`, reachable only when the caller explicitly accepts a requires-human seat (`--accept-requires-human`) with delivery/session minima at or below prefilled/new; refused under the default zero-value minimum. Identity pins the `claude://` scheme owner (bundle `com.anthropic.claudefordesktop`) and revalidates before the `open` write. | yes (gated) |
| ChatGPT.app (Codex app) | `execute javascript` is DEAD (issue #640: `-1723`, `AllowJavaScriptAppleEvents` pref compiled out) — kill for inject. But the `codex://` deep-link is a live prefill seat: `codex://threads/new?prompt=` prefills a NEW thread; `codex://threads/<uuid>?prompt=` opens the EXACT EXISTING conversation and prefills it. Neither auto-submits. **Shipped** behind the registration capability gate (`internal/keepalive/adapter/codexapp.go`): registered in `DefaultRegistry`, two targets (`codex-app:new` → SessionNew, `codex-app:thread:<uuid>` → SessionExistingExact) declared via `TargetCapabilityDeclarer`; reachable only when the caller explicitly accepts a requires-human seat with delivery/session minima at or below prefilled/new-or-existing-exact; refused under the default zero-value minimum. Identity pins the `codex://` scheme owner (bundle `com.openai.codex`) and revalidates before the `open` write. Dispatch caveat: `open` exiting 0 proves DISPATCH only, not delivery — the app can refuse a deep-link with no adapter-visible signal (e.g. a thread with an ACTIVE WRITER shows an error toast and leaves the composer empty), so the thread target must name an IDLE conversation; the app is AX-opaque so the adapter cannot observe the refusal. | yes (gated, deep-link only) |
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

Do not write GUI wake adapters from this table beyond the implemented Claude Desktop prefill seat. Hermes stays refused until a live Desktop-owned gateway attach; the Claude Desktop prefill seat is implemented in `internal/keepalive/adapter/claudedesktop.go` behind the registration capability gate; ChatGPT stays kill.

## Implementation note

The Go `Capability` type (`internal/keepalive/adapter/capability.go`) covers
`activation`, `delivery`, `session`, and `requires_human` — the four axes a
seat advertises and a caller's minimum is checked against via `Satisfies`
(refusal over substitution). The `evidence` dimension from the vector table
above is deliberately **not** a Go axis: it is a prose-level contract whose
semantics (`notifier_live` vs stronger, never `drained`) live elsewhere in AMQ
(receipts, wake-lock inspection). Omitting it from the type is intentional, not
an oversight — a speculative axis would invite half-implemented strength
claims. Adapters declare a `Capability` via the `CapabilityDeclarer` interface;
an adapter that does not implement it is treated as `UnknownCapability()` —
weakest on every ordered axis and most-restrictive on the tolerance axis
(`requires_human` true) — so it is refused unless the caller explicitly
tolerates a human-required seat, never masquerading as an unattended
full-strength seat. The capability gate runs at
registration (`internal/keepalive/app/app.go`), after target resolution
(discovery when no explicit target was given, then normalization) and before
any write — a refusal mutates no registry state and performs no injection.
(Discovery may run a read-only identity probe when no explicit target was
given, since the adapter must prove its identity before naming a seat; this
probe is not a write.)

### Identity-pin exception for the deep-link prefill seats

This launch-type prefill seat applies to both the Claude Desktop seat
(`claude://`, bundle `com.anthropic.claudefordesktop`) and the Codex app seat
(`codex://`, bundle `com.openai.codex`). Each pins the `codex://`/`claude://`
scheme-owner bundle id only, and revalidates it before the `open` write. The
ADR's full identity pin set (Team ID, resolved executable, adapter version,
session id, process generation, endpoint identity) is deliberately deferred
for these seats: they have no persistent process to pin a generation on (a
deep-link launch is stateless), and the remaining pins are same-machine-
attacker hardening deferred under the personal single-user tool policy. The
Team-ID codesign pin in particular is tracked as a named backlog follow-up,
not implemented in v1. This exception is recorded here so a reader knows the
narrower pin is intentional, not an omission. (Note: the Codex app also
registers `http`/`https`; only the `codex` scheme is pinned.)

## Consequences

- `amq-64q.2` local inspect is complete. Hermes Desktop is not installed here,
  so Hermes stays refused until a live Desktop-owned gateway attach.
- The Claude Desktop prefill seat is shipped behind the registration
  capability gate (`internal/keepalive/adapter/claudedesktop.go`): registered
  in `DefaultRegistry`, reachable only when the caller explicitly accepts a
  requires-human seat with delivery/session minima at or below prefilled/new,
  and refused under the default zero-value minimum. TTY inject is unchanged.
  ChatGPT stays out. GUI wake does not inherit TTY repair.
