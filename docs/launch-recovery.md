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
