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
definite-pre-create error that proves no resource was made. Partial creation is
never adoptable.

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
