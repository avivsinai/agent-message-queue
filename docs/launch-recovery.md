# Managed launch recovery

Managed launcher backends can create a terminal resource before AMQ commits its
authoritative binding. AMQ records `meta/launch/journal.json` before `Create` so
a crash cannot turn that gap into permission to spawn a duplicate layout.

The journal is a private, mode-0600 recovery transaction. It binds the canonical
project and session-root paths, session name, backend profile, host and backend
instance identities, roster digest, semantic plan digest, launch nonce, and
creation time. It can also retain the validated candidate binding and final
conversation records needed to finish an interrupted commit. It contains no
message data and is never a launcher binding.

For a managed tmux join, the journal also records the authorized session
binding and one identity-checked delta per added window. Each delta carries
the session ID, name, nonce, epoch, window, and pane identity; AMQ revalidates
that session before the next window mutation. A crash after a delta is written
replays only the remaining deltas, so it cannot duplicate an added window or
write to a replacement session.

On the next launch, AMQ holds the session lease and follows these rules:

- A valid binding from the same launch generation is authoritative. AMQ
  completes any interrupted conversation writes and removes the journal.
- Without a matching binding, the journaled backend must implement `Reclaim`
  and prove the exact resource state within a bounded timeout.
- Proven absence permits recreation with the journaled, trusted plan.
- An adoptable resource must match the backend identity, profile, nonce, and
  full candidate binding. AMQ revalidates it immediately before committing the
  binding.
- Incomplete, foreign, unknown, changed, or unsupported recovery returns
  `action_required` and preserves the journal. Incomplete output includes the
  exact resource inventory known to the backend.

`Create` failures preserve the journal unless the backend returns the typed
definite-pre-create error that proves no resource was made. That definite
pre-create path also removes the pending execution tickets. Partial creation is
never adoptable. A Ghostty crash between window creation and binding persistence
leaves that window for manual close; AMQ never treats the gap as proven absence
and never creates a duplicate.

Cmux Create uses `--focus false` and restores the previously selected workspace
after Create, including on failure cleanup. Close restores the workspace that
was selected at Close time, skipping the workspace being closed. Focus then
Close can leave a neighbor workspace selected.

If an operator chooses to abandon recovery evidence, first inspect the exact
target without mutation:

```bash
amq cleanup --launch-journal --root <session-root> --dry-run --json
```

Removal requires confirmation or `--yes`. It removes only the unchanged
journal under the session lease. It never closes or adopts backend resources.

## Commands backend execution acknowledgement

An emitted command does not make a minted conversation resumable by itself.
Before AMQ prints the command, it writes a mode-0600 execution ticket for the
handle. The ticket binds the launch nonce, physical project, session-root and
working-directory identities, backend and adapter mode, AMQ and provider
executable identities, and the complete provider arguments and environment
overlay.

`coop exec` uses a private PID-preserving wrapper for emitted commands. The
wrapper checks the complete ticket immediately before provider execution. A
mismatch returns `action_required` and does not promote conversation state.
For a mint adapter, the wrapper promotes only the exact pending generation to
ready. For a capture adapter, it records `spawn_attempted`; only verified
provider start evidence can make the conversation ready.

If provider execution returns an error, AMQ changes the exact minted ready
generation back to pending with reason `spawn_failed`. A process kill in the
small interval after ready promotion and before the kernel replaces the AMQ
image can leave a ready record. The normal stale-conversation policy absorbs
this case; AMQ does not infer a second successful spawn.

## Codex daemon capture

A managed Codex seat uses an existing compatible app-server daemon when its
startup state is clear and its launch options have a verified native resume
contract. AMQ never starts the daemon or edits saved Codex settings. Otherwise
the existing embedded launch is retained, and launch output, remote bindings,
and `doctor --ops` share its `Buzz approvals: terminal only: <reason>` line.

The seat needs no Codex setting. Its command is `codex --remote
unix://<socket> -c check_for_update_on_startup=false resume -C <dir>
<thread>`. AMQ builds `<socket>` from the validated binding and spells it as
Codex computes its control socket: a set `CODEX_HOME` canonicalized, otherwise
`$HOME/.codex`. An explicit `--remote` equal to that socket attaches the TUI
to the local daemon with no embedded fallback, so the `-c` that turns the
update prompt off does not move the seat off the daemon. Codex treats the
socket as its local daemon only when it is the daemon's private link (a
symlink into the user-owned 0700 shared socket directory); AMQ checks that
shape and otherwise keeps the embedded launch. A managed requirement that turns
the update check on also keeps it embedded.

The native execution ticket owns a pre-spawn `thread/start` observation. Its
`codex_daemon_thread_v1` evidence binds the handle, launch nonce, provider
version, working directory, physical session identity, socket, and daemon
process identity. It is distinct from the embedded seat's notify evidence.
The thread's AMQ routing record replaces shell-environment overrides, and is
published while the exact generation and handle remain locked, before provider
execution is acknowledged.

Before creating the thread, AMQ durably records `identity_requested`. If the
request outcome is uncertain and no evidence was persisted, automatic execution,
replacement, and ticket removal refuse and preserve the state. Persisted evidence
is reused after a crash; a journal replay cannot reset the acquisition to pending.
The TUI resumes only the acquired thread ID, after startup and directory checks
succeed again. A failed provider exec retains that identity for recovery.
Ready conversation evidence remains bound to its physical session root. Routing
publication requires the recorded socket and daemon process; a daemon restart or
endpoint change requires explicit recovery rather than silent re-adoption.

A failure proven to occur before `thread/start` returns the ticket to `pending`;
thread creation and later protocol failures preserve uncertain progress. Native
thread startup uses the same 60-second contention budget as session naming.
Sibling seats of one launch wait for its lease in turn; each live holder gets
120 seconds, which covers that budget and its checks, and the wait starts again
when the lease passes to the next holder.
An incomplete acquired ticket refuses a replacement generation: retry the
original emitted command to reuse its thread.

When retrying an incomplete native launch cannot succeed, inspect the Codex
daemon for the known thread or an orphan, then inspect the exact cleanup target:

```bash
amq cleanup --launch-execution <handle> --root <session-root> --dry-run
amq cleanup --launch-execution <handle> --root <session-root> --yes
```

Cleanup explicitly abandons an unchanged `identity_requested`,
`identity_acquired`, or `spawn_attempted` ticket under its exact session lease and
handle lock. The dry run prints the known thread ID so it can be resumed by hand.
Acquired progress requires valid bound evidence; when capture committed before
the ticket's identity update, cleanup validates that capture and displays its
thread too. Removal compares ticket bytes, file identity, and evidence references
again under the lease. It leaves all Codex threads, evidence, routing records,
conversations, and launch journals untouched. Cleanup does not require the old
executable or daemon to remain available. Tickets and evidence from an earlier
supported Codex release stay readable after an upgrade, so inspection and
cleanup still work; only the current release runs a new or recovered
execution. Recovery refusals use exit 6 and name the retry or cleanup command.

Every execution attempt, including recovery, checks inherited native-backend
exclusions and runs the full startup admission (provider version, sign-in,
directory trust, daemon version, hook trust, terminal instructions, managed
update requirements, and the seat's `--remote` matching the verified local
socket) on the bound daemon before publishing routing. A recovered
execution that fails admission keeps its acquired thread; it never creates a
replacement.

A conversation resumes on the backend that created it; launch never moves it
implicitly. A conversation captured by notify resumes embedded. One whose
capture is `codex_daemon_thread_v1` resumes on the daemon when the launch
options are admitted, the daemon is eligible, the daemon listens at the socket
that captured the thread, and it reads the thread in the launch directory at
the verified version on the same session. The socket path is fixed by the
canonical `CODEX_HOME`. A restarted daemon at that socket is a successor; a
daemon at another socket is not, even when it answers with the same thread.
Static admission, including executable containment, completes before any probe
runs the provider or reads the daemon. Any other outcome (a changed endpoint,
no daemon, an option, sign-in, trust, or version that is not admitted, or a
thread the daemon does not prove) refuses with exit 6 and names the cause and
the fix, because another backend or rollout store may hold a different thread
under the same ID; `--allow-fresh-fallback` starts a new conversation instead.
Codex 0.167.0 exposes no stable persistence-store identity, so AMQ cannot
distinguish a same-socket successor that uses a different SQLite store when it
returns the same thread ID and working directory.
A daemon resume ticket names its thread from creation and asks the
daemon to create nothing: it skips `identity_requested`, a failed verification
leaves it `pending`, and the TUI starts only after `codex_daemon_thread_v1`
evidence for the new generation binds the same thread and routing is published
again. The conversation record keeps its original evidence. Cleanup and retry of
an incomplete resume follow the rules above. When the thread is loaded
and another client is subscribed to it or a turn runs, Codex keeps its running
settings and warns that the resume settings are ignored.

Prepare authorizes the selected native or embedded identity contract. A daemon
or sign-in change that changes that selection before Apply requires a new
Prepare; Apply does not execute a different plan under the earlier authorization.

## Native resume settings

The Codex 0.167.0 TUI forwards `--model <value>`,
`--sandbox read-only|workspace-write`, and `--ask-for-approval on-request` on
resume, and retains `--no-alt-screen` for terminal presentation. That the daemon
applies the forwarded settings is not yet verified live. Native admission fixes
these sandbox and approval values independently of the embedded argument grammar. Settings keep
their order in the execution ticket, followed by its acquired thread ID and any
initial prompt.

A caller's command-line configuration overrides, such as
`-c approvals_reviewer=...` or `-c model_reasoning_effort=...`, and `--search`
keep the embedded path, because their effect on a daemon TUI is not verified.
Those launches retain the original arguments and explain the terminal-only
approval capability. Wrappers, environment overlays, bypass
options, and unverified or duplicate options also retain the embedded path.
AMQ does not translate reviewer choices into protocol settings or alter saved
Codex configuration.
