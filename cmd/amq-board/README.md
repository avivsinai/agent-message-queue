# amq-board

`amq-board` shows published tasks for one AMQ project. It reads sources through
an explicit sync command. A view refresh only reads the published snapshot.

```sh
amq-board sync --init              # first initialization and acquisition
amq-board sync --watch             # acquire and publish once per minute
amq-board                         # text
amq-board --json                  # complete model, schema amq/board/v3
amq-board --pane-json             # bounded native pane, amq/board-pane/v1
amq-board --html --out board.html  # one self-contained page
amq-board serve                   # live view on 127.0.0.1
amq-board check                    # one line per floor problem (exit 6 when any)
amq-board check --notify lead,alt --as floor  # send new incidents once per recipient
amq-board retro [--since DUR] [--json]  # request history per seat
amq-board classify --classifier NAME --next 8 --json  # export requests to label
amq-board classify --file labels.json                 # import labels
```

Views keep old publications readable as history. A publication past
`liveness_window` or with no publication time starts text and HTML with the
STALE sentence `Every number below is from then.` The specific publication age
follows it when known. `--json` and `--pane-json` always include a boolean
`stale` and `published_age_seconds`. A view reads no source and performs no
refresh.

`amq-board serve` follows `pane.json` on its existing polls. The page has one
STALE banner. Its local clock marks the current publication old after
`liveness_window`, including between polls. An old loaded page still becomes
old if the bound is shorter than its refresh interval. Reception of an
explicitly stale or over-age publication shows the banner; a fresh observed
publication hides it. No poll reads a source or retries a failed provider.

`retro` refuses old input and exits 1, like `check`: sync before acting.
`check`'s `source_failed` kind means the applicable source's `read_failed` fact
is true: the source supplied no current list for a read it attempted. Retained
cards do not turn that failure into success. Healthy partial, capacity-limited,
unavailable and disabled sources are not source failures; a source's coverage
names those other limits. Machine-readable source coverage includes
`read_failed` where applicable. A `source_failed` problem carries the source
under `source`, not `session`, so that source name is also its incident
identity. Floor-problem text still uses the source's existing coverage gap.

`amq presence list` prints every stored note with its age, or `(age unknown)`
for a missing or invalid `note_at`. A note older than `liveness_window` still
prints here; only the board Floor hides it. `note_at` changes when the note
text changes, not on a liveness Touch.

The installed `amq` must support `amq thread list --all-sessions --json`.
An unavailable command is reported as incomplete history. The board does not
scan mailboxes itself or fall back to a different source.

### Check the floor

`amq-board check` reads the published snapshot, not sources. After `amq-board sync`:

- `seat_stalled`: a seat has unread mail and shows no activity since it arrived.
- `seat_at_limit`: a seat is at its plan limit and owns current work.
- `account_near_cap`: a harness account's freshest plan-limit reading (not older than the 15-minute limit bound) shows 90 percent or more of a cap used. It needs no mapped worker and no card. Codex reports the pre-limit figures; Claude Code and pi report nothing before the limit, so they give no line.
- `ready_not_started`: a seat with nothing in progress is idle or off and has ready work assigned.
- `in_progress_not_worked`: a card is in progress longer than the liveness window, and its owner's last observed activity is older than that window. A seat with no observed activity is not listed.
- `reply_overdue` covers overdue agent replies, owner questions and owner rulings. For owner questions and rulings the sender reads the thread before taking the default or closing the ruling. Owner todos and review requests stay under Needs you.

An unclosed request older than `obligation_max_age` (14 days by default) expires, as in `amq thread open`. It leaves Needs you and the overdue check. Set `obligation_max_age` to `0` to disable expiry.

`news_overdue` means a request's first-news clock passed without news from its owner. A news-only obligation stays in the projection even without a completion deadline. A message with references is news about only those requests; a message without references can be news for each open request in its thread.

### Notify floor problems

`amq-board check --notify lead,alt --as floor` sends one ordinary status message with label `floor` for each new incident to each listed recipient. Create `floor` as a mailbox in the board's session. If the board uses the base queue directly rather than a session, name a complete declared base-root mailbox and complete declared base-root recipients instead. The sender is this dedicated observer, never a working seat or interactive agent. Every sender and recipient resolves in the live route at the board's session root or base root before the first send. A sender equal to a recipient is an error. This is an explicit broadcast to all listed recipients, not one alternate receiving only after another fails. Each send has the observed problem as its text, so both recipients see what the observer saw. The sender receives no notice copy.

The delivered set is under the board directory, scoped by incident, recipient and that recipient root's session name (empty for the base queue). One lock covers reading, evaluation, sends and pruning. Each incident/recipient/session pair is recorded after its send succeeds. The operator's supervisor schedules the two commands; AMQ starts nothing:

```sh

# the OS supervisor's schedule definition runs this pair, e.g. every minute
amq-board sync && amq-board check --notify lead,alt --as floor
```

A stale or unusable existing board snapshot sends `board_stale` once for each recipient and retains all current incident records; any read or decode error exits 1, even if a partially decoded value looks fresh. With no board store, the command sends nothing, creates no file under the board store and returns the same no-board error. This leaves the next `amq-board sync --init` able to initialize that store. Incidents in another session are independent. It does not deduplicate a stale record against seat/request incidents. An incident's identity ignores prose, age and the limit's example card. A fresh snapshot with no such incident clears its record; a later recurrence sends again. An unread floor notice remains unread mail and can make a seat that reads nothing look stalled once; it does not create new card or request incidents. A crash between a send and its durable record can send that one notice twice immediately after retry. A failed send returns 1 after every other notice was tried; the next run retries the missing pairs. Without `--notify`, exits are 0 for a clean floor, 6 for floor problems, 1 for an unusable board and 2 for a bad invocation. A notification failure returns 1; `--json` keeps the documented successful report schema and no stdout report is emitted for an unusable snapshot.

The in-progress inactivity rule requires an observed activity time. Other rules
use their own evidence, including unread mail, ready assignments and observed
account limits.

Exit `0` when no floor problem exists, `6` when at least one exists, and `1` when the snapshot cannot decide (including stale or missing data). `--json` emits the same rows in `amq/board-check/v1`. With `--notify --as`, exit 1 also means an identity, snapshot or delivery error; the report keeps a valid snapshot's existing output before the final exit code is selected.

```sh
amq-board check
amq-board check --json
```

### Retro

`amq-board retro --since 14d` measures requests opened during the last 14 days, the default period. Add `--json` for durations in seconds. The numbers measure recorded messages and cannot tell you how well an agent worked.

- **Seat** identifies the recipient by session and handle. Your seat appears as `you`.
- **Requests** counts one request for each recipient of a question, review request, or todo that opens an obligation.
- **Open** counts requests still open under the rules of `amq thread open`.
- **First reply** gives the median and longest time from opening to the recipient's first reply in the conversation.
- **Done** gives the median and longest time from opening to the message that closed the request.
- **Follow-ups** counts follow-up notices that fired for the request.
- **Rework rounds** gives the total requester replies and the largest count on one request, excluding follow-up notices and the closing message.

## Tasks

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

Copy commands in the page run only when the user runs them in a terminal.
Source/context commands can appear on a card; owner corrections remain
explicitly labelled in Details and do not make a task a Next action.

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

## Requests

Every recorded `question`, `review_request`, and `todo` in AMQ history is a
request, outside task and work counts. The board keeps three things apart:

- The message is a fact.
- Observations are derived for each view and never stored. A direct reply is
  a message whose last ref is the request, sent by a recipient; its thread and
  time do not matter. Later activity is a message in the same thread, with a
  strictly later recorded time, sent by a recipient. Neither means answered
  or finished.
- Opinions are labels from a classifier the caller runs. Each label names its
  classifier and the evidence digest it judged, and never changes a fact, an
  observation, or a count.

A party's scope comes from what AMQ records: `reply_to` (handle@session) and
`reply_project` on routed sends, and, for each root that holds a copy, the
agents that `amq thread list` reports for that thread in that root. A handle
is scoped to the one copy root whose agents name it; for a routed message the
sender's session is never the recipient's. When no root or several roots
name the handle, or the recipient is in another project, the scope is
unknown and the recipient is not assessed: a copy location alone is never
proof, and a copy without recorded agents names no handle. The board never guesses a session from a bare
handle.

Requests from the last 14 days, by recorded message time, are Recent; the
rest are Older, or Undated when the time is missing, unreadable, or in the
future. Age never completes a request. Recent requests are grouped by scoped
addressee, with requests that have no direct reply first. Needs you lists open
obligations owed by the owner across the retained history, alongside waiting cards.
The Requests headline is "N recent requests to you with no direct reply observed": a
request addressed to the owner handle `user`, in any root, with no direct
reply from it. The Requests section states the rest in one sentence, "N
recent requests to agents have no direct reply observed", and calls it the
agents' bookkeeping: agents often answer with a new message instead of a
reply. A request counts once, as the owner's when the owner has not replied
directly, otherwise when any scoped recipient sent no direct reply; later
thread activity is a secondary observation on the row. In a `p2p/` thread
every message between the pair shares the thread, so later activity there is
weak evidence. Requests with an unknown recipient scope are listed and
counted separately in the Requests section, which also states incomplete
history coverage. Neither count is a count of unfinished work.

Owner dismissal and action cancellation remove a request from the view. An
attachment or an adopted suggestion binds a task to the request; the request
stays listed and shows the task's recorded state as separate context. A
request that names a current card's bead ID appears on that card as related
context; this never attaches, admits, or moves work.

### Classification on demand

```sh
amq-board classify --classifier MODEL/requests-v1 --next 8 --json > batch.json
amq-board classify --file labels.json
```

Export is read-only. It lists up to `--next` Recent requests without a label
from that classifier for their current evidence digest, newest first. Each
candidate carries whole messages, never clipped: the request, its direct
parent, direct replies, later thread messages, then other referrers, up to
eight messages and 16 KiB, with the count of omitted evidence messages. A
request larger than 16 KiB alone is `unclassifiable` and takes `unclear`.

The evidence digest covers the request, its direct parent, its referrers,
same-thread messages not recorded earlier, the scoped identities, bound
tasks (ID, binding, recorded state, cancellation), owner disposition, the
resolved sender scope of every evidence message, and the structured history
coverage (complete, and the count of omitted records; never its wording),
including messages left out of the slice. Each successful import drops stored
labels whose message is gone and earlier labels of the messages it labels;
other labels stay. An import that adds and drops nothing publishes nothing. New evidence makes the request current for
classification again; the earlier label stays stored but is no longer shown.

The label file has schema `amq/board-request-labels/v1`, the export's
`project_id` and `classifier`, and `labels` entries with `message_key`,
`evidence_digest`, `value` (`done`, `open`, `superseded`, `not_an_ask`, or
`unclear`), and an optional `quote` of `message_key`, `field` (`body` or
`subject`), and `text`. Every value except `unclear` needs an exact quote from
a message in the candidate's slice; the check proves where the text came
from, not that the judgment is right. Import validates the whole file under
the writer lock and publishes it as one snapshot update in
`request_labels`, keyed by the SHA-256 of `[message_key, evidence_digest,
classifier]`. An identical label is accepted again; a different value for
the same key is refused, so a revised classifier uses a revised name. The
board runs no model and schedules nothing; the `amq-board` skill describes
the classification loop for a harness model.

## Withdrawals

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
on AMQ work has not been measured. Unsupported matches stay unmatched;
bounded retrieval does not establish that an outcome is new.
Replies, status updates, and answered requests can supply attachment evidence;
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
snapshot with its journal, a recovery copy, immutable hash-addressed evidence,
and a writer lock.
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
Each publication appends its changes to `journal.jsonl` beside the snapshot, so
a refresh writes what changed, not the whole history. When the journal reaches
a quarter of the snapshot, the next publication writes a new snapshot: the
old snapshot and its journal are kept as `snapshot.backup.json` and
`snapshot.backup.journal.jsonl`, which together are the last generation before
the new snapshot, and `journal.jsonl` is removed once the new snapshot is
durable. Readers replay the journal over the snapshot; a journal that does not
continue the snapshot's generation one by one requires recovery.

An interrupted publication leaves `write-pending`, and its text says which
step was interrupted:

- `journal append pending`: the last journal line can lack a newline. Its
  generation was never published: remove that partial line, then
  `write-pending`.
- `new base pending: generation N`: if `snapshot.json` has generation N, the
  new snapshot is published: remove `journal.jsonl` if it is still there, then
  `write-pending`. Otherwise the old snapshot and `journal.jsonl` are still the
  published state: remove only `write-pending`.
- `durability uncertain`: the publication is written, but its directory sync
  failed. Once the disk is healthy, remove `write-pending`.

To restore the recovery copy, copy `snapshot.backup.json` to `snapshot.json`
and `snapshot.backup.journal.jsonl` to `journal.jsonl` (or remove
`journal.jsonl` when there is no backup journal).

The snapshot bound is 64 MiB, including 1 MiB reserved for coverage-only
publication; the evidence bound is 256 MiB. A publication past the snapshot
bound first prunes the oldest history that no task, suggestion, or withdrawal
holds, down to three quarters of the bound, and does not acquire those
messages again. Pruned ids are kept for 30 days after their messages'
creation; a message older than that which appears for the first time after a
prune is not acquired. A pruned message's dismissal, action cancellation, and request
labels go with it; tasks, their attachments, and task-level corrections stay.
Each new snapshot removes every evidence file that neither it, the recovery
copy, nor a pending retirement names, so pruned and abandoned evidence does not
count against the evidence bound. A refresh that would pass the evidence bound first publishes a new snapshot,
which removes the evidence nothing names, and measures again. Only named
evidence still past the bound prunes the oldest unheld history, by the bytes
over it, and publishes incomplete source coverage; the pruned evidence goes
at a later publication, and ingestion resumes. Evidence staged by a refresh
and removed by a new snapshot before that refresh publishes is staged once
more. If the snapshot is still at its bound, or nothing is left
to prune, ingestion stops and publishes incomplete source coverage;
identities and corrections are not evicted.

## Sources and configuration

Sync reads AMQ through read-only CLI commands. Historical metadata comes from
`amq thread list --all-sessions --json`; full bodies come from the existing
`amq thread --id THREAD --agents HANDLES --include-body --json --root ROOT`.
Missing bodies, conflicting message content, and read bounds are coverage gaps.
Beads comes from `bd --readonly --sandbox list --json --all --limit 0`; an
open bead's description and notes show in its details; every bead shows a
one-sentence summary of its description.
Its namespace is the canonical `database_path` returned by `bd --readonly
--sandbox info --json`, checked before and after acquisition.
Closed task identities remain known. The board never changes source records.

Roster, usage, GitHub, and configured provider coverage remain secondary facts.
Codex goals are read with `sqlite3` read-only, without writing to the Codex
home. A live WAL database needs its `-wal` and `-shm` files. When Codex has
checkpointed and removed `-wal`, the main file holds every commit and is read
immutable, which creates no sidecar; if the main file changes during that
read, the source degrades and the next sync reads again. Codex goal aliases
use canonical `CODEX_HOME` plus the native goal UUID; the thread UUID is
container context. A replacement goal keeps a distinct permanent
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
separate from AMQ container attribution. The source reads filesystem task
lists and states that storageV5 is not read as its scope. A list with no task
file is no tasks. Unbound lists and unreadable or unsupported files are
incomplete coverage; only a read that cannot run (the Claude home or its
task directory unreadable, or the project worktrees unknown) degrades the
source, and a degraded source always states its reason.
A single 16 MiB acquisition byte budget covers task payloads and session scope
reads across all homes. A stopped or incomplete scope preserves prior facts.
The filesystem adapter cannot certify other backends are empty. Native file
absence retains the prior task fact; it does not complete or cancel a task.
Native acquisition does not use model inference. Optional local suggestions
are a separate owner-adopted surface.

Configuration uses `~/.amq/board.json` and `.amq/board.json`. Settings include
`sources`, `humans`, `forges`, `providers`, `liveness_window`, `stale_after`,
`shipped_window` (default `7d`), `obligation_max_age`, and user-only `usage`. Sync and serve reload configuration;
a refused edit keeps the last good value. Source identity is restart-only.
Usage collection can be disabled with `{"usage":{"enabled":false}}`. Enabled
usage reads numeric harness usage facts; Codex plan limits use its installed
app-server interface and the existing bounded collector cache.

## Views

Text, JSON, static HTML, and the live page use the same pure task projection.

Flow is the default page view and a `== Flow ==` text section. It puts every
current card in one stage, first match wins: In review (a linked pull request
is open and not a draft), Blocked (the tracker records a blocker), In progress
(the tracker says started, or the only open pull request is a draft), Shipped
(done or merged within `shipped_window`, with a recorded close time), Ready
(assigned, not started), and Backlog (everything else that is current). A
bead's close time is its `closed_at`; Codex goals and Claude tasks record no
close time, so their finished work stays in History. Shipped lists the 12
newest cards and counts the rest. Flow cards also include the open GitHub pull
requests and issues of the published snapshot. A forge item joins a task card
only when the task's bead records it as the same work (`external_ref gh-N`); a
pull request that only names a bead stays its own card and shows the name.
Forge cards are never tasks: they have no task ID, cannot take attachments,
and stay outside task counts. Needs you (a structured field waits on you) is a
flag on the card, not a stage; the strip above the board lists the cards that
need you, the unread messages for you, and the requests to you that have no
direct reply. A pull request card shows its checks as CI green, CI red,
CI running, or no CI. `--json` carries `flow` (stage, title, card IDs,
omitted count) and `forge_cards`; each card carries `stage`, `stage_since`,
`flags`, and `ci`. Pane task rows carry `stage`. Next, Work, Backlog, and
Requests remain.

Floor is a page view and a `== Floor ==` text section with one station per
agent seat of the AMQ roster. A seat is shown when its wake is live, presence
or usage shows activity inside `liveness_window`, it has unread mail newer than
`stale_after`, or a current card names it; the other mailboxes fold into one
dormant count per session, and so does a mailbox whose fresh mail arrived after
more than `stale_after` without activity. The state is the first that matches:
Limit reached (the account reports a reached limit whose reset is ahead, and
neither the seat nor any session of that account was active after the limit
was read: a limit is account-wide), Stalled (mail older than
`liveness_window` waits and the last activity is older than that mail; unread
ages date from the AMQ read),
Working (activity inside `liveness_window`), Idle (the wake is live and the
inbox is counted; no mail waits without an activity record), Off (no wake and
no recent activity; the inbox is counted), and Unknown (the inbox was not
counted, or mail waits and no activity was observed). Activity is the
worker's newest usage record, or presence last seen when no wake is live: a
live wake refreshes presence itself, so it proves only the wake. The plate
names the fact it used. The board cannot see processes and never says a
worker is running or has stopped. A seat lists the current cards that name
it, In progress first, then In review, Ready, and Blocked; a card that names a
handle without a session is listed under each seat of that handle with
"handle in N sessions". The Floor ages a presence note from its own `note_at`, not wake activity. It prints the age, removes the note at the liveness bound, and reports `age unknown` if the original record has no timestamp. `amq presence list` preserves the old note and labels the same age.

The agent's presence note (Says) and free-text status
are quoted, never treated as facts. A limit read before later activity shows
as "limit reported N ago; active since" (or "the account was active since");
a limit with no reset time shows when it was read. Claude Code error turns
(`isApiErrorMessage`, model `<synthetic>`) are not spend or activity. `--json` carries `stations` and
`dormant_mailboxes`; harness, note, notifier mode and reason, and doorbell
parked come from `amq who --json` and `amq doctor --ops --json` at sync.
Roster agents carry their usage and reached account limit in every view. Each
seat also shows one context line: the prompt size of the newest counted
response of the worker's mapped sessions (input + cache reads + cache writes,
`last_context_tokens` in `--json`) with its age, and the 5 h window's
cache-read share (`cache_read / (fresh + cache_read)`) when the window holds
cache reads. pi records no cache reads, so a pi seat shows no share. It is a
measured fact, not advice: no threshold, no forecast. A pi
seat joins through its live bridge's `session_id`, so its spend and last
activity attach to the seat. To read a pi home outside `~/.pi`, add
`{"usage":{"homes":{"pi":["~/other-home"]}}}` to `~/.amq/board.json`.
A request that names a current card's bead ID appears on that card as
related context, with a count and the newest request's thread command. Text
shows at most 25 Recent requests per addressee and the 25 most recently
updated historical tasks, with counts. Per addressee, the page's Requests
view lists up to 25 Recent requests without a direct reply, with their
commands in Details, and the newest 5 with a direct reply from every scoped
recipient as compact rows; the rest are counted. The searchable Older
requests view lists the newest 200 and counts the rest. An opinion's quote names its sender and
age. The text and HTML views withhold bodies. `--json` and `model.json` include
the published request records and their bodies. Inspect the original thread when
you need the text of a reply referenced by key.

Static HTML is self-contained and escapes source content. The live server binds
127.0.0.1 on a free port. Its random token protects every route; Host checks
protect against DNS rebinding. The page never runs source commands.

The live page reloads the snapshot once per minute or when Refresh is pressed.
It shows publication freshness and source coverage. Every view names the
sources not fully read in one plain line (read failed, unavailable, or
partly read). The line names an owner step the board detects (bind an
unbound Claude task list, install `sqlite3`). It says there is nothing for
the owner to do only when every source read ran and none of its gaps is an
owner step; otherwise it points to Sources, which gives each reason. A failed reload retains the
last rendered snapshot with a visible source error. `~/.amq/board/serve.json`
(mode 0600) lets local clients find the view; shutdown removes its own record.

The optional `amq-board` Claude Code panel reads `amq/board-pane/v1` from the
live server's token-protected `pane.json` or `amq-board --pane-json`. Both use
one bounded projection of the published model. `--json` and `model.json` export
the full published model, including its source coverage.

The pane shows all current task totals and up to 100 waiting tasks, with omitted
row counts. It includes up to 32 source summaries and states any omissions.
History and requests are outside its task counts. Commands are exact
context commands only; the pane does not copy owner mutations. Publication time
controls freshness, and recorded progress does not mean observed execution.

Both pane transports retain a 4 MiB encoded JSON limit. An unexpected overflow
fails visibly instead of clipping JSON. The panel starts no source acquisition,
initialization, or worker. Run `amq-board sync --watch` separately for updates.
