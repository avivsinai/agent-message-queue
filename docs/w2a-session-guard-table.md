# W2a Step 2: session-pin decision table

This table is the behavior-preserving freeze derived from
[`w2a-session-guard-derivation.md`](w2a-session-guard-derivation.md) and
Claude’s R1–R6 rulings. Rows are policy decisions, not call sites. The pure
implementation is `internal/sessionguard/session_guard_table.go`; the authorized Step 3
rewire now feeds the existing handlers through it.

## Dimensions

- Policy: `Mailbox`, `Source`, `List`, `Env`, or `DoctorRepair`. Route explain
  is `Source` with the JSON error channel.
- Pin: absent, valid legacy, valid identity, or invalid (malformed,
  incomplete, stale, or unauthenticated).
- Relation: `Unbound` (no pin/tree conflict), `Match`, `Mismatch` (pin or CWD
  conflict after evaluator normalization), or `OwnPinnedBase`.
- Flags: routed mailbox target, ignore pin, explicit root/context,
  cross-project source route, from-session source route, and revalidation
  phase. Explicit root/cross-project only affect the pre-table CWD check; they
  never waive a pin mismatch.

The four non-success channels are first-class verdicts: exit-5 refusal,
warning-and-continue, structured doctor error with process exit 0, and route
JSON error with process exit 0.

## Canonical 15 rows

| Row | Policy/input condition | Verdict |
|---|---|---|
| R01 | Any policy; unbound relation (including explicit env repin) | Allow |
| R02 | Any policy; valid pin and matching target | Allow |
| R03 | Mailbox; routed `--session` after route preflight | Allow |
| R04 | Mailbox/source/doctor; validated `--ignore-session-pin` early bypass | Allow |
| R05 | Source; `--from-session` after separate source identity preflight | Allow |
| R06 | List; own pinned base with explicit root | Allow; suppress warning |
| R07 | Doctor repair; own pinned base | Allow repair |
| R08 | Mailbox/source/env; valid pin or established tree mismatch | Refuse, exit 5 |
| R09 | Mailbox/source/env; invalid pin | Refuse, exit 5 |
| R10 | List; valid pin/tree mismatch | Warn and continue |
| R11 | List; invalid pin | Warn and continue |
| R12 | Doctor repair; valid pin/tree mismatch | Structured error, continue, exit 0 |
| R13 | Doctor repair; invalid pin | Structured error, continue, exit 0 |
| R14 | Source with JSON channel (route explain); valid mismatch | JSON error, exit 0 |
| R15 | Source with JSON channel (route explain); invalid pin | JSON error, exit 0 |

`IgnorePin` and `Routed` are early policy rows only after the existing caller
preflight. `send --from-session` remains a visible bypass contract; the
resolver follow-up is outside W2a. `CrossProject` never turns a source pin
mismatch into an allow.

## Site-to-row mapping (19 decision sites plus one evaluator-only diagnostic)

| Source site | Policy/phase | Rows consulted |
|---|---|---|
| `internal/cli/read.go:50` | Mailbox entry | R01, R02, R03, R04, R08, R09 |
| `internal/cli/drain.go:51` | Mailbox entry | R01, R02, R03, R04, R08, R09 |
| `internal/cli/watch.go:73` | Mailbox entry | R01, R02, R03, R04, R08, R09 |
| `internal/cli/watch.go:101` | Mailbox revalidation | R01, R02, R03, R04, R08, R09 |
| `internal/cli/monitor.go:72` | Mailbox entry | R01, R02, R03, R04, R08, R09 |
| `internal/cli/monitor.go:99` | Mailbox revalidation | R01, R02, R03, R04, R08, R09 |
| `internal/cli/list.go:86` | CWD warning inspection | R10 |
| `internal/cli/list.go:102` | Warning inspection | R01, R02, R06, R10, R11 |
| `internal/cli/dlq.go:101` | Mailbox entry | R01, R02, R03, R04, R08, R09 |
| `internal/cli/dlq.go:288` | Mailbox entry | R01, R02, R03, R04, R08, R09 |
| `internal/cli/dlq.go:419` | Mailbox entry | R01, R02, R03, R04, R08, R09 |
| `internal/cli/dlq.go:601` | Mailbox entry | R01, R02, R03, R04, R08, R09 |
| `internal/cli/send.go:171` | Source identity-pin branch | R01, R02, R04, R05, R08, R09 |
| `internal/cli/send.go:187` | Source legacy branch | R01, R02, R04, R05, R08, R09 |
| `internal/cli/reply.go:60` | Source entry | R01, R02, R04, R08, R09 |
| `internal/cli/route.go:147` | Source JSON probe | R01, R02, R14, R15 |
| `internal/cli/env.go:162` | Ambient env resolution | R01, R02, R08, R09 |
| `internal/cli/doctor.go:176` | Wake-lock repair gate | R01, R02, R04, R07, R12, R13 |
| `internal/cli/doctor.go:437` | Evaluator-only diagnostic | no decision row |
| `internal/cli/doctor.go:561` | Mailbox repair gate | R01, R02, R04, R07, R12, R13 |

There are 19 decision-site mappings in this table; the doctor diagnostic is a
separate evaluator-only site. Watch/monitor revalidation must abort on a
mid-wait repin and uses the same refusal row as entry. Doctor’s pinned-base exception and structured-error/exit-0
reporting are contract behavior. Route explain’s mismatch remains JSON error
with exit 0 so it stays a probing surface.

The table receives normalized evaluator facts. An explicit `amq env` repin is
represented as `PinAbsent`/`Unbound` because that path deliberately bypasses
the ambient pin check; an invalid pin remains `R09` unless the caller’s
validated ignore bypass is reached. `send` loads the pin before its guard, so
its `R04` replay uses a valid pin while malformed input is a preflight `R09`.

## Scope boundary

The pure table remains side-effect-free: filesystem pin loading, CWD discovery,
identity authentication, `validatePinOverride`, send foreign-root usage checks,
route planning, doctor rendering, and command error formatting stay at their
own layers. The authorized Step 3 rewire now feeds those evaluated facts into
the table at the mapped guard sites; each caller still owns its existing error
construction, warning/JSON rendering, and process-exit behavior. The rewire
does not change the resolver, capability checks, or any delivery operation.
