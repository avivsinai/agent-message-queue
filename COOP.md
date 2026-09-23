# Co-op mode

Co-op mode lets agents work in parallel and coordinate through AMQ. AMQ is the
messaging layer; the initiator or external orchestrator owns the task plan.

## Swarm vs co-op

- **Co-op** is peer-to-peer messaging between agents in one AMQ session.
- **Swarm** bridges Claude Code Agent Teams task notifications into AMQ.
- **Two-host work** uses the signed `amq-bridge` companion. A same-machine
  co-op root is not a cross-host transport.

Swarm notifications do not provide direct external-to-teammate DMs. External
messages go to the team leader's AMQ inbox, and the leader forwards them through
the team mechanism.

## Co-op workflow

The [README getting started guide](README.md#getting-started) owns installation,
`amq setup`, and `amq launch`. This guide starts after launch. The `commands`
backend prints one complete `coop exec` command per configured agent and exits
with code `6` until an operator starts those commands. Managed launchers create
their panes or windows themselves.

### Running co-op mode

When the `commands` backend is selected, paste every emitted command into its
own terminal exactly as printed. The command carries the session, launch nonce,
execution ticket, and provider command from `.amq/launch.json`; do not rebuild it
from an example.

Start both agents before sending a first message. A newly started wake can
baseline messages already waiting, leaving them unread without notifying. If a
message was sent first, run `amq drain --include-body` in the target agent.

Disable wake when the environment has no usable terminal:

```sh
amq coop exec --no-wake claude
```

### Multiple pairs (isolated sessions)

Create each named session before launching it:

```sh
amq session create auth
amq launch --session auth

amq session create api
amq launch --session api
```

Each session has isolated inboxes and threads. Relative roots resolve separately
in separate Git worktrees. To share a mailbox intentionally, use the same
absolute `.amqrc` root or configure one absolute `AMQ_GLOBAL_ROOT`; remove or
replace a conflicting relative project `.amqrc` before relying on the global
root. Do not rely on a parent worktree's queue being discovered. Use
`amq doctor --ops` when a delivery receipt times out and the sessions may have
diverged.

Participating commands refuse a conflicting session pin. Prefer named routing:

```sh
amq list --session auth --new
amq drain --session auth --include-body
```

Intentional raw-root access can use an explicit root. Add
`--ignore-session-pin` only when deliberately overriding a conflicting active
pin. See [session routing and safety](docs/session-routing.md).

### Low-level provisioning

`amq coop init` provisions a queue for scripts and direct flows. Direct
`coop exec` remains available for deliberate launches and legacy automation; it
is not a second project-onboarding flow. Without an eligible root, it can create
`.amqrc` and `.agent-mail` in the worktree; `--no-init` turns that into an error.

Provider arguments follow `--`. Permission-bypass arguments are operator-only
and are rejected from committed `.amq/launch.json`; use the committed safe
provider settings or consult `amq coop exec --help` before using a bypass.

For scripts without `exec`:

```sh
amq coop init
amq_context="$(amq env --me claude)" && eval "$amq_context"
```

`amq env` replaces the complete shell context. Route explicitly with
`--session` or `--project` when the current pin is not the intended queue.

## External orchestrators

Integrations use the same queue primitives. If an orchestrator needs a global
root for spawned agents:

```sh
export AMQ_GLOBAL_ROOT="$HOME/.agent-mail"
amq integration symphony init --me codex
amq integration kanban bridge --me codex
amq doctor --ops
```

See the public launch contract for typed launch requests and placement values:
[docs/launch-api.md](docs/launch-api.md).

### Notify-hook fallback

`amq wake` may be unavailable on hardened Linux systems and is unavailable
natively on Windows. Use an explicit desktop notify hook when terminal input
injection is not available:

```toml
notify = ["python3", "/path/to/repo/scripts/codex-amq-notify.py"]
```

## Roles and phase flow

The initiator starts the task, owns decisions, and receives updates. A leader
may coordinate phases; workers execute assigned work. The collaboration phases
are research, design, code, review, and test. Split code ownership by file or
module to avoid concurrent edits.

At work start, send a `kind=status` message with an ETA to the initiator. Send
updates at phase changes, preserve the existing thread, and finish by replying
with the changes and verification. Ask decisions from the initiator.

## CLI commands

The concise command, flag, message-kind, and priority reference is
[docs/cli.md](docs/cli.md). The examples below cover the co-op loop.

### Send and receive

```sh
amq send --to codex --subject "Review: parser" --kind review_request --body "..."
amq drain --include-body
amq list --new
amq reply --id "<message-id>" --kind review_response --body "LGTM with comments"
```

`list` peeks without consuming. `drain` moves messages to `cur` and emits
receipts. Use `send --wait-for drained` when delivery consumption proof matters.
Replace `<message-id>` with the ID returned by `list` or `drain`. When a live
wake is already running, drain after its notification rather than blocking in a
second consumer.

Use `watch` only when no live wake is delivering notifications:

```sh
amq watch --timeout 60s
```

## Remote endpoint

A remote endpoint is optional. It does not replace co-op messaging.

1. Install `amq-remote` from its release asset. The Homebrew `amq` formula does not include it. See [INSTALL.md](INSTALL.md).
2. Declare harness targets in `<AM_ROOT>/extensions/remote/manifest.json`. Kinds are `claude`, `codex`, `amit`, and `fake`.
3. Run `amq-remote up --root <absolute-root>`. That supervises `serve`. One `up` owns a root; a second `up` for the same root refuses.
4. Use `submit`, `status`, `wait`, and `cancel` against that endpoint. `doctor` reports whether the endpoint is reachable.
5. `share --session <id>` mints the session body key the owner signs. It does not start the endpoint.
6. To reach a target from Buzz, add a `relay` object (manifest `schema_version` 2) with one share per target, pinned to its `native_session_id`. Buzz DM content is readable by the relay operator.
7. `doctor` lists each broken boundary under `failing`, with a remedy, and exits 6 while any remains.

Flags and exit codes: [amq-remote](cmd/amq-remote/README.md). The design is [the remote-control ADR](docs/adr-remote-control.md). Pinned harness seams are [the compatibility manifest](docs/remote-compat.md).

## Wake command (optional)

Co-op works without wake. Before replacing or repairing one, inspect it:

```sh
amq wake check --me codex --json
```

Read [Wake operations](docs/wake-operations.md) before any wake mutation.

For zero synthetic terminal input, choose one of these alternatives:

```sh
# Alternative A: start a notify-only wake yourself.
amq wake --me claude --inject-mode none --bell &

# Alternative B: let coop exec start and check its managed wake.
amq coop exec --require-wake --wake-inject-mode none claude
```

`none` never writes terminal input. Other input-injecting modes can activate a
permission or approval dialog; input deferral reduces collisions but cannot
detect an idle dialog. Use `none` when zero synthetic input is required.
Removing Enter or sanitizing payload text does not prevent single-key approval
shortcuts. A partially typed prompt already consumed by the app can still be
submitted after the quiet window. Urgent interrupts bypass input deferral;
when input remains active through `--input-max-hold`, wake emits an out-of-band
notice instead of injecting. Unavailable input sampling remains best-effort.

### Supervisor recipes

AMQ remains daemon-free. Let the operating system supervise the CLI process.
Keep notification and consumption separate: `wake` notifies; `monitor` drains
and emits receipts.

For systemd, use separate services and an absolute root:

```ini
[Service]
Environment=AM_ROOT=/absolute/path/to/shared/.agent-mail/collab
ExecStart=/usr/local/bin/amq wake --me claude --inject-mode none --bell
Restart=always
RestartSec=2
```

The consumer uses the same environment with:

```ini
ExecStart=/usr/local/bin/amq monitor --me claude --timeout 0 --include-body --json
```

For launchd, put equivalent absolute arguments in `ProgramArguments`, enable
`KeepAlive`, and use separate plists for `wake` and `monitor`:

```xml
<key>ProgramArguments</key>
<array>
  <string>/usr/local/bin/amq</string>
  <string>wake</string>
  <string>--root</string><string>/absolute/path/to/shared/.agent-mail/collab</string>
  <string>--me</string><string>claude</string>
  <string>--inject-mode</string><string>none</string>
</array>
<key>KeepAlive</key><true/>
<key>ThrottleInterval</key><integer>2</integer>
```

For the consumer plist, replace the command arguments after the executable with
`monitor --root <absolute-root> --me claude --timeout 0 --include-body --json`.
Route output to supervisor-managed logs and secure the unit for the mailbox
owner.

On macOS, `amq-keepalive` reattaches wake delivery to registered Ghostty or cmux
targets through a user LaunchAgent. It does not parse mailbox or wake state;
the core `amq` CLI owns those operations. See
[docs/amq-keepalive.md](docs/amq-keepalive.md).

```sh
go build ./cmd/amq-keepalive
./amq-keepalive attach --adapter ghostty --me claude
./amq-keepalive install-launchd
./amq-keepalive doctor
```

Native Windows supports core queue commands and direct keepalive injection, not
`coop exec`, `wake`, or terminal supervision. Use WSL for the complete workflow.
Linux raw TTY injection can be disabled by kernel hardening; see the
[platform capability matrix](INSTALL.md#platform-capability-matrix).

## Message format and spec workflow

The message body and header schema are documented in
[skills/amq-cli/references/message-format.md](skills/amq-cli/references/message-format.md).
When `--priority` is omitted, `--kind status` defaults to `low`; other kinds
default to `normal`.
The `amq-spec` skill owns the collaborative specification workflow:
Research -> Discuss -> Draft -> Review -> Present -> Execute. Use ordinary AMQ
messages and preserve the `spec/<topic>` thread.

## Troubleshooting

Inspect routing, unread messages, and wake state before changing anything:

```sh
amq doctor --ops
amq list --new --json
amq wake check --json
```

Follow [wake operations](docs/wake-operations.md) for recovery.
