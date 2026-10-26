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

Each agent runs under its handle from `.amq/launch.json`. To name an agent
apart from its harness, configure it with `amq setup --agents reviewer=claude`;
for a direct run, use `amq coop exec --me reviewer claude`.

Start both agents before sending a first message. A newly started wake can
baseline messages already waiting, leaving them unread without notifying. If a
message was sent first, run `amq drain --include-body` in the target agent.

Disable wake when the environment has no usable terminal:

```sh
amq coop exec --no-wake claude
```

### Claude Code hooks

```sh
amq coop install-hooks
```

This adds two hooks to `~/.claude/settings.json`:

- **SessionStart.** On startup, resume, `/clear`, and compaction it gives the
  agent a short preamble: its handle, session and peers, its unread count, and
  whether a wake rings the terminal (so it must not run `watch`, `monitor`, or
  a polling loop).
- **Stop.** It blocks a stop once for unread mail it has not reported yet, so
  the agent drains before it ends its turn. Under a live wake it counts only
  mail that the wake will not ring for (see
  [Wake operations](docs/wake-operations.md)). After 5 blocks in one chain it
  allows the stop with a notice.

The hooks run `amq` only in sessions with `AM_ME` set, which `amq coop exec`
and `amq launch` do. A plain `claude` without `AM_ME` gets no preamble and no
Stop guard, and other Claude Code sessions start no `amq`. Running the command
again replaces the entries in place; `amq coop uninstall-hooks` removes them.
AMQ no longer ships the old project hook scripts `scripts/claude-session-start.sh`,
`scripts/amq-stop-hook.sh` and `scripts/claude-amq-user-prompt-submit.py`.
Remove project hook entries that run them; the install command and
`amq doctor` list such entries.

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
`coop exec` remains available for deliberate launches and scripts; it
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

Codex runs it after each turn. It reads the mailbox named by `AM_ROOT` and
`AM_ME`, which `amq coop exec codex` sets, and does nothing without them. It
raises one desktop notification for a set of unread messages, and another
only when that set changes.

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

A remote endpoint is optional and does not replace co-op messaging. The
[amq-remote companion](cmd/amq-remote/README.md) attaches to a running harness
session; its manifest, commands, flags, and exit codes are in that README.

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
detect an idle dialog. For Claude Code, `amq wake install-dialog-hook` holds
the doorbell while AskUserQuestion, ExitPlanMode, or a permission dialog is open;
see [Dialog guard](docs/wake-operations.md#dialog-guard-claude-code) for the
dialogs it cannot see. Use `none` when zero synthetic input is required.
Removing Enter or sanitizing payload text does not prevent single-key approval
shortcuts. A partially typed prompt already consumed by the app can still be
submitted after the quiet window. Urgent interrupts bypass input deferral;
when input remains active through `--input-max-hold`, wake emits an out-of-band
notice instead of injecting. Unavailable input sampling remains best-effort.

agy runs no hooks with Google Cloud sign-in, so `amq coop exec agy` runs agy
on a pseudo-terminal and stays as its terminal. It turns on agy's terminal
status (`AGY_CLI_OSC7501=on`, `AGY_CLI_OSC7501_MESSAGES=brief`) and holds the
doorbell while agy reports a permission or sign-in prompt or a running turn.
The doorbell rings once, within 2 seconds after agy reports the turn done or
idle. When you set `AGY_CLI_OSC7501` to anything but `on`, agy runs as
before: a doorbell can answer its permission prompt, and doorbells typed
during a turn can use up the attempt budget.

For Cursor, `amq wake install-dialog-hook --harness cursor` holds the
doorbell from each tool call of an `amq coop exec cursor-agent` seat to the
end of that turn, so a doorbell never answers "Run this command?" (Enter
selects "Run (once)"). Cursor reads hooks only when a session starts, and it
runs no hook for prompts it shows before any tool call: ask-question, mode
switch, web search, and image approval. A doorbell can answer those; see
[Dialog guard](docs/wake-operations.md#dialog-guard-cursor-approvals). Grok
has no hook for its approval prompt, so `amq coop exec grok` keeps the full
doorbell, and a doorbell that arrives while Grok asks for approval can answer
it. That is a known limit.

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

Platform limits for `coop exec`, `wake`, and terminal supervision are in the
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
