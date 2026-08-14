# Session routing and safety

AMQ keeps each named session in its own queue root. A participating shell is
pinned by `AM_ROOT`, `AM_BASE_ROOT`, and `AM_SESSION`; identity-aware shells
also carry AMQ-managed root identity tokens. For a named session,
`AM_BASE_ROOT` is the authorized parent. For a sessionless context, it is the
exact root and `AM_SESSION` is empty. Do not set the identity tokens manually.

## Root selection

Root precedence is explicit `--root`, `AM_ROOT`, project `.amqrc`,
`AMQ_GLOBAL_ROOT`, then eligible implicit fallbacks. Within a Git checkout,
only repo-local implicit state is eligible. An unreadable or invalid project
`.amqrc` blocks lower-precedence fallback; an explicit `--root` or `AM_ROOT`
can override it intentionally.

A direct `--root` selects a queue. It is not a federation route. `send` refuses
an explicit root in a different base tree when the caller has an active
session and supplies no `--project`, `--session`, or `--from-session`. Use
`--project` or `--session` for replyable routing because these options add the
sender-origin metadata that `reply` needs. Bare-root scripts without session
evidence keep direct-root behavior and can create an unreplyable message.

## Session guard

`read`, `drain`, `monitor`, `watch`, and DLQ commands compare their target with
the active pin before reading or moving mailbox state. `watch` and `monitor`
repeat this check while they wait. `send` and `reply` apply the same guard to
their local source. A mismatch exits with code 5. Target routing does not
authorize a mismatched source.

An implicit participating command also refuses when its pin conflicts with an
initialized queue in the current project. A live identity-bound sessionless
pin is the narrow exception: when both identity tokens authenticate its exact
root, that explicit context outranks ambient project discovery. Named, legacy,
incomplete, stale, and mismatched pins still refuse.

Use a named `--session` or `--project` route when possible. For intentional
raw-root access, `--ignore-session-pin` requires a non-empty explicit `--root`.
Explicitly empty root or session values are usage errors. `--base-root` gives
`doctor` configuration authority only; it does not bypass the guard.

`list` and `doctor --root` remain read-only inspection paths on a mismatch and
report a warning. Doctor repairs are allowed for the authenticated pinned base
root or with an explicit `--root` plus `--ignore-session-pin`. Other mismatch
repairs are skipped and reported as structured doctor errors while the doctor
process continues with exit 0.

`doctor --fix-mailboxes` creates only missing required directories for the
configured roster and reserved `user` handle. It reports discovered mailboxes
outside that roster but never repairs them. It does not edit, move, overwrite,
or delete message files; unsafe types, symlinks, unreadable paths, and
concurrent layout changes fail closed. `--base-root` must name the target or
its direct parent.

`send --from-session` is deliberately double-explicit and resolves its source
from the supplied raw base; callers must verify that base. See
[issue #104](https://github.com/avivsinai/agent-message-queue/issues/104).

## Environment replacement

Every shell-mode `amq env` result replaces `AM_ROOT`, `AM_ME`, `AM_BASE_ROOT`,
and `AM_SESSION` as one context. Sessionless output pins the exact root and
emits an empty session. An ambient root that conflicts with an existing pin is
rejected unless a non-empty `--root` or `--session` explicitly repins it.
`amq env --session` routes from a valid pinned base before it checks project
configuration.

## Creation and backlog discovery

Create named sessions explicitly with `amq session create <name>`. `coop exec`
uses the declared default session, or `collab` when no default is declared.
Its missing-session bootstrap is deprecated except for zero-configuration
`collab`; scripts must use `session create` or `init --root`. Bare repositories
cannot host implicit bootstrap, and `coop exec --no-init` keeps the refusal.

An empty `drain` or `list --new` performs a shallow sibling-session scan and
prints exact inspection commands. `doctor --ops` reports the same state as
`sibling_backlog`. When the active session differs from the base queue,
`doctor --ops` can also report `base_backlog`; JSON output includes the target,
session, agent, pending count, and exact inspection command.

Git worktree diagnostics remain in `doctor --ops`, not the send path. Relative
and auto-detected roots belong to one worktree; deliberate sharing requires an
absolute `.amqrc` root or `AMQ_GLOBAL_ROOT`.
