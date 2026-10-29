# amq-board

`amq-board` shows published tasks for one AMQ project. It reads sources through
an explicit sync command. A view refresh only reads the published snapshot.

```sh
amq-board sync --init              # first initialization and acquisition
amq-board sync --watch             # acquire and publish once per minute
amq-board                         # text
amq-board --json                  # complete model, schema amq/board/v2
amq-board --pane-json             # bounded native pane, amq/board-pane/v1
amq-board --html --out board.html  # one self-contained page
amq-board serve                   # live view on 127.0.0.1
```

The installed `amq` must support `amq thread list --all-sessions --json`.
An unavailable command is reported as incomplete history. The board does not
scan mailboxes itself or fall back to a different source.

## Tasks and unresolved asks

A supported native task and an explicitly admitted AMQ outcome have a permanent
project-local task ID. Each admitted task has one card. Native references and
exact AMQ messages are evidence on that card. Delivery copies share one logical
message key: canonical base namespace plus message ID. Root, session, and mailbox
observations remain separate facts. A title match, thread membership,
or mentioned bead ID does not merge tasks or admit new work.

Recorded `in_progress` is tracker progress. The board does not treat assignment,
a live notifier, or a recent session as proof that a task is running. Source
disappearance keeps the last task state in History, outside current work counts
and Next; it never completes a task.

AMQ's native full-history lifecycle fold classifies request actions independently
from tasks. Explicit native recipient outcomes resolve an action. An open recipient action
keeps it open; omitted and continuation-only IDs remain unknown. Routing and
coverage gaps also leave status unknown. Admission and attachment do not change
source lifecycle status. Historical questions, review requests, and todos
that remain open or unknown enter the unresolved inbox,
outside task and work counts. Their current status is unknown until the local
operator attaches, admits, or dismisses them. Copy commands in the page run
only when the user runs them in a terminal. Source/context commands can appear
on a card; owner corrections remain explicitly labelled in Details and do not
make a task a Next action.

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

Use `amq-board catalogue` to inspect current, completed, cancelled, and historical
tasks together. `--task ID` also resolves a copied ID after consolidation.
Sources keep their own facts; the owner chooses which alias supplies lifecycle
state when tasks are consolidated or an alias is moved.

```sh
amq-board consolidate --task kept-ID --absorb duplicate-ID --authority ALIAS --generation N
amq-board move-alias --alias ALIAS --task destination-ID --donor-authority owner --generation N
```

Read the catalogue's current generation and authority aliases before either
command. Absorbed IDs redirect to the survivor. Consolidation preserves owner
cancellation; reopening requires an explicit owner action.

To adopt recorded withdrawals, run `amq-board adopt --file withdrawals.json`.
Both messages must already be in the published history. The command is the
local operator's authorization; a source handle or message body does not grant
authority. The full batch commits together.

```json
{"withdrawals":[{"message_key":"/root#request-id","withdrawal_key":"/root#withdrawal-id"}]}
```

## Local suggestions

Suggestions are optional interpretations of exact quoted message spans. They
remain outside task and work counts until the owner adopts them. Their accuracy
on AMQ work has not been measured. Unsupported matches remain unresolved;
bounded retrieval does not establish that an outcome is new.
Replies, status updates, and resolved requests can supply attachment evidence;
they do not establish current work or qualify for a new task.

Each pass handles one whole message with at most eight quoted units. Episode
context contains whole exact referenced messages and direct replies, with a
bound of eight messages and 16 KiB. Excess required context is refused, not
clipped. Omitted shared-thread context is stated and prevents a new-task
suggestion. The worker skips unchanged attempts and advances through pending
messages; `--message KEY` selects an exact message for a manual pass.

```sh
amq-board suggest --endpoint http://127.0.0.1:11434 --model qwen3.5:4b --digest FULL_DIGEST
amq-board suggestions
amq-board adopt-suggestion --suggestion ID --choice SELECTED_ID --generation N
amq-board ignore-suggestion --suggestion ID --generation N
```

Supply the full model digest from your local installation. The endpoint must
be literal loopback HTTP. The client checks the pinned Ollama version, model
digest, and disabled-cloud status; it does not start a server or download a
model. A failed check leaves tasks unchanged.

Text, JSON, and web views show the selected interpretations, source quotes,
and owner commands. Adoption requires the current generation and every
published selected choice ID; repeat `--choice` for multiple selections. A
new task also requires `--confirm-independent` after the owner reviews existing
and historical candidates. It is marked inferred/requested, never running.
Adoption applies to the quoted span, so other evidence in the message remains
separate. Ignoring a suggestion preserves the source message.

For sync-time processing, explicitly run `amq-board sync --suggestions` and
set `suggestions.endpoint`, `suggestions.model`, and `suggestions.digest` in
the user configuration `~/.amq/board.json`. Provider settings are user-only and
reload with the existing configuration. Ordinary sync does not run inference.

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
Codex goal aliases use canonical `CODEX_HOME` plus the native goal UUID; the
thread UUID is container context. A replacement goal keeps a distinct permanent
task. Explicit AMQ bindings can attribute a container; cwd alone does not name
an owner. Active status records source progress, never observed execution.

Claude filesystem tasks are read from any Claude Code version; a task file
without the expected id, subject, description, status, blocks and blockedBy
fields is refused. Their identity includes canonical home, list ID and task ID. Default UUID lists need
a matching session transcript in that exact home with a project cwd. Team and
custom lists need an explicit project-local snapshot binding:

```sh
amq-board bind-claude-list --home /absolute/claude-home --list native-list
amq-board unbind-claude-list --home /absolute/claude-home --list native-list
amq-board unbind-claude-list --binding-key '["/absolute/claude-home","native-list"]'
```

Binding changes share the serialized owner path. Revocation accepts the exact
stored key even when the native home or its path alias has disappeared. Acquisition with a removed
or replaced binding cannot admit tasks at commit. Native task owners remain
separate from AMQ container attribution. Unsupported producer versions, unbound
lists, unreadable files and storageV5 scopes are reported as incomplete coverage.
A single 16 MiB acquisition byte budget covers task payloads and session scope
reads across all homes. A stopped or incomplete scope preserves prior facts.
The filesystem adapter cannot certify other backends are empty. Native file
absence retains the prior task fact; it does not complete or cancel a task.
Native acquisition does not use model inference. Optional local suggestions
are a separate owner-adopted surface.

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

The optional `amq-board` Claude Code panel reads `amq/board-pane/v1` from the
live server's token-protected `pane.json` or `amq-board --pane-json`. Both use
one bounded projection of the published model. Full `--json` and `model.json`
remain complete evidence exports.

The pane shows all current task totals and up to 100 waiting tasks, with omitted
row counts. It includes up to 32 source summaries and states any omissions.
History and unresolved messages are outside its task counts. Commands are exact
context commands only; the pane does not copy owner mutations. Publication time
controls freshness, and recorded progress does not mean observed execution.

Both pane transports retain a 4 MiB encoded JSON limit. An unexpected overflow
fails visibly instead of clipping JSON. The panel starts no source acquisition,
initialization, or worker. Run `amq-board sync --watch` separately for updates.
