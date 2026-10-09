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

## Hold by priority

Each doorbell can start a full-context turn, and mail comes in bursts. Two
opt-in settings let a wake hold its first doorbell. For example:

```bash
amq wake --me claude --hold-normal 5m --hold-low 30m
amq wake config --me claude --hold-normal 5m --hold-low 30m
```

The first command sets the hold on a fresh start. The second changes it on a
running wake. Both write [`.wake.settings`](#live-settings).

- `hold_normal` and `hold_low` (flags `--hold-normal` and `--hold-low`) set the hold for `normal` and `low` mail.
  A message without a priority counts as `normal`. Both default to `0`,
  so holds are off unless requested. The example's `5m` / `30m` is a workload
  choice for batching routine mail, not a default.
- The first undrained message sets one deadline: its inbox arrival time plus
  the hold for its priority. Before the first attempt, a later message can
  pull this deadline earlier, never later. When the deadline passes, the
  first doorbell becomes eligible. A drain before then cancels it; a drain
  after any doorbell takes every pending message, including held mail.
- Arrival time comes from the file's modification time in `inbox/new`, not
  the sender's header clock. A restart with the same hold settings reads
  the same file and retains its deadline. A missing or future-dated
  modification time bypasses the hold. No separate hold state is persisted.
- The hold is a setting, not target metadata. It lives in `.wake.settings`, so
  `amq wake repair`, `coop exec`, keepalive, and self-upgrade replacements
  start with the stored hold. A change to the hold applies to the pending
  cohort: a shorter hold releases held mail at the next scan or within 2
  seconds, and a longer hold moves the deadline later from the file's
  modification time. After the first attempt, holds no longer apply.
- `urgent` mail skips the priority hold, including when a cohort is parked.
  A distinct urgent message can revive a parked cohort for one further
  attempt. Ordinary urgent mail still passes through watcher debounce and
  input-quiet deferral; urgent mail with the configured interrupt label follows
  the interrupt path.
- The hold changes only the first doorbell's timing. After an attempt, the
  configured retry and backoff rules apply, including the finite input-attempt
  budget for an unchanged cohort. Injection modes and recovery are unchanged.
  A hold does not ack or delete a message, or guarantee when the recipient
  will drain it.

Use `amq send --priority urgent` or `amq reply --priority urgent` for a
time-critical verdict or unblocking request that should skip the hold.
Ordinary review responses remain normal by default.

## Live settings

A running wake has two kinds of input.

- Identity and transport are fixed for the life of the process: `--me`,
  `--root`, `--inject-mode`, `--inject-via`, `--inject-arg`, `--inject-cmd`,
  `--interrupt-cmd`, `--retry-until`, and `--no-self-upgrade`. They are bound
  into the lock, the target, or the terminal authority. To change one, restart
  the wake through its owning terminal or supervisor after a `wake check`.
- Settings are policy values the wake reads each time it uses them: `debounce`,
  `hold_normal`, `hold_low`, `preview_len`, `bell`, `defer_while_input`,
  `input_quiet_for`, `input_poll_interval`, `input_max_hold`, `interrupt`,
  `interrupt_label`, `interrupt_priority`, `interrupt_notice`,
  `interrupt_cooldown`, and `inject_timeout`. Each has a flag of the same name
  with `-` in place of `_`.

The settings file is `<root>/agents/<handle>/.wake.settings`, canonical JSON:

```json
{"schema":1,"settings":{"hold_normal":"5m0s","hold_low":"30m0s"}}
```

A key that is absent has its default. Durations are Go duration strings. The
file must be mode 0600 and owned by the current user, and the wake refuses a
file that is not, one with an unknown key or a newer schema, or one with a
value that fails validation. A refused file never changes the running settings.
Use `amq wake config` to edit it. For a hand edit, keep mode 0600.

**Precedence.** Effective settings are the defaults overlaid by the file.
Explicit settings flags on a fresh `amq wake` write their keys into the file,
including a value equal to the default. A resume (self-upgrade or
`amq wake restart`) ignores settings flags in argv when the file exists, so a
later `wake config` change is not undone by the original command line. When the
file is absent, a resume seeds it once from the settings flags in its argv; a
resume never exits because of the file. Repair, `coop exec`, and keepalive pass
no settings flags and start fresh on the stored file. A start that loses the
lock, and `--accept-existing-wake` that adopts a live wake, write nothing. A
fresh start with explicit flags refuses to start when the file is refused or
cannot be written. A fresh start without settings flags runs with the defaults,
logs the error, and records `refused`. A self-upgrade or restart candidate is
refused at preflight while the file is refused, and the refusal is remembered
for the generation: fix the file, then request `amq wake restart`.

**Apply timing.** The wake checks the file at the start of each inbox scan and
every 2 seconds. The 2-second check rescans only when a hold changes; every
other key applies at its next use, so a change is in force for the next message. An armed debounce
timer keeps its duration; the new debounce applies from the next inbox event.

```bash
amq wake config --me claude                       # show values, source, wake status
amq wake config --me claude --hold-low 10m --wait # set, then wait for the wake
amq wake config --me claude --unset hold_low      # return a key to its default
amq wake config --me claude --reset --hold-low 10m # replace the file with exactly these keys
```

`wake config` shows each setting with its source (`file` or `default`) and the
wake status: `applied`, `pending`, `refused: <error>`, `unreported`, or
`no running wake`. A refused file shows `file: refused: <error>` and the rows
show defaults, not the running wake's values; the running wake keeps its last
applied settings. The JSON status `unreported` (text: `running wake does not
report live settings`) means the wake runs an older image or its status write
failed; restart it to adopt live settings. `show` and `set` never create the
agent directory: a missing mailbox exits 3.
Any setting flag or `--unset` validates the whole new set and writes it. On a
refused file, `set` and `--unset` exit 1 and point at `--reset`, which replaces
the file with exactly the given keys (all defaults when none are given). While
the file is absent and the wake is `unreported`, `set` and `--unset` exit 6: the
wake may run command-line settings the file lacks. Restart it (a resume seeds the
file from its flags) or use `--reset` with the full set. A bad
value exits 2 and writes nothing. A restart-only flag exits 2 with the restart
instruction. `--wait` waits until the running wake records the new file digest
in `.wake.settings.applied`; `--timeout` bounds it (default 60s, `0` waits
forever), exit 4 on timeout, exit 1 when the wake refuses the file, exit 6
when the wake stays `unreported` for 5 s, and exit 0 at once when no wake runs. `--json` prints the same data. The wake records its
state in `.wake.settings.applied`; the file is not a receipt of message
consumption.

## Doctor

`amq doctor --ops` reports queue depth, sibling-session backlog, DLQ age,
presence freshness, integration hints, and wake health. Wake locks have two
conservative problem states:

- `stale`: AMQ proves that the recorded process is gone, mismatched, or not the
  same `amq wake`. `--fix-wake-locks` rechecks and removes only this exact lock.
  When the lock's image or restart stage lives under a directory that no longer
  exists, `wake check --json` and `doctor --ops` report `reason_code=binary_dir_gone`
  and name `amq doctor --ops --fix-wake-locks` as the next action instead of a
  raw ENOENT.
- `unverified`: AMQ cannot prove ownership or staleness. Startup fails closed
  and doctor preserves the lock for operator inspection.

Stale-lock removal and staged-executable deletion use independent identity
checks. If a stage no longer matches its saved file identity (for example,
device numbering changed after a reboot), cleanup preserves that file and
prints its path, while exact stale-lock removal can continue. Doctor also
quarantines an abandoned restart record while preserving its unmatched stage
when no wake lock exists. This does not authorize deleting the retained file.

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
`--if-generation` is a compare-and-swap against the generation from
`amq wake check`: schema 1 reports `wake_generation` and `wake_target_digest`
(omitted when empty); schema 2 reports `wake.generation` and
`wake.target_digest` (JSON null when absent). A replacement published after
that check is refused and preserved.

`--takeover` is an explicit take by another surface of the same user, such as
a desktop app retiring a wake that a CLI started. The caller still passes its
own `--inject-via`; retire adopts the saved target's injector identity and
retry policy instead. It requires `--if-generation`. The generation, lock,
process-identity and owner-bound checks still apply, so a replaced or
owner-bearing wake is refused.

Darwin does not signal by numeric PID. A process that appears between the last
identity recheck and TERM/KILL is never signaled; raw numeric signaling is
`operator_only`. Live inject-via retirement uses the cooperative control
socket. Raw wakes are stopped from the owning terminal or supervisor.

Retirement preserves mailbox contents. Exact lock removal is its commit point:
a failure before that point is `refused`; a later target or state cleanup
failure is `retired_with_residue`, an exit-0 success with a warning. The other
successful result is `retired`. A wake that removes its own lock while exiting
from the retire's SIGTERM is `retired` once that exact process is gone; a lock
that vanishes while the process stays alive is `refused`. A replacement
generation is never selected for cleanup.

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
