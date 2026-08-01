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

**Commit domain:** the lock becomes authoritative when `.wake.lock` is linked
into the retained agent directory. Readers verify both its content and file
identity. Removal is conditional on the exact inspected claim still matching;
a replacement lock is not selected for cleanup.

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

**Independence invariant:** changing injector behavior is not an ownership
transition. If target and lock shared one commit domain, a target replacement
could implicitly replace ownership, and a failure between target installation
and lock publication could not be represented without inventing a new partial
state. If their cleanup domains were combined, release of an old claim could
delete a newer target.

Source anchors: `wake_target.go` and `wake_owner_storage_unix.go`.

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

An authoritative inject-via claim has two durable publication steps:

1. Write, validate, atomically rename, re-read, and directory-sync
   `.wake.target`.
2. Write the lock temporary file, publish `.wake.lock` without replacing an
   existing claim, remove the temporary name, and directory-sync the lock
   commit.

The target can therefore be installed when no lock has committed. Failures
after target installation report the installed target snapshot, and cleanup
does not assume that an uncommitted lock makes the target safe to delete. Once
the lock link succeeds, errors removing the temporary name or syncing the
directory are reported as committed-lock errors.

This order gives target intent and wake ownership separate observable commit
points. Recovery must classify the point reached rather than infer a single
all-or-nothing state across both files.

## Crash-interleaving test matrix

| Case | Injected interleaving | Required observation |
| --- | --- | --- |
| Target commits before lock | Fail after target rename or target directory sync and before the lock link. | No authoritative lock exists. The installed target is reported and preserved unless exact snapshot-based cleanup is separately authorized. A later reader does not infer ownership from the target alone. |
| Lock replacement during a reader | Replace `.wake.lock` between the reader's pathname snapshot, opened-file read, and final comparison while reading lock-, target-, prepared-, or ready-bound state. | The operation reports changed or inconclusive state and performs no mutation based on the old snapshot. The replacement remains present. |
| Prepared marker generation | Exercise absent, stale-generation, current-generation/current-digest, and current-generation/wrong-digest markers. | Absence and stale generation remain not prepared; the exact current marker is accepted; an incompatible current marker is refused rather than treated as readiness. |
| Ready file replacement during cleanup | Publish a caller ready file, replace its pathname, then run failure cleanup for the original publication. | Cleanup removes only the original unchanged snapshot. The replacement is preserved and is not reported as the original receipt. |
| Guard release before waits | Pause immediately before a child, pidfd, or control wait and attempt lifecycle-guard acquisition from another participant. | The second participant can acquire the guard. Completion paths may reacquire it for final publication or cleanup; the wait itself never owns it. |
| Endpoint generation mismatch | Address or request a Darwin control endpoint with the wrong generation, or replace a Linux reload socket before authorization or retirement. | The request is refused without lifecycle mutation. Cleanup removes only the endpoint authorized by the released generation or exact socket identity and preserves a successor. |
| Crash at every authoritative publication point | Stop after temporary-file creation, target rename, target sync, lock link, lock-temp removal, and final directory sync. | Pre-lock states never claim ownership; post-link states are classified as committed even if later cleanup or sync reports an error. Recovery preserves ambiguous installed artifacts and never removes a different generation by pathname alone. |
