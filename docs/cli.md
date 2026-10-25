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
- [amq peer](#amq-peer)
- [amq who](#amq-who)
- [amq route](#amq-route)
- [amq doctor](#amq-doctor)
- [amq shell-setup](#amq-shell-setup)
- [amq completion](#amq-completion)
- [amq thread open](#amq-thread-open)
- [amq presence set](#amq-presence-set)
- [amq presence list](#amq-presence-list)
- [amq dlq list](#amq-dlq-list)
- [amq dlq read](#amq-dlq-read)
- [amq dlq retry](#amq-dlq-retry)
- [amq dlq purge](#amq-dlq-purge)
- [amq wake check](#amq-wake-check)
- [amq wake config](#amq-wake-config)
- [amq wake repair](#amq-wake-repair)
- [amq wake restart](#amq-wake-restart)
- [amq wake recover-owner](#amq-wake-recover-owner)
- [amq wake retire](#amq-wake-retire)
- [amq wake dialog](#amq-wake-dialog)
- [amq wake install-dialog-hook](#amq-wake-install-dialog-hook)
- [amq wake uninstall-dialog-hook](#amq-wake-uninstall-dialog-hook)
- [amq coop init](#amq-coop-init)
- [amq coop exec](#amq-coop-exec)
- [amq coop install-hooks](#amq-coop-install-hooks)
- [amq coop uninstall-hooks](#amq-coop-uninstall-hooks)
- [amq coop hook](#amq-coop-hook)
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
- [amq peer pair](#amq-peer-pair)
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
  cleanup      Remove selected tmp, wake quarantine, launch recovery, or empty mailbox artifacts
  watch        Wait for new messages (uses fsnotify)
  drain        Drain new messages (read, move to cur, emit receipts)
  monitor      Combined watch+drain for co-op mode
  reply        Reply to a message (auto thread/refs)
  dlq          Dead letter queue management
  wake         Background waker (TIOCSTI injection)
  upgrade      Upgrade amq to the latest release
  env          Output shell commands to set environment variables
  coop         Co-op mode setup (init, exec, hooks)
  swarm        Claude Code Agent Teams integration (join, tasks, bridge)
  integration  Optional interoperability adapters
  receipts     Message delivery receipts
  session      Create, list, and resume named AMQ sessions
  peer         Pair a peer host through amq-bridge
  who          Show sessions and agents in current project
  route        Explain canonical routing
  doctor       Verify installation and configuration
  shell-setup  Output shell aliases (amc/amx/amg/amy)
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
Detects Claude, Codex, Cursor, Grok, and pi through their adapter capability probes.
Launcher detection is probe-only; setup never calls a launcher backend.

Options:
  -agents string
        Comma-separated agents: an adapter (claude) or handle=adapter (reviewer=claude)
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
        Error on unknown handles (default: send and reply warn)
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
        Message priority: urgent, normal, low (default when --kind is set: low for status, normal otherwise)
  -project string
        Target peer project name (delivers to a peer project's inbox)
  -refs string
        Comma-separated related message ids
  -root string
        Root directory for the queue (default ".agent-mail")
  -session string
        Target session (delivers to a different session's inbox)
  -strict
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
```

## amq read

```text
$ amq read --help
Usage:
  amq read --me <agent> --id <msg_id> [--session <name>] [options]

Read a message by id.

If the message is in inbox/new, AMQ only moves it to inbox/cur after parse and header validation succeed.
If the message in inbox/new is corrupt or malformed, AMQ moves it to DLQ and emits a dlq receipt.

--id also resolves bridged messages by their header ID: messages delivered by amq-bridge keep
the header ID but are stored under a transfer filename (xfer-<host>-<transfer>.md). If the ID
matches more than one stored message, the command fails with a general error (exit 1) naming
both paths; a missing message exits 3.

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
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
```

## amq trace

```text
$ amq trace --help
Usage:
  amq trace <message-id> [options]

Join current on-disk evidence for one message without mutating the queue.

It reports message copies, route fields, visible delivery artifacts, DLQ entries,
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
  amq cleanup [--tmp-older-than <duration>] [--wake-quarantine-older-than <duration>] [--launch-journal --root <session-root>] [--mailbox <handle> --root <root>] [--dry-run] [--yes] [options]

--mailbox uses --root as given and removes <root>/agents/<handle>, with its
read history (inbox/cur), outbox, and receipts. It refuses (exit 6) while
inbox/tmp, inbox/new, any dlq leaf, or extensions/ holds an entry, or while a
live or uncertain wake holds the handle. One lock (meta/removing.lock)
serializes removals under a root; a second run exits 6 (busy). A dry run,
and a run that finds nothing to remove, write nothing. The mailbox moves to
meta/removing/<handle>-<nonce>, is checked, checked again after 250 ms, and
is put back if anything arrived. A delivery that stalls longer than that
between opening the inbox and publishing can still fail or be removed with
the mailbox. A later run removes a clean tree an interrupted run left there,
and restores one that holds mail to agents/<handle> so it can be drained.
meta/config.json is not changed: a later local send to the handle recreates
the mailbox and delivers. Remove the handle from the config to make send warn
(or refuse with --strict).
JSON: absent = no live mailbox when the run started; removed = this run
deleted the live mailbox; restored = this run moved a leftover back to
agents/<handle>, which exists at the end; recovered = leftovers deleted;
preserved = where a tree was kept; refusal = why the run exited 6.

Options:
  -dry-run
        Show what would be removed without deleting
  -json
        Emit JSON output
  -launch-journal
        Remove one exact stuck managed-launch recovery journal
  -mailbox string
        Remove one agent mailbox under --root when it holds no unread mail, dead letters, extension data, or live wake
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
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

--id also resolves bridged messages by their header ID (stored under a transfer filename). If the ID
matches more than one stored message, the command fails with a general error (exit 1) naming both
paths; a missing message exits 3.
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
        Message ID to reply to (a bridged message may be addressed by its header ID even though it is stored under a transfer filename)
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
        Error on unknown handles (default: send and reply warn)
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

Hold by priority (default off): --hold-normal and --hold-low delay the
  first doorbell for routine mail. Its deadline is the inbox file's mtime
  plus its priority's hold; missing priority counts as normal. A later
  message can only pull the deadline earlier. A future-dated mtime gets
  no hold. Urgent mail skips the hold, even behind a parked cohort.
  Ordinary urgent mail still follows debounce and input deferral; mail
  with the configured interrupt label uses the interrupt path. After the
  first attempt, the configured retry policy applies. A drain can take held
  mail, up to its limit (default 20, filename order). The hold never acks
  or deletes mail or guarantees consumption time.
  Example: --hold-normal 5m --hold-low 30m; use --priority urgent for
  time-critical unblocking requests or verdicts.

Settings (hold, debounce, preview, bell, input deferral, interrupt notice,
  inject timeout, self-upgrade) live in <root>/agents/<me>/.wake.settings,
  over the machine file ~/.amq/wake.settings (every wake of this user on this
  machine), over the built-in defaults, key by key. Explicit settings flags on
  a fresh start write their keys to the agent file. A resume (self-upgrade or
  restart) reads the file and ignores settings flags in argv; if the file is
  absent it writes them once. Repair, coop exec and keepalive pass no settings
  flags and use the stored files. A refused machine file never stops a start;
  the wake runs without it. Change running wakes with amq wake config
  (--machine for all).

Interrupt notices (default on): urgent messages tagged with label "interrupt"
  trigger an interrupt notice. Ctrl+C injection is opt-in with
  --interrupt-cmd ctrl-c; it sends real SIGINT to the foreground process
  group and can interrupt or crash the agent.

Safety: raw, paste, --inject-cmd, --inject-via, and opt-in interrupt Ctrl+C
  can activate a focused permission/approval dialog. With the Claude Code
  dialog hook (amq wake install-dialog-hook), the wake writes no input while
  AskUserQuestion, ExitPlanMode, or a permission dialog is open, and rings
  about 2s after it closes; after a permission dialog it rings when the turn
  ends or the person types. A marker older than 10m gets attention output,
  still no input. --harness codex and --harness cursor guard their approval
  prompts until the turn ends. Dialogs without a hook (workspace trust,
  pickers, MCP elicitation, Cursor prompts before a tool call) are not
  guarded. amq wake dialog is the hook receiver; amq wake
  uninstall-dialog-hook removes the hooks. Use none when AMQ must enforce
  zero synthetic input; stderr output may scribble until redraw.

Self-upgrade: eligible co-op wakes observe their stable launch symlink and
  replace the running image only with a strictly newer installed AMQ while
  preserving PID, terminal ownership, and unread work. Disable at start with
  --no-self-upgrade or AMQ_WAKE_NO_SELF_UPGRADE=1, or live with amq wake config
  --self-upgrade=false (--machine for every wake). A live true does not enable
  a wake started with --no-self-upgrade; restart it to enable.

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
  -harness string
        Program the wake types into (claude, codex, agy, pi, cursor, grok, or other for generic behavior); amq coop exec sets it. Without it, a handle containing claude or codex picks raw typing
  -hold-low duration
        Hold the first doorbell for low-priority mail up to this long (0 = no priority hold)
  -hold-normal duration
        Hold the first doorbell for normal-priority mail up to this long (0 = no priority hold)
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
  -self-upgrade
        Allow automatic replacement by a newer installed AMQ image (false stops it live; true needs a wake started without --no-self-upgrade) (default true)
  -strict
        Error on unknown handles (default: send and reply warn)
```

## amq upgrade

```text
$ amq upgrade --help
Usage:
  amq upgrade [--all] [-y]

Downloads and installs the latest amq release from GitHub.
When amq is installed via Homebrew or Scoop, amq upgrade delegates to the package manager instead of overwriting it; pass -y to run the delegate.
--all upgrades companion binaries in the raw or resolved executable directory of a direct-install amq, or in ~/.local/bin; a running amq-keepalive or amq-remote is never killed.
Retire live wakes started by the previous binary before its install directory is removed.
If wake check reports binary_dir_gone, run amq doctor --ops --fix-wake-locks.

Options:
  -all
        Also upgrade present companion binaries (amq-keepalive, amq-bridge, amq-acp, amq-remote, amq-owner-sign, amq-board)
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
  init             Initialize project for co-op mode
  exec             Set up co-op mode and exec into an agent
  install-hooks    Install the co-op Claude Code hooks for AM_ME sessions
  uninstall-hooks  Remove the co-op Claude Code hooks
  hook             Claude Code hook receiver: print the co-op preamble

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

## amq peer

```text
$ amq peer --help
amq peer - Operator-only pairing

amq peer pair execs amq-bridge peer add and prints the local pairing line.
It writes nothing until --confirm-fingerprint matches the peer line.
Agents do not run peer pair, peer add, or peer remove.

Subcommands:
  pair  Pin a peer line and print the local line

Examples:
  amq peer pair --confirm-fingerprint abcdefghijklmnopqrstuvwxyz --allow-in codex 'amq1:box:...'

Use "amq peer <subcommand> --help" for details.
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
        Error on unknown handles (default: send and reply warn)
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
  - Skill installation (Claude Code / Codex / Grok Build / agy)

With --ops, also checks runtime health:
  - Queue depth and oldest unread per agent
  - DLQ count and age
  - Presence freshness
  - Git worktree mailbox divergence
  - Wake lock health
  - Integration hints (Kanban, Symphony)

With --migrate-home, move <legacy>/launch to ~/.amq/state/launch in one
rename, so launches keep their trust. It refuses (exit 6) and changes
nothing when an amq on PATH is older than 0.97.0 or its version cannot be
read, when ~/.amq/state/launch already exists, or when the two directories
are on different filesystems. <legacy> is $XDG_STATE_HOME/amq, else the
user config directory (macOS, Windows), else ~/.local/state/amq.

It also moves ~/.amq-keepalive to ~/.amq/keepalive, changes --registry in
the supervisor plist and the AMQ SessionStart hooks that run the moved
script, and bootstraps the supervisor job again. First stop the job
(launchctl bootout, keep the plist), the amq-remote login service, and
every amq-keepalive and amq-remote process. It refuses while one runs,
while a unit is loaded or a lock is held, or when an amq, amq-keepalive or
amq-remote older than 0.97.0 can run. Each part runs when its own checks
pass. A rerun finishes an interrupted move.

--json requires --dry-run or --yes.

Options:
  -base-root string
        Config-authority root for an explicit session --root
  -dry-run
        With --migrate-home, show the move without changing anything
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
  -migrate-home
        Move launch trust and the keepalive home from their legacy locations into ~/.amq
  -ops
        Include runtime operational checks
  -root string
        Exact AMQ root to inspect or repair
  -yes
        With --migrate-home, skip the confirmation prompt
```

## amq shell-setup

```text
$ amq shell-setup --help
Usage:
  amq shell-setup [options]

Outputs shell aliases for quick co-op session management.

Defines four functions (names customizable via flags):
  amc [session] [flags]  → amq coop exec [--session <s>] claude [flags]
  amx [session] [flags]  → amq coop exec [--session <s>] codex -- --dangerously-bypass-approvals-and-sandbox [flags]
  amg [session] [flags]  → amq coop exec [--session <s>] grok [flags]
  amy [session] [flags]  → amq coop exec [--session <s>] agy [flags]

Usage:
  eval "$(amq shell-setup)"           # Add to current shell (default names)
  amq shell-setup --shell fish         # Fish shell output
  amq shell-setup --claude-alias cc --codex-alias cx --grok-alias gk --agy-alias ag  # Custom names

Examples after setup:
  amc                    # Start Claude Code (default session)
  amc feature-x          # Isolated session for feature-x
  amx feature-x          # Codex in same isolated session
  amg feature-x          # Grok in same isolated session
  amy feature-x          # agy in same isolated session

Options:
  -agy-alias string
        Function name for Antigravity CLI (agy) shortcut (default "amy")
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

## amq thread open

```text
$ amq thread open --help
Usage:
  amq thread open [--all-sessions] [--owed-by <handles>] [--owed-to <handles>] [--kind <kinds>] [--max-age <dur>] [options]

List open review requests, questions, and todos per thread.
An obligation closes when its owner replies with the expected kind or a closing label.
Read-only: it never moves a message, writes a receipt, or reads a body.

Options:
  -all-sessions
        Scan every canonical session under the base root
  -json
        Emit JSON output
  -json-schema int
        JSON schema version: 1 (requires --json) (default 1)
  -kind string
        Comma-separated opener kinds: review_request, question, todo
  -limit int
        List at most N obligations, oldest first, after the whole window is reduced (0 = no limit)
  -max-age string
        Ignore openers older than this (e.g. 14d, 36h; 0 = read every file) (default "14d")
  -me string
        Agent handle (or AM_ME)
  -owed-by string
        Comma-separated handles that owe a reply
  -owed-to string
        Comma-separated handles that are owed a reply
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
```

## amq wake config

```text
$ amq wake config --help
Usage:
  amq wake config [--me <agent>] [--machine] [setting flags] [--unset <key>]... [--reset] [--wait] [options]

Show or change the live settings of the agent's wake.

Each setting comes from the agent's file (.wake.settings), else the machine file
(~/.amq/wake.settings, shared by every wake of this user on this machine), else the
built-in default. With no setting flag and no --unset, print each setting, its value,
and which layer it comes from, then what the running wake did with the files.
Otherwise validate the whole new set and store it. A running wake applies a change
within a few seconds. Flags fixed for a running wake (--inject-via, --inject-mode,
--inject-arg, --inject-cmd, --interrupt-cmd, --retry-until) are refused.
An invalid file is shown as refused and cannot be changed with set or --unset;
--reset replaces it with exactly the given setting flags. --reset with no setting
flag means no overrides: the agent's file then falls through to the machine file and
the defaults. --unset returns a key to the next layer (machine or default).

With --machine, set, --unset and --reset change the machine file; --me is optional
there and, when given, adds that agent's wake status. --machine --wait needs --me and
waits on that one wake; other wakes are not tracked. --wait exits 1 when the wake
refuses the file this command changed; a refusal of the other file is a warning
and exit 0.

While the agent file is absent and the running wake does not report live settings
(unreported) past a short startup grace, set and --unset exit 6; the wake may be an older
image, have a failed status write, or be a resume still storing its command-line settings
in the file. Restart the wake or use --reset with the full set.

Options:
  -bell
        Ring terminal bell on new messages
  -debounce duration
        Debounce window for batching messages (default 250ms)
  -defer-while-input
        Best-effort: defer non-interrupt injection while terminal input appears active (default true)
  -hold-low duration
        Hold the first doorbell for low-priority mail up to this long (0 = no priority hold)
  -hold-normal duration
        Hold the first doorbell for normal-priority mail up to this long (0 = no priority hold)
  -inject-timeout duration
        Timeout for one --inject-via command (default 5s)
  -input-max-hold duration
        Maximum time to defer one wake injection (0 = no hold) (default 15s)
  -input-poll-interval duration
        Polling interval while waiting for quiet terminal input (default 200ms)
  -input-quiet-for duration
        Quiet window before deferred injection (advisory only on Linux; tty atime granularity is ~8s) (default 1.2s)
  -interrupt
        Enable interrupt injection for urgent interrupt messages (default true)
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
  -machine
        Show or change the machine settings (~/.amq/wake.settings) shared by every wake of this user
  -me string
        Agent handle (or AM_ME)
  -preview-len int
        Max subject preview length (default 48)
  -reset
        Replace the settings file with exactly the given setting flags (none = no overrides)
  -root string
        Root directory for the queue (default ".agent-mail")
  -self-upgrade
        Allow automatic replacement by a newer installed AMQ image (false stops it live; true needs a wake started without --no-self-upgrade) (default true)
  -strict
        Error on unknown handles (default: send and reply warn)
  -timeout duration
        Maximum time for --wait (0 = wait forever) (default 1m0s)
  -unset value
        Return a setting to the next layer (machine, then default) by key, e.g. hold_normal (repeatable)
  -wait
        Wait until the running wake has applied the settings files
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
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
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
--takeover is for an explicit take by another surface of the same user: it skips only the injector match; the generation, lock and owner checks still apply.

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
        Error on unknown handles (default: send and reply warn)
  -takeover
        Retire a wake another surface started (a take): match the saved target's injector instead of --inject-via; requires --if-generation
```

## amq wake dialog

```text
$ amq wake dialog --help
Usage:
  amq wake dialog [--me <agent>] [--root <path>] [--harness claude|codex|cursor] open|close

Claude Code hook receiver for the wake dialog guard; amq wake install-dialog-hook
installs it. Reads the hook input JSON on stdin.

open writes <root>/agents/<me>/.wake.dialog; the wake writes no terminal input while
it is present. close removes it; a close with a tool_use_id removes only the marker
of that tool call, and a close with unreadable input keeps it. A close with empty
stdin removes it. With --harness codex or cursor only a turn or session event
removes it; for codex without AM_* the seat is the one amq coop exec recorded for
the hook's session_id.
Always exits 0 once the arguments parse and prints nothing.

Options:
  -harness string
        Hook input of claude, codex or cursor (default "claude")
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default: AM_ROOT, else .amqrc discovery from the hook's cwd)
```

## amq wake install-dialog-hook

```text
$ amq wake install-dialog-hook --help
Usage:
  amq wake install-dialog-hook [--dry-run] [--harness claude|codex|cursor|all]

Install the wake dialog guard hooks in ~/.claude/settings.json, with --harness
codex in $CODEX_HOME/hooks.json (Codex runs them once you trust them in /hooks),
or with --harness cursor in ~/.cursor/hooks.json (Cursor reads it when a session
starts; it holds the doorbell from a tool call to the end of that turn).

While Claude Code shows AskUserQuestion, ExitPlanMode or a permission dialog,
the hooks keep a marker in the agent's mailbox and every wake of that agent
holds its doorbell, so injected input cannot answer the dialog. The hooks find
the agent through AM_ME and AM_ROOT in the Claude Code environment, which
amq coop exec and amq launch set; without them the guard does nothing.

Running again keeps the same entries and replaces entries of another amq
binary. Other settings and hooks are not changed. Running Claude Code
sessions pick the change up through the settings watcher; if /hooks does not
list the AMQ_DIALOG_HOOK entries, restart the session.

Options:
  -dry-run
        Print the hooks without changing the settings file
  -harness string
        claude, codex, cursor, or all (each harness whose home exists) (default "claude")
```

## amq wake uninstall-dialog-hook

```text
$ amq wake uninstall-dialog-hook --help
Usage:
  amq wake uninstall-dialog-hook [--harness claude|codex|cursor|all]

Remove the wake dialog guard hooks (AMQ_DIALOG_HOOK entries) from
~/.claude/settings.json, $CODEX_HOME/hooks.json with --harness codex, or
~/.cursor/hooks.json with --harness cursor.
Other settings and hooks are not changed.

Options:
  -harness string
        claude, codex, cursor, or all (each harness whose home exists) (default "claude")
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
  amq coop exec agy                                 # Antigravity CLI, caller flags forwarded as-is
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

## amq coop install-hooks

```text
$ amq coop install-hooks --help
Usage:
  amq coop install-hooks [--dry-run]

Install the co-op hooks in ~/.claude/settings.json.

SessionStart (startup, resume, clear, compact) gives the agent its co-op
preamble: handle, session, peers, unread count, and whether a wake rings the
terminal. Stop blocks a stop once for unread mail that no wake doorbell will
announce. The hooks run only in sessions with AM_ME set, which amq coop exec
and amq launch set; other Claude Code sessions start no amq.

Running again keeps the same entries and replaces entries of another amq
binary. Other settings and hooks are not changed. It also lists old project
hook entries that run the co-op scripts; remove them, or both run.

Options:
  -dry-run
        Print the hooks without changing the settings file
```

## amq coop uninstall-hooks

```text
$ amq coop uninstall-hooks --help
Usage:
  amq coop uninstall-hooks

Remove the co-op hooks (AMQ_COOP_HOOK entries) from ~/.claude/settings.json.
Other settings and hooks are not changed.
```

## amq coop hook

```text
$ amq coop hook --help
Usage:
  amq coop hook session-start|stop

Claude Code hook receivers for the co-op hooks; amq coop install-hooks installs them.
They read AM_ME, AM_ROOT and the session pin from the environment, never read
.amqrc, and always exit 0 once the arguments parse.

session-start prints the co-op preamble as SessionStart additionalContext:
the agent's handle, session and peers, its unread count, and whether a wake
rings this terminal. Without AM_ME or an absolute AM_ROOT, with a session pin
that does not match AM_ROOT, or when the mailbox does not exist, it prints
nothing.

stop blocks the stop once for unread mail it has not reported yet. Under a
live wake it counts only mail created at or before the wake's startup
baseline, because the wake rings for later mail. It allows the stop after
5 blocks in one chain, and prints a systemMessage when the context conflicts
or the mailbox cannot be read.
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
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
```

## amq session list

```text
$ amq session list --help
Usage:
  amq session list [options]

List named sessions under the base root.
Canonical sessions are listed normally. Safe legacy roots are marked legacy_name with an exact --root hint.
--json adds conversations[] per session: {handle, provider, id, state, source}, the harness sessions that exact AMQ records tie to an agent. source is launch (managed launch record), codex-thread (the Codex thread coop exec created), exec-pid (the Claude Code process coop exec became, joined with Claude's sessions/<pid>.json), or remote (an amq-remote binding of that process). state is ready, revoked (a later coop exec replaced it; listed for 7 days), or uncertain. Nothing is inferred from a cwd.

Options:
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: send and reply warn)
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
        Error on unknown handles (default: send and reply warn)
```

## amq peer pair

```text
$ amq peer pair --help
Usage:
  amq peer pair [line] [options]

Exec amq-bridge peer add, then print the local pairing line.
Writes nothing until --confirm-fingerprint matches the peer line.

Options:
  -allow-in string
        comma-separated inbound handles
  -confirm-fingerprint string
        26-character fingerprint that must match before the pin is written
  -from-line string
        pairing line from amq-bridge peer line
  -json
        Emit JSON output
  -me string
        Agent handle (or AM_ME)
  -root string
        Root directory for the queue (default ".agent-mail")
  -strict
        Error on unknown handles (default: send and reply warn)
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
