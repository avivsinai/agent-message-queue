<!-- GENERATED FILE. DO NOT EDIT. Run `make docs-cli` to regenerate. -->
# AMQ CLI reference

This reference is generated from the built `amq` binary. It includes the
root command, every command listed by root help, and every listed
subcommand. The binary's `--help` output is authoritative for aliases and
options not repeated here.

## Command index

- [amq](#amq)
- [amq init](#amq-init)
- [amq setup](#amq-setup)
- [amq launch](#amq-launch)
- [amq send](#amq-send)
- [amq list](#amq-list)
- [amq read](#amq-read)
- [amq thread](#amq-thread)
- [amq trace](#amq-trace)
- [amq presence](#amq-presence)
- [amq cleanup](#amq-cleanup)
- [amq watch](#amq-watch)
- [amq drain](#amq-drain)
- [amq monitor](#amq-monitor)
- [amq reply](#amq-reply)
- [amq dlq](#amq-dlq)
- [amq wake](#amq-wake)
- [amq upgrade](#amq-upgrade)
- [amq env](#amq-env)
- [amq coop](#amq-coop)
- [amq swarm](#amq-swarm)
- [amq integration](#amq-integration)
- [amq receipts](#amq-receipts)
- [amq session](#amq-session)
- [amq who](#amq-who)
- [amq route](#amq-route)
- [amq doctor](#amq-doctor)
- [amq shell-setup](#amq-shell-setup)
- [amq completion](#amq-completion)
- [amq presence set](#amq-presence-set)
- [amq presence list](#amq-presence-list)
- [amq dlq list](#amq-dlq-list)
- [amq dlq read](#amq-dlq-read)
- [amq dlq retry](#amq-dlq-retry)
- [amq dlq purge](#amq-dlq-purge)
- [amq wake check](#amq-wake-check)
- [amq wake repair](#amq-wake-repair)
- [amq wake restart](#amq-wake-restart)
- [amq wake recover-owner](#amq-wake-recover-owner)
- [amq wake retire](#amq-wake-retire)
- [amq coop init](#amq-coop-init)
- [amq coop exec](#amq-coop-exec)
- [amq swarm list](#amq-swarm-list)
- [amq swarm join](#amq-swarm-join)
- [amq swarm leave](#amq-swarm-leave)
- [amq swarm tasks](#amq-swarm-tasks)
- [amq swarm claim](#amq-swarm-claim)
- [amq swarm complete](#amq-swarm-complete)
- [amq swarm fail](#amq-swarm-fail)
- [amq swarm block](#amq-swarm-block)
- [amq swarm bridge](#amq-swarm-bridge)
- [amq integration symphony](#amq-integration-symphony)
- [amq integration kanban](#amq-integration-kanban)
- [amq receipts list](#amq-receipts-list)
- [amq receipts wait](#amq-receipts-wait)
- [amq session create](#amq-session-create)
- [amq session list](#amq-session-list)
- [amq session resume](#amq-session-resume)
- [amq route explain](#amq-route-explain)
- [amq integration symphony init](#amq-integration-symphony-init)
- [amq integration symphony emit](#amq-integration-symphony-emit)
- [amq integration kanban bridge](#amq-integration-kanban-bridge)

## amq

```text
$ amq --help
amq - agent message queue

Usage:
  amq <command> [options]

Commands:
  init         Initialize the queue root and agent mailboxes
  setup        Configure project launch and session defaults
  launch       Launch or resume a configured AMQ session
  send         Send a message
  list         List inbox messages
  read         Read a message by id
  thread       View a thread
  trace        Join current evidence for a message
  presence     Set or list presence
  cleanup      Remove selected tmp, wake quarantine, or launch recovery artifacts
  watch        Wait for new messages (uses fsnotify)
  drain        Drain new messages (read, move to cur, emit receipts)
  monitor      Combined watch+drain for co-op mode
  reply        Reply to a message (auto thread/refs)
  dlq          Dead letter queue management
  wake         Background waker (TIOCSTI injection)
  upgrade      Upgrade amq to the latest release
  env          Output shell commands to set environment variables
  coop         Co-op mode setup (init, exec)
  swarm        Claude Code Agent Teams integration (join, tasks, bridge)
  integration  Optional interoperability adapters
  receipts     Message delivery receipts
  session      Create, list, and resume named AMQ sessions
  who          Show sessions and agents in current project
  route        Explain canonical routing
  doctor       Verify installation and configuration
  shell-setup  Output shell aliases (amc/amx/amg)
  completion   Generate shell completions (bash, zsh, fish)

Global options:
  --no-update-check  Disable update check

Environment:
  AM_ROOT             Queue root directory (from flags, env, config, or coop exec session setup)
  AM_ROOT_ID          Opaque physical identity token for AM_ROOT when available
  AM_ME               Default agent handle
  AM_BASE_ROOT        Authorized session base, or exact root for a sessionless pin
  AM_BASE_ROOT_ID     Opaque physical identity token for AM_BASE_ROOT when available
  AM_SESSION          Pinned session identity (empty means exact-root context)
  AMQ_GLOBAL_ROOT     Global root fallback (for agents spawned by external orchestrators)
  AMQ_NO_UPDATE_CHECK  Disable update check (1/true/yes/on)
  AMQ_WAKE_NO_SELF_UPGRADE  Disable automatic wake self-upgrade (1/true/yes/on)

Exit codes:
  0  Success
  1  General error
  2  Usage error
  3  Not found
  4  Timeout
  5  Context mismatch
  6  Action required

Use "amq <command> --help" for more information about a command.
```

## amq init

```text
$ amq init --help
Usage:
  amq init --root <path> --agents a,b,c [--force]

Options:
  -agents string
        Comma-separated agent handles (required)
  -force
        Overwrite existing config.json if present
  -root string
        Root directory for the queue (default ".agent-mail")
```

## amq setup

```text
$ amq setup --help
Usage:
  amq setup [options]

Configure this project after previewing every change.
Detects Claude, Codex, Cursor, and Grok through their adapter capability probes.
Launcher detection is probe-only; setup never calls a launcher backend.

Options:
  -agents string
        Comma-separated detected agent adapters
  -apply string
        Apply only when the recomputed preview matches this digest
  -default-session string
        Default session name
  -json
        Emit JSON output
  -launcher-preference string
        Comma-separated launcher preference
  -layout string
        Advisory layout intent (default "columns")
  -no-gitignore
        Do not modify .gitignore
  -preview
        Preview changes without writing
  -project-root string
        Directory treated as the project root (writes .amqrc, .amq, and .gitignore there)
  -root string
        Queue root written to .amqrc (default ".agent-mail")
  -y    Accept the preview without prompting
```

## amq launch

```text
$ amq launch --help
Usage:
  amq launch [options]

Reconcile one existing AMQ session from the committed launch declaration.
Unknown sessions are never created. Non-interactive trust, stale identity, and Inspect uncertainty exit 6.

Options:
  -allow-fresh-fallback
        Allow fresh conversations when saved identities are stale
  -apply string
        Public ApplyRequestV1 file, or - for stdin (requires --json)
  -fresh
        Start fresh conversations for this launch
  -json
        Emit JSON output
  -launcher string
        Launcher backend (auto or commands) (default "auto")
  -me string
        Agent handle (or AM_ME)
  -placement string
        Public PlacementV1 JSON object (with --plan)
  -plan string
        Public LaunchIntentV1 file, or - for stdin
  -prepare
        Prepare the public launch intent without mutation (requires --plan and --json)
  -rebind
        Confirm a deliberate launcher binding change
  -request string
        Public PrepareRequestV1 file, or - for stdin (requires --json)
  -require-agent
        Require at least one agent to launch or attach
  -root string
        Root directory for the queue (default ".agent-mail")
  -session string
        Named session to launch or resume
  -strict
        Error on unknown handles (default: warn)
```

## amq send

```text
$ amq send --help
Usage:
  amq send --me <agent> --to <recipients> [--project <name>] [--session <name>] [--from-session <name>] [options]


Cross-session example:
  amq send --to codex --session auth --thread xsession/auth-review --body "..."
  amq send --root .agent-mail --from-session cto --me alice --to bob --session qa --body "..."

Receipt example:
  amq send --to codex --body "please review" --wait-for drained --wait-timeout 60s

Cross-project examples:
  amq send --to codex --project infra-lib --body "hello from here"
  amq send --to codex@infra-lib:collab --body "inline syntax"

Options:
  -allow-empty
        Allow sending a blank body (otherwise an empty body is rejected)
  -allow-self
        Allow an intentional same-root send to the sender's own handle
  -body string
        Body string, @file, or - / empty to read stdin
  -context string
        JSON context object or @file.json
  -from-session string
        Source session for setup-terminal cross-session sends
  -ignore-session-pin
        With explicit --root, ignore a conflicting AM_SESSION source pin
  -json
        Emit JSON output
  -kind string
        Message kind: brainstorm, review_request, review_response, question, answer, decision, status, todo
  -labels string
        Comma-separated labels/tags
  -me string
        Agent handle (or AM_ME)
  -priority string
        Message priority: urgent, normal, low (default: normal if kind set)
  -project string
        Target peer project name (delivers to a peer project's inbox)
  -refs string
        Comma-separated related message ids
  -root string
        Root directory for the queue (default ".agent-mail")
  -session string
        Target session (delivers to a different session's inbox)
  -strict
        Error on unknown handles (default: warn)
  -subject string
        Message subject
  -thread string
        Thread id (required for multiple recipients; default p2p/<a>__<b> for single-recipient sends)
  -to string
        Receiver handle (comma-separated)
  -wait-for string
        Wait for receipt stage after send (drained, dlq)
  -wait-timeout duration
        Timeout for --wait-for (0 = wait forever) (default 2m0s)
```

## amq list

```text
$ amq list --help
Usage:
  amq list --me <agent> [--session <name>] [--new | --cur] [options]

Options:
  -cur
        List messages in inbox/cur
  -from string
        Filter by sender handle
  -json
        Emit JSON output
  -kind string
        Filter by message kind
  -label value
        Filter by label (can be repeated)
  -limit int
        Limit number of messages (0 = no limit)
  -me string
        Agent handle (or AM_ME)
  -new
        List messages in inbox/new
  -offset int
        Offset into sorted results (0 = start)
  -priority string
        Filter by priority (urgent, normal, low)
  -root string
        Root directory for the queue (default ".agent-mail")
  -session string
        Target session under the resolved base root
  -strict
        Error on unknown handles (default: warn)
```

## amq read

```text
$ amq read --help
Usage:
  amq read --me <agent> --id <msg_id> [--session <name>] [options]

Read a message by id.

If the message is in inbox/new, AMQ only moves it to inbox/cur after parse and header validation succeed.
If the message in inbox/new is corrupt or malformed, AMQ moves it to DLQ and emits a dlq receipt.

Options:
  -id string
        Message id
  -ignore-session-pin
        With explicit --root, ignore a conflicting AM_SESSION pin
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -session string
        Target session under the resolved base root
  -strict
        Error on unknown handles (default: warn)
```

## amq thread

```text
$ amq thread --help
Usage:
  amq thread --id <thread_id> [options]

Options:
  -agents string
        Comma-separated agent handles (optional)
  -id string
        Thread id
  -include-body
        Include body in output
  -json
        Emit JSON output
  -limit int
        Limit number of messages (0 = no limit)
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: warn)
```

## amq trace

```text
$ amq trace --help
Usage:
  amq trace <message-id> [options]

Join current on-disk evidence for one message without mutating the queue.

Phase A reports message copies, route fields, visible delivery artifacts, DLQ entries,
delivery receipts, thread references, and durable agent-owned notification write attempts.
Notification evidence never proves that a TUI or person saw, displayed, or submitted it.

Options:
  -json
        Emit JSON output
  -root string
        Root directory for the queue (default ".agent-mail")
```

## amq presence

```text
$ amq presence --help
amq presence - Agent presence metadata

Set or inspect agent availability, status, and optional notes.

Subcommands:
  set   Update presence status
  list  List presence data

Examples:
  amq presence set --me claude --status busy --note "reviewing PR"
  amq presence list --json

Use "amq presence <subcommand> --help" for details.
```

## amq cleanup

```text
$ amq cleanup --help
Usage:
  amq cleanup [--tmp-older-than <duration>] [--wake-quarantine-older-than <duration>] [--launch-journal --root <session-root>] [--dry-run] [--yes] [options]

Options:
  -dry-run
        Show what would be removed without deleting
  -json
        Emit JSON output
  -launch-journal
        Remove one exact stuck managed-launch recovery journal
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: warn)
  -tmp-older-than string
        Duration (e.g. 36h)
  -wake-quarantine-older-than string
        Remove preserved wake quarantine artifacts older than this duration
  -yes
        Skip confirmation prompt
```

## amq watch

```text
$ amq watch --help
Usage:
  amq watch --me <agent> [--session <name>] [options]

Options:
  -ignore-session-pin
        With explicit --root, ignore a conflicting AM_SESSION pin
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -poll
        Use polling fallback instead of fsnotify (for network filesystems)
  -root string
        Root directory for the queue (default ".agent-mail")
  -session string
        Target session under the resolved base root
  -strict
        Error on unknown handles (default: warn)
  -timeout duration
        Maximum time to wait for messages (0 = wait forever) (default 1m0s)
```

## amq drain

```text
$ amq drain --help
Usage:
  amq drain --me <agent> [--session <name>] [options]

Drains new messages: reads, moves to cur, emits receipts.
Designed for hook/script integration. Quiet when empty.

Options:
  -ignore-session-pin
        With explicit --root, ignore a conflicting AM_SESSION pin
  -include-body
        Include message body in output
  -json
        Emit JSON output
  -limit int
        Max messages to drain (0 = no limit) (default 20)
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -session string
        Target session under the resolved base root
  -strict
        Error on unknown handles (default: warn)
```

## amq monitor

```text
$ amq monitor --help
Usage:
  amq monitor --me <agent> [--session <name>] [options]

Combined watch+drain: waits for messages, drains them, outputs structured payload.
Use --peek to watch without moving messages to cur (no ack).
Ideal for co-op mode background watchers in Claude Code or Codex.

Options:
  -ignore-session-pin
        With explicit --root, ignore a conflicting AM_SESSION pin
  -include-body
        Include message body in output
  -json
        Emit JSON output
  -limit int
        Max messages to drain (0 = no limit) (default 20)
  -me string
        Agent handle (or AM_ME)
  -peek
        Peek without moving messages to cur
  -poll
        Use polling fallback instead of fsnotify
  -root string
        Root directory for the queue (default ".agent-mail")
  -session string
        Target session under the resolved base root
  -strict
        Error on unknown handles (default: warn)
  -timeout duration
        Max time to wait for messages (0 = wait forever) (default 1m0s)
```

## amq reply

```text
$ amq reply --help
Usage:
  amq reply --me <agent> --id <msg_id> [options]

Reply to a message with automatic thread/refs handling.
Finds the original message, sets to/thread/refs automatically.
Cross-session replies are routed via reply_to header.
To follow up on a sent cross-session message, use amq send --session instead.
Use --wait-for drained to block until the recipient ingests the reply,
mirroring amq send --wait-for.

Options:
  -allow-empty
        Allow sending a blank body (otherwise an empty body is rejected)
  -body string
        Body string, @file, or - / empty to read stdin
  -context string
        JSON context object or @file.json
  -id string
        Message ID to reply to
  -ignore-session-pin
        With explicit --root, ignore a conflicting AM_SESSION source pin
  -json
        Emit JSON output
  -kind string
        Message kind: brainstorm, review_request, review_response, question, answer, decision, status, todo (default: same as original, review_response for review_request, answer for question)
  -labels string
        Comma-separated labels/tags
  -me string
        Agent handle (or AM_ME)
  -priority string
        Message priority: urgent, normal, low
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: warn)
  -subject string
        Override subject (default: Re: <original>)
  -wait-for string
        Wait for receipt stage after reply (e.g., drained)
  -wait-timeout duration
        Timeout for --wait-for (default 2m0s)
```

## amq dlq

```text
$ amq dlq --help
amq dlq - Dead letter queue management

Subcommands:
  list   List dead-lettered messages
  read   Read a DLQ message with failure info
  retry  Retry a DLQ message (move back to inbox)
  purge  Permanently remove DLQ messages

Use "amq dlq <subcommand> --help" for details.
```

## amq wake

```text
$ amq wake --help
Usage:
  amq wake --me <agent> [options]

Background waker: injects terminal notification when messages arrive.
Run as background job before starting CLI: amq wake --me claude --interrupt-cmd none &

Inject modes:
  auto  - Detect CLI type: raw for Claude Code/Codex, paste for others
  raw   - Plain text + CR, no bracketed paste (works with Ink-based CLIs)
  paste - Bracketed paste with delayed CR (works with crossterm-based CLIs)
  none  - Output notice on wake stderr; zero terminal input injection
          (urgent interrupts degrade to one bell + output notice)

External injection:
  --inject-via runs a local executable for each notification, bypassing
  the TIOCSTI/stdin-TTY startup requirement. Fixed arguments use repeatable
  --inject-arg; AMQ appends the sanitized notification payload as the
  final argv element. The command is not run through a shell.
  --retry-until drained (default) reannounces until inbox progress.
  --retry-until injected stops reannouncing an unchanged cohort after
  the injector proves acceptance (stderr AMQ_INJECT_PROGRESS=accepted,
  exit 0) or, for marker-less legacy injectors, after a bare exit 0.
  AMQ_INJECT_PROGRESS=deferred retains the cohort and retries through
  the wake loop; failed and uncertain outcomes are terminal for that
  unchanged cohort and are never silently replayed — a new inbox
  change re-arms delivery. Standalone ownerless wakes execute the
  injector once per physical inbox cohort.
  Example: amq wake --me orchestrator --inject-via /path/to/ghostty-bridge \
    --inject-arg exec --inject-arg "$TERMINAL_ID"
  Trust boundary: --inject-via executes local code, and the payload can
  contain sanitized but message-derived header content.

Input deferral (default on): wake samples terminal input only after
  a message is pending, then injects after a short quiet window.
  Collision reduction only: it cannot detect permission/approval dialogs.
  A pause longer than --input-quiet-for can still inject while a prompt
  is being composed. If input remains active through --input-max-hold,
  wake emits the notice out-of-band and skips synthetic input. If input
  sampling is unavailable, injection remains best-effort. Interrupt
  messages bypass deferral.
  Atime sampling uses stdin (when a TTY) for cross-platform fidelity;
  Linux tty atime is updated at ~8s granularity, so it cannot establish
  a precise 1200ms idle window. On Linux this heuristic is advisory.

Interrupt notices (default on): urgent messages tagged with label "interrupt"
  trigger an interrupt notice. Ctrl+C injection is opt-in with
  --interrupt-cmd ctrl-c; it sends real SIGINT to the foreground process
  group and can interrupt or crash the agent.

Safety: raw, paste, --inject-cmd, --inject-via, and opt-in interrupt Ctrl+C
  can activate a focused permission/approval dialog. Use none when AMQ
  must enforce zero synthetic input; stderr output may scribble until redraw.

Self-upgrade: eligible co-op wakes observe their stable launch symlink and
  replace the running image only with a strictly newer installed AMQ while
  preserving PID, terminal ownership, and unread work. Disable with
  --no-self-upgrade or AMQ_WAKE_NO_SELF_UPGRADE=1.

EXPERIMENTAL: Uses TIOCSTI ioctl (macOS/Linux). May not work on all systems.

Options:
  -baseline-existing
        Ignore messages already waiting when this wake starts
  -bell
        Ring terminal bell on new messages
  -debounce duration
        Debounce window for batching messages (default 250ms)
  -debug
        Log injection diagnostics to stderr
  -defer-while-input
        Best-effort: defer non-interrupt injection while terminal input appears active (default true)
  -inject-arg value
        Argument for --inject-via before the payload (repeatable)
  -inject-cmd string
        Command to inject (power user mode)
  -inject-mode string
        Injection mode: auto, raw, paste, none (auto detects CLI type) (default "auto")
  -inject-timeout duration
        Timeout for one --inject-via command (default 5s)
  -inject-via string
        External executable for injection (payload appended as last arg, bypasses TTY requirement)
  -input-max-hold duration
        Maximum time to defer one wake injection (0 = no hold) (default 15s)
  -input-poll-interval duration
        Polling interval while waiting for quiet terminal input (default 200ms)
  -input-quiet-for duration
        Quiet window before deferred injection (advisory only on Linux; tty atime granularity is ~8s) (default 1.2s)
  -interrupt
        Enable interrupt injection for urgent interrupt messages (default true)
  -interrupt-cmd string
        Interrupt command to inject: none (default) or ctrl-c (sends real SIGINT to the foreground process group and can interrupt or crash the agent) (default "none")
  -interrupt-cooldown duration
        Minimum time between interrupts (default 7s)
  -interrupt-label string
        Label required to trigger interrupt (default "interrupt")
  -interrupt-notice string
        Custom interrupt notice (default: auto)
  -interrupt-priority string
        Priority required to trigger interrupt (default "urgent")
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -no-self-upgrade
        Disable automatic replacement by a newer installed AMQ image
  -preview-len int
        Max subject preview length (default 48)
  -retry-until string
        Doorbell acknowledgement: drained or injected (default "drained")
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: warn)
```

## amq upgrade

```text
$ amq upgrade --help
Usage:
  amq upgrade [--all] [-y]

Downloads and installs the latest amq release from GitHub.
When amq is installed via Homebrew or Scoop, amq upgrade delegates to the package manager instead of overwriting it; pass -y to run the delegate.
--all upgrades companion binaries in the raw or resolved executable directory of a direct-install amq, or in ~/.local/bin; a running amq-keepalive is never killed.
Retire live wakes started by the previous binary before its install directory is removed.
If wake check reports binary_dir_gone, run amq doctor --ops --fix-wake-locks.

Options:
  -all
        Also upgrade present companion binaries (amq-keepalive, amq-bridge, amq-acp)
  -y    Run the package-manager delegate without an AMQ confirmation; the manager may still prompt
```

## amq env

```text
$ amq env --help
Usage:
  amq env [options]

Outputs shell commands that replace the complete AMQ root/session context.

Configuration precedence (highest to lowest):
  Root: flags > env (AM_ROOT) > project .amqrc > AMQ_GLOBAL_ROOT > implicit fallbacks
  Inside a Git worktree or bare repository: repo-local .agent-mail; ~/.amqrc is ineligible
  Outside Git: ~/.amqrc > detected .agent-mail
  Me:   flags > env (AM_ME)

Note: .amqrc only configures 'root'. Agent identity ('me') is set
per-terminal via --me or AM_ME, since different terminals may use
different agents on the same project.

Examples:
  amq_context="$(amq env --me claude)" && eval "$amq_context"
  amq_context="$(amq env --session feature-x --me claude --export)" && eval "$amq_context"
  amq_context="$(amq env --me codex --wake)" && eval "$amq_context"
  amq_context="$(amq env --session feature-x --me claude)" && eval "$amq_context"
  amq env --json                                # Machine-readable output
  amq env --session-name                         # Print session name (for statusline)

Options:
  -export
        Also print a note confirming the resolved terminal pin
  -json
        Output as JSON (for scripts)
  -me string
        Agent handle (overrides AM_ME)
  -root string
        Root directory (overrides .amqrc and AM_ROOT)
  -session string
        Session name (shorthand for --root .agent-mail/<name>)
  -session-name
        Print current session name (for statusline integration)
  -shell string
        Shell format: sh, bash, zsh, fish (default "sh")
  -wake
        Include amq wake & in output
```

## amq coop

```text
$ amq coop --help
amq coop - Co-op mode for multi-agent collaboration

Subcommands:
  init  Initialize project for co-op mode
  exec  Set up co-op mode and exec into an agent

Examples:
  amq coop exec claude
  amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox
  amq coop exec --session feature-x claude

Use "amq coop <subcommand> --help" for details.
```

## amq swarm

```text
$ amq swarm --help
amq swarm - Claude Code Agent Teams integration

Register external agents and interact with the shared task list.

Subcommands:
  list      List discovered Agent Teams
  join      Register an external agent in a team
  leave     Deregister an agent from a team
  tasks     List tasks from the shared task list
  claim     Claim a task
  complete  Mark a task as completed
  fail      Mark a task as failed
  block     Mark a task as blocked
  bridge    Run bridge process (sync tasks -> AMQ notifications)

Examples:
  amq swarm list
  amq swarm join --team my-team --me codex
  amq swarm bridge --team my-team --me codex

Use "amq swarm <subcommand> --help" for details.
```

## amq integration

```text
$ amq integration --help
amq integration - Optional interoperability adapters

Subcommands:
  symphony  Lightweight Symphony hook adapter
  kanban    Experimental Cline Kanban bridge

AMQ's core transport is still the message. These adapters convert
external lifecycle or task events into ordinary AMQ messages.

Use "amq integration <subcommand> --help" for details.
```

## amq receipts

```text
$ amq receipts --help
amq receipts - Query and wait for message lifecycle receipts

Subcommands:
  list  List receipts (optionally filtered)
  wait  Wait for a receipt to appear

Examples:
  amq receipts list --me claude --msg-id msg_001
  amq receipts wait --me claude --msg-id msg_001 --stage drained --timeout 60s

Use "amq receipts <subcommand> --help" for details.
```

## amq session

```text
$ amq session --help
amq session - Named session lifecycle

session create provisions a canonical named session and its roster mailboxes.
session list reports canonical sessions and safe legacy roots.
session resume uses the same fail-closed reconciliation engine as amq launch.

Subcommands:
  create  Create a named session and its roster mailboxes
  list    List sessions under the base root
  resume  Resume an existing session through launch reconciliation

Examples:
  amq session create feature-x
  amq session list --json
  amq session resume feature-x --json

Use "amq session <subcommand> --help" for details.
```

## amq who

```text
$ amq who --help
Usage:
  amq who [options]

List sessions and agents in the current project.
Shows active/stale status and whether activity comes from a verified notifier or recent commands.

Options:
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: warn)
```

## amq route

```text
$ amq route --help
amq route - Explain AMQ route resolution without sending a message

Subcommands:
  explain  Explain a send route as canonical JSON

Examples:
  amq route explain --to codex --json
  amq route explain --to qa --project project-b --session qa --json

Use "amq route <subcommand> --help" for details.
```

## amq doctor

```text
$ amq doctor --help
Usage:
  amq doctor [options]

Verify AMQ installation and configuration.

Checks:
  - Binary location
  - .amqrc configuration
  - Mailbox directory permissions
  - Agent configuration (config.json)
  - Extension metadata manifests and diagnostics
  - Skill installation (Claude Code / Codex / Grok Build)

With --ops, also checks runtime health:
  - Queue depth and oldest unread per agent
  - DLQ count and age
  - Presence freshness
  - Git worktree mailbox divergence
  - Wake lock health
  - Integration hints (Kanban, Symphony)

Options:
  -base-root string
        Config-authority root for an explicit session --root
  -fix-mailboxes
        Create missing required directories for configured mailboxes
  -fix-wake-locks
        With --ops, remove stale wake lock files
  -ignore-session-pin
        With explicit --root, allow repair outside the pinned session context
  -json
        Output as JSON
  -json-schema int
        JSON schema version: 1 or 2 (requires --json) (default 1)
  -ops
        Include runtime operational checks
  -root string
        Exact AMQ root to inspect or repair
```

## amq shell-setup

```text
$ amq shell-setup --help
Usage:
  amq shell-setup [options]

Outputs shell aliases for quick co-op session management.

Defines three functions (names customizable via flags):
  amc [session] [flags]  → amq coop exec [--session <s>] claude [flags]
  amx [session] [flags]  → amq coop exec [--session <s>] codex -- --dangerously-bypass-approvals-and-sandbox [flags]
  amg [session] [flags]  → amq coop exec [--session <s>] grok [flags]

Usage:
  eval "$(amq shell-setup)"           # Add to current shell (default names)
  amq shell-setup --shell fish         # Fish shell output
  amq shell-setup --claude-alias cc --codex-alias cx --grok-alias gk  # Custom names

Examples after setup:
  amc                    # Start Claude Code (default session)
  amc feature-x          # Isolated session for feature-x
  amx feature-x          # Codex in same isolated session
  amg feature-x          # Grok in same isolated session

Options:
  -claude-alias string
        Function name for Claude Code shortcut (default "amc")
  -codex-alias string
        Function name for Codex CLI shortcut (default "amx")
  -grok-alias string
        Function name for Grok CLI shortcut (default "amg")
  -shell string
        Shell format: bash, zsh, fish (default "bash")
```

## amq completion

```text
$ amq completion --help
Usage:
  amq completion <bash|zsh|fish>

Generate shell completion scripts.

Examples:
  eval "$(amq completion bash)"
  amq completion zsh > "${fpath[1]}/_amq"
  amq completion fish > ~/.config/fish/completions/amq.fish
```

## amq presence set

```text
$ amq presence set --help
Usage:
  amq presence set --me <agent> --status <status> [options]

Options:
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -note string
        Optional note
  -root string
        Root directory for the queue (default ".agent-mail")
  -status string
        Status string
  -strict
        Error on unknown handles (default: warn)
```

## amq presence list

```text
$ amq presence list --help
Usage:
  amq presence list [options]

Options:
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: warn)
```

## amq dlq list

```text
$ amq dlq list --help
Usage:
  amq dlq list --me <agent> [--session <name>] [--new | --cur] [options]

Options:
  -cur
        List only inspected DLQ messages (dlq/cur)
  -ignore-session-pin
        With explicit --root, ignore a conflicting AM_SESSION pin
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -new
        List only unread DLQ messages (dlq/new)
  -root string
        Root directory for the queue (default ".agent-mail")
  -session string
        Target session under the resolved base root
  -strict
        Error on unknown handles (default: warn)
```

## amq dlq read

```text
$ amq dlq read --help
Usage:
  amq dlq read --me <agent> --id <dlq_id> [--session <name>] [options]

Options:
  -id string
        DLQ message ID to read
  -ignore-session-pin
        With explicit --root, ignore a conflicting AM_SESSION pin
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -session string
        Target session under the resolved base root
  -strict
        Error on unknown handles (default: warn)
```

## amq dlq retry

```text
$ amq dlq retry --help
Usage:
  amq dlq retry --me <agent> --id <dlq_id> [--session <name>] [--force] [options]

Or: amq dlq retry --me <agent> --all [--session <name>] [--force]

Options:
  -all
        Retry all DLQ messages
  -force
        Force retry even if max retries exceeded
  -id string
        DLQ message ID to retry
  -ignore-session-pin
        With explicit --root, ignore a conflicting AM_SESSION pin
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -session string
        Target session under the resolved base root
  -strict
        Error on unknown handles (default: warn)
```

## amq dlq purge

```text
$ amq dlq purge --help
Usage:
  amq dlq purge --me <agent> [--session <name>] [--older-than <duration>] [--dry-run] [--yes] [options]

Options:
  -dry-run
        Show what would be removed without deleting
  -ignore-session-pin
        With explicit --root, ignore a conflicting AM_SESSION pin
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -older-than string
        Duration (e.g. 24h) - only purge messages older than this
  -root string
        Root directory for the queue (default ".agent-mail")
  -session string
        Target session under the resolved base root
  -strict
        Error on unknown handles (default: warn)
  -yes
        Skip confirmation prompt
```

## amq wake check

```text
$ amq wake check --help
Usage:
  amq wake check --me <agent> [options]

Inspect wake start and restart capability without mutation.

Reports whether this process can start a full-strength terminal wake,
whether an existing wake is live or repairable, the running and current
AMQ images, and the exact non-destructive next action.

Only restart_capability=agent_safe authorizes an agent-side action.

Options:
  -json
        Emit JSON output
  -json-schema int
        JSON schema version: 1 or 2 (requires --json) (default 1)
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: warn)
```

## amq wake repair

```text
$ amq wake repair --help
Usage:
  amq wake repair --me <agent> [options]

Repair an eligible wake by restarting it from a saved inject-via target.

Accepts proven-stale or unverified ownerless generic locks. Refuses
owner-bound or invalid unverified claims and raw terminal wake targets.
This command only uses .wake.target files created for --inject-via wakes.

Options:
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: warn)
```

## amq wake restart

```text
$ amq wake restart --help
Usage:
  amq wake restart --me <agent> [options]

Ask a live owner-bound wake to replace itself without a caller TTY.

The existing wake keeps its terminal and PID, validates a fresh AMQ image,
then execs that image only from a quiescent delivery boundary.

Options:
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: warn)
```

## amq wake recover-owner

```text
$ amq wake recover-owner --help
Usage:
  amq wake recover-owner --me <agent> [options]

Recover one exact owner-bound wake claim.

Live release requires the exact AMQ_WAKE_OWNER token and the caller's
current OS session to match the persisted owner. There is no force mode.

Options:
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: warn)
```

## amq wake retire

```text
$ amq wake retire --help
Usage:
  amq wake retire --me <agent> --inject-via <path> [options]

Stop an identity-confirmed live inject-via wake or remove its exactly-bound proven-stale lock.

The expected executable and ordered arguments must exactly match the saved target.
Pass --if-generation with the generation from amq wake check so a replacement published after that check is refused.
Retirement preserves the mailbox, removes the exact saved target and coupled state projection, and never stops raw wakes.

Options:
  -if-generation string
        Retire only if the current lock generation still matches this exact value
  -inject-arg value
        Expected fixed injection argument (repeatable)
  -inject-via string
        Expected external injection executable
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -retry-until string
        Expected doorbell acknowledgement: drained or injected (default "drained")
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: warn)
```

## amq coop init

```text
$ amq coop init --help
Usage:
  amq coop init [options]

Initialize a project for co-op mode with sensible defaults.

Creates:
  - .amqrc file with root configuration
  - Mailbox directories for each agent
  - Updates .gitignore (unless --no-gitignore)

Defaults:
  --root=.agent-mail  --agents=claude,codex,user

Explicit three-engine example (not a default):
  --agents claude,codex,grok,user

Options:
  -agents string
        Comma-separated agent handles (default "claude,codex,user")
  -force
        Overwrite existing config if present
  -json
        Output as JSON
  -no-gitignore
        Do not modify .gitignore
  -root string
        Root directory for the queue (default ".agent-mail")
```

## amq coop exec

```text
$ amq coop exec --help
Usage:
  amq coop exec [options] <command> [-- <command-flags>]

Set up co-op mode and exec into the agent (replaces this process).

Sets AM_ROOT (always a session subdirectory) and AM_ME,
starts amq wake in background, then
replaces itself with the given command via exec.

If neither --session nor --root is given, defaults to the declared
default_session from .amq/launch.json, or collab when none is declared.
The agent handle is derived from the command basename unless --me is set.

Examples:
  amq coop exec claude                              # Exec into Claude Code (declared session or collab)
  amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox  # Codex with flags
  amq coop exec grok                                # Grok CLI, caller flags forwarded as-is
  amq coop exec --session feature-x claude          # Isolated session
  amq coop exec --root .agent-mail/auth claude      # Explicit root (no session default)
  amq coop exec --require-wake --wake-inject-mode none claude  # Zero-input wake
  amq coop exec --wake-inject-via /path/to/injector codex
  amq coop exec --me myagent bash                   # Debug shell with AMQ env
  amq coop exec --named=false claude              # Disable automatic session naming

Wake readiness:
  Coop never reuses a generic wake because it has no persisted
  exact-owner identity. Only an exact owner-bound inject-via wake can be
  reused; stop an older generic wake before retrying coop exec.

Options:
  -managed-no-wake-reason string

  -managed-symphony-event value

  -managed-symphony-workspace-key string

  -me string
        Agent handle (override auto-derivation from command name)
  -named
        Stamp the session and AM_ME onto the spawned CLI session name (default true)
  -no-gitignore
        When auto-initializing, do not modify .gitignore
  -no-init
        Don't auto-initialize if .amqrc is missing
  -no-wake
        Don't start amq wake in background
  -require-wake
        Fail if amq wake cannot start and acquire its lock
  -root string
        Root directory (override auto-detection)
  -session string
        Session name (shorthand for --root .agent-mail/<name>)
  -wake-inject-arg value
        Fixed argument for wake --inject-via before the payload (repeatable)
  -wake-inject-mode string
        Wake injection mode: auto, raw, paste, none (default "auto")
  -wake-inject-via string
        Start wake with this absolute --inject-via executable, enabling later amq wake repair
  -y    Skip confirmation prompts (including clearing a blocking wake)
```

## amq swarm list

```text
$ amq swarm list --help
Usage:
  amq swarm list [options]

List all discovered Claude Code Agent Teams.

Scans ~/.claude/teams/ for team configurations.

Options:
  -json
        Emit JSON output
```

## amq swarm join

```text
$ amq swarm join --help
Usage:
  amq swarm join --team <name> --me <agent> [options]

Register an agent in a Claude Code Agent Team.

The agent is added to the team's config.json with the specified type.
This makes the agent discoverable by the team lead and other teammates.

Options:
  -agent-id string
        Agent ID (auto-generated if empty)
  -json
        Emit JSON output
  -me string
        Agent handle (e.g., codex)
  -team string
        Team name (required)
  -type string
        Agent type (external, codex, claude-code) (default "external")
```

## amq swarm leave

```text
$ amq swarm leave --help
Usage:
  amq swarm leave --team <name> --agent-id <id>

Deregister an agent from a Claude Code Agent Team.

Options:
  -agent-id string
        Agent ID to remove (required)
  -json
        Emit JSON output
  -team string
        Team name (required)
```

## amq swarm tasks

```text
$ amq swarm tasks --help
Usage:
  amq swarm tasks --team <name> [options]

List tasks from the shared Agent Teams task list.

Tasks are stored at ~/.claude/tasks/{team-name}/.

Options:
  -json
        Emit JSON output
  -status string
        Filter by status: pending, in_progress, completed, failed, blocked
  -team string
        Team name (required)
```

## amq swarm claim

```text
$ amq swarm claim --help
Usage:
  amq swarm claim --team <name> --task <id> --me <agent>

Claim a task from the shared task list.

Sets the task status to in_progress and assigns it to the agent.
Uses agent_id from the team config as assigned_to for CC interop.

Options:
  -agent-id string
        Agent ID to use as assigned_to (auto-detect from team config)
  -json
        Emit JSON output
  -me string
        Agent handle
  -task string
        Task ID to claim (required)
  -team string
        Team name (required)
```

## amq swarm complete

```text
$ amq swarm complete --help
Usage:
  amq swarm complete --team <name> --task <id> --me <agent>

Mark a task as completed in the shared task list.

Optionally attach structured proof-of-work via --evidence.

Options:
  -agent-id string
        Agent ID (auto-detect from team config)
  -evidence string
        Evidence JSON object or @file.json
  -json
        Emit JSON output
  -me string
        Agent handle
  -task string
        Task ID to complete (required)
  -team string
        Team name (required)
```

## amq swarm fail

```text
$ amq swarm fail --help
Usage:
  amq swarm fail --team <name> --task <id> --me <agent> [options]

Mark a task as failed in the shared task list.

Only the current assignee can fail an in-progress task.

Options:
  -agent-id string
        Agent ID (auto-detect from team config)
  -json
        Emit JSON output
  -me string
        Agent handle
  -reason string
        Failure reason
  -task string
        Task ID to fail (required)
  -team string
        Team name (required)
```

## amq swarm block

```text
$ amq swarm block --help
Usage:
  amq swarm block --team <name> --task <id> --me <agent> [options]

Mark a task as blocked in the shared task list.

Only the current assignee can block an in-progress task.

Options:
  -agent-id string
        Agent ID (auto-detect from team config)
  -json
        Emit JSON output
  -me string
        Agent handle
  -reason string
        Blocking reason
  -task string
        Task ID to block (required)
  -team string
        Team name (required)
```

## amq swarm bridge

```text
$ amq swarm bridge --help
Usage:
  amq swarm bridge --team <name> --me <agent> [options]

Run the swarm bridge process.

Watches the Agent Teams task list for changes relevant to the
specified agent and delivers notifications via AMQ.

The bridge translates between Claude Code Agent Teams' shared task
list and AMQ's Maildir-based messaging, enabling Codex agents to
participate in the swarm.

Run this alongside the Codex agent session. Press Ctrl+C to stop.

Options:
  -agent-id string
        Agent Teams agent_id (auto-detect from team config)
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -poll
        Use polling instead of fsnotify
  -poll-interval duration
        Poll interval for task changes (polling mode) (default 3s)
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: warn)
  -team string
        Team name (required)
```

## amq integration symphony

```text
$ amq integration symphony --help
amq integration symphony - Lightweight Symphony hook adapter

Subcommands:
  init  Patch WORKFLOW.md hooks with AMQ-managed fragment
  emit  Emit an AMQ message for a symphony lifecycle event

Examples:
  amq integration symphony init --me codex
  amq integration symphony init --me codex --check
  amq integration symphony emit --event after_create --me codex

Use "amq integration symphony <subcommand> --help" for details.
```

## amq integration kanban

```text
$ amq integration kanban --help
amq integration kanban - Experimental Cline Kanban bridge

Subcommands:
  bridge  Run websocket bridge (Kanban runtime -> AMQ messages)

Warning:
  Experimental adapter. Depends on a preview WebSocket surface that may change.

Examples:
  amq integration kanban bridge --me codex
  amq integration kanban bridge --me codex --url ws://127.0.0.1:3484/api/runtime/ws
  amq integration kanban bridge --me codex --workspace-id my-workspace

Use "amq integration kanban <subcommand> --help" for details.
```

## amq receipts list

```text
$ amq receipts list --help
Usage:
  amq receipts list --me <agent> [--msg-id <id>] [--stage <stage>] [options]

Options:
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -msg-id string
        Filter by message ID
  -root string
        Root directory for the queue (default ".agent-mail")
  -stage string
        Filter by stage (drained, dlq)
  -strict
        Error on unknown handles (default: warn)
```

## amq receipts wait

```text
$ amq receipts wait --help
Usage:
  amq receipts wait --me <agent> --msg-id <id> [--stage <stage>] [--timeout <duration>] [options]

Options:
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -msg-id string
        Message ID to wait for (required)
  -poll-interval duration
        Polling interval (default 1s)
  -root string
        Root directory for the queue (default ".agent-mail")
  -stage string
        Stage to wait for (drained, dlq) (default "drained")
  -strict
        Error on unknown handles (default: warn)
  -timeout duration
        Maximum time to wait (0 = wait forever) (default 1m0s)
```

## amq session create

```text
$ amq session create --help
Usage:
  amq session create <name> [options]

Create a named session and its roster mailboxes under the base root.
Canonical names only ([a-z0-9_-]+). Existing sessions fail loudly; create is never silent.

Options:
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: warn)
```

## amq session list

```text
$ amq session list --help
Usage:
  amq session list [options]

List named sessions under the base root.
Canonical sessions are listed normally. Safe legacy roots are marked legacy_name with an exact --root hint.

Options:
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: warn)
```

## amq session resume

```text
$ amq session resume --help
Usage:
  amq session resume <name> [options]

Reconcile one existing AMQ session from the committed launch declaration.
Unknown sessions are never created. Non-interactive trust, stale identity, and Inspect uncertainty exit 6.

Options:
  -allow-fresh-fallback
        Allow fresh conversations when saved identities are stale
  -apply string
        Public ApplyRequestV1 file, or - for stdin (requires --json)
  -fresh
        Start fresh conversations for this launch
  -json
        Emit JSON output
  -launcher string
        Launcher backend (auto or commands) (default "auto")
  -me string
        Agent handle (or AM_ME)
  -placement string
        Public PlacementV1 JSON object (with --plan)
  -plan string
        Public LaunchIntentV1 file, or - for stdin
  -prepare
        Prepare the public launch intent without mutation (requires --plan and --json)
  -rebind
        Confirm a deliberate launcher binding change
  -request string
        Public PrepareRequestV1 file, or - for stdin (requires --json)
  -require-agent
        Require at least one agent to launch or attach
  -root string
        Root directory for the queue (default ".agent-mail")
  -session string
        Named session to launch or resume
  -strict
        Error on unknown handles (default: warn)
```

## amq route explain

```text
$ amq route explain --help
Usage:
  amq route explain --to <handle> [--project <project>] [--session <session>] --json

Explains canonical AMQ routing without sending a message.

Examples:
  amq route explain --to codex --json
  amq route explain --to qa --project project-b --session qa --json

Options:
  -from-cwd string
        Working directory to resolve .amqrc and auto-detection from
  -from-root string
        Source AMQ root to explain from
  -json
        Emit JSON output
  -me string
        Sender handle (or AM_ME)
  -project string
        Target peer project name
  -root string
        Source AMQ root to explain from (alias for --from-root)
  -session string
        Target session
  -to string
        Receiver handle
```

## amq integration symphony init

```text
$ amq integration symphony init --help
Usage:
  amq integration symphony init [--workflow <path>] --me <agent> [options]


Patches WORKFLOW.md hooks section with AMQ-managed hook fragments.
Use this as a small optional adapter, not a workflow control plane.

The managed fragment is marked with comments:
  # BEGIN AMQ MANAGED
  amq integration symphony emit --event <event> --me <agent> || true
  # END AMQ MANAGED

Existing user hook content is preserved.
Running init twice is idempotent (no duplication).

Options:
  -check
        Inspect without writing
  -force
        Rewrite even if fragment exists
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (pinned in hooks) (default ".agent-mail")
  -workflow string
        Path to WORKFLOW.md (default "WORKFLOW.md")
```

## amq integration symphony emit

```text
$ amq integration symphony emit --help
Usage:
  amq integration symphony emit --event <event> --me <agent> [options]


Emits an AMQ message for a symphony lifecycle event.
Designed to be called from WORKFLOW.md hook scripts.

Events: after_create, before_run, after_run, before_remove

Options:
  -event string
        Lifecycle event: after_create, before_run, after_run, before_remove (required)
  -identifier string
        Workspace key (default: basename of workspace)
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -workspace string
        Workspace path (default: current directory)
```

## amq integration kanban bridge

```text
$ amq integration kanban bridge --help
Usage:
  amq integration kanban bridge --me <agent> [options]


Runs a long-lived websocket bridge from the Kanban runtime state stream
to AMQ integration messages.

Experimental: depends on a preview WebSocket surface and may need updates
if the upstream runtime changes.

The bridge emits lifecycle/handoff notifications for task session changes.

Options:
  -json
        Emit JSON startup info
  -me string
        Agent handle (or AM_ME)
  -reconnect duration
        Reconnect delay after websocket disconnect (default 3s)
  -root string
        Root directory for the queue (default ".agent-mail")
  -url string
        Kanban runtime websocket URL (default "ws://127.0.0.1:3484/api/runtime/ws")
  -workspace-id string
        Workspace ID (appended as ?workspaceId=<id> query param)
```
