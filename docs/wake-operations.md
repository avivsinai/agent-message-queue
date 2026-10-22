# Wake operations

This reference defines the operator-facing contracts for `wake check`,
`repair`, `recover-owner`, `retire`, self-upgrade, and doctor wake repairs. For
durable file and crash invariants, see
[Wake state invariants](wake-state-invariants.md) and the
[wake lifecycle document](wake-lifecycle.md).

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

`wake check` and `doctor` JSON has two schemas. Schema 1 is the default and is
byte-compatible for a missing lock; a live or stale lock adds
`wake_generation` and `wake_target_digest` when they exist (omitted when
empty). Schema 2 is explicit with `--json-schema=2` (which requires `--json`).
It gives a closed action kind, actor, reason code, and an argv command object
for a directly executable action or a safe operator diagnostic. Missing
evidence is JSON `null`, including `wake.generation` and `wake.target_digest`;
`image.status="unknown"` is a real classification, not an error. In doctor
schema 2, each wake-lock entry carries the same decision under `wake_check`.

`doorbell_ready` (schema 1, present only when true) and `wake.doorbell_ready`
(schema 2) report that the live wake still types a doorbell for new mail. It
reads the wake's current state, not the lock's start mode: the lock mode and
the mode the wake records in presence are `raw` or `paste`, no notifier status
is recorded (an output-only demotion, attention-only fallback, or input
recovery records one), and the doorbell is not parked. Unreadable or missing
state reports false. Only `doorbell_ready` proves that pending mail rings;
`live_wake` alone does not.

The wake child appends full runtime diagnostics to the private
`agents/<agent>/.wake.log`; the controlling terminal receives only
terminal-safe attention output, so fatal-error text stays out of full-screen
agent composers. A new wake truncates the log when it has reached 1 MiB, and
the 30-second maintenance tick checks again, so a long-lived wake bounds its
own log. The keepalive launcher's detached variant is in
[the keepalive reference](amq-keepalive.md#detached-wake-diagnostics).

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
  first doorbell becomes eligible. Draining all pending mail before then
  cancels it. A drain can take held mail, but respects its message limit
  (default 20, filename order); urgent hold bypass does not change drain order.
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

## Dialog guard (Claude Code)

Doorbell text and its Enter answer whatever has focus. When Claude Code shows
AskUserQuestion, ExitPlanMode, or a permission dialog, injected input picks an
option the person did not choose. The terminal gives no signal that tells an
open dialog from an idle prompt, so the guard uses Claude Code hooks:

```bash
amq wake install-dialog-hook
```

The installer adds marked command hooks (`AMQ_DIALOG_HOOK=1`) to the user
settings file `~/.claude/settings.json` and leaves other content as it is.
Running it again changes nothing; `amq wake uninstall-dialog-hook` removes the
entries. Running sessions pick up the change through the settings watcher; if
`/hooks` does not list the entries, restart the session. `amq doctor` warns
when `~/.claude` exists and the hook is missing or runs an amq binary that is
gone.

- **Marker.** The hooks run `amq wake dialog open|close`, which writes or
  removes `<root>/agents/<me>/.wake.dialog`. `open` runs on `PreToolUse` for
  AskUserQuestion and ExitPlanMode, and on every `PermissionRequest`. `close`
  runs on `PostToolUse` for the same two tools (only for the tool call that
  opened the marker), and on `UserPromptSubmit`, `Stop`, `SessionStart`, and
  `SessionEnd`. The receiver exits 0 once its arguments parse and prints
  nothing, so it never decides a tool call or a permission.
- **Identity.** The receiver finds the mailbox through `AM_ME` and `AM_ROOT`
  in the Claude Code environment, which `amq coop exec` and `amq launch` set.
  A Claude Code process started without them gets no marker, and its wake
  behaves as it does without the hook.
- **Hold.** Before each doorbell, reminder, or `--inject-cmd`/`--inject-via`
  delivery, the wake reads the marker. While it exists, the wake writes no
  terminal input, checks again every 2 seconds, and rings the full doorbell
  once the marker is gone. The message stays in `inbox/new`. A hold is not a
  delivery attempt: it does not use the reminder budget or write the
  notification ledger. Every terminal write checks the marker again, so a
  dialog that opens during a doorbell stops its remaining bytes; the held
  Enter is sent after the dialog closes. Urgent and interrupt mail is held the
  same way.
- **Whole-turn hold after a permission dialog.** Permission hook input has no
  tool call id, so only `Stop` or `UserPromptSubmit` clears a permission
  marker. A doorbell that would land mid-turn after a permission dialog lands
  when the turn ends or the person types.
- **Esc and stale markers.** Esc on a dialog is a user interrupt, and Claude
  Code runs no `Stop` hook for it. A denied permission is the same. The marker
  then stays until the person's next prompt. A marker older than 10 minutes,
  dated in the future, or one the wake cannot read, still blocks input. When
  it blocks a delivery attempt, the wake adds attention output (title, bell,
  desktop notice) on the attention cadence instead of waiting silently.

The guard does not cover dialogs without a hook: the workspace trust dialog,
MCP elicitation, slash-command pickers, the `/resume` and `/model` menus, the
rewind selector, and login or update prompts. It also cannot recall bytes the
terminal already holds when a dialog opens. Use `--inject-mode none` when AMQ
must write no terminal input at all.

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
  `interrupt_cooldown`, `inject_timeout`, and `self_upgrade`. Each has a flag
  of the same name with `-` in place of `_`.
- `self_upgrade` (default `true`) is the self-upgrade kill switch. `false`
  stops new self-upgrade candidates at the next maintenance tick (every 30
  seconds) without a restart; a restart request already published finishes.
  `true` does not enable a wake started with `--no-self-upgrade` or
  `AMQ_WAKE_NO_SELF_UPGRADE=1`: the wake stays disabled and its self-upgrade
  decision says to restart it. `wake check --json --json-schema=2` shows the
  decision under `self_upgrade`.

The settings file is `<root>/agents/<handle>/.wake.settings`, canonical JSON:

```json
{"schema":1,"settings":{"hold_normal":"5m0s","hold_low":"30m0s"}}
```

A key that is absent has its default. Durations are Go duration strings. The
file must be mode 0600 and owned by the current user, and the wake refuses a
file that is not, one with an unknown key or a newer schema, or one with a
value that fails validation. A refused file never changes the running settings.
Use `amq wake config` to edit it. For a hand edit, keep mode 0600.

**Precedence.** Effective settings are the defaults overlaid by the machine
file, then by the agent file.
Explicit settings flags on a fresh `amq wake` write their keys into the file,
including a value equal to the default. A resume (self-upgrade or
`amq wake restart`) ignores settings flags in argv when the file exists, so a
later `wake config` change is not undone by the original command line. When the
file is absent, a resume seeds it once from the settings flags in its argv; a
resume never exits because of the file. Repair, `coop exec`, and keepalive pass
no settings flags and start fresh on the stored file. A start that loses the
lock, and a start that adopts an existing live wake, write nothing. A
fresh start with explicit flags refuses to start when the file is refused or
cannot be written. A fresh start without settings flags runs with the defaults,
logs the error, and records `refused`. A self-upgrade or restart candidate is
refused at preflight while the file is refused, and the refusal is remembered
for the generation: fix the file, then request `amq wake restart`.

**Machine layer.** `~/.amq/wake.settings` holds defaults for every wake of
this user on this machine. It has the same JSON shape and trust rules as the
agent file (`~/.amq` is a real directory owned by the user and not group-writable
or world-writable, the file is mode 0600, and neither is a symlink; a missing
file always means no machine layer, even when `~/.amq` fails these checks), and it never travels with a root. Per key, the effective value is
the first of: the agent file, the machine file, the built-in default. A key the
agent file sets, including a value equal to the default, pins that key against
the machine layer. Every running wake re-reads the machine file with the agent
file and applies a change within 2 seconds.

A refused machine file never blocks a start or an upgrade. A running wake keeps
its last good machine settings and reports `refused` once. While the file is
refused, `amq wake config` shows the values without the machine layer. A start or resume that
finds a refused machine file runs without the machine layer (agent file over
built-in defaults), because a start has no last good value. A self-upgrade
under a refused machine file therefore can change the effective settings.
Resume preflight does not read the machine file. If the machine layer is valid
alone but the merge with the agent file is not (the interrupt keys depend on
each other), that wake runs without the machine layer and the applied record
names the merge error.

**Apply timing.** The wake checks the file at the start of each inbox scan and
every 2 seconds. The 2-second check rescans only when a hold changes; every
other key applies at its next use, so a change is in force for the next message. An armed debounce
timer keeps its duration; the new debounce applies from the next inbox event.

```bash
amq wake config --me claude                       # show values, source, wake status
amq wake config --me claude --hold-low 10m --wait # set, then wait for the wake
amq wake config --me claude --unset hold_low      # return a key to its default
amq wake config --me claude --reset --hold-low 10m # replace the file with exactly these keys
amq wake config --machine --hold-normal 5m --hold-low 30m  # machine default
amq wake config --machine --unset hold_low        # back to the next layer
amq wake config --machine --reset --hold-low 10m  # replace the machine file
```

`--machine` edits `~/.amq/wake.settings` instead of an agent file. It needs no
`--me`, opens no mailbox, and creates `~/.amq` with mode 0700 when it is
missing. Edits take an exclusive lock, so two edits do not interleave. The
machine file must be valid on its own over the built-in defaults. `--unset`
removes a key so the next layer applies (for an agent key, the machine value or
default; for a machine key, the default). `--reset` with no flags means no
overrides: an empty agent file, or an empty machine file. With `--me`
resolved, `--machine` also shows that agent's wake status, and `--machine
--wait` waits on that one wake; without a resolved `--me`, `--wait` is a usage
error. There is no list of wakes, so a machine-scope edit does not check
whether other wakes report.

`wake config` shows each setting with its source (`default`, `machine`, or
`file`, where `file` is the agent file) and a `machine_file` block (path,
status, error) beside the agent `file` block. The wake status is: `applied`, `pending`, `refused: <error>`, `unreported`, or
`no running wake`. A refused file shows `file: refused: <error>` and the rows
show defaults, not the running wake's values; the running wake keeps its last
applied settings. The JSON status `unreported` (text: `running wake does not
report live settings`) means the wake runs an older image, its status write
failed, or a resumed wake is still storing its command-line settings in the file;
restart it to adopt live settings, or wait a moment if it just resumed. `show` and `set` never create the
agent directory: a missing mailbox exits 3.
Any setting flag or `--unset` validates the whole new set and writes it. On a
refused file, `set` and `--unset` exit 1 and point at `--reset`, which replaces
the file with exactly the given keys (all defaults when none are given). While
the file is absent and the wake stays `unreported` for the 5 s startup grace, `set`
and `--unset` exit 6: the wake may run command-line settings the file lacks. Restart it (a resume seeds the
file from its flags) or use `--reset` with the full set. A bad
value exits 2 and writes nothing. A restart-only flag exits 2 with the restart
instruction. `--wait` waits until the running wake records the new file digest
in `.wake.settings.applied`, and for the machine layer its digest as well (a
wake that records no machine fields and sees a machine file shows
`unreported`); `--timeout` bounds it (default 60s, `0` waits
forever), exit 4 on timeout, exit 1 when the wake refuses the file the command changed (a refusal of the other file, agent or machine, is a stderr warning and exit 0), exit 6
when the wake stays `unreported` for 5 s, and exit 0 at once when no wake runs. `--json` prints the same data. The wake records its
state in `.wake.settings.applied`; the file is not a receipt of message
consumption.

## Self-upgrade

A resume-eligible wake started by `coop exec` follows the stable AMQ launch
path (for example the Homebrew `bin/amq` symlink). When an installer points
that path at a strictly newer release image, the wake waits for a quiescent
delivery boundary and replaces its running image in place: the PID, terminal
ownership, and unread work stay. On Darwin, a running wake whose mapped
executable still has the recorded device and inode counts as the same image
after the package manager removes the old path. On Homebrew or Scoop installs,
`amq upgrade` delegates to the package manager.

- **Candidates.** The wake reads the candidate's version from the recorded
  `-X main.version` linker assignment and never runs the candidate to discover
  it. A build with any linker flag other than `-s`, `-w`, and
  `-X main.version`, a versioned `go install`, a `-trimpath` build, or an image
  whose build metadata cannot be read is deferred. An incumbent without a
  parsable version does not self-upgrade. On Darwin, the staged image must pass
  `/usr/bin/codesign --verify --strict`.
- **Preflights.** After the image is bound and verified, the wake runs it
  twice before the exec: `--version` must print the version read from the
  metadata, and a launch-contract preflight must confirm the exact wake argv
  and bootstrap. Each probe runs in its own process group with a timeout and
  is killed with that group. A descendant that calls `setsid` or `setpgid`
  leaves the group and escapes this cleanup.
- **Not eligible.** Pinned binaries, ownerless and keepalive wakes, repair
  flows, destructive interrupts, and `--inject-cmd` wakes never self-upgrade.
- **Disable.** Start the wake with `--no-self-upgrade` or
  `AMQ_WAKE_NO_SELF_UPGRADE=1`, or set the live `self_upgrade` setting to
  `false` (see [Live settings](#live-settings)).
- **Failure guard.** A failed candidate is tried at most once per wake
  generation, with memory of the 8 most recent candidates. Before the exec,
  the wake writes a private `.wake.selfupgrade.attempt` record; a later wake
  that reaches maintenance refuses the same image while that attempt is fresh.
  The guard cannot help when the new image dies before any wake reaches
  maintenance. A wake built without the guard ignores the record, so a mixed
  pair of old and new images has no crash-loop protection.
- **No rollback.** AMQ does not write or keep the executable. Recover from a
  bad image by reinstalling or selecting a known-good package version.

Schema-2 `wake check` and `doctor` JSON report the latest decision under
`self_upgrade`. The `amq-keepalive` supervisor has its own contract in
[the keepalive reference](amq-keepalive.md#self-upgrade-and-rollback).

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

## Injector identity

AMQ identifies an external injector (`--inject-via`/`--wake-inject-via`) by its
resolved executable path and its ordered fixed arguments
(`--inject-arg`/`--wake-inject-arg`), which `.wake.target` stores as
`inject_via` and `inject_args`. Put any provider target identity that
distinguishes a pane, window, or session in those fixed arguments. Environment
variables and external provider configuration are not part of the identity, so
repair, recovery, and retirement cannot detect a target change made only
through them: after repointing a provider that way, restart the wake.

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
