# AMQ keepalive

`amq-keepalive` keeps an AMQ wake bound to the terminal adapter target that a
user explicitly registered. It is built, tested, and released from this AMQ
repository; there is no second source repository to update.

The helper stores only its private registry under `~/.amq-keepalive/`. It does
not parse AMQ mailbox, lock, presence, or target files. Wake state is created,
checked, and retired through the public `amq` CLI.

## Build and version

`make build` creates `./amq` and `./amq-keepalive` with the same release stamp.
The release publishes a separate keepalive archive under the same AMQ tag.

```sh
make build
./amq-keepalive -v
./amq-keepalive --version
./amq-keepalive version
```

A plain `go build ./cmd/amq-keepalive` falls back to Go build metadata and then
to `dev` when no version stamp is available.

## Adapters and targets

The supported target forms are:

- `cmux:surface:<uuid>` for an exact cmux surface UUID;
- `ghostty:terminal:<id>` for an exact Ghostty terminal id;
- `codex-queue:thread:<uuid>` for a live Codex GUI/TUI thread with an active writer (`codex queue --thread --message`);
- `claude-print:session:<uuid>` for an existing Claude Code session (`claude -p --resume` stream-json; submitted when the child echoes this inject's stdin with `isReplay` after `system/init`);
- `file` targets for deterministic development and tests.

cmux short references such as `surface:2` are rejected because they can drift.
cmux UUIDs are canonicalized before registration, and the adapter fails closed
when a surface UUID is missing, not `type==terminal`, or physically ambiguous.
cmux 0.64.3 `system.tree` omits tty for most surfaces; identity is the surface
UUID; tty corroborates when present. Ghostty uses its native AppleScript
interface and does not use titles, focus stealing, System Events, or the
clipboard.

The cmux CLI is resolved from `AMQ_KEEPALIVE_CMUX`,
`CMUX_BUNDLED_CLI_PATH`, `PATH`, or the standard application bundle locations.

Operator live discover probes skip unless the env is `1`. Run them from a
shell inside the matching surface:

```sh
AMQ_CMUX_LIVE=1 go test ./internal/keepalive/adapter -run TestCmuxLiveDiscoverProbe -count=1 -v
AMQ_GHOSTTY_LIVE=1 go test ./internal/keepalive/adapter -run TestGhosttyLiveDiscoverProbe -count=1 -v
AMQ_CLAUDE_LIVE=1 go test ./internal/keepalive/adapter -run TestClaudePrintLiveResumeAck -count=1 -v
```

## Attach and reattach

Register without starting a wake when another owner, such as `amq coop exec`,
will create it:

```sh
amq-keepalive reattach --adapter cmux --no-start
```

Register and start or reuse the exact wake directly:

```sh
amq-keepalive reattach --adapter cmux
```

`attach` adds a registration. `reattach` replaces prior registrations for the
same AMQ root and agent only after adapter discovery, target validation, and
wake readiness succeed. A matching live wake is reused only when its injector
and fixed target arguments match. A different live target, physical ownership
collision, ambiguous target, timeout, or pre-readiness exit leaves the old
registry entry unchanged.

The common flags are `--adapter`, `--target`, `--root`, `--base-root`,
`--session`, `--me`, `--amq`, `--self`, `--wake-ready-timeout`, and
`--no-start`. Omitted AMQ identity fields are resolved with `amq env --json`.

For a recreated terminal, `--retire-detached` may retire one previous wake only
after the saved adapter target is independently proven absent and AMQ rechecks
the exact process, injector, adapter, and target identity. The operation is
bounded and retries exact-target startup once; it never retargets a live wake.

## Supervisor and diagnostics

Run one reconciliation pass:

```sh
amq-keepalive supervise --once
```

Or run a foreground loop, normally under the supplied user LaunchAgent:

```sh
amq-keepalive supervise --interval 1m
amq-keepalive install-launchd
```

The supervisor probes each saved adapter target, starts or verifies its exact
wake, and records transition state in the registry. It emits a transition-only
warning with the root, agent, adapter, target, failure count, and concrete AMQ
error. Repeated passes in the same backoff do not spam stderr.

`amq-keepalive doctor` prints the registry as JSON. `gc` is dry-run by default:

```sh
amq-keepalive doctor
amq-keepalive gc --min-detached-age 24h
amq-keepalive gc --min-detached-age 24h --apply
```

`gc --apply` retires only entries that have been marked detached for the
minimum age and whose target is again proven absent. AMQ must confirm the exact
wake retirement before the registry row is forgotten.

### Self-upgrade

The continuous supervisor watches the direct executable path named by its
launch arguments. After each completed supervisor pass, and before the next
interval wait, it reads that path's embedded Go build metadata for a strictly
newer semantic version without executing the candidate. A candidate must also
have different bytes from the running image and must pass the same ownership,
mode, identity, and hash checks used by wake self-upgrade. A truncated, non-Go,
or otherwise unknown image defers the upgrade.

On Darwin, the private `0700` PID-scoped stage is re-verified immediately
before pathname exec because Darwin has no `fexecve`; same-UID races in that
narrow window are outside AMQ's threat model, as for wake self-upgrade.

The `register` and `attach` commands keep `executablePath()` as their default
injector path. Only `supervise` derives its default self-upgrade locator from
the launch `argv[0]` or `PATH`, because launchd and direct invocations name the
stable file that a sibling-and-rename install replaces.

When the candidate is accepted, keepalive calls `execve` in place. The process
PID, command arguments, and environment are preserved. The new image re-reads
the registry and reinitializes supervisor state; open descriptors remain open
only when they are not marked close-on-exec. Registry reconciliation must
finish before this check; an uncertain pass or candidate identity defers the
upgrade.

AMQ does not write the executable or retain the previous image, so it cannot
roll back a successful replacement. Recovery from a bad installed image is an
operator or package-manager action, such as reinstalling or selecting a known
good package version.

Embedded version reads that fail or produce unknown metadata defer and are
retried after a later pass. Exec failures fail closed and are recorded in the
private `.selfupgrade.json` sidecar next to the registry; each exec-attempted
candidate is refused at most once per generation. A new process generation
starts with an empty refusal set. The sidecar is mode `0600`; a corrupt or
unsafe sidecar disables self-upgrade for that process. Use
`supervise --no-self-upgrade` to opt out.

### Detached wake stderr protocol

The short-lived launcher cannot leave the wake writing to a pipe whose reader
has exited. Before starting AMQ, keepalive therefore starts a detached copy of
itself identified in process arguments as `__wake-stderr-drain` and by the
private environment marker `AMQ_KEEPALIVE_INTERNAL_STDERR_DRAIN_V1=1`.
Descriptor 3 is the wake stderr pipe and descriptor 4 is a private capture file.

The helper drains through EOF, stores at most 16 KiB of wake stderr, and records
up to 4 KiB of its own diagnostic output. Both temporary files are mode `0600`
and are unlinked when the launcher finishes inspecting the pre-readiness result.
If that best-effort unlink fails, the readiness-directory scavenger removes the
`wake-*` residue after 24 hours.
Failure to create the diagnostic channel aborts before AMQ starts. Keepalive
cannot promise actionable readiness failures without a working bounded stderr
path, so this setup is fail-closed rather than launching an undiagnosable wake.
A pre-readiness child exit receives a short bounded grace period so its
concrete stderr is reported instead of a generic readiness timeout.

The capture channel is pre-readiness diagnostics only. If its detached reader
unexpectedly exits after readiness, the established wake ignores `SIGPIPE` so
a later diagnostic write returns `EPIPE` instead of terminating the notifier.
The wake's private `.wake.log` remains the canonical full runtime diagnostic.

Keepalive removes any ambient `AMQ_WAKE_OWNER` when it creates a managed wake.
The resulting wake is intentionally ownerless and may outlive the short
launcher. An owner-bound wake is never silently taken over.

## Injection contract

AMQ starts the wake with this binary as `--inject-via` and appends the payload
as the final argument. The stable adapter entry point is:

```text
amq-keepalive inject <adapter> <target> <payload>
```

cmux uses JSON RPC and sends the text and Enter as separate calls. Message text
is not interpreted by a shell or by cmux's friendly CLI unescaper.

## SessionStart hook

Install the supported wrapper into Claude Code, Codex, or both:

```sh
amq-keepalive install-hook --agent both
amq-keepalive install-hook --agent both --dry-run
```

The installer writes
`~/.amq-keepalive/hooks/amq-keepalive-session-start.sh`, backs up a config
before modifying it, and installs an idempotent SessionStart registration. The
wrapper selects cmux when `CMUX_SURFACE_ID` is present and otherwise selects
Ghostty. It bounds discovery and reattach, logs failures to
`~/.amq-keepalive/session-start.log`, and returns `{}` so an adapter failure does
not prevent the agent host from starting.

Useful overrides include `AMQ_KEEPALIVE_BIN`, `AMQ_KEEPALIVE_ADAPTER`,
`AMQ_KEEPALIVE_TARGET`, `AMQ_KEEPALIVE_CMUX`, `AMQ_KEEPALIVE_REGISTRY`,
`AMQ_KEEPALIVE_AMQ`, `AMQ_KEEPALIVE_ROOT`, `AMQ_KEEPALIVE_SESSION`,
`AMQ_KEEPALIVE_ME`, `AMQ_KEEPALIVE_TIMEOUT_SECONDS`, and
`AMQ_KEEPALIVE_WAKE_TIMEOUT_MILLISECONDS`.

For a launcher that will immediately run `amq coop exec --require-wake`, use
`reattach --no-start`. Starting a wake during reattach would create an
ownerless process that the subsequent owner-bound co-op launch must refuse.
For a brand-new AMQ root, initialize the root before registering the wake:

```sh
amq init --root "$session_root" --agents claude,codex,user
amq-keepalive reattach --adapter cmux --no-start --root "$session_root" --me codex
```

## Retirement and recovery

Retire a deleted cmux workspace only after all of its registered surfaces are
gone:

```sh
amq-keepalive retire-session \
  --root "$HOME/.agent-mail/example" \
  --adapter cmux \
  --agents codex,claude
```

The command requires exactly one registry entry per requested agent, proves
every target absent before mutating any wake, and asks `amq wake retire` to
revalidate the exact live process and injector identity. Only confirmed retired
or already-absent wakes are forgotten. Mailboxes and the AMQ session root are
preserved.

Owner recovery is deliberately operator-driven. If retirement reports an
owner-bound claim, first verify that the recorded owner process is dead, then
run `amq wake recover-owner` for that exact AMQ root and agent and retry the
keepalive retirement. Keepalive does not have enough owner context to perform
that transition safely on its own.

The supervisor also deliberately does not call `amq wake repair` before
target-aware startup. The registry target is authoritative; repairing first
could resurrect an obsolete saved adapter target.

`forget --id <registry-id>` removes only the selected registry row and does not
retire its wake. Use it only after independently resolving the wake lifecycle.

## LaunchAgent removal

```sh
amq-keepalive uninstall
```

`install-launchd` and `uninstall` refuse to overwrite or remove an unrelated
plist. Custom registry parent permissions are preserved; files created by the
tool remain private.

## Safety boundaries

- Keepalive never launches or resurrects terminal sessions.
- A workspace id, surface UUID, or TTY alone is insufficient when physical
  identity is ambiguous.
- A live differing target is not retargeted automatically.
- Missing or ambiguous evidence fails closed and preserves the registry row.
- Reboot survival comes from reattaching the recreated session, not from
  assuming terminal identifiers survive a restart.

Changes to keepalive, AMQ wake ownership, launchers, cmux targeting, or TTY
identity must follow the production cmux component matrix and live Gates 1–5 in
`ohade/playground/amq-coop-setup/PRODUCTION-CMUX-ACCEPTANCE.md`.
