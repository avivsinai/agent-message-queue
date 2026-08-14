# Wake operations

This reference defines the operator-facing contracts for `wake check`,
`repair`, `recover-owner`, `retire`, and doctor wake repairs. For durable file
and crash invariants, see [Wake state invariants](wake-state-invariants.md) and
the [wake lifecycle document](wake-lifecycle.md).

## Inspect before mutation

Run `amq wake check --me <agent> --json` before stopping, repairing, or
replacing a wake. The check is read-only. Its `restart_capability` is
`agent_safe`, `operator_only`, or `unavailable`, and it supplies the next
action when one is safe. Only `agent_safe` authorizes an automated agent to
act. `operator_only` requires the owning terminal or supervisor.

A non-TTY agent must preserve a live wake unless the check reports
`agent_safe`. The check is advice, not mutation authority: every mutating
command revalidates the current process, target, generation, root, and owner
state before it changes anything.

## Doctor

`amq doctor --ops` reports queue depth, sibling-session backlog, DLQ age,
presence freshness, integration hints, and wake health. Wake locks have two
conservative problem states:

- `stale`: AMQ proves that the recorded process is gone, mismatched, or not the
  same `amq wake`. `--fix-wake-locks` rechecks and removes only this exact lock.
- `unverified`: AMQ cannot prove ownership or staleness. Startup fails closed
  and doctor preserves the lock for operator inspection.

Wake-lock repair follows the session guard. A target outside the authenticated
pinned base is inspected but not changed unless the command has an explicit
root and `--ignore-session-pin`. A guard refusal is a structured doctor error;
doctor otherwise exits 0.

`notifier_live` means the wake-lock inspector confirmed a live wake process.
It proves prompt notification, not message consumption. `recent_activity`
means only that `last_seen` is fresh. AMQ does not claim `consumer_live`
without a separate monitor heartbeat or lock.

## Repair

`amq wake repair --me <agent>` can replace a proven-stale inject-via wake. It
can also supersede an unverified ownerless generic lock only after the saved
target and continuity state pass the same fail-closed validation. Raw TTY
wakes, owner-bound or invalid claims, and leftover targets without an eligible
lock are not repairable.

Repair requires a private mode-0600 `.wake.target` whose digest matches the
lock. It also requires `.wake.repair-floor` to match the exact generation,
target, physical root, boot, and owner state. The floor contains only the file
identities already suppressed by that wake, not message IDs. Repair passes it
to the replacement instead of taking a new inbox baseline, so arrivals during
downtime and same-name DLQ retries remain eligible. Missing, corrupt, or
mismatched continuity state requires a normal wake restart.

Replacement diagnostics go to `agents/<agent>/.wake.repair.log`. Repair must
not keep output pipes open after its command response exits. `doctor --ops`
may report `target_present`, `repair_available`, and `repair_reason`, but it
never starts a wake.

## Owner recovery

`amq wake recover-owner` releases one cooperative owner claim. A live owner
must present the AMQ-managed `AMQ_WAKE_OWNER` token. A conclusively dead owner
does not require the token. There is no force mode: unknown, live, legacy, or
malformed owner state is preserved.

## Retirement

`amq wake retire` requires the expected absolute inject-via executable and its
ordered fixed arguments. It stops only an identity-confirmed live inject-via
wake with an unchanged saved target, using Linux pidfd signaling or the Darwin
control socket. It can also remove an exactly bound proven-stale lock.

Retirement preserves mailbox contents. Exact lock removal is its commit point:
a failure before that point is `refused`; a later target or state cleanup
failure is `retired_with_residue`, an exit-0 success with a warning. The other
successful result is `retired`. A replacement generation is never selected
for cleanup.

The lifecycle boundaries are:

- `repair` replaces a proven-stale eligible inject-via wake.
- `recover-owner` releases one cooperative owner claim.
- `doctor --ops --fix-wake-locks` removes one proven-stale lock.
- `retire` stops an identity-confirmed inject-via wake.
- launchd, systemd, or the owning shell stops a raw wake.

Retirement does not unload a supervisor and cannot promise that the supervisor
will not start another wake. Long-running wake and monitor supervision belongs
to launchd, systemd, or another layer above daemon-free AMQ. The
[co-op guide](../COOP.md#supervisor-recipes) owns supervisor recipes; the
[keepalive reference](amq-keepalive.md) covers the macOS companion.
