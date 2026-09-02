# Wake state invariants

AMQ represents wake lifecycle state with several files and sockets under an
agent directory. These artifacts are related by generation, target digest, and
owner identity, but they do not have the same owner or commit point. This map
records the current boundaries that readers, writers, cleanup, and recovery
code rely on.

## `.wake.lock`: live wake claim

**Owns:** process ownership and deduplication for one wake generation. The
record binds the root and agent to a PID, process start token, boot ID,
generation, wake mode, and, when applicable, cooperative owner and target
digest.

A cooperative owner claim is scoped to the OS session that created it. The
live owner (typically `coop exec --wake-inject-via`) releases only its own
exact claim by presenting the inherited `AMQ_WAKE_OWNER` token via
`amq wake recover-owner`; `recover-owner` does not require that token when the
recorded owner is conclusively dead. There is no force mode: an unknown, live,
or legacy owner state is preserved rather than guessed at. The token is
generated and consumed by AMQ and must never be set manually.

**Commit domain:** the lock becomes authoritative when `.wake.lock` is linked
into the retained agent directory. Readers verify both its content and file
identity. Removal is conditional on the exact inspected claim still matching;
a replacement lock is not selected for cleanup. For managed retirement, exact
lock removal is the commit point: pre-commit failure is `refused`; post-commit
target/state residue cannot undo retirement and is reported as
`retired_with_residue` rather than a refusal.

**Independence invariant:** ownership changes only when the lock publication or
exact-claim removal commits. Treating a target update, preparation marker, ready
file, or socket event as the ownership commit would allow non-ownership state to
create, replace, or retire a live claim. Combining cleanup domains would also
allow cleanup for an old generation to remove a replacement.

Source anchors: `wake_lock.go`, `wake_lock_at_unix.go`, and
`wake_owner_storage_unix.go`.

## `.wake.target`: injector behavior contract

**Owns:** the resolved `inject-via` executable, its ordered fixed arguments,
root, agent, creation metadata, and optional cooperative owner. Its serialized
digest is stored in the corresponding lock.

**Commit domain:** the target is written and atomically renamed independently
of `.wake.lock`. During authoritative acquisition, the installed target is
re-read and directory-synced before lock publication. During release, target
cleanup occurs only after lock removal and only when file identity, raw bytes,
parsed value, and digest still match the captured target snapshot.
Managed retirement follows the same retained-directory capability boundary:
after exact lock removal commits, it removes only the captured matching target
and its exact corresponding bound state. Unbound projection state is removed
only when its target-section digest matches that target. Mailbox contents and
replacement target/state artifacts are preserved.

**Independence invariant:** changing injector behavior is not an ownership
transition. If target and lock shared one commit domain, a target replacement
could implicitly replace ownership, and a failure between target installation
and lock publication could not be represented without inventing a new partial
state. If their cleanup domains were combined, release of an old claim could
delete a newer target.

Source anchors: `wake_target.go` and `wake_owner_storage_unix.go`.

## `notification-attempts.jsonl`: durable notifier lifecycle audit

**Owns:** the audit of one AMQ notification attempt for one message cohort.
It does not own inbox delivery, provider consumption, terminal ownership, or
the in-memory doorbell schedule. Schema 2 writes one prepared `attempt` event
followed by ordered result states: `deferred`, `retried`, `accepted`, or
`failed`. A deferred/retried sequence keeps the same AttemptID and cohort;
`accepted` and `failed` are terminal. A later unread notification gets a new
AttemptID. Schema 1 prepared/result records remain readable.

**Commit domain:** each event is one O_APPEND JSON line in the single journal,
with the existing rotation lock protecting rotation. The ledger is best effort:
a journal write failure never blocks wake delivery. Readers fold only
contiguous, valid lifecycle sequences. An out-of-order or contradictory
sequence is `invalid`, never accepted evidence.

**Transport taxonomy:** an external injector may claim provider dispatch only
with stderr marker `AMQ_INJECT_PROGRESS=accepted` and exit zero. A deferred
marker is a pre-dispatch busy result and produces no attention or acceptance
claim. An uncertain marker wins over every other marker and enters recovery.
Timeout is failed even when stderr contains deferred. A bare legacy exit zero
is byte-write/uncertain evidence, not provider acceptance. Raw TIOCSTI remains
`written` byte evidence only. `amq doctor --ops` projects the same folded state
and warns for deferred/retried attempts.

Source anchors: `internal/notificationattempt/ledger.go`, `wake.go`, and
`doctor_ops.go`. Raw-mode injection carries byte-level evidence only
([#703](https://github.com/avivsinai/agent-message-queue/issues/703)); provider
acceptance requires the injector marker protocol.

Retirement results are exactly `refused`, `retired`, and
`retired_with_residue`. The last is exit-0 success with a warning: ownership is
already retired, but target/state cleanup failed or was skipped. The next
acquisition converges conclusively ownerless residue through the
quarantine/supersession path. Owner-bearing residue follows owner recovery;
malformed ownership remains inspection-only.

## `.wake.<lock|target>.quarantined.<timestamp>`: preserved blocked input

**Owns:** no wake authority. A quarantine artifact preserves the exact inode
and bytes of a blocked lock or orphan target while freeing the live pathname
for a fresh acquisition decision.

**Commit domain:** under the lifecycle guard and retained agent-directory FD,
AMQ revalidates identity and raw bytes, renames without replacement, syncs the
directory, and reopens the quarantine name to verify the moved inode and bytes.
The caller then discards every pre-move observation and starts acquisition from
a fresh inspection. The lock case is deliberately narrow: only aged,
syntax-invalid/empty/truncated, same-owner, regular 0600 generic files qualify.
Fresh creating, 0400, owner-shaped, unreadable, oversized, special, and
valid-JSON wrong-known-type locks are preserved at `.wake.lock`. With no lock,
a targetless acquisition may quarantine an exact readable regular 0600 orphan
target only when its bytes are conclusively ownerless. Clean owner-bearing and
malformed owner-shaped targets stay at the live pathname. `recover-owner` may
remove the clean form only after proving its exact owner dead; malformed
ownership remains inspection-only.

**Independence invariant:** quarantine is preservation, not cleanup or
ownership. Exact names are reported by `doctor --ops` independently of lock
discovery only after a complete root-wide scan. Any root or agent-directory
open/validation failure produces `wake_quarantine.error` and blocks explicit
quarantine cleanup rather than exposing a partial result. Ordinary tmp cleanup
never removes them. Explicit
`--wake-quarantine-older-than` cleanup captures and revalidates exact identity
and bytes under the same guard before `unlinkat` and directory sync; ambiguity
or replacement preserves the artifact.

Source anchors: `wake_quarantine_unix.go`, `wake_unix.go`, and `cleanup.go`.

## `.wake.prepared`: generation preparation marker

**Owns:** the fact that one exact lock generation and target digest completed
wake preparation. It does not own the wake claim or caller admission.

**Commit domain:** the marker is published under the lifecycle guard only after
the current lock still matches the expected generation and its lock/target
relationship validates. Readers distinguish absence, a marker for a stale
generation, and a valid marker for the current generation. Cleanup uses a
captured generation-file snapshot and removes only that unchanged marker.

**Independence invariant:** lock acquisition does not imply preparation, and a
prepared marker from an earlier generation does not prepare its replacement.
Folding preparation into the lock or target would erase the observable
difference between not yet prepared, stale prior-generation evidence, and a
failed or torn marker publication.

Source anchors: `wake_prepared_unix.go`, `wake_ready_unix.go`, and
`wake_lock_at_unix.go`.

## Caller ready files: caller admission receipt

**Owns:** notification to one waiting caller that the expected wake generation
is ready. A ready file carries the generation and target digest, but its path
and lifetime belong to that caller-facing handshake rather than to the retained
agent state.

**Commit domain:** a ready file is published only after the current lock,
target, requested owner, and, for an existing wake, `.wake.prepared` validate.
Publication captures the installed file identity and bytes. Error cleanup
removes only that unchanged publication; a file that was replaced after
publication is preserved.

**Independence invariant:** caller admission is neither wake ownership nor wake
preparation, and consuming or replacing a caller's receipt must not mutate the
retained claim. Combining ready files with owner state or output delivery would
make caller-local cleanup capable of removing shared lifecycle evidence and
would make readiness indistinguishable from successful notification delivery.

Source anchors: `wake_ready_unix.go` and `wake_prepared_unix.go`.

## `.wake.lifecycle.lock`: mutation serialization

**Owns:** exclusion between cooperating mutations and compound validations in
one retained agent directory. It contains no wake generation or lifecycle
decision.

**Commit domain:** callers acquire an exclusive `flock`, validate that the
opened guard is still the guard at its pathname, perform a bounded group of
directory-relative reads or mutations, and release the guard when the callback
returns. The lock order is lifecycle guard, then wake lock/target/ready access.
The guard is released before child waits, pidfd exit waits, and control waits.

**Independence invariant:** serialization is not persisted lifecycle state.
Putting generation or readiness data in the guard, or retaining it across a
wait, would change the lock order and could block the child or control path that
must acquire the same guard to finish publication or exact-generation cleanup.

Source anchor: `wake_lifecycle_guard_unix.go`.

## Control and reload endpoints: ephemeral authenticated transport

**Owns:** a live process endpoint for generation-bound control traffic. The
socket listener and authenticated peer exchange are runtime capabilities, not
durable proof that a wake exists.

**Commit domain:** on Darwin, the control socket name is derived from the
canonical root, agent, and generation and the corresponding path is advertised
by the lock only when the resume advertisement validates. Requests revalidate
the live lock and generation. Cleanup is tied to the released claim's recorded
socket path. On Linux, the current `.wr.*` reload endpoint is a separate,
unadvertised, refusal-only seam; it authenticates the lock, process, owner,
peer, and session and is retired by exact socket identity.

**Independence invariant:** a pathname or socket inode does not own the durable
claim. Persisting listener lifetime as owner state would let a stale endpoint
stand in for a live generation, while treating lock cleanup as unconditional
socket cleanup could unlink a successor endpoint. Control and reload endpoints
therefore retain explicit generation or socket-identity fences and may be
absent even when the ordinary notifier remains valid.

Source anchors: `wake_control_darwin.go`, `wake_reload_transport_linux.go`,
`wake_resume_protocol.go`, and `wake_owner_storage_unix.go`.

## Authoritative claim publication order

An authoritative inject-via claim has three durable publication steps:

1. Write, validate, atomically rename, re-read, and directory-sync
   `.wake.target`.
2. Publish and re-read the canonical target-section `.wake.state` projection.
   This is a durable shadow only; it does not claim ownership.
3. Write the lock temporary file, publish `.wake.lock` without replacing an
   existing claim, remove the temporary name, and directory-sync the lock
   commit.

The target and state shadow can therefore be installed when no lock has
committed. Failures before the lock link preserve both artifacts; a later
targetless acquisition may quarantine only an exact conclusively ownerless
target, then must inspect again before superseding the corresponding projection
or publishing a lock. Clean owner-bearing residue requires dead-owner recovery;
malformed ownership remains inspection-only.
Once the lock link succeeds, errors removing the temporary name or syncing the
directory are reported as committed-lock errors.

This order gives target intent and wake ownership separate observable commit
points. Recovery must classify the point reached rather than infer a single
all-or-nothing state across both files.

## Crash-interleaving test matrix

| Case | Injected interleaving | Required observation |
| --- | --- | --- |
| Target/state commit before lock | Fail after target publication, state publication, or either directory sync and before the lock link. | No authoritative lock exists. The installed target and state shadow are preserved. A later targetless acquisition may move the exact target to quarantine only when conclusively ownerless, then fresh-inspect before superseding matching projection state or creating ownership. Clean owner-bearing residue requires dead-owner recovery. |
| Lock replacement during a reader | Replace `.wake.lock` between the reader's pathname snapshot, opened-file read, and final comparison while reading lock-, target-, prepared-, or ready-bound state. | The operation reports changed or inconclusive state and performs no mutation based on the old snapshot. The replacement remains present. |
| Prepared marker generation | Exercise absent, stale-generation, current-generation/current-digest, and current-generation/wrong-digest markers. | Absence and stale generation remain not prepared; the exact current marker is accepted; an incompatible current marker is refused rather than treated as readiness. |
| Ready file replacement during cleanup | Publish a caller ready file, replace its pathname, then run failure cleanup for the original publication. | Cleanup removes only the original unchanged snapshot. The replacement is preserved and is not reported as the original receipt. |
| Guard release before waits | Pause immediately before a child, pidfd, or control wait and attempt lifecycle-guard acquisition from another participant. | The second participant can acquire the guard. Completion paths may reacquire it for final publication or cleanup; the wait itself never owns it. |
| Endpoint generation mismatch | Address or request a Darwin control endpoint with the wrong generation, or replace a Linux reload socket before authorization or retirement. | The request is refused without lifecycle mutation. Cleanup removes only the endpoint authorized by the released generation or exact socket identity and preserves a successor. |
| Crash at every authoritative publication point | Stop after target temporary-file creation, target rename/sync, state temporary-file creation, state rename/sync/re-read, lock link, lock-temp removal, and final directory sync. | Pre-lock target/state shadows never claim ownership and are preserved; post-link states are classified as committed even if later cleanup or sync reports an error. Recovery preserves ambiguous installed artifacts and never removes a different generation by pathname alone. |
