# W2a Step 1: session-pin guard derivation

Base inspected: `bd48b80b69b4852f24a2455ba62e95c72d0f58dc` (`origin/main`),
2026-08-01. This is a source-derived packet only. It intentionally contains
no decision-table encoding, production change, or call-site rewiring.

## Counting rule and resolved membership

The source has fifteen primary logical guard sites when mutually exclusive
`send` branches are one source check and long-lived revalidation callbacks are
the callback phase of their owning handler:

1. `read`
2. `drain`
3. `watch` initial guard
4. `monitor` initial guard
5. `list` warning path
6-9. `dlq list`, `dlq read`, `dlq retry`, `dlq purge`
10. `send` source guard (two mutually exclusive branches)
11. `reply`
12. `route explain`
13. `env` ambient-context check
14-15. `doctor` wake-lock repair and mailbox repair gates

Claude’s R1 ruling resolves the membership: `route.go:147` is a table
consumer, `doctor.go:437` is evaluator-only, and the watch/monitor callbacks
share their entry rows. There are therefore 15 canonical policy rows and 19
mapped decision-site consumers (including the distinct `list.go:86` CWD
warning check, both `send` branches, and both live revalidation callbacks),
plus one evaluator-only diagnostic at `doctor.go:437`.

## Shared evaluator used by the primary sites

`sessionPinMismatch(target)` canonicalizes `target`, then `loadSessionPin`
reads `AM_SESSION`, `AM_BASE_ROOT`, `AM_ROOT`, and optional
`AM_ROOT_ID`/`AM_BASE_ROOT_ID`. A malformed or incomplete pin is a
`ContextMismatchError` (exit 5). A legacy pin validates ambient `AM_ROOT` and
requires lexical equality with its expected base/session root. An identity pin
authenticates the base, direct session child, target root, and identity tokens.
With no pin, an established conflicting source tree is still a mismatch;
without positive pin/tree evidence the result is allow.

`guardMailboxContext(command, target, routed, ignorePin, explicitRoot)` first
allows `routed` or `ignorePin`. Otherwise it runs
`guardCwdLocalContext` when the root was not explicit, then
`sessionPinMismatch`. A mismatch is wrapped as exit 5 with
`refusing <command>: ... Use --session <name> ...`; cwd ambiguity uses its
own exit-5 refusal. `validatePinOverride` at the participating handlers
requires explicit `--root` for the bypass and rejects the bypass with
`--session`.

`guardPinnedSourceContext(command, target, crossProject, ignorePin,
explicitRoot)` allows `ignorePin`; it skips the cwd-local check only for an
explicit root or cross-project route, but still evaluates
`sessionPinMismatch`. Its mismatch is exit 5 with
`Target routing does not authorize a mismatched source...`.

## Fifteen primary logical sites

| # | Source | Exact inputs consulted | Allow / warning / refusal outcome |
|---:|---|---|---|
| 1 | `internal/cli/read.go:50` `runRead` | `root,routed := resolveMailboxRoot(common,--session)`; `--ignore-session-pin`; whether `--root` was visited. `validatePinOverride` already ran. | `guardMailboxContext("read",...)`: routed or valid bypass allows; otherwise cwd conflict or pin mismatch returns exit 5 (`refusing read...`) before delivery-root open/read. |
| 2 | `internal/cli/drain.go:51` `runDrain` | Same target and flags as read. | Same evaluator, `refusing drain...`, before mailbox inspection or move. |
| 3 | `internal/cli/watch.go:73` `runWatch` | Same resolved target, routed bit, ignore flag, and root-explicit bit. | Same evaluator, `refusing watch...`, before capability open and live wait. The callback recheck is listed below as an extra phase. |
| 4 | `internal/cli/monitor.go:72` `runMonitor` | Same resolved target, routed bit, ignore flag, and root-explicit bit. | Same evaluator, `refusing monitor...`, before initial collect/live wait. The callback recheck is listed below as an extra phase. |
| 5 | `internal/cli/list.go:102` `runList` | Only when `!routed`: resolved `root`, pin state, cwd-local evidence, and `common.rootExplicit()` for the pinned-base exception. There is no ignore flag. | `sessionPinMismatch` and cwd conflicts are warnings, not refusals; a context-mismatch from `resolveMailboxRoot` is also downgraded to a warning and falls back to the ambient root. An explicit inspection of the pin's own base root suppresses the final warning. Listing continues read-only. |
| 6 | `internal/cli/dlq.go:101` `runDLQList` | Resolved target/routed bit, `--ignore-session-pin`, root-explicit bit. | `guardMailboxContext("dlq list",...)`; exit-5 `refusing dlq list...` on mismatch before DLQ listing. |
| 7 | `internal/cli/dlq.go:288` `runDLQRead` | Same mailbox inputs. | Exit-5 `refusing dlq read...` before DLQ read/capability open. |
| 8 | `internal/cli/dlq.go:419` `runDLQRetry` | Same mailbox inputs. | Exit-5 `refusing dlq retry...` before retry mutation. |
| 9 | `internal/cli/dlq.go:601` `runDLQPurge` | Same mailbox inputs. | Exit-5 `refusing dlq purge...` before purge mutation. |
| 10 | `internal/cli/send.go:171` or `:187` `runSendWithAfterBodyRead` | `sourceRoot` (initially ambient `root`), `targetProject` (`crossProject`), `fromSession`, explicit-root bit, ignore flag, and full legacy/identity pin. `:171` is the identity-pin/no-`from-session` branch; `:187` is the no-pin/legacy/no-`from-session` branch. | The mutually exclusive branches implement one source guard. Cross-project skips only the cwd-local check; target session does not. A mismatch is exit 5 `refusing send...Target routing does not authorize...`. `from-session` skips this guard and later resolves an explicitly named source child after its separate identity check. The separate unqualified foreign `--root` check at `send.go:176-182` is a usage error, not this guard. |
| 11 | `internal/cli/reply.go:60` `runReply` | Local source `root`, ignore flag, explicit-root bit; `crossProject=false`; no target session flag. | `guardPinnedSourceContext("reply",...)`; exit-5 `refusing reply...` on mismatch. A second source identity snapshot check occurs after this guard. |
| 12 | `internal/cli/route.go:147` `explainRoute` | `sourceRoot` from `--from-root`/route resolution, `targetProject` (`crossProject`), and whether `--from-root`/`--root` was supplied. No ignore flag exists. | Calls the source guard with command label `send`, but catches the error into `routeExplainResult.Error`; `runRouteExplain` serializes JSON and returns nil, so a mismatch is normally process exit 0 rather than exit 5. |
| 13 | `internal/cli/env.go:162` `runEnv` | `contextExplicit` (`--root` or `--session` was visited) and resolved `root`; otherwise `sessionPinMismatch(root)`. `--session` has already performed pin/base/direct-child validation at `env.go:117-153`. | With no explicit context, mismatch is exit 5 `refusing env...Use explicit --session...`; malformed pin is also exit 5. An explicit root/session deliberately bypasses this post-resolution mismatch check. |
| 14 | `internal/cli/doctor.go:176` `runDoctor` | Only `--ops --fix-wake-locks` with no ignore; target `root`, pin state, and `isPinnedBaseRoot(root)`. | Invalid pin or a mismatch outside the pinned base appends a `Wake lock repair` error check and disables repair; doctor continues and normally exits 0. A mismatched target recognized as the pinned base root is allowed to repair; explicit ignore bypasses the check. |
| 15 | `internal/cli/doctor.go:561` `inspectDoctorMailboxes` | Only `--fix-mailboxes` with no ignore; target `root`, pin state, and `isPinnedBaseRoot(root)`. | Invalid pin or mismatch outside the pinned base returns a `Mailboxes` error check and no repair; outer doctor continues (normally exit 0). Pinned-base exception and explicit ignore allow repair. |

## Additional textual phases outside the fifteen grouping

- `internal/cli/watch.go:101` re-runs the mailbox guard from
  `revalidateContext` during watcher setup, idle, events, and finalization;
  `deliveryRoot.VerifyBase()` is chained after it.
- `internal/cli/monitor.go:99` does the same for monitor setup, wait, and
  drain phases.
- `internal/cli/list.go:86` performs the cwd-local conflict warning check;
  `list.go:102` performs the later pin warning and pinned-base suppression.
- `internal/cli/doctor.go:437` (`checkSessionPinIdentity`) renders
  `sessionPinMismatch` as `ok`/`warn` diagnostic state, distinct from the two
  mutation gates above.
- `internal/cli/delivery_root.go:7-22` (`snapshotMailboxDeliveryRoot`) is a
  post-guard identity capability check for identity pins, not a call to the
  session guard.

## Behavioral differences to preserve or rule

1. Mailbox guards refuse with exit 5; `list` warns and continues; doctor
   mutation gates report structured errors and continue; `route explain`
   returns a JSON error with exit 0. These are not interchangeable outcomes.
2. `guardMailboxContext` treats `routed` and `ignorePin` as early allows;
   `guardPinnedSourceContext` treats only `ignorePin` as an early allow and
   still checks the pin for cross-project routes. Both skip cwd ambiguity for
   explicit roots, but only the source guard also skips it for cross-project
   routing.
3. `send` has identity-vs-legacy branch selection and a deliberate
   `--from-session` source-route escape from the normal source guard. `reply`
   and `route explain` do not share that branch.
4. `watch` and `monitor` can discover a pin mismatch after a long wait, unlike
   one-shot read/drain/DLQ entry guards.
5. Doctor permits a pinned-base-root repair through `isPinnedBaseRoot`, while
   the ordinary mailbox/source guards would refuse a mismatched target.
6. Environment explicit `--root`/`--session` is a deliberate repin path;
   ambient env resolution is the only path using the direct mismatch refusal.

## CLAUDE.md prose comparisons requiring a ruling before Step 2

- Lines 117 and 119-121 describe mismatch exit 5 for participating/session
  guards, but the current route-explain, list, and doctor surfaces have the
  structured/diagnostic outcomes above. The route-explain command is not named
  in the prose; decide whether it belongs in the table or is a parity-only
  inspection surface.
- The prose says `send` and `reply` apply the same local-source check and that
  target routing never authorizes a mismatched source. Current `send
  --from-session` intentionally skips the normal source guard after its
  separate source identity check; decide whether the known limitation at the
  end of line 117 is the ruling for this branch.
- Lines 374-387 and 119-121 say doctor inspection can warn and repair refuses
  a mismatched target unless ignored. Current code additionally authorizes a
  target proven to be the pin's own base root (`isPinnedBaseRoot`) and reports
  the refusal as a doctor check while returning normally. Decide whether that
  base-root exception and exit behavior are contract semantics.
- Line 117 says explicitly blank `--root`/`--session` values are usage errors;
  `parseFlagsWithPositionals` enforces this before these sites, so the source
  agrees (the guard itself does not own that validation).
- Line 123 says ambient env conflicts are rejected unless explicit root/session
  repins; `env.go:162` agrees, but its explicit `--session` path performs a
  separate authenticated base/direct-child validation before bypassing the
  ambient mismatch check.
- The prose names watch/monitor as one guard before mailbox state access; the
  source also revalidates during live operation. Decide whether those callback
  phases are equivalence cases in Step 2 or remain a separate revalidation
  contract.

No decision table or rewiring should be produced until these rulings and the
canonical fifteen-site count are confirmed.
