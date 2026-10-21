# Wake lifecycle state document

`agents/<agent>/.wake.state` is a validated projection of two artifacts: the
injector behavior in `.wake.target` and the exact-generation preparation marker
in `.wake.prepared`. This document defines its format, how it is published, and
how readers choose between it and the files it mirrors.

A wake claim is either bound or unbound:

- A **bound** claim is a `.wake.lock` that carries `state_generation` and
  `state_digest`. These fields bind the lock to the target section of
  `.wake.state`. Every inject-via claim that `amq wake` publishes is bound.
  A raw wake has no target and no `.wake.state`.
- An **unbound** claim is a lock without those fields, written by an older
  binary. For it, `.wake.target`, `.wake.prepared`, and `.wake.lock` are the
  authority and `.wake.state` is a disposable projection.

Reading never rewrites a claim. An unbound claim becomes bound only when its
wake restarts and publishes a new claim.

The normative words MUST, MUST NOT, SHOULD, and MAY describe the contract that
`amq wake` implements. Per-file ownership and commit domains, and the crash
matrix for every publication point, are in
[wake state invariants](wake-state-invariants.md#crash-matrix).

## Scope

`.wake.state` mirrors the target and the prepared marker and nothing else:

| Artifact | Why it stays outside `.wake.state` |
| --- | --- |
| `.wake.lock` | The sole ownership and deduplication authority. Its link or exact removal is the ownership commit. A state document MUST never create, replace, or retire a lock. |
| `.wake.repair-floor` | Private mode-0600 continuity state with device/inode suppression identities and successor lineage. Its lifetime and sensitivity differ from target and prepared state. It never records message IDs. |
| Caller ready files | Caller-scoped admission receipts. Their replacement and cleanup MUST NOT mutate retained wake state. |
| `.wake.lifecycle.lock` | Serializes mutations and compound reads; holds no lifecycle decision. |
| Control and reload endpoints | Ephemeral, generation- or socket-identity-bound capabilities, not durable proof that a wake exists. |

`.wake.state` is not an ownership claim, a readiness receipt, or a repair
floor. A target section without a lock does not imply ownership, and a
prepared section does not imply caller readiness.

## Document format

### Shape

The document has one schema and exactly two sections. This JSON is
illustrative; the installed byte order is defined by the canonical-bytes rule
below.

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
    "retry_until": "injected",
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

`target` is required whenever `.wake.state` exists. If `.wake.target` is
absent after a guarded cleanup, `.wake.state` MUST also be absent.

The `target` section mirrors the validated `.wake.target` value, and
`legacy_present` is true. `retry_until` is part of injector behavior and target
identity: missing means `drained`, and `injected` is the only stored value. Its
delivery contract is in
[the doorbell acknowledgement policy](wake-doorbell-acknowledgement.md).

`prepared` is either `null`, the explicit absent marker, or a copy of the
`.wake.prepared` generation and target digest. A marker that names a dead
generation is stale evidence, not preparation. The section is valid for
readiness only when its generation equals the current lock generation and its
target digest equals the current lock/target digest.

### Digests and canonical bytes

There are exactly three digest kinds:

1. `target_digest`: `sha256:` plus the hex SHA-256 of `json.Marshal(wakeTarget)`
   without a trailing newline. It MUST agree with the lock's target digest
   whenever a lock/target relationship is validated.
2. `legacy_digest`: the SHA-256 of the exact installed bytes of the mirrored
   file (`.wake.target` or `.wake.prepared`), including formatting and any
   trailing newline. It is never computed by parse and reserialize, so raw
   corruption stays visible.
3. The canonical-state digest: the SHA-256 of the exact installed
   `.wake.state` bytes. It is document evidence only and never the lock
   binding.

The installed bytes MUST be one canonical JSON stream: `json.Marshal` of the
versioned Go struct (not `MarshalIndent`), with no maps, unordered extensions,
alternate number forms, insignificant whitespace, or trailing newline. The
writer re-opens the installed file and requires the bytes to equal what it
wrote. The reload transport request in `wake_reload_transport.go` uses the same
re-encode-equality rule.

### Closed schema

A reader MUST require every document and section schema to equal exactly 1
and MUST reject, without guessing, an older or newer schema, an unknown field,
a missing required field, a wrong type, a malformed digest, an invalid root,
agent, owner, or generation, and non-canonical bytes. A rejected document is
unverified state, not an instruction to delete or repair anything; the
[read rules](#read-rules) say what the reader does next.

Every present known `.wake.lock` field, including optional fields, MUST use
its declared JSON type; `null` is not an encoding for omission. A known field
occurs at most once, including names that match under Go's case-insensitive
JSON field matching. Unknown lock fields, including duplicates and nested
objects, stay opaque and tolerated.

### Adding target fields

`state_digest` binds a lock to the digest of the target it published, so any
change to the bytes the target digest covers breaks every existing binding. A
new target field MUST be additive in the byte sense:

- It is declared `omitempty` and left unset at its default, so an unchanged
  target serializes byte-identically before and after the field exists.
- Normalization MAY apply when comparing values but MUST NOT be persisted. A
  mutation that rewrites the target passes the stored value through unchanged.
- A field whose default cannot be omitted is not additive. It needs a new
  state schema and republication of every bound claim.

A bound read has no legacy fallback, so persisting a normalized default would
leave every bound claim permanently inconclusive after an upgrade.

## Lifetime and authority

| Section or artifact | Lifetime | Validity | Writer and remover |
| --- | --- | --- | --- |
| `target` / `.wake.target` | Spans lock generations. It can stay installed with no lock after a pre-lock failure. | Describes injector intent only. A target without a lock is never ownership. A digest or identity change is an inconclusive snapshot, not permission to clean it. | A guarded mutating path. Release removes only the exact captured target after the exact lock is gone. |
| `prepared` / `.wake.prepared` | Exactly one lock generation and target digest. | Absent or dead-generation means not prepared. Current generation plus current digest is preparation. Current generation with a wrong digest is invalid and MUST NOT be treated as ready. | A guarded path after lock/target validation. Release removes only an unchanged matching marker. |
| `.wake.repair-floor` | Successor lineage. | Handed to a successor; never rebuilt by resnapshotting the inbox. | Repair-floor paths only; `.wake.state` readers never rewrite it. |
| `.wake.lock` | One owner process generation. | Only the lock's validated identity and commit state authorize ownership. The state binding does not change this. | The lock publication and exact-removal protocol. |

## Publication

### State publication sequence

Every write or removal of `.wake.state` runs under the lifecycle guard as part
of an already-authorized mutating operation. A failed state write MUST NOT make
a legacy operation look committed when it was not. The sequence is:

1. Validate the destination and the current legacy snapshots.
2. Create a unique 0600 temporary file with `O_CREAT|O_EXCL|O_NOFOLLOW`.
3. Write the canonical bytes, `fsync` the file, and close it.
4. `fsync` the agent directory.
5. Revalidate the destination and atomically rename the temporary file to
   `.wake.state`.
6. `fsync` the agent directory.
7. Re-open the installed file and verify ownership, mode, exact bytes, the
   closed schema, and every recorded legacy digest against a fresh snapshot of
   the installed legacy files.

Removal captures the file identity and bytes, re-reads them immediately before
`unlink`, unlinks only the unchanged snapshot, and syncs the directory. A
replacement document is preserved.

### Publication order

Every new inject-via claim is published in this order. The owner-bearing path
is `publishAuthoritativeWakeClaimWithDebugAt` in `wake_owner_storage_unix.go`;
the ownerless path is `acquireWakeLockWithOptionsInDir` in `wake_unix.go`.

1. `.wake.target`: write and sync a 0600 temporary file, rename it, re-read
   the installed identity and bytes, and sync the directory.
2. `.wake.state`: `publishWakeStateAndBindLockAt` publishes the state document
   with the sequence above, then sets the lock's `state_generation` to the
   lock generation and `state_digest` to the target section's
   `target_digest`.
3. `.wake.lock`: the owner-bearing path writes and syncs a lock temporary
   file, links it to `.wake.lock` without replacement, removes the temporary
   name, and syncs the directory; the ownerless path creates `.wake.lock` with
   `O_EXCL`. The no-replace link or exclusive create is the ownership commit; a
   later temp-removal or sync error is reported as a committed-lock error.
4. Verify: re-read the lock and the bound state (`validateWakeBoundStateAt`).

The state document never authorizes the lock: lock publication validates the
target directly. A failure before the lock commits leaves a target and state
with no lock; neither claims ownership, and both are preserved. With `--debug`, AMQ
logs the prior target and state digests before it replaces such a no-lock
residue. A later targetless acquisition handles that residue through
[quarantine](wake-state-invariants.md#wakelocktargetquarantinedtimestamp-preserved-blocked-input).

Every later mutation writes the legacy artifact first and then refreshes the
projection (`reconcileWakeStateAfterLegacyMutationAt`):

- **Prepared marker.** Validate the current lock and target, publish
  `.wake.prepared`, then refresh `.wake.state`. The lock is not rewritten,
  because it binds only the generation and the target-section digest. For a
  bound claim, a refresh failure after the marker commits is an error; for an
  unbound claim it is tolerated and readers fall back to the legacy marker.
- **Cleanup.** Remove the exact legacy artifact, then refresh the projection.
  When the last target is removed, the exact `.wake.state` snapshot is removed
  too; a state document never outlives its target.

A crash leaves either the prior or the new installed artifact; a temporary
name is never state. Readers re-read the installed identity and bytes and use
the document only after it passes the live digest checks in the
[read rules](#read-rules).

Managed retirement uses the same retained-directory boundary; its commit point
and results are in [wake operations](wake-operations.md#retirement).

## Read rules

1. **Document eligibility.** A reader uses `.wake.state` only after strict
   parsing, canonical-byte verification, root/agent and lifetime validation,
   semantic digest validation, and raw-digest equality with every live legacy
   file it mirrors. Existence must match in both directions: a legacy file
   without its section (or with `prepared: null`) is a mismatch, and a section
   without its legacy file is a mismatch.
2. **Unbound claims fall back.** For an unbound claim, every document failure
   (missing, torn read, corrupt or newer JSON, non-canonical bytes, invalid
   section, existence or digest mismatch) silently falls back to the
   self-consistent legacy files. The document is never authority.
3. **Bound claims fail closed.** For a bound claim, a read that cannot validate
   the document or its binding does not fall back. It returns the typed
   `wakeSnapshotReadChangedError`-family inconclusive, retry-only result,
   performs no mutation, and does not guess which side is newer. The next
   authorized mutating path re-reads the same lock generation and target and
   refreshes the document, or aborts if the generation changed.
4. **Read paths never rewrite.** `wake check`, doctor inspection, list, status,
   read-only repair assessment, and every other read surface MUST NOT create,
   refresh, delete, or repair `.wake.state` or any legacy file.
5. **Replacement during a read.** A reader re-opens and compares file identity
   and raw bytes. A replaced legacy file is inconclusive and retry-only, and
   the old snapshot is never used for cleanup, readiness, repair, or
   ownership.
6. **Stale prepared is not preparation.** A prepared section for a dead
   generation reads as not prepared and cannot authorize a ready file, repair,
   or owner transition.
7. **Deletion is scoped.** Deleting the document is a guarded cleanup only when
   the target is absent or the exact document snapshot was captured. It never
   deletes a replacement and never touches the legacy files because the
   projection is invalid. Deleting the `.wake.state` of an unbound claim
   removes only the projection.
8. **Repair floor stays separate.** No read or write path embeds, rewrites, or
   rebuilds `.wake.repair-floor`.

The typed `wakeSnapshotReadChangedError` behavior and the unverified/retry
decision in `wake_check_unix.go` are the classification precedent.

## Lock binding

The lock references the target section of the state document; the state
document never references the lock as authority.

- `state_generation` MUST equal the lock's own generation. It binds this
  target projection to one owner generation even though the target section
  spans generations.
- `state_digest` MUST equal the target section's `target_digest`, the same
  value as the lock's `target_digest`. It is never a digest of the whole
  document.
- The two fields are present together or not at all.
- The prepared section is not covered by the binding. It binds itself by
  `generation` plus `target_digest`, so a later prepared refresh can change the
  document bytes without changing the lock.

A bound result requires lock generation = `state_generation`, lock target
digest = target section `target_digest` = lock `state_digest`, and every
applicable legacy raw digest and existence marker to agree.

Existing locks are never rewritten. There is no in-place migration or lock
compare-and-swap: adding binding fields to a running owner's lock must never
stop it. An unbound lock stays unbound, under the fallback rules, until its
wake restarts and publishes a bound claim. An older binary may still release
its own exact claim; a rewrite that drops the binding fields leaves the pair
inconclusive.

Recovery of a bound claim validates the binding under the lifecycle guard
before stopping a wake or unlinking its lock. An inconclusive binding refuses
and preserves the lock, target, prepared marker, and state document; it never
falls back to a destructive legacy cleanup.

One terminal-cleanup exception exists for a retained agent-directory
descriptor (`wakeDetachedCleanupOnlyError`). After selecting an exact unchanged
lock snapshot, AMQ MAY remove only that lock from the retained directory when
the canonical agent path opens and a directory-identity comparison proves the
retained directory was replaced by a different canonical directory. The result
is a typed cleanup-only error and authorizes no repair, publication, or other
mutation through the detached descriptor. An absent or unopenable canonical
path, or any failed identity comparison, is inconclusive: the residue is
preserved.

## Mixed versions

| Situation | Read behavior | Mutation behavior |
| --- | --- | --- |
| Unbound claim from an older wake, read by this version | Legacy files are authority; the document is used only when it validates. | No mutation of the live claim from state-only evidence. A guarded mutating path may refresh the projection. |
| Bound claim, read by an older binary | The older binary reads the legacy files and ignores `.wake.state`. | It may release its own exact claim. It cannot validate the binding. |
| Missing, stale, invalid, or newer-schema document | Unbound: silent legacy fallback. Bound: inconclusive, retry-only. | No read surface repairs it; only a guarded mutating path refreshes it. A newer schema is preserved for a compatible reader. |
| Lock/state generation or digest mismatch | Unbound: legacy fallback. Bound: inconclusive, retry-only. | No cleanup, readiness, repair, owner transition, or state rewrite from the mismatched observation. |

Repair handoffs fail closed the same way. A wake whose repair handoff does not
announce its inbox parent directory identity (source schema 1) cannot be
repaired in place; restart it from its owning terminal so it announces at
schema 2. AMQ does not infer the missing identity from path names. When a newer
source schema reaches an older binary, upgrade `amq` or restart the newer wake
under the matching version.
