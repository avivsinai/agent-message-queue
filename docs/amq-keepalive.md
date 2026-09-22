# AMQ keepalive

`amq-keepalive` keeps an AMQ wake bound to a terminal adapter target that an
operator explicitly registers. It is a companion binary, not a daemon and not
part of the core `amq` process. Its private registry lives under
`~/.amq-keepalive/`; wake state is created and retired through the public AMQ
CLI.

## Build and version

`make build` creates `amq-keepalive` with the release version. Releases include
separate macOS and native Windows archives.

```sh
make build
./amq-keepalive --version
```

## Adapters and targets

Supported target forms include:

- `cmux:surface:<uuid>` for an exact cmux surface;
- `ghostty:terminal:<id>` for an exact Ghostty terminal;
- `codex-queue:thread:<uuid>` for a live Codex thread with an active writer;
- `claude-print:session:<uuid>` for an existing Claude Code session; and
- `file` targets for deterministic development and tests.

On native Windows, the supported contract is direct `inject` through
`codex-queue` or `claude-print`. `wake`, `coop exec`, `attach`, `reattach`, and
`supervise` remain outside the Windows contract. Use `inject` directly:

```powershell
amq-keepalive.exe inject codex-queue "codex-queue:thread:$env:CODEX_THREAD_ID" "check the AMQ inbox"
amq-keepalive.exe inject claude-print "claude-print:session:<uuid>" "check the AMQ inbox"
```

cmux short references such as `surface:2` are rejected because they can drift.
Targets are canonicalized and ambiguous or missing identity fails closed. The
cmux CLI is resolved from `AMQ_KEEPALIVE_CMUX`, `CMUX_BUNDLED_CLI_PATH`, `PATH`,
or standard application locations.

## Attach and reattach

Register without starting a wake when another owner will create it:

```sh
amq-keepalive reattach --adapter cmux --no-start
```

Register and start or reuse the exact wake directly:

```sh
amq-keepalive reattach --adapter cmux
```

`attach` adds a registration. `reattach` replaces registrations for the same
root and agent only after discovery, target validation, and wake readiness
succeed. A matching live wake is reused only when its injector and fixed target
arguments match. A differing, ambiguous, or unavailable target leaves the old
registration unchanged.

Common identity flags are `--adapter`, `--target`, `--root`, `--base-root`,
`--session`, `--me`, `--amq`, `--self`, `--wake-ready-timeout`, and `--no-start`.
Omitted AMQ identity fields are resolved with `amq env --json`.

`--retire-detached` may retire a previous wake only after its saved target is
proven absent and AMQ rechecks process, injector, adapter, and target identity.
It never retargets a live wake.

## Supervisor and diagnostics

Run one reconciliation pass or a foreground loop:

```sh
amq-keepalive supervise --once
amq-keepalive supervise --interval 1m
amq-keepalive install-launchd
```

The supervisor probes saved targets, starts or verifies exact wakes, and records
transition state. `doctor` prints the registry as JSON. `gc` is dry-run by
default; `gc --apply` forgets only entries detached for the requested age after
the target is proven absent and AMQ confirms exact wake retirement.

### Self-upgrade and rollback

The continuous supervisor checks the executable path named by its launch
arguments. It replaces the running image only with a strictly newer semantic
version that passes ownership, mode, identity, and hash checks. It does not
replace an equal or older image. The running process keeps its PID and command
arguments across `execve`.

Use release binaries for self-upgrade. Candidate discovery reads the recorded
`-X main.version` linker assignment without running the candidate. Versioned
`go install` and `-trimpath` builds do not provide the required candidate
metadata. Unknown metadata defers replacement. On macOS, the staged candidate
must also pass `/usr/bin/codesign --verify --strict`.

AMQ does not retain the previous image or roll back a successful replacement.
Recover a bad image by reinstalling or selecting a known-good package version.
An older rollback, an equal-version replacement, or a supervisor started with
`--no-self-upgrade` requires a service-manager restart. A corrupt or unsafe
upgrade state file disables self-upgrade and defers refresh.

Before replacement, keepalive records a bounded unsettled attempt. A fresh
matching unsettled attempt is refused until a healthy pass settles it. This
prevents immediate repeated replacement but cannot recover an image that dies
before its replacement process reaches maintenance.

### Detached wake diagnostics

The short-lived launcher uses a private detached stderr drain so it does not
leave a wake writing to a dead pipe. Pre-readiness capture is bounded to 16 KiB
of wake stderr and 4 KiB of helper diagnostics; temporary files are mode `0600`
and removed after readiness inspection. The wake's private
`agents/<agent>/.wake.log` remains the full runtime log. A failed diagnostic
channel aborts before AMQ starts rather than launching an undiagnosable wake.

Keepalive removes ambient `AMQ_WAKE_OWNER` when it creates a managed wake. The
resulting wake is intentionally ownerless. An owner-bound wake is never silently
taken over.

## Injection contract

AMQ starts the wake with this binary as `--inject-via` and appends the payload as
the final argument:

```text
amq-keepalive inject <adapter> <target> <payload>
```

Message text is not interpreted by a shell. cmux sends text and Enter as
separate JSON-RPC calls.

## SessionStart hook

Install the supported wrapper into Claude Code, Codex, or both:

```sh
amq-keepalive install-hook --agent both
amq-keepalive install-hook --agent both --dry-run
```

The installer backs up configuration before modifying it and installs an
idempotent SessionStart registration. The wrapper selects cmux when
`CMUX_SURFACE_ID` is present and otherwise selects Ghostty. Discovery and
reattach are bounded; failures are logged and do not prevent agent startup.
Read `~/.amq-keepalive/session-start.log` for hook failures.

Useful overrides include `AMQ_KEEPALIVE_BIN`, `AMQ_KEEPALIVE_ADAPTER`,
`AMQ_KEEPALIVE_TARGET`, `AMQ_KEEPALIVE_CMUX`, `AMQ_KEEPALIVE_REGISTRY`,
`AMQ_KEEPALIVE_AMQ`, `AMQ_KEEPALIVE_ROOT`, `AMQ_KEEPALIVE_SESSION`,
`AMQ_KEEPALIVE_ME`, `AMQ_KEEPALIVE_TIMEOUT_SECONDS`, and
`AMQ_KEEPALIVE_WAKE_TIMEOUT_MILLISECONDS`.

For a launcher that will immediately run `amq coop exec --require-wake`, use
`reattach --no-start`; the owner-bound launch must not encounter an ownerless
wake. Initialize a new AMQ root before registering a wake.

## Retirement and recovery

Retire a deleted cmux workspace only after all registered surfaces are gone:

```sh
amq-keepalive retire-session \
  --root "$HOME/.agent-mail/example" \
  --adapter cmux \
  --agents codex,claude
```

The command requires one registry entry per requested agent, proves every target
absent, and asks `amq wake retire` to revalidate process and injector identity.
Only confirmed retired or already-absent wakes are forgotten. Mailboxes and the
session root are preserved.

Owner recovery is operator-driven. If retirement reports an owner-bound claim,
verify that the owner process is dead, run `amq wake recover-owner` for that
exact root and agent, then retry retirement. Keepalive cannot safely perform
that transition itself.

The supervisor does not call `amq wake repair` before target-aware startup: the
registry target is authoritative, and repair could resurrect an obsolete target.
`forget --id <registry-id>` removes only a registry row; it does not retire its
wake.

## LaunchAgent removal

```sh
amq-keepalive uninstall
```

`install-launchd` and `uninstall` refuse to overwrite or remove an unrelated
plist. Custom registry parent permissions are preserved.

## Safety boundaries

- Keepalive never launches or resurrects terminal sessions.
- A workspace ID, surface UUID, or TTY alone is insufficient when identity is
  ambiguous.
- A live differing target is never retargeted automatically.
- Missing or ambiguous evidence fails closed and preserves the registry row.
- Reboot survival comes from reattaching a recreated session, not assuming
  terminal identifiers survive a restart.
