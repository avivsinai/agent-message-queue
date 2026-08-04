# Wake lifecycle state document

Status: W3 P2a implemented; W3.5/P2b binding contract under review.
W3.2 through W3.4 implement the legacy-authoritative `.wake.state`
projection described here. This document additionally defines the W3.5/P2b
binding contract implemented by the candidate under review; that is not a
claim that the candidate is merged, released, or deployed, and it does not
add migration. Legacy authority continues through P2a for existing unbound
claims, which are never rewritten.

The normative words MUST, MUST NOT, SHOULD, and MAY describe the contract that
the W3 implementation and its tests must preserve.

## 1. Scope and exclusions

W3 v1 adds one retained document at
`agents/<agent>/.wake.state`. It is a validated projection of two existing
artifacts:

- the injector behavior in `.wake.target`; and
- the exact-generation preparation marker in `.wake.prepared`.

The projection is deliberately narrower than the complete wake directory.
The following artifacts stay outside `.wake.state`:

| Artifact | Why it stays outside |
| --- | --- |
| `.wake.lock` | It is the sole ownership and deduplication authority. Its link or exact removal is the ownership commit. A state document MUST never create, replace, or retire a lock. |
| `.wake.repair-floor` | It is private mode-0600 continuity state containing device/inode suppression identities and successor lineage. Its lifetime and sensitivity differ from target/prepared state. It remains a coupled external artifact, not an embedded section, and never records message IDs. |
| Caller ready files | They are caller-scoped admission receipts. Their replacement and cleanup rules must not mutate retained wake state. |
| `.wake.lifecycle.lock` | It serializes mutations and compound reads but contains no lifecycle decision. It is released before child, pidfd, or control waits. |
| Control and reload endpoints | They are ephemeral, generation- or socket-identity-bound capabilities, not durable proof that a wake exists. |

`.wake.state` is not an ownership claim, a readiness receipt, a repair floor,
or a replacement for any of those artifacts. In particular, a target section
without a lock does not imply ownership, and a prepared section does not imply
caller readiness.

The design is anchored in `docs/wake-state-invariants.md` and the existing
boundaries in `internal/cli/wake_target.go`,
`internal/cli/wake_owner_storage_unix.go`, `internal/cli/wake_prepared_unix.go`,
`internal/cli/wake_ready_unix.go`, and
`internal/cli/wake_repair_floor_unix.go`.

## 2. Document format and versioning

### 2.1 v1 shape

The v1 document has one document schema and exactly two state sections. The
following is illustrative JSON; field order in the installed bytes is defined
by the canonicalization rule below.

```json
{
  "schema": 1,
  "target": {
    "schema": 1,
    "mode": "inject-via",
    "root": "/absolute/canonical/root",
    "agent": "codex",
    "created": "2026-08-01T22:00:00Z",
    "inject_via": "/absolute/path/to/injector",
    "inject_args": ["fixed", "argument"],
    "owner": {
      "pid": 1234,
      "process_start": "...",
      "boot_id": "...",
      "session_id": 99
    },
    "legacy_present": true,
    "target_digest": "sha256:<hex>",
    "legacy_digest": "sha256:<hex>"
  },
  "prepared": {
    "schema": 1,
    "generation": "32-lowercase-hex-characters",
    "legacy_present": true,
    "target_digest": "sha256:<hex>",
    "legacy_digest": "sha256:<hex>"
  }
}
```

`target` is required whenever `.wake.state` exists. `prepared` is either
`null` or the exact marker represented by `.wake.prepared`; a non-null marker
may name a dead generation and is then stale evidence, not preparation for the
current wake. If `.wake.target` is absent after a guarded cleanup, the state
document MUST also be absent; a partial document without a target is invalid.

The `target` section mirrors the validated `.wake.target` value. Its
`target_digest` is the existing semantic target digest: `sha256:` plus the
hexadecimal SHA-256 of `json.Marshal(wakeTarget)` without a trailing newline.
It MUST agree with the corresponding lock target digest whenever a lock/target
relationship is being validated. `legacy_present` MUST be true for an existing
target section. Its `legacy_digest` is the SHA-256 of the exact installed
`.wake.target` bytes, including formatting and any trailing newline.

The `prepared` section mirrors the existing `wakeReady` marker. Its generation
and target digest MUST be copied exactly from the legacy marker. Its
`legacy_present` MUST be true when the object is present, and its
`legacy_digest` is the SHA-256 of the exact installed `.wake.prepared` bytes.
`prepared: null` is the explicit absent marker for the legacy prepared file.
The section is valid for readiness only when its generation equals the current
lock generation and its target digest equals the current lock/target digest.

### 2.2 Canonical bytes and digests

The installed `.wake.state` bytes MUST be one canonical JSON byte stream:

1. Encode the versioned Go struct with `json.Marshal`, not
   `json.MarshalIndent`; this gives deterministic struct-field order and
   preserves ordered `inject_args`.
2. Reject maps, unordered extensions, alternate number forms, insignificant
   whitespace, and a trailing newline in the canonical state representation.
3. Re-open the installed file and require the bytes to equal the bytes that
   were written. A transient whole-document canonical-state digest, when
   needed for artifact evidence, is `sha256:` plus the hexadecimal SHA-256 of
   those exact installed bytes; it is never the P2b lock binding.

This uses the same `json.Marshal` re-encode-equality mechanism already used by
the reload transport request in `internal/cli/wake_reload_transport.go`; it is
an existing canonicalization precedent, not a new serialization convention.
There are exactly three digest kinds in this contract:

1. `target_digest`: the existing semantic digest of `json.Marshal(wakeTarget)`;
2. `legacy_digest`: the raw stored-byte digest of each mirrored legacy file;
   it is never computed by parse-and-reserialize; and
3. the canonical-state digest of the exact installed `.wake.state` bytes,
   used only for document evidence and never as the P2b lock binding.

Legacy digests therefore do not change merely because the embedded JSON is
semantically equivalent, while raw corruption remains visible.

### 2.3 Closed growth and fail-closed reads

Schema growth is closed. A v1 reader MUST require every document and present
section schema to equal exactly 1, and MUST reject, without guessing:

- a document or section schema older or newer than 1;
- an unknown document or section field;
- a missing required field, wrong type, malformed digest, invalid root/agent,
  invalid owner, or invalid generation;
- a target or prepared section schema other than 1; or
- non-canonical installed bytes.

An invalid or newer document is unverified state, not an instruction to delete
or repair anything. P2a readers fall back to the self-consistent legacy pair
as specified in section 6. P2b readers classify a lock/document binding
failure as inconclusive and retry-only.

Every present known `.wake.lock` field, including optional fields, MUST use
its declared JSON type; `null` is not an encoding for omission. A known field
MUST occur at most once, including names that match under Go JSON's
case-insensitive field matching. Unknown lock fields, including duplicate
unknown names and nested objects, remain opaque and tolerated for forward
compatibility.

### Digest-affecting target fields

`state_digest` binds a lock to the digest of the target it published, so any
change to the bytes covered by the target digest invalidates every binding
published before that change. New target fields MUST therefore be additive in
the byte sense:

- A new field MUST be declared `omitempty` and MUST be left unset at its
  default value, so that an otherwise unchanged target serializes
  byte-identically before and after the field exists.
- Normalization of such a field MAY be applied when comparing two values, but
  MUST NOT be persisted. A legacy mutation that rewrites the target MUST pass
  the stored value through unchanged rather than writing its normalized form.
- A field whose default cannot be omitted is not an additive change. It
  requires a state generation change and republication of every bound claim.

A bound read has no legacy fallback. Persisting a normalized default would
shift the target digest while the already-published lock retained the older
`state_digest`, so every bound claim would become permanently inconclusive and
retry-only after an upgrade.

## 3. Lifetime and authority table

The sections have independent lifetime bindings. The document is a projection;
the authority column names the artifact that still decides the operation.

| Section/artifact | Lifetime binding | Validity and stale behavior | Writer/remover |
| --- | --- | --- | --- |
| `target` / `.wake.target` | Spans lock generations. It may remain installed with no lock after target-before-lock publication or after an ambiguous failure. | A valid target describes injector intent only. Absence of a lock never turns it into ownership. A digest or identity change is an inconclusive snapshot, not permission to clean it. | An authorized guarded mutating path. Release cleanup removes only the exact captured target after the exact lock is gone. |
| `prepared` / `.wake.prepared` | Bound to exactly one lock generation and target digest. | Missing, absent, or a dead-generation marker means not-prepared. Current generation plus current target digest is valid preparation. A current-generation wrong-digest marker is invalid and MUST NOT be treated as ready. | A guarded path after lock/target validation. Release cleanup removes only an unchanged matching marker. |
| `.wake.repair-floor` | Independent successor lineage and device/inode suppression lifetime. | It remains separate coupled state. A dead-generation floor is handed to a successor; it is never reconstructed by resnapshotting inbox state. | Existing repair-floor authority paths only; `.wake.state` readers never rewrite it. |
| `.wake.lock` | One exact owner/process generation. | Only the lock's validated identity and commit state authorize ownership. P2b adds a state reference but does not reverse this authority. | Existing lock publication/removal protocol; P2b state-reference updates preserve the exact owner generation. |
| Ready files, guard, endpoints | Caller, serialization, and transport lifetimes respectively. | Replacement or endpoint mismatch is handled by identity/generation fences and never by inferring durable state from a path. | Their existing scoped owners and cleanup paths. |

The target section can outlive a lock. The prepared section cannot prepare a
different generation. The repair floor is recorded by this specification only
as a coupled external artifact; it is not silently folded into the document.

## 4. Write protocol and fsync points

### 4.1 Common rules

Every state refresh or removal MUST run under the existing lifecycle guard and
MUST be part of an already-authorized mutating operation. In P2a, the
operation first leaves the legacy files self-consistent, then projects their
installed bytes into `.wake.state`. P2b has one explicit publication
exception: a new bound claim may publish and verify the target section before
the lock link, as section 7.1 specifies, but that state projection never
authorizes the link. A state write that fails MUST NOT make a legacy operation
appear committed when it was not.
This guard requirement binds writers of every document schema generation; a
writer that mutates state without the guard is out of contract regardless of
schema.

The state document uses a private 0600 temporary file in the agent directory.
The required state publication sequence is:

1. Validate the destination and current legacy snapshots.
2. Create a unique 0600 temporary file with `O_CREAT|O_EXCL|O_NOFOLLOW`.
3. Write the canonical bytes and `fsync` the temporary file. Close it.
4. `fsync` the agent directory before installation.
5. Revalidate the destination, then atomically rename the temporary name to
   `.wake.state`.
6. `fsync` the agent directory after installation.
7. Re-open the installed file, verify regular-file ownership/mode, verify the
   exact bytes, parse the closed v1 shape, and verify every recorded legacy
   digest against a fresh installed-file snapshot.

A failed pre-rename state publication leaves the previous document in place
or leaves no document. A new document visible after rename is accepted only
after the re-read and digest checks; a crash before the post-rename directory
sync may leave either the old or new document, and both outcomes are safe
because legacy digests decide P2a preference.

When removing `.wake.state`, the mutating path MUST capture its file identity
and bytes, re-read them immediately before unlink, unlink only the unchanged
snapshot, and `fsync` the directory. A replacement document is preserved.

### 4.2 Legacy-first P2a sequence

P2a keeps `.wake.target` and `.wake.prepared` authoritative and makes the
document disposable. The sequence for a guarded mutation is:

1. Publish or remove the legacy target using its existing path-specific
   protocol. The normal metadata path writes and file-syncs a 0600 temp,
   closes it, syncs the directory before rename, revalidates the destination,
   renames atomically, syncs the directory after rename, and verifies the
   installed bytes. The authoritative inject-via acquisition path in
   `wake_owner_storage_unix.go` uses its existing target temp/file-sync,
   rename, installed-target identity/raw-byte re-read, and post-rename
   directory sync sequence.
2. For an authoritative acquisition, publish the lock only after the target
   is verified and synced: write and file-sync the lock temp, publish it with
   the no-replace link, remove the lock temp, and sync the directory. A
   successful link is the committed lock even if removing the temp name or a
   later directory sync reports an error. The lock remains the ownership
   authority; P2a does not add state-reference fields to it.
3. Publish or remove `.wake.prepared` using the existing generation-marker
   protocol. Validate the current lock and target first; write a 0600 temp and
   file-sync it, atomically rename it, re-read its identity and raw bytes, and
   sync the agent directory after installation. The marker is accepted only
   for the exact current generation and target digest.
4. After each legacy artifact that the mutation changed is self-consistent,
   assemble one `.wake.state` snapshot from fresh reads. Include the target
   section and the current prepared section (or `null`) and publish it with
   section 4.1's state sequence.

If a mutation removes the last target, legacy removal is followed by guarded
exact-snapshot removal of `.wake.state`; the state document must not survive a
missing target. If a crash or error leaves an installed target with no lock,
the target and any state document remain non-authoritative. A later targetless
acquisition may quarantine the exact target and then, only after a fresh
inspection, supersede corresponding stale projection state. No pathname-only
cleanup or ownership inference is permitted.

**P2a must be abortable by simply deleting the doc.**

Deleting `.wake.state` during P2a therefore removes only the projection. It
does not invalidate, delete, or rewrite a self-consistent legacy target,
prepared marker, or lock.

### 4.3 Publication crash boundaries

The following boundaries are observable and must remain safe:

| Boundary | May be visible after recovery | Required interpretation |
| --- | --- | --- |
| Before a legacy temp file is file-synced | No new authoritative legacy artifact | Keep the prior installed artifact; discard only the private temp if it is still the writer's exact temp. |
| After legacy temp sync, before directory sync/rename | No new authoritative legacy artifact | The temp is not the retained artifact. Do not infer a state transition. |
| After legacy rename, before post-rename directory sync | Old or new installed legacy artifact | Re-read the installed identity/raw bytes. Either valid artifact is authoritative for its own bytes; the document must pass the live digest check before use. |
| After legacy post-rename sync, before state publication | New self-consistent legacy artifact and old/missing state doc | Prefer legacy in P2a because the state legacy digest mismatches or is absent. No read path rewrites the doc. |
| Before state rename | Old/missing state doc | Legacy remains authoritative; private state temp is not state. |
| After state rename, before state post-rename sync | Old or new state doc | In P2a, verify canonical bytes and all live legacy digests; a mismatch silently falls back to legacy. For a bound P2b claim, the same mismatch is typed inconclusive/retry-only and authorizes no mutation. |
| After state post-rename sync and verification | New state projection | It is usable only while every current legacy digest and lifetime rule continues to match. |

### 4.4 Quarantine preservation and explicit cleanup

Acquisition may free a blocked live pathname only by preserving one of two
exact artifacts:

1. An aged syntax-invalid, empty, or truncated generic `.wake.lock` that is a
   same-owner regular 0600 file. A fresh `creating` lock, a 0400 or
   owner-shaped lock, an unreadable/oversized/special file, and valid JSON with
   a wrong known-field type never qualify.
2. With no lock, a targetless acquisition may preserve an exact readable
   regular 0600 orphan `.wake.target`.

The mover holds the lifecycle guard and retained agent-directory FD, revalidates
inode and raw bytes, performs a Darwin/Linux no-replace rename to
`.wake.<lock|target>.quarantined.<UTC-nanosecond>`, syncs the directory, and
reopens the destination to verify the same inode and bytes. Success forces a
new acquisition loop; stale observations never authorize fresh lock
publication. Any uncertainty, collision, or replacement preserves/refuses.

`doctor --ops` scans those exact names independently of `.wake.lock` and always
reports `wake_quarantine` with `count` and nullable `newest_age_seconds`.
Quarantine is preservation. Only explicit
`amq cleanup --wake-quarantine-older-than <duration>` may remove it; dry-run
does not mutate, and actual cleanup revalidates identity/raw bytes under the
lifecycle guard before `unlinkat` plus directory sync. Ordinary tmp cleanup
never selects quarantine.

### 4.5 Managed retirement commit and residue

Every retirement effect uses the retained agent-directory FD. Before lock
removal, any identity, target, generation, or bound-state uncertainty returns
`refused` and preserves the claim. Exact lock removal is the commit point. Once
it commits, retirement cannot be downgraded to refusal: complete target/state
cleanup returns `retired`; failed or skipped cleanup returns exit-0 warning
`retired_with_residue`.

Post-commit cleanup removes only the exact captured target and its corresponding
state. For unbound state, the target-section digest must match the retired
target; a mismatch is preserved as residue. The mailbox and every replacement
lock, target, or state artifact are always preserved. Residue converges
automatically on the next acquisition through quarantine/supersession; no
bespoke recovery action is part of the contract.

The sequence is intentionally legacy-first. A torn state projection can make
the projection unusable, but it cannot make a torn legacy operation look safe.

## 5. Crash matrix and required observations

The seven existing contract-net cases are the acceptance floor. The new state
document adds observations but does not weaken any existing required result.

| Case | W3 geometry | Required observation | Mutation prohibition |
| --- | --- | --- | --- |
| Target commits before lock | `.wake.target` and possibly its target section are installed before `.wake.lock` links. | No authoritative lock exists. The target remains preserved state; target alone never implies ownership. A targetless acquisition may quarantine its exact inode/bytes, then must fresh-inspect before superseding matching projection state or publishing a lock. | Do not create ownership from the shadow, remove it by pathname, or clean a different generation. |
| Lock replacement during a reader | A reader may observe the lock, state, target, prepared marker, or their digest, then a replacement may occur before the final comparison. | Re-open and compare file identity/raw bytes. A legacy-artifact replacement is `wakeSnapshotReadChangedError`-family inconclusive evidence and must classify as unverified/retry-only. For a P2a shadow-document replacement or torn read, section 6.3 resolves the observation as silent legacy fallback; document retry-only classification applies only after P2b binding. | Do not use the old snapshot for cleanup, readiness, repair, or ownership. Preserve the replacement. |
| Prepared marker generation | State and legacy may show four distinct observations: (a) absent marker / `prepared: null`; (b) stale-generation marker / object; (c) current-generation plus current target digest; or (d) current-generation plus a wrong target digest. | (a) is not-prepared. (b) is stale evidence and not-prepared. (c) is the only valid preparation. (d) is refused and is never treated as ready. | Do not promote absent, stale, or wrong-digest state into readiness or owner admission. |
| Ready-file replacement during cleanup | Caller ready files remain external while the retained state projection is unchanged. | Cleanup compares the original ready publication's identity, bytes, and semantics, then preserves a replacement. | Do not remove or report a replacement as the original receipt. |
| Guard release before waits | The guard covers bounded validation/publication only; waits happen after it is released. | Another participant can acquire the guard while a child, pidfd, or control wait is in progress; completion may reacquire it. | Do not hold the guard across a wait or encode a wait in `.wake.state`. |
| Endpoint generation mismatch | Control/reload endpoints remain ephemeral and may be addressed with a wrong generation or replaced socket. | The request/refusal is tied to the expected generation/socket identity. A mismatch is refused without changing durable state. | Do not mutate state, lock, target, or floor from an unauthenticated endpoint/path. |
| Crash at every publication point | Stop after temp creation, file sync, pre-rename directory sync, rename, post-rename sync, state verification, lock link, lock-temp removal, or final directory sync. Include the P2b target-state-before-lock geometry and the later prepared-legacy-before-state refresh gap. | Pre-lock states never claim ownership. Post-link lock states are committed even when later cleanup/sync reports an error. In P2a, state/doc mismatch falls back to live legacy. For a bound P2b claim, a torn/mismatched state document is typed inconclusive/retry-only until an authorized mutating path refreshes it. A subsequent targetless acquisition may quarantine an exact no-lock target and only then fresh-inspect before superseding matching projection residue. Before replacement, debug mode records the prior target and state digests. | Never let a document, temp name, stale marker, quarantine artifact, or pathname-only cleanup remove another generation or authorize a mutation. |

The tests MUST exercise the state-specific versions of these cases, including
crash hooks after every legacy and state boundary. The existing typed
`wakeSnapshotReadChangedError` behavior and the unverified/retry decision in
`wake_check_unix.go` remain the classification precedent.

## 6. Dual-read and dual-write rules

1. **Legacy authority in P2a.** `.wake.target`, `.wake.prepared`, and
   `.wake.lock` retain their existing authority and commit points. The state
   document is only a projection until P2b is explicitly activated.
2. **Document eligibility.** A reader may prefer `.wake.state` only after strict
   v1 parsing, canonical-byte verification, target/prepared lifetime
   validation, root/agent validation, semantic digest validation, and exact
   raw-byte digest equality with every live legacy artifact it mirrors. The
   equality check includes existence in both directions: a legacy artifact
   present while its state section is absent (or `prepared: null`) is a
   mismatch, and a state section present while its legacy artifact is absent
   is also a mismatch.
3. **P2a fallback regime.** Before P2b migration, every state-document read
   failure takes the same silent path: missing/`ENOENT`, torn read, corrupt or
   newer JSON, noncanonical bytes, invalid section, existence mismatch, and
   legacy digest mismatch all fall back to the self-consistent legacy
   artifacts. P2a does not retry or narrate these shadow-document failures
   beyond optional debug evidence, and it never treats the shadow as authority.
4. **P2b inconclusive regime.** After a claim has crossed the P2b migration
   boundary, a bound read that cannot validate the state document or its
   target-section binding does not fall back to legacy. It returns the typed
   `wakeSnapshotReadChangedError`-family inconclusive/retry-only result from
   the fatality table. It performs no mutation and does not guess which side
   is newer. Legacy-only state is still a deliberate pre-migration/unbound
   condition, not a successful P2b read.
5. **READ PATHS NEVER REWRITE.** Wake check, doctor inspection, list, status,
   read-only repair assessment, and every other read surface MUST NOT create,
   refresh, delete, or repair `.wake.state` (or any legacy artifact). A doc
   refresh is allowed only under the lifecycle guard on an already-authorized
   mutating path, after that path has made the legacy artifacts self-consistent.
6. **One projection after legacy work.** In P2a, a mutating path writes the
   changed legacy artifacts first, then projects fresh installed snapshots
   into one state document. P2b's only ordering exception is the new-claim
   target projection before the lock link in section 7.1; it never uses state
   to justify that link or any legacy mutation. A prepared marker is always
   written legacy-first, followed by the state refresh.
7. **Stale prepared is not preparation.** A prepared section naming a dead
   generation is retained only as stale evidence when it mirrors the legacy
   marker. It reads as not-prepared and cannot authorize a ready file, repair,
   or owner transition.
8. **State deletion is scoped.** Deleting the document is a safe P2a abort or
   guarded cleanup only when the target is absent or the exact document
   snapshot was captured. It never deletes a replacement and never touches the
   legacy authority merely because the projection is invalid.
9. **Repair-floor separation.** No dual-read or dual-write path embeds,
   rewrites, or reconstructs `.wake.repair-floor` as a side effect of reading
   or projecting target/prepared state.

These rules preserve the property that an old CLI can continue to operate on
the legacy files while a new CLI has a stale or absent projection. They also
make read-only inspection genuinely read-only rather than a hidden repair
path.

## 7. P2b migration, lock binding, and ABI update

P2b is a separate activation boundary. It MUST NOT be smuggled into P2a by
adding lock fields or making a state document an implicit owner claim.

### 7.1 Binding contract

The P2b lock adds the exact fields `state_generation` and `state_digest` to the
versioned `.wake.lock` ABI. The direction is fixed: the lock references the
target section of the state document; the state document never references the
lock as authority.

- `state_generation` MUST equal the lock's own current generation. It binds
  this target projection to one exact owner generation even though the target
  section itself spans generations.
- `state_digest` MUST equal the target section's existing semantic
  `target_digest`: `sha256:` plus the hexadecimal SHA-256 of
  `json.Marshal(wakeTarget)`. It is the target-section digest, mirroring the
  existing `.wake.lock.TargetDigest` contract. It MUST NOT be a digest of the
  whole `.wake.state` document.
- The prepared section self-binds independently by
  `generation + target_digest`. It is deliberately not covered by the lock's
  state digest because the legacy prepared marker is published after lock
  acquisition and W3.3 refreshes the state document when that marker changes.
- A lock may be unbound during the explicit pre-P2b or pre-preparation
  transition. It is not ready and cannot pass a P2b state-bound mutation gate.
  Once P2b publication binds a lock, later prepared-section publication may
  change the document bytes without changing the lock reference, provided the
  target section digest and lock generation remain unchanged and the full
  P2b document validation succeeds.

The ordering follows the existing publication geometry:

1. For a new bound claim, publish and verify the target legacy artifact and
   its target section before the lock link, then publish the lock with
   `state_generation` and the target-section `state_digest`. The state
   document is never used to authorize the link; the lock publication still
   validates the target directly.
2. Existing locks are never rewritten. A pre-P2b lock stays unbound until its
   wake naturally restarts and republishes under step 1. There is no in-place
   migration or lock compare-and-swap: the running owner must never be killed
   by the act of adding state-reference fields to its own lock. An existing
   live lock continues under P2a fallback rules until that restart.
3. For later prepared publication, write the legacy marker first, refresh the
   state document, and validate its four-way prepared observation. Do not
   rewrite the lock merely because the prepared section or whole document
   bytes changed; the lock binds only generation plus target-section digest.
4. Re-read lock, target, and state after each binding/publication operation.
   Require lock generation = `state_generation`, lock target digest = target
   section `target_digest`, lock `state_digest` = target section
   `target_digest`, and all applicable legacy raw digests/existence markers to
   agree before returning a bound result.

The two P2b crash geometries have distinct recovery rules. For the new-claim
path, the target-section state publication uses section 4.1's temp file-sync,
pre-rename directory sync, atomic rename, post-rename directory sync, and
re-read. A crash before the no-replace lock link leaves a target projection
without ownership; a successful lock link is the ownership commit even if
later lock-temp removal or directory sync reports an error. The document never
authorizes that link.

For a bound claim's later preparation, `.wake.prepared` commits first and the
state refresh commits second. A crash in that gap leaves the bound lock and a
legacy prepared marker with a state existence or digest mismatch. In P2a this
is silent legacy fallback. In bound P2b it is typed inconclusive/retry-only;
readers do not rewrite the document. The next authorized mutating lifecycle
path re-reads the same current lock generation and target, refreshes the state
document, and either converges the marker or aborts/retries if the generation
changed. No reader or cleanup path treats the gap as permission to mutate.

If a new publication cannot install a bound lock after the target section is
durable, the document remains a safe shadow and the claim remains unbound. In
P2a this is a silent legacy fallback; after a P2b claim has been activated it
is typed inconclusive/retry-only. No cleanup, readiness, repair, or ownership
mutation is authorized from it. A replacement observed during any step is the
typed `wakeSnapshotReadChangedError` family, not a stale-success result.

### 7.2 Migration and review gate

P2b activation at new lock publication MUST run under the lifecycle guard and
must first prove the live legacy pair is self-consistent. It may create the
document on that authorized mutating path, but a read-only inspection never
performs migration. Existing live locks are never rewritten: a pre-P2b lock
remains unbound and usable through P2a fallback until its wake restarts and
republishes with the new fields.

Recovery and rollback of a bound claim MUST validate the current binding under
the lifecycle guard before stopping a wake or unlinking its lock. An
inconclusive binding refuses and preserves the lock, target, prepared marker,
and state document; it never falls back to a destructive legacy cleanup. A
target/state shadow with no lock is likewise non-authoritative preservation; a
targetless acquisition may move only the exact target into quarantine and must
fresh-inspect before converging matching projection residue. Here rollback
means using an older binary or reader that remains compatible with the retained
legacy artifacts; it does not rewrite a lock to remove P2b fields. Existing
unbound claims remain P2a, and an older binary may still release its own exact
claim.

There is one narrow terminal-cleanup exception for a retained agent-directory
descriptor. After selecting an exact unchanged lock snapshot, AMQ MAY remove
only that lock from the retained directory when opening the canonical agent
path succeeds and comparing directory identities proves that the retained
directory was replaced by a different canonical directory. The operation MUST
return a typed cleanup-only error and MUST NOT authorize any subsequent repair,
publication, or other mutation through the detached descriptor. An absent or
unopenable canonical path, or any failure to compare both identities, is
inconclusive: preserve the residue and fail closed.

The lock's exact-key ABI golden test MUST be updated in the same PR as the new
fields, migration, state binding, and acceptance tests. P2b is held until
W3.2 (inert state primitives), W3.3 (dual-write), and W3.4 (dual-read) have
soaked on main and the exact mixed-version/crash matrix is green.

Per the finishing-review constraint, W3.5/P2b is one coherent external-review
PR: the migration, lock binding, ABI golden update, and acceptance-document
lockstep are self-contained and readable together. It is not split into
independent public PRs that hide the point-of-no-return contract.

## 8. Mixed-version matrix

The matrix distinguishes what a reader may observe from what a writer may
mutate. “Legacy-only” means the existing target/prepared/lock protocol remains
the authority; “unbound” is not a successful P2b state.

| Live/reader combination | Read behavior | Write and mutation behavior |
| --- | --- | --- |
| Old CLI, legacy-only files; new CLI reader | New CLI validates/falls back to legacy. No document is required. | Old CLI behavior remains valid. New CLI may project only on an already-authorized mutating path. |
| New CLI P2a with missing document; old CLI reader | New CLI and old CLI both use legacy. | No read-triggered migration. A guarded new-CLI mutation may dual-write; old CLI remains unaware. |
| New CLI with stale/invalid/noncanonical document; old CLI reader | In P2a, new CLI silently falls back to legacy and old CLI ignores the document. For a bound P2b claim, the new CLI returns typed inconclusive/retry-only instead. | Neither reader may repair the document from a read surface. Only a guarded new-CLI mutation may refresh it after legacy validation, and a bound P2b reader must wait for that authorized path. |
| Old live wake, new CLI reader | New CLI accepts the legacy lock/target relationship as P2a/unbound evidence; it does not invent a state binding. | New CLI MUST NOT mutate the old live claim from state-only evidence. It may use existing legacy-safe read/retry rules. |
| New live P2a wake, old CLI reader | Old CLI uses legacy files and ignores `.wake.state`. | Old CLI operations retain P2a behavior. The new CLI does not treat the old reader's ignorance as P2b approval. |
| New P2b bound wake, old CLI reader | Old CLI may read legacy state, but cannot validate the state reference. | A legacy-safe old-CLI release may remove its own exact claim; any old-CLI rewrite that drops binding fields makes the pair unbound/inconclusive to new readers. No state-only mutation is allowed. |
| New CLI P2b reader with legacy-only or unbound lock | Read legacy only where its self-consistency is provable; report no P2b binding. | No readiness, repair, or binding-dependent mutation. Convergence requires the wake to restart and republish; no in-place migration/reference update is allowed. |
| New CLI with lock/state generation or digest mismatch | For a legacy-only/unbound P2a claim, use self-consistent legacy fallback. For a bound P2b claim, classify as typed inconclusive/unverified and retry-only. Do not choose whichever side is newer. | No cleanup, readiness, repair, owner transition, or state rewrite from the mismatched observation. |
| Any reader with newer document or section schema | In P2a, treat the shadow document as a read failure and silently use legacy. For a bound P2b claim, fail closed as unverified/retry-only; do not guess fields or downgrade the schema. | No mutation. Preserve the artifacts for a compatible reader or explicit operator handling. |

The existing `wake_owner_mixed_version_test.go` is the pattern for proving that
an older binary preserves a newer owner-bound claim. W3 extends that matrix to
state-document absence, stale legacy digests, unbound transitions, and the
P2b lock-reference ABI. A green matrix proves compatibility behavior only; it
does not itself authorize landing, migration activation, release, or deploy.
