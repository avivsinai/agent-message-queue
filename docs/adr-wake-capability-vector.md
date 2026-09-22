# ADR: Wake is a capability vector

## Status

Accepted.

## Context

Wake targets are not interchangeable. A TTY injector, a GUI deep link, and a
native provider queue expose different activation, delivery, session, and
evidence guarantees. Treating a GUI seat as a pty, or silently substituting
notification or prefill for injection, lies about delivery.

TTY injection remains defined by [wake operations](wake-operations.md). Raw
TIOCSTI proves only that AMQ wrote terminal bytes; it does not prove provider
presentation, submission, or consumption. The raw path therefore records
`written` evidence and never claims provider acceptance.

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
inject-to-notify, submit-to-prefill, or existing-exact-to-new downgrade is
allowed. Unknown apps fail closed, and prompt text must not drive generic GUI
automation.

App adapters pin bundle ID, Team ID, resolved executable, adapter version,
session ID, process generation, and endpoint identity. A mismatch fails closed.
The stateless deep-link exception is defined below. Repair or
restart remains `operator_only` until the identity and generation checks match
the TTY contract. `notifier_live` is not consumption.

An external `--inject-via` provider must use the AMQ transport protocol. Exit
zero is accepted only with the exact stderr marker
`AMQ_INJECT_PROGRESS=accepted`. `deferred` means pre-dispatch busy or
transition and keeps the same unread cohort on the retry ladder. `uncertain`
wins over every other marker and enters recovery. Other nonzero exits and
timeouts are terminal `failed` outcomes for the current AttemptID and are not
silently replayed. A bare legacy exit zero is `written`/uncertain evidence,
never accepted provider dispatch. See the [wake doorbell acknowledgement
policy](wake-doorbell-acknowledgement.md).

## Implemented adapter constraints

The capability vector is enforced at registration. An adapter that does not
declare capability is treated as weakest on ordered axes and as
`requires_human`; it cannot masquerade as an unattended full-strength seat.
The gate runs after target resolution and before any registry write or
injection. Discovery may run a read-only identity probe.

The Go `Capability` type in `internal/keepalive/adapter/capability.go` models
`activation`, `delivery`, `session`, and `requires_human`. Evidence is a
separate contract, not a fifth ordered Go axis; receipts and wake inspection
define its meaning.

The native Windows companion supports direct submitted adapters only. It does
not provide the Unix TTY lifecycle, `amq wake`, `coop exec`, attach, reattach,
or terminal supervision. These limitations are part of the public contract.

- `codex-queue` accepts only `codex-queue:thread:<uuid>`, never `:new`.
  It requires an active writer for that exact thread; an idle lock file is
  not sufficient. Successful `codex queue` execution proves enqueue to the
  writer, not turn completion. Changing ambient `CODEX_HOME` requires a wake
  restart.
- `claude-print` accepts only `claude-print:session:<uuid>`, never `:new`.
  It refuses a session with a live owner or uncertain owner evidence and
  serializes AMQ injections with an exclusive lock. The child uses fixed
  `--permission-mode auto --allowedTools 'Bash(amq *)'` arguments. Submitted
  evidence is the matching post-init `isReplay` echo of this child's input,
  not a historical message or a completed turn. `CLAUDE_CONFIG_DIR` is
  ambient; a change requires a wake restart.
- Claude Desktop and Codex app deep links require explicit acceptance of a
  human-required seat with compatible prefill and session minima. A successful
  `open` command proves dispatch only: the app can reject a link without an
  adapter-visible error. Codex exact-thread links must name an idle thread.
  Neither deep-link adapter automatically submits text or proves consumption.
  JavaScript and Accessibility injection are not supported alternatives.

GUI wake does not inherit TTY repair.

### Deep-link identity exception

The Claude Desktop (`claude://`) and Codex app (`codex://`) prefill seats are
stateless launches, so they pin and revalidate the scheme-owner bundle rather
than a persistent process generation. They do not claim the full identity-pin
set or an acceptance receipt. This is a deliberate narrow capability, not a
silent downgrade.

## Consequences

- A caller can compare seats by declared capability and evidence instead of
  guessing from product names.
- A refusal is explicit when the requested guarantee is unavailable.
- Native Windows support does not imply Unix terminal lifecycle support.
- No GUI path claims consumption, and no raw terminal path claims provider
  acceptance.
