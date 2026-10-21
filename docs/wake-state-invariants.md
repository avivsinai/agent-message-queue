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
lock removal is the commit point; its results are in
[wake operations](wake-operations.md#retirement).

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

**Transport taxonomy:** the injector marker protocol (`accepted`, `deferred`,
`uncertain`, timeout) is defined in
[the doorbell acknowledgement policy](wake-doorbell-acknowledgement.md#cli-contract).
A bare legacy exit zero and raw TIOCSTI are `written` byte evidence only, never
provider acceptance. `amq doctor --ops` projects the same folded state and
warns for deferred/retried attempts.

Source anchors: `internal/notificationattempt/ledger.go`, `wake.go`, and
`doctor_ops.go`. Raw-mode injection carries byte-level evidence only
([#703](https://github.com/avivsinai/agent-message-queue/issues/703)); provider
acceptance requires the injector marker protocol.

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
ownership. `doctor --ops` scans the exact names independently of lock
discovery and reports `wake_quarantine` with `count` and a nullable
`newest_age_seconds` only after a complete root-wide scan. Any root or
agent-directory open/validation failure produces `wake_quarantine.error` and
blocks explicit quarantine cleanup rather than exposing a partial result.
Ordinary tmp cleanup never removes them. Only
`amq cleanup --wake-quarantine-older-than <duration>` does: its dry run does not
mutate, and the real run captures and revalidates exact identity and bytes
under the same guard before `unlinkat` and directory sync; ambiguity or
replacement preserves the artifact.

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

## `.wake.settings`: operator policy values

**Owns:** the wake's policy settings (hold, debounce, preview, bell, input
deferral, interrupt notice, inject timeout) as canonical JSON, mode 0600. It
holds no identity, generation, or transport state.

**Commit domain:** the file is written atomically on the agent directory
descriptor, by `amq wake config` (including `--reset`), by a fresh `amq wake` with
explicit settings flags, or once by a resume that finds no file and has
explicit settings flags, under the lifecycle guard. A running wake only reads it, at
each inbox scan and every 2 seconds, and validates it before it swaps the
settings. A present file always wins over argv on a resume. A refused file never changes the running settings. A wake never
derives identity from it, and no wake cleanup (release, retire, repair,
self-upgrade, or `doctor --fix-wake-locks`) removes it.

**Independence invariant:** settings are not ownership. If the file shared a
commit domain with the lock or target, a policy change would imply an owner or
injector transition, and a failed settings write would leave an unrepresentable
partial wake state.

Source anchors: `wake_settings.go` and `wake_settings_unix.go`.

## `~/.amq/wake.settings`: machine policy defaults

**Owns:** default policy values for every wake of this user on this machine, in
the same canonical JSON and mode 0600 as `.wake.settings`. It sits below the
agent file in precedence, per key. It holds no identity, generation, or
transport state.

**Commit domain:** the operator writes it through `amq wake config --machine`,
under an exclusive lock on `~/.amq/wake.settings.lock`. Wakes only read it
(each inbox scan and every 2 seconds), with the same trust checks as the agent
file (`~/.amq` must be a real directory owned by the user and not
group-writable or world-writable; a missing file always means no machine
layer), and never write it. A refused file keeps the last good machine layer in a
running wake and is skipped by a start. No wake cleanup (release, retire,
repair, self-upgrade, `doctor --fix-wake-locks`, or `amq cleanup`) removes it.

**Independence invariant:** it is per user and host and never travels with a
root. Its commit domain is separate from the lock and target, so a machine-wide
policy change implies no owner or injector transition in any wake, and a
refused machine file cannot block a lifecycle change.

Source anchor: `wake_machine_settings_unix.go`.

## `.wake.settings.applied`: applied-settings record

**Owns:** the status (`applied` or `refused`), the digest of the settings file
bytes the wake observed, and the error text, bound to the root, agent, and wake
generation. It is not a receipt: receipts record message consumption.

**Commit domain:** the running wake writes it under the lifecycle guard after
it re-inspects the lock and finds the same generation, and skips the write when
the content is unchanged. The write is best effort: a failure is logged,
never blocks or rolls back a settings swap, and is retried on the next check.
Lock release, owner-claim removal, and restart-residue repair remove it with the
other generation-bound diagnostics. `retire` leaves it; the generation binding
makes a stale record inert. `amq wake config --wait` reads it.

The record also carries `machine_status` and `machine_digest`: the status of
the machine layer (`applied` or `refused`, with the error) and the digest of
the `~/.amq/wake.settings` bytes the wake observed (the `"absent"` token when
the file is missing). They are additive under schema 1: a reader that does
not know them ignores them, and the `digest` field keeps its meaning. A record
without them reports nothing about the machine layer.

**Independence invariant:** the record reports what a generation applied; it
does not decide what the settings are. Putting it in the settings file would let
a reader mistake a stale record for the operator's intent.

Source anchor: `wake_settings_unix.go`.

## `.wake.dialog`: open Claude Code dialog

**Owns:** whether a Claude Code dialog is open for this agent, as one JSON
line, mode 0600. Only the hook receiver (`amq wake dialog open|close`) writes
or removes it. The wake only reads it, before each delivery attempt and each
terminal write, never infers it from other wake state, and never writes it; no
wake cleanup removes it. A marker that is unreadable or older than the quiet
hold still blocks input. Source anchors: `wake_dialog.go` and
`wake_dialog_unix.go`.

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

## Claim publication order

A new inject-via claim publishes `.wake.target`, then the `.wake.state`
projection, then `.wake.lock`; the exact steps and the code that owns them are
in [the wake lifecycle document](wake-lifecycle.md#publication-order). The
target and state can therefore be installed when no lock has committed; neither
claims ownership. Once the lock commits, a later temp-removal or
directory-sync error is a committed-lock error. Recovery classifies the point reached rather
than inferring one all-or-nothing state across the files.

## Crash matrix

Each row is an interleaving that a crash, a concurrent writer, or a replaced
file can produce, and the observation the code guarantees for it.

| Case | Interleaving | Required observation |
| --- | --- | --- |
| Target/state commit before lock | Fail after target publication, state publication, or either directory sync and before the lock link. | No lock exists. The installed target and state are preserved and claim nothing. A later targetless acquisition handles them through [quarantine](#wakelocktargetquarantinedtimestamp-preserved-blocked-input) and a fresh inspection. |
| Lock replacement during a reader | Replace `.wake.lock` between the reader's pathname snapshot, opened-file read, and final comparison while reading lock-, target-, prepared-, or ready-bound state. | The operation reports changed or inconclusive state and performs no mutation based on the old snapshot. The replacement remains present. |
| Prepared marker generation | The marker is absent, names a stale generation, matches the current generation and digest, or names the current generation with a wrong digest. | Absence and stale generation remain not prepared; the exact current marker is accepted; an incompatible current marker is refused rather than treated as readiness. |
| Ready file replacement during cleanup | Publish a caller ready file, replace its pathname, then run failure cleanup for the original publication. | Cleanup removes only the original unchanged snapshot. The replacement is preserved and is not reported as the original receipt. |
| Guard release before waits | Pause immediately before a child, pidfd, or control wait and attempt lifecycle-guard acquisition from another participant. | The second participant can acquire the guard. Completion paths may reacquire it for final publication or cleanup; the wait itself never owns it. |
| Endpoint generation mismatch | Address or request a Darwin control endpoint with the wrong generation, or replace a Linux reload socket before authorization or retirement. | The request is refused without lifecycle mutation. Cleanup removes only the endpoint authorized by the released generation or exact socket identity and preserves a successor. |
| Crash at every publication point | Stop after target temporary-file creation, target rename/sync, state temporary-file creation, state rename/sync/re-read, lock link, lock-temp removal, and final directory sync. | Pre-lock target and state never claim ownership and are preserved; post-link states are committed even if later cleanup or sync reports an error. Recovery preserves ambiguous installed artifacts and never removes a different generation by pathname alone. |
| Prepared marker before state refresh | Stop after `.wake.prepared` commits and before the `.wake.state` refresh. | An unbound claim falls back to the legacy marker. A bound claim is inconclusive and retry-only until an authorized mutating path refreshes the state; no reader rewrites it. |
| Torn or mismatched state document | Read `.wake.state` while it is missing, torn, non-canonical, of a newer schema, or out of step with the legacy files. | An unbound claim falls back silently to the legacy files. A bound claim is inconclusive and retry-only, and no mutation is authorized from the observation. |
