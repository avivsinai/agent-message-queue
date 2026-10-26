# amq-board

`amq-board` shows published tasks for one AMQ project. It reads sources through
an explicit sync command. A view refresh only reads the published snapshot.

```sh
amq-board sync --init              # first initialization and acquisition
amq-board sync --watch             # acquire and publish once per minute
amq-board                         # text
amq-board --json                  # schema amq/board/v2
amq-board --html --out board.html  # one self-contained page
amq-board serve                   # live view on 127.0.0.1
```

The installed `amq` must support `amq thread list --all-sessions --json`.
An unavailable command is reported as incomplete history. The board does not
scan mailboxes itself or fall back to a different source.

## Tasks and unresolved asks

A native Beads task and an explicitly admitted AMQ outcome have a permanent
project-local task ID. Each admitted task has one card. Native references and
exact AMQ messages are evidence on that card. Delivery copies share one logical
message key: canonical base namespace plus message ID. Root, session, and mailbox
observations remain separate facts. A title match, thread membership,
or mentioned bead ID does not merge tasks or admit new work.

Recorded `in_progress` is tracker progress. The board does not treat assignment,
a live notifier, or a recent session as proof that a task is running. Source
disappearance keeps the last task state and marks the missing source fact;
it never completes a task.

AMQ's native full-history lifecycle fold classifies request actions independently
from tasks. Explicit native recipient outcomes resolve an action. An open recipient action
keeps it open; omitted and continuation-only IDs remain unknown. Routing and
coverage gaps also leave status unknown. Admission and attachment do not change
source lifecycle status. Historical questions, review requests, and todos
that remain open or unknown enter the unresolved inbox,
outside task and work counts. Their current status is unknown until the local
operator attaches, admits, or dismisses them. Copy commands in the page run
only when the user runs them in a terminal.

```sh
amq-board admit --message '/canonical/base#message-id' --title 'Deliver an outcome'
amq-board attach --task task-ID --message '/canonical/base#message-id'
amq-board detach --task task-ID --message '/canonical/base#message-id'
amq-board move --from-task old-ID --task new-ID --message '/canonical/base#message-id'
amq-board dismiss --message '/canonical/base#message-id'
amq-board cancel --scope task --task task-ID
amq-board reopen --scope task --task task-ID
amq-board cancel --scope action --message '/canonical/base#message-id'
amq-board reopen --scope action --message '/canonical/base#message-id'
```

Admission reuses its permanent ID on replay. `--anchor` can set an explicit
stable admission anchor. Attach adds that exact message to the named task without changing other bindings.
Detach removes only the named binding; move names both the source and destination.
Task cancellation and request/review action cancellation are separate. Source
sync cannot clear either cancellation. Reopen is an explicit scoped action.

To adopt recorded withdrawals, run `amq-board adopt --file withdrawals.json`.
Both messages must already be in the published history. The command is the
local operator's authorization; a source handle or message body does not grant
authority. The full batch commits together.

```json
{"withdrawals":[{"message_key":"/root#request-id","withdrawal_key":"/root#withdrawal-id"}]}
```

## Publication and recovery

The board resolves `base_root` and `base_root_id` through `amq env --json` and
binds that identity for the process lifetime. Sessions with the same canonical
base share `<base>/extensions/board/`. The extension contains a versioned JSON
snapshot, a recovery copy, immutable hash-addressed evidence, and a writer lock.
The persistent host identity lives at `~/.amq/board-host-id` and is shared across
projects. A host mismatch requires recovery. Back up the whole extension and
keep it on one host's local filesystem.

Initialization is explicit. An existing directory, missing initialized state,
invalid identity, or interrupted publication requires recovery. The board never
allocates replacement IDs from a cache miss or automatically restores a backup.
Restore a known complete extension only after reconciling acknowledged owner
corrections; an older backup can lack cancellations.

Source acquisition runs outside the lock. Publication reloads the latest state
under the interprocess lock and retains owner corrections. Evidence is written
and synchronized before the snapshot references it. The snapshot is synchronized,
renamed, and its directory synchronized. Readers wait cancellably on the writer lock. An abandoned publication marker
after lock acquisition requires recovery. An uncertain publication stops writes.
The snapshot bound is 64 MiB, including 1 MiB reserved for coverage-only
publication; the evidence bound is 256 MiB. At a bound,
ingestion stops and publishes incomplete source coverage; identities and corrections
are not evicted.

## Sources and configuration

Sync reads AMQ through read-only CLI commands. Historical metadata comes from
`amq thread list --all-sessions --json`; full bodies come from the existing
`amq thread --id THREAD --agents HANDLES --include-body --json --root ROOT`.
Missing bodies, conflicting message content, and read bounds are coverage gaps.
Beads comes from `bd --readonly --sandbox list --json --brief --all --limit 0`.
Its namespace is the canonical `database_path` returned by `bd --readonly
--sandbox info --json`, checked before and after acquisition.
Closed task identities remain known. The board never changes source records.

Roster, usage, GitHub, and configured provider coverage remain secondary facts.
Native Claude task and Codex goal reconciliation and model inference are not
supported by this slice.

Configuration uses `~/.amq/board.json` and `.amq/board.json`. Settings include
`sources`, `humans`, `forges`, `providers`, `liveness_window`, `stale_after`,
`obligation_max_age`, and user-only `usage`. Sync and serve reload configuration;
a refused edit keeps the last good value. Source identity is restart-only.
Usage collection can be disabled with `{"usage":{"enabled":false}}`. Enabled
usage reads numeric harness usage facts; Codex plan limits use its installed
app-server interface and the existing bounded collector cache.

## Views

Text, JSON, static HTML, and the live page use the same pure task projection.
Static HTML is self-contained and escapes source content. The live server binds
127.0.0.1 on a free port. Its random token protects every route; Host checks
protect against DNS rebinding. The page never runs source commands.

The live page reloads the snapshot once per minute or when Refresh is pressed.
It shows publication freshness and source coverage. A failed reload retains the
last rendered snapshot with a visible source error. `~/.amq/board/serve.json`
(mode 0600) lets local clients find the view; shutdown removes its own record.

The optional `amq-board` Claude Code panel reads the same published JSON, either
from the live server or from `amq-board --json`. It starts no source acquisition.
