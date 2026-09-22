# Session-pin decision table

This table is the behavior-preserving contract for session-pin policy. Rows are
policy decisions, not call sites. The pure implementation is
[`internal/sessionguard/session_guard_table.go`](../internal/sessionguard/session_guard_table.go),
and the existing handlers feed their normalized evaluator facts through it.

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

The evaluator normalizes the target relation before this table runs. An absent
pin with no established conflicting tree is `Unbound`; a valid pin with a
matching target is `Match`; a pin or CWD conflict is `Mismatch`; and the
explicit pinned base-root inspection/repair case is `OwnPinnedBase`. Filesystem
resolution, identity authentication, preflight validation, and CWD conflict
detection remain outside the pure table.

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
preflight. `send --from-session` has a separate source identity preflight.
`CrossProject` never turns a source pin
mismatch into an allow.

## Site-to-row mapping

| Source site | Policy/phase | Rows consulted |
|---|---|---|
| `internal/cli/read.go` (`runRead`) | Mailbox entry | R01, R02, R03, R04, R08, R09 |
| `internal/cli/drain.go` (`runDrain`) | Mailbox entry | R01, R02, R03, R04, R08, R09 |
| `internal/cli/watch.go` (`runWatch` and revalidation) | Mailbox entry/revalidation | R01, R02, R03, R04, R08, R09 |
| `internal/cli/monitor.go` (`runMonitor` and revalidation) | Mailbox entry/revalidation | R01, R02, R03, R04, R08, R09 |
| `internal/cli/list.go` (`runList`) | CWD and warning inspection | R01, R02, R06, R10, R11 |
| `internal/cli/dlq.go` (`runDLQ*`) | Mailbox entry | R01, R02, R03, R04, R08, R09 |
| `internal/cli/send.go` (`runSendWithAfterBodyRead`) | Source branches | R01, R02, R04, R05, R08, R09 |
| `internal/cli/reply.go` (`runReply`) | Source entry | R01, R02, R04, R08, R09 |
| `internal/cli/route.go` (`explainRoute`) | Source JSON probe | R01, R02, R14, R15 |
| `internal/cli/env.go` (`runEnv`) | Ambient env resolution | R01, R02, R08, R09 |
| `internal/cli/doctor.go` (repair gates) | Wake-lock and mailbox repair | R01, R02, R04, R07, R12, R13 |
| `internal/cli/doctor.go` (identity diagnostic) | Evaluator-only diagnostic | no decision row |

Watch/monitor revalidation must abort on a
mid-wait repin and uses the same refusal row as entry. Doctor’s pinned-base
exception and structured-error/exit-0 reporting are contract behavior. Route
explain’s mismatch remains a JSON error with exit 0 so it stays a probing
surface.

The table receives normalized evaluator facts. An explicit `amq env` repin is
represented as `PinAbsent`/`Unbound` because that path deliberately bypasses
the ambient pin check; an invalid pin remains `R09` unless the caller’s
validated ignore bypass is reached. `send` loads the pin before its guard, so
its `R04` replay uses a valid pin while malformed input is a preflight `R09`.

## Scope boundary

The pure table remains side-effect-free: filesystem pin loading, CWD discovery,
identity authentication, `validatePinOverride`, send foreign-root usage checks,
route planning, doctor rendering, and command error formatting stay at their
own layers. Each caller feeds evaluated facts into the table and owns its
error construction, warning/JSON rendering, and process-exit behavior. The
table does not resolve paths, check capabilities, or perform delivery.
