# Co-op Mode: Phased Parallel Work

Co-op mode enables multiple agents (e.g., Claude Code and Codex CLI) to work **in parallel where safe, coordinate where risky**, leveraging cognitive diversity (different models = different training = different blind spots) to catch errors that same-model review would miss.

AMQ is the communication layer in this setup, not the coordinator. The initiator, the pair, or an external orchestrator still owns the task plan; AMQ keeps the conversation, handoffs, and thread continuity intact.

## Swarm vs Co-op

- **Co-op**: lightweight, peer-to-peer messaging between agents via AMQ threads (`p2p/...`).
- **Swarm**: join Claude Code Agent Teams and coordinate via the shared task list (`amq swarm ...`).
- **Messaging**: swarm bridge delivers task notifications only. Claude Code teammates can `amq send` to external agents, but external agents cannot DM a specific Claude Code teammate directly. External -> team messages must go to the leader's AMQ inbox, then the leader drains and forwards via Claude Code internal messaging.

For swarm command reference, see [CLAUDE.md](CLAUDE.md).

## Co-op Workflow

Use the [README Getting started](README.md#getting-started) to install AMQ, configure
the project with `amq setup`, and run `amq launch`. `--launcher auto` (the
default) may select `tmux`, `cmux`, or `ghostty` and run the declared plan
in-app. The `commands` backend prints one complete `coop exec` command for
each configured agent and exits `6` until those commands are started. This
document begins at that co-op-specific step; it does not define a second
setup path.

### Running Co-op Mode

When launch uses the `commands` backend, paste each complete emitted command
into its own terminal. The emitted lines are the canonical execution surface:
they include the exact session, launch nonce, execution ticket, and provider
command declared in `.amq/launch.json`. Do not shorten or rebuild them from
an example. Managed `tmux`, `cmux`, and `ghostty` backends do not print those
lines; they create the panes or windows themselves.

Include Grok in the setup roster when it should join the session. AMQ starts
only adapters that pass their capability probe. For Cursor, `amq setup` uses
the current `agent` command when it is on `PATH`, and falls back to legacy
`cursor-agent` only when `agent` is absent.

To disable auto-wake (e.g., in CI or non-TTY environments):
```bash
amq coop exec --no-wake claude
```

### Low-level provisioning

`amq coop init` is the non-interactive provisioning primitive. Direct
`coop exec` provisioning also remains available for scripts and legacy flows,
but it is not the canonical onboarding path. With no eligible root, `coop exec`
creates `.amqrc` and `.agent-mail` at the worktree top; `--no-init` makes that
condition an error instead. Without `--session` or `--root`, it uses the
declared `default_session` from `.amq/launch.json`, or `collab` when none is
declared.

For a deliberate direct launch, provider flags follow `--`. Dangerous bypass
flags are operator-controlled here and are rejected from committed
`.amq/launch.json` arguments:

```bash
amq coop exec claude -- --dangerously-skip-permissions
amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox
amq coop exec grok
```

Creating a missing named session, explicit root, or declared default session
still works in this release and prints one deprecation warning. Use
`amq session create <name>` or `amq init --root` instead; the next major release
makes those missing-target cases exit `3`. The zero-configuration `collab`
bootstrap in a repo with neither `.amqrc` nor `.amq/launch.json` remains the
documented exception.

### Multiple Pairs (Isolated Sessions)

Create each named session explicitly, then launch it. When launch uses the
`commands` backend, run the emitted commands in separate terminals:

```bash
# Pair A: auth feature
amq session create auth
amq launch --session auth

# Pair B: api refactor
amq session create api
amq launch --session api
```

When launch uses the `commands` backend, paste the emitted commands into
separate terminals.

Each pair has isolated inboxes and threads. Messages stay within their root.
Equivalent explicit root form: `--root .agent-mail/<session>`.

That isolation also applies across git worktrees. A relative project root such
as `{"root":".agent-mail"}` resolves separately inside every worktree, even when
the session names match. To share a mailbox intentionally, configure the same
absolute `.amqrc` root in each worktree, or remove the relative project config
and set `AMQ_GLOBAL_ROOT` to one absolute base. Use `amq doctor --ops` when a
delivery receipt times out; it can name divergent same-session roots when a
peer has fresher presence in another worktree.

Participating commands fail closed when the active session pin conflicts with
the selected queue. For read-side access, prefer the named route:

```bash
amq list --session auth --new
amq drain --session auth --include-body
```

Use `--session <name>` for sibling routing. Deliberate raw-root access requires
an explicit root plus `--ignore-session-pin`. See
[Session routing and safety](docs/session-routing.md) for the complete guard,
doctor repair, fallback, and worktree contracts.

### For Scripts/CI

When you can't use `exec` (non-interactive environments):
```bash
amq coop init
amq_context="$(amq env --me claude)" && eval "$amq_context"
```

`amq env` replaces the full shell context. For a sessionless root it sets
`AM_BASE_ROOT` to that exact root and leaves `AM_SESSION` empty.

An initialized cwd-local queue is also a routing safety signal. If the terminal
is pinned to a different root, implicit participating commands refuse instead
of silently following the pin. Repin to the cwd-local queue, route deliberately
with `--session`/`--project`, or pass an explicit `--root` to confirm the active
queue; ordinary pin checks still apply.

## External Orchestrators

Co-op mode and orchestrator integrations use the same queue primitives. If you are wiring AMQ into Symphony or Cline Kanban, make the queue root discoverable globally so spawned agents land in the same mailbox tree:

```bash
export AMQ_GLOBAL_ROOT="$HOME/.agent-mail"
amq integration symphony init --me codex
amq integration kanban bridge --me codex
amq doctor --ops
```

Integration messages are self-delivered and carry `context.orchestrator` plus labels such as `orchestrator:*`, `task-state:*`, `handoff`, and `blocking`, so they fit naturally into the same `amq drain` / `amq reply` workflow used for co-op.

Orchestrators that compile public launch requests send `target.base_root` as the one profile child of the `.amqrc` root, `on_live: keep` only for a proven-live seat, mapped placement enums (`current-window` → `current_window`, `vertical` → `columns`, `horizontal` → `rows`), and bootstrap text through `initial_input`. The decoder accepts only those underscore enums. See [the public launch API](docs/launch-api.md).

### Fallback: Notify Hook (if wake unavailable)

`amq wake` uses TIOCSTI which may be unavailable on:
- Hardened Linux (CONFIG_LEGACY_TIOCSTI=n)
- Native Windows (`wake` is unavailable; use WSL with a Linux asset)

If wake fails, configure the notify hook for desktop notifications:

```toml
# ~/.codex/config.toml
notify = ["python3", "/path/to/repo/scripts/codex-amq-notify.py"]
```

### Plan Mode Prompt Hook (Claude)

When Claude is in plan mode, it cannot run shell tools directly. Use the
`UserPromptSubmit` hook to inject AMQ context before prompt processing:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "AMQ_PROMPT_HOOK_MODE=plan AMQ_PROMPT_HOOK_ACTION=list python3 $CLAUDE_PROJECT_DIR/scripts/claude-amq-user-prompt-submit.py"
          }
        ]
      }
    ]
  }
}
```

Set `AMQ_PROMPT_HOOK_ACTION=drain` to auto-drain on submit (instead of list/peek).

## Roles

- **Initiator** = whoever starts the task (agent or human). Owns decisions and receives all updates.
- **Leader/Coordinator** = coordinates phases, merges, and final decisions (often the initiator).
- **Worker** = executes assigned phases and reports back to the initiator.

**Default pairing note**: Claude is often faster and more decisive, while Codex tends to be deeper but slower. That commonly makes Claude a natural coordinator and Codex a strong worker. This is a default, not a rule — roles are set per task by the initiator. Grok CLI can join as an additional optional peer/worker (e.g. a third `amq coop exec grok` in a three-way session) without changing this default two-engine pairing note.

## Phased Flow

| Phase | Mode | Description |
|-------|------|-------------|
| **Research** | Parallel | Both explore codebase, read docs, search. No conflicts. |
| **Design** | Parallel -> Merge | Both propose approaches. Leader merges/decides. |
| **Code** | Split | Divide by file/module. Never edit same file. |
| **Review** | Parallel | Both review each other's code. Leader decides disputes. |
| **Test** | Parallel | Both run tests, report results to leader. |

## Core Principles

1. **Parallel where safe** - Research, design, review, and test phases run in parallel.
2. **Split where risky** - Code phase divides files/modules to avoid conflicts.
3. **Never branch** - Always work on same branch (joined work).
4. **Leader coordinates** - The initiator or designated leader handles phase transitions and final decisions.

## Initiator Rule

- The **initiator** is whoever started the task (agent or human).
- Always report progress and completion to the initiator.
- Ask questions only to the initiator. Do not ask a third party.

## Progress Protocol (Start / Heartbeat / Done)

- **Start**: send `kind=status` with an ETA to the initiator as soon as you begin.
- **Heartbeat**: update on phase boundaries or every 10-15 minutes.
- **Done**: send Summary / Changes / Tests / Notes to the initiator.
- **Blocked**: send `kind=question` to the initiator with options and a recommendation.

## Modes of Collaboration

Pick one mode per task; the initiator decides.

- **Leader + Worker**: leader decides, worker executes; best default.
- **Co-workers**: peers decide together; if no consensus, ask the initiator.
- **Duplicate**: independent solutions or reviews; initiator merges results.
- **Driver + Navigator**: driver codes, navigator reviews/tests and can interrupt.
- **Spec + Implementer**: one writes spec/tests, the other implements.
- **Reviewer + Implementer**: one codes, the other focuses on review and risk detection.

## CLI Commands

### Send

```bash
amq send --to codex --subject "Review: New parser" --kind review_request --body "..."
amq send --to codex --priority urgent --kind question --body "Blocked on API"
```

### Receive

```bash
amq drain --include-body                          # One-shot, silent when empty
amq watch --timeout 60s                           # Block until message arrives
amq list --new                                    # Peek without side effects
```

### Reply (Auto Thread/Refs)

```bash
amq reply --id "msg_123" --kind review_response --body "LGTM with minor suggestions..."
```

## Wake Command (Optional)

> Co-op works without wake. `coop exec` starts it automatically.

Before replacing or repairing a wake, inspect its exact capability:

```bash
amq wake check --me codex
amq wake check --me codex --json
```

This command never changes wake state. `restart_capability=agent_safe` is the
only result an automated agent may act on, using the returned `next_action`.
For `operator_only`, leave the live wake running and hand the action to its
owning terminal or supervisor. For `unavailable`, preserve the state and
diagnose it. A process without a controlling TTY must not kill or replace a
live raw wake, and TIOCSTI refusal must not be treated as permission to weaken
delivery to attention-only. The result includes both the running wake image and
the currently invoked AMQ image; legacy locks may report unknown image fields.
`amq doctor --ops` shares these fields for discovered locks.

`amq wake` uses TIOCSTI to inject notifications into your terminal by default.
Pass `--baseline-existing` when starting a new wake after the agent already
owns its terminal. Messages already present in `inbox/new` remain unread and do
not trigger that fresh wake; later arrivals do. `coop exec` and `wake repair`
add the flag to wakes they start. Reuse requires generation-bound proof that
the live wake completed watcher preparation. It does not retroactively baseline
that wake, so pending backlog can still notify; SessionStart draining mitigates
that residual.

For the first notification test, start both `coop exec` agents before sending
the message. If a message was already waiting when the target wake started, it
will not notify; it remains unread and visible to `amq drain --include-body`.

Wake treats one transport execution only as a delivery attempt. While the same
inbox cohort remains unread, it retries on its own capped backoff. The first
notification is immediate. Attempts that inject the fixed doorbell start at 5
seconds because they drive the agent; attention-only attempts start at 30
seconds because they alert a human. Input attempts double to a 2-minute cap;
attention-only attempts continue through 4 and 8 minutes to a 15-minute cap.
Retries never give up while the cohort remains unread. Contextual peer headers
appear only in terminal output or attention; terminal input always uses the
fixed doorbell. The delay starts after the prior injector process exits or times
out. Because an external injector is arbitrary local code, retries can duplicate
its side effects. Added messages join the pending cohort and share its next
notification without resetting the retry ladder. Input-delivery additions may
pull a decayed deadline forward to the delivery floor 5 seconds after the last
input attempt, or immediately if that floor has already passed; attention-only
additions retain the cohort's current decayed deadline. Bursts within the
debounce window remain consolidated.
Removing or replacing any message is durable progress and immediately rearms
the next notification.
Owner-bound retries do not emit attention when terminal input succeeds.
Transient foreground authority or input-quiet refusals keep the input retry
armed while rate-limiting their separate attention output. Output-only delivery
repeats on its slower cadence, and a short or failed attention write stays
pending on that cadence instead of terminating the notifier. Recovery-required
state never retries uncertain terminal input; it repeats the manual
drain-and-restart notice on that same attention cadence until the unread cohort
drains.

For orchestrators or hardened environments without a controlling TTY, use an
explicit external transport:

```bash
amq wake --me orchestrator \
  --inject-via ghostty-bridge \
  --inject-arg exec \
  --inject-arg "$TERMINAL_ID"
```

`--inject-via` is an executable path, not a shell command line. Repeat
`--inject-arg` for fixed arguments; AMQ appends the sanitized notification text
as the final argv element. This executes a local process for each notification,
and the payload can include sanitized but message-derived header content such as
sender and subject.

The resolved executable plus the ordered fixed arguments are the injector's
saved identity. Put any target identity needed to distinguish a pane, window,
or session in `--inject-arg`. Ambient environment variables and provider
configuration are invisible to repair and retire, so changing only those
channels cannot select a different target safely.

For permission-prompt workflows, use AMQ's fail-closed zero-input mode:

```bash
amq wake --me claude --inject-mode none --bell &

# Or let coop exec start it and prove readiness before launching the agent.
amq coop exec --require-wake --wake-inject-mode none claude
```

`none` never writes terminal input. `coop exec` gives its wake child separate
process capabilities: full stdout/stderr diagnostics append to the private
`agents/<agent>/.wake.log`, while notification attention uses a dedicated
terminal descriptor. Codex and Claude receive only terminal-safe title, bell,
and supported desktop-notification sequences on that descriptor, so runtime,
cleanup, and top-level fatal diagnostics cannot overwrite the active composer.
After `.wake.log` reaches 1 MiB, the next `coop exec` wake launch truncates it.
`.wake.repair.log` is truncated only when the next eligible `amq wake repair`
attempt opens replacement-wake diagnostics, before child start. One long-lived
child can exceed either launch bound. Without a controlling terminal, attention
is appended to the same durable log.
Urgent interrupt messages degrade to terminal-safe attention instead of Ctrl+C.
Because
`--inject-via` is arbitrary local
code and may itself inject terminal input, `none` rejects `--inject-via`,
`--inject-arg`, and `--inject-cmd`. A directly launched or externally
supervised `amq wake` should likewise route stdout/stderr to a private log when
it shares a terminal with an alternate-screen agent.

### Supervisor recipes

AMQ remains daemon-free. If `wake` or `monitor` must stay attached across
terminal restarts, let the operating system supervise the CLI process. Keep
notification and consumption separate: `wake` only scans and notifies;
`monitor` is the path that drains messages and emits receipts.

For systemd, use separate services with an absolute root. A notifier unit can
use:

```ini
[Service]
Environment=AM_ROOT=/absolute/path/to/shared/.agent-mail/collab
ExecStart=/usr/local/bin/amq wake --me claude --inject-mode none --bell
Restart=always
RestartSec=2
```

A consumer unit uses the same environment but a different `ExecStart`; it
restarts after each emitted batch:

```ini
[Service]
Environment=AM_ROOT=/absolute/path/to/shared/.agent-mail/collab
ExecStart=/usr/local/bin/amq monitor --me claude --timeout 0 --include-body --json
Restart=always
RestartSec=2
```

For launchd, put the equivalent absolute arguments in `ProgramArguments` and
enable `KeepAlive`. Use one plist for `wake` and another for `monitor`:

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
Route stdout/stderr to supervisor-managed logs and secure the unit/plist for the
local user who owns the mailbox.

On macOS, the separate `amq-keepalive` companion executable (built from
`cmd/amq-keepalive`) packages this recipe for wake delivery that must follow a
specific terminal session rather than a fixed plist. It keeps a private
registry of explicitly attached terminal targets (Ghostty and cmux adapters),
reattaches `wake` after reboot or sleep through a user LaunchAgent
(`install-launchd`), and can register SessionStart reattach hooks for Claude
Code and Codex (`install-hook`). It does not add keepalive behavior to the core
`amq` command: the OS supervises the companion, it never parses AMQ mailbox,
lock, presence, or target files, and it talks to AMQ only through the public
CLI (target-aware `amq wake`, `amq env --json`).

The supervisor inventories cmux once per due pass, checks active targets every
five minutes, and exponentially backs detached targets off from five minutes
to one hour. It fails closed if multiple live cmux surface UUIDs alias the same
TTY and repeats that check immediately before injection. Reattach persists a
recoverable inactive reservation before starting a wake, while spawned wakes
run in a separate Unix session and are never killed when the helper's readiness
wait is canceled. `retire-session` and `gc --apply` delegate retirement to
AMQ's identity-safe `wake retire` contract. They remove a registry row only
after AMQ confirms the exact saved injector target was retired; refusals,
ambiguous identity, and command failures preserve the row for a later retry.

```bash
go build ./cmd/amq-keepalive
./amq-keepalive attach --adapter ghostty --me claude
./amq-keepalive install-launchd
./amq-keepalive doctor
```

See [docs/amq-keepalive.md](docs/amq-keepalive.md) for the complete command and
safety reference.

**Options:**
- `--inject-mode auto|raw|paste|none` - Injection strategy; `none` enforces zero terminal input
- `--wake-inject-mode auto|raw|paste|none` - `coop exec` pass-through for its managed wake
- `--inject-via <executable>` - External transport executable; bypasses TIOCSTI and local TTY startup checks
- `--inject-arg <arg>` - Fixed argument before the payload; repeat for multiple arguments
- `--inject-timeout 5s` - Maximum runtime for one external injection command
- `--bell` - Ring terminal bell on new messages
- `--debounce 250ms` - Batch rapid messages
- `--defer-while-input` / `--defer-while-input=false` - Best-effort quiet-window gate before non-interrupt injection
- `--input-quiet-for 1200ms` - Required quiet window before deferred injection
- `--input-poll-interval 200ms` - Poll interval while waiting for terminal input to quiet
- `--input-max-hold 15s` - Maximum deferral; input still active at the deadline emits the notice out-of-band and skips synthetic input
- `--interrupt` / `--interrupt=false` - Enable/disable urgent interrupt notices; Ctrl+C still requires `--interrupt-cmd ctrl-c`

Every input-injecting mode can activate a focused permission or approval dialog:
raw and paste payload bytes, `--inject-cmd`, external injectors, and urgent Ctrl+C
all carry this hazard. Single-key dialog shortcuts mean that removing Enter or
sanitizing the payload is not a safety boundary.

Input deferral is collision reduction, not modal detection or a prompt-buffer
guarantee. It samples unread TTY input bytes and recent terminal reads only
after a wake notification is pending. An idle approval dialog is
indistinguishable from an idle composer. If the foreground app has already
consumed a partially typed prompt and the user pauses longer than
`--input-quiet-for`, wake can still inject and submit. Explicit urgent interrupt
messages bypass this deferral. If input stays active through
`--input-max-hold`, wake emits the notice out-of-band and skips synthetic input;
if input sampling is unavailable, injection remains best-effort. Use `none`
when AMQ must guarantee zero synthetic input.

**Platform support:** `coop exec` and `wake` require macOS or Linux. Native
Windows supports core queue commands and `coop init`, but not these two
commands; use WSL with a Linux asset for the complete workflow. Linux raw TTY
injection may be disabled by kernel hardening (`CONFIG_LEGACY_TIOCSTI`). See
the [platform capability matrix](INSTALL.md#platform-capability-matrix).

## Message Format

```json
{
  "schema": 1,
  "from": "codex",
  "to": ["claude"],
  "thread": "p2p/claude__codex",
  "subject": "Code review needed",
  "priority": "urgent",
  "kind": "review_request",
  "labels": ["parser"],
  "context": {"paths": ["internal/cli/drain.go"], "focus": "sorting"}
}
```

### Priority Levels

| Priority | Behavior |
|----------|----------|
| `urgent` | Interrupt current work |
| `normal` | Add to TODO list |
| `low` | Batch for later |

### Message Kinds

| Kind | Reply Kind | Default Priority |
|------|------------|------------------|
| `review_request` | `review_response` | normal |
| `question` | `answer` | normal |
| `decision` | — | normal |
| `todo` | — | normal |
| `status` | — | normal |
| `brainstorm` | — | normal |

When `--kind` is set but `--priority` is omitted, the CLI defaults priority to
`normal`.

## Spec Workflow

The spec workflow is a **skill-managed protocol** — agents follow the instructions in the amq-spec skill's `spec-workflow.md` using standard AMQ messaging primitives (`amq send`, `amq drain`, `amq thread`) with existing generic kinds and `workflow:spec` labels.

Phases: **Research -> Discuss -> Draft -> Review -> Present -> Execute**

All spec messages use thread `spec/<topic>` and labels `workflow:spec,phase:<name>`. See the amq-spec skill's `spec-workflow.md` for the full protocol.

## Context Object Schema

```json
{
  "paths": ["internal/cli/send.go"],
  "symbols": ["Header", "runSend"],
  "focus": "error handling in validation",
  "commands": ["go test ./internal/cli/..."]
}
```

## Troubleshooting

### Wake not working
```bash
amq wake --me claude                              # Watch for warnings
amq drain --include-body                          # Manual fallback
```

### Messages not appearing
```bash
amq list --me claude --new --json                 # Check inbox directly
amq watch --me claude --poll                      # Force poll mode
```
