# amq-board

`amq-board` shows a fleet of coding agents on one board. Run it in a project
that has an AMQ queue:

```sh
amq-board            # text
amq-board --json     # the board model (schema amq/board/v1)
amq-board --html --out board.html  # the board as one static page
amq-board --refresh  # ignore the GitHub cache and fetch once
amq-board serve      # the page, live on 127.0.0.1
```

It prints four columns, in this order:

- **Waiting on you**: requests owed by the reserved `user` handle, and work
  that waits on you or that you own and is blocked. Above the cards, each
  session shows its count of unread messages for `user`; that count overlaps
  the cards and is never added to them.
- **In flight**: work an owner holds, you included (an AMQ request owed by an
  agent, a bead in progress or assigned, an open PR waiting on review).
- **Blocked**: work that cannot move, and whose action it waits on.
- **Unclaimed**: work with no owner.

A source strip names each source with its status (`ok`, `degraded`,
`unavailable`) and what it did not assess. An empty column says "nothing"
only when every source that feeds it read completely; otherwise it names the
gap, for example "obligations before 2026-10-08". The roster lists each
session's live agents and agents with unread mail, DLQ messages, or open
requests; the rest are counted as quiet.

Columns come only from structured fields (message kinds, bead status,
assignees, dependencies). Subjects and titles are display text: a bead id in
a subject is shown as a related reference and never moves or merges a card.

## Static page

`amq-board --html` prints the same board as one self-contained HTML page;
`--out <file>` writes it to a file instead (through a temporary file renamed
into place). Open the file in a browser. The page loads nothing from the
network and runs only its own script and stylesheet, pinned by hash in its
Content Security Policy. Every source value is escaped. The board model is
embedded once as JSON (`<script type="application/json" id="model">`).

Each item is one strip: its ref, title, reason, age, owner, and the time its
source was last checked. Waiting on you shows every copy command on the strip;
the other columns keep commands, references, and evidence under Details. A
stale strip, or one whose owners disagree, sits out of line with its mark.
Ages are as of generation; the header says how old the page is and marks it
stale after 5 minutes, and the footer has the command that regenerates it.
The page never runs a command: Copy puts the command on the clipboard, and
the filter field hides strips that do not match a handle, ref, or title.

## Live page

`amq-board serve` serves the same page on `127.0.0.1`, on a free port, and
prints its URL once. The URL holds a random token, and every route lives under
it: `/<token>/` is the page, `/<token>/model.json` is the last built model
(the same JSON as `amq-board --json`), and `/<token>/refresh` refreshes now.
A request without the exact token is 404, a request whose `Host` is not the
printed address is refused, and any method other than GET is refused. The
page carries the static page's Content Security Policy plus `connect-src
'self'`.

The model is rebuilt every minute, and a request only reads the last one.
GitHub answers from its cache until the TTL ends, and each external provider
runs at most once per its TTL. Refresh on the page ignores those caches,
at most once a minute: a press within a minute of the last forced refresh, or
while a refresh runs, joins it, so five quick presses make one GitHub fetch.
The page reads `model.json` every 15 seconds and reloads when the model is
newer, unless a details panel is open or text is selected; the filter stays.

Each refresh reads `~/.amq/board.json` and `.amq/board.json` again. A valid
edit applies on that refresh without a restart. A refused edit (bad JSON or a
value the board warns about) keeps the last good configuration and is
reported once on stderr. The project directory is fixed at start. With
`forges.github.token_once` (default `true`, user file only), `serve` reads
`gh auth token` once and passes it as `GH_TOKEN` to its later `gh` calls only,
held in memory, so a session prompts the keychain once. It resolves the
token's account once (`gh api user`) and keys the GitHub cache by it, so a
later `gh auth switch` never serves the other account's cache. The usage
settings are part of the configuration a refused edit keeps. Ctrl-C stops
the view, cancels a running refresh's commands, and waits for them.

While it runs, `serve` writes `~/.amq/board/serve.json` (mode `0600`) with its
`url`, `token`, `pid`, and `started_at`, so a local client can find it without
reading the terminal. A file whose `pid` is not alive is stale. Ctrl-C or
SIGTERM removes the file; a second `serve` replaces it, and the first one does
not remove the second one's file when it exits.

## Claude Code panel

The `amq-board` Claude Code plugin is opt-in and separate from `amq-cli`, so
sessions that do not use it start no extra process. Install it from the
`avivsinai/skills-marketplace` marketplace:

```sh
claude plugin marketplace add avivsinai/skills-marketplace
claude plugin install amq-board@avivsinai-marketplace
```

Then type `/board` in a session of an AMQ project. A side panel shows the
Waiting on you strips (title, reason, and age), the In flight, Blocked, and
Unclaimed counts, one line of source status, and when the board was generated,
marked stale after 5 minutes. A status line shows `board: N waiting · M in
flight` while the panel is open. The panel refreshes every minute while it is
open, and starts nothing before `/board` or after the panel closes. Closing it
ends a refresh under way, which then draws nothing and sets no status, and
asks Claude Code to stop that refresh's `amq-board` run.

Each refresh reads the model from a running `amq-board serve` for the same
project, found through `~/.amq/board/serve.json` and used only while its
`pid` is alive. Without one, it runs `amq-board --json` in the session's
directory, one run at a time; a run that has not answered in 90 seconds ends
the refresh with "no answer in 90 s". GitHub then answers from the shared
cache within its TTL. When neither works, the panel
shows one line with the reason, for example `amq-board not found`. Every value
is drawn as plain text with control and format characters removed. The panel
runs no command from the board: a strip's copy command only puts it on the
clipboard. The plugin needs Claude Code 2.1.308 or later and does nothing on
an older version.

To check a change to the plugin, run from the repository root with Claude Code
2.1.308 or later; neither command needs a login:

```sh
claude plugin validate plugins/amq-board
claude plugin test plugins/amq-board
```

## Sources

| Source | Read | Notes |
| --- | --- | --- |
| AMQ | `amq env --json`, `amq session list --json`, `amq who --json`, per session `amq doctor --ops --json --root <session>`, `amq thread open --all-sessions --json` | Always on. AMQ resolves the root from the current directory. `doctor --ops` counts unread mail only for agents in a session's `config.json`; other mailboxes show "unread not counted", and a mailbox doctor could not read shows "not counted" with the reason and degrades the source. A failed `amq env` also degrades the source: the rest still shows, without cross-project answer actions. |
| beads | `bd --readonly --sandbox list --json --brief --status open,in_progress,blocked,deferred --limit 0` | When `bd` is on `PATH` and the project has `.beads/`. `--limit 0` reads every row (bd stops at 50 otherwise). Deferred beads are read so that a deferred blocker still blocks; they are not shown. A bead id mentioned elsewhere and not open is looked up with one `bd --readonly --sandbox list --all --id ...` call, at most 200 ids per run. |

| GitHub | `git remote get-url origin`, `gh config get user -h github.com`, and one `gh api graphql --hostname github.com` call for every repository in scope | When `gh` is on `PATH` and the `origin` remote is on github.com, or `forges.github.repos` names repositories. One call per refresh (plus the review-request pages below) because a gh call that reads the token can prompt the keychain; the response is cached for the TTL (default 10 min) under `~/.amq/board/cache/`, keyed by host, repo set, account (the viewer login the response names), and query version, with no token in it. A second run within the TTL makes no `gh api` call. Concurrent runs refresh one at a time: a run that waits reads the entry the other run wrote. After a failed refresh, runs for the next 60 seconds show the last entry as degraded with the failure and make no `gh api` call; `--refresh` skips that wait. With `GH_TOKEN` or `GITHUB_TOKEN` set, the account is the token's, which the configured user does not name, so the cache is not read. The first 50 open PRs and issues per repository are read; more are reported as "N more PRs not fetched". PRs that request your review are found by a search even past that page; the search reads up to 5 pages of 50, each page after the first one more `gh api graphql` call, and reports the rest as "N more review requests not fetched". |
| External providers | The executable and fixed arguments registered in `~/.amq/board.json` | Data only: the board validates every field, names the source `ext:<name>`, and drops invalid items, counting them as skipped. A provider cannot add a command to a card. |

On GitHub, a PR that requests your review, or one an agent authored that is
approved, mergeable, not a draft, and passing checks ("ready to merge"),
waits on you. An approved PR that is a draft or conflicting stays in flight
and shows those facts. A PR you authored is yours and waits on you. A bead
whose `external_ref` is `gh-<n>` and PR `<n>` are one card when exactly one
GitHub repository is in scope.

Each source runs under its own deadline. When AMQ fails, beads cards still
show and the roster says why it is missing; when `bd` is missing, beads shows
as unavailable and AMQ cards remain.

## Usage and limits

The board shows the tokens spent in the last 5 h and 7 d, and what each
harness account reports about its limits. It reads the harnesses' own local
records in place: Claude Code transcripts under `~/.claude/projects/`, Codex
rollouts under `~/.codex/sessions/`, and pi sessions under
`~/.pi/agent/sessions/`. It opens only those session files, keeps only their
numeric usage fields, ids and timestamps, and makes no network call. A
session counts for the project when its recorded working directory is in one
of the project's Git worktrees.

- **Spent** is per worker. A harness session's spend goes to the agent that
  `amq session list --json` `conversations[]` ties it to (records in state
  `ready` or `revoked`); subagent and child-thread spend rolls up to it.
  "Fresh" is uncached input plus cache writes plus output; cached reads are
  shown beside it. Project sessions with no such record show under the
  account's "other sessions in this project". A working directory alone never
  attributes spend.
- **Limits** are per account and never shown as a worker's budget: a Codex
  credit balance with its age, a Claude limit reached until its reset, or
  "limit unknown" with the reason (Claude Code writes plan usage only to its
  status line; pi records no plan limits). A worker whose account reached a
  limit shows "limit reached until HH:MM".
- Each reader counts only records of the harness versions it was checked
  against: Claude Code 2.1.267 up to 3, Codex 0.114.0 up to 1, and pi
  session format 3. A record of any other version, or with none, is not
  counted; a worker whose only records are such shows "spent unknown" with
  the version, never a number.
- One run lists and reads at most 300 MB in 1 s, checked per chunk read, so
  one long line cannot outrun it. A total that missed anything (a file the
  budget stopped, a file that could not be opened or identified, a line over
  16 MiB, an unverified version, or part of the file list) reads "at least",
  and the account says what it missed.

The `usage` key of `~/.amq/board.json` (the user file only) configures
this:

```json
{
  "usage": {
    "enabled": true,
    "homes": {"claude": ["~/.claude-work"]},
    "budget": {"wall": "1s", "bytes": "300MB"}
  }
}
```

- `enabled: false` reads no harness file.
- Homes listed under `homes` are read in addition to each harness's default
  home, each as its own account (a second Claude config directory, a second
  `CODEX_HOME`, a pi-format variant's home). A home listed twice, or the
  default listed again, is read once. A home is absolute or starts with
  `~/`.
- `budget.wall` is a Go duration; `budget.bytes` is a size in B, KB, MB, or
  GB, 1024-based.
- A file amq-board refuses (bad JSON, an unknown harness, a relative home, a
  bad budget) reads no usage at all and says why on stderr, so a mistake
  never reads homes the file meant to keep closed.

## Configuration

`~/.amq/board.json` (all keys optional):

```json
{
  "humans": {"beads_assignees": ["<name>"], "github_logins": []},
  "identities": {"github": {"<login>": "<amq handle>"}},
  "liveness_window": "30m",
  "stale_after": "36h",
  "obligation_max_age": "14d",
  "sources": {"beads": true, "github": true},
  "forges": {"github": {"repos": [], "ttl": "10m", "token_once": true,
                        "ready_to_merge_waits_on_human": true}},
  "providers": [{"name": "tracker", "exec": "/abs/path", "args": [], "ttl": "10m"}]
}
```

A project may override `liveness_window`, `stale_after`,
`obligation_max_age`, `sources`, `forges.github.repos`, and
`forges.github.ready_to_merge_waits_on_human` in `.amq/board.json`, and turn
a registered provider on or off with `{"name": "tracker", "enabled": false}`.
Executables, arguments, humans, identities, the GitHub TTL, and
`token_once` come only from the user file;
the board refuses them in a project file with a warning. The reserved AMQ
`user` and your GitHub login are always you; a GitHub author maps to an
agent only through `identities`.

## Read-only

The board writes to no source and never runs the commands it prints. It runs
commands as argument vectors, never through a shell.

`bd` with an embedded Dolt database updates the modification time of files
under `.beads/embeddeddolt/.../.dolt/noms/` (`journal.idx`, `manifest`, and
the table file) each time it opens the database, even with `--readonly`. Their
size and content do not change. To check that a run changes nothing, compare
path, size, and content hash of every file under `.beads/` before and after:

```sh
snap() { find .beads -type f -exec shasum -a 256 {} + | sort -k2; find .beads -type f -exec stat -f '%N %z' {} + | sort; }
before=$(mktemp); after=$(mktemp)
snap > "$before"; amq-board >/dev/null; snap > "$after"; diff "$before" "$after" && echo unchanged
```

(On Linux, use `stat -c '%n %s'`.)

## Commands on cards

Commands are printed for you to copy. Each one is complete: it never ends
inside a quote, and a message body is read from standard input (`--body -`;
type the text, then Ctrl-D). Values are validated first; a value that fails
validation produces no command. AMQ commands route with `--session`, and with
`--project` for a peer project. Run them in a shell of the board's project
(`amq env` or `amq coop exec`); from another shell they fail closed and
deliver nothing.

- **complete**: answers a request waiting on you with the kind and labels
  that close it (for a `todo`, `--kind status --labels done`). The card
  leaves on the next run.
- **acknowledge**: sends `--kind status`; the card stays.
- **show thread**, **message**, **show bead**, **claim bead**.

## Live acceptance check

Run this check with the installed `amq`, `amq-board`, and `bd` before you
accept a board change. It uses a throwaway Git project and the handles
`alpha` and `beta`; remove the project when you are done.

1. Make the project and two sessions:

   ```sh
   mkdir board-check && cd board-check && git init -q
   amq coop init --agents alpha,beta,user
   amq session create s1 --me alpha
   amq session create s2 --me beta
   ```

2. In a second terminal, as `alpha` in `s1`, send a review request to `beta`:

   ```sh
   eval "$(amq env --session s1 --me alpha)"
   echo "Please review the backoff change." |
     amq send --to beta --kind review_request --subject "Review: courier backoff" --body -
   ```

3. In a third terminal, as `beta` in `s2`, send two todos to `user`:

   ```sh
   eval "$(amq env --session s2 --me beta)"
   echo "30 s or 2 min?" | amq send --to user --kind todo --subject "Pick the retry window" --body -
   echo "Check the wording." | amq send --to user --kind todo --subject "Confirm the release note" --body -
   ```

4. In the first terminal, the owner's, run
   `eval "$(amq env --session s1 --me user)"` and then `amq-board`. In
   flight shows "Review: courier backoff", owned by `beta` in `s1`. Waiting
   on you shows both todos, with the line "s2: 2 unread for user" above
   them. The roster lists `s1` and `s2`.
5. In a separate terminal in this project, run `amq-board serve` and open
   the URL it prints. Keep the owner's terminal free for the commands below.
   The page shows the same strips. The same address without the token answers
   404.
6. On "Pick the retry window", press Copy on the complete command, paste it
   into the owner's terminal, type a reply, and press Ctrl-D. Press Refresh
   on the page, or wait one minute: the strip leaves Waiting on you. The
   unread line does not change, because it counts messages, not cards.
7. On "Confirm the release note", run the acknowledge command (under More
   commands and evidence) the same way and refresh: the strip stays.
8. Run `bd init`, then
   `bd update "$(bd create "Courier retry" --silent)" --status in_progress --assignee alpha`,
   and refresh: the bead shows in In flight. Run the `.beads` check under
   [Read-only](#read-only): it prints `unchanged`.
9. Take `bd` off `PATH` for one run:

   ```sh
   nobd=$(mktemp -d) && ln -s "$(command -v amq)" "$nobd/amq"
   PATH="$nobd:/usr/bin:/bin" "$(command -v amq-board)"
   ```

   The sources line shows `beads unavailable: bd not on PATH`, and the AMQ
   cards still show.
10. Press Ctrl-C in the `serve` terminal: it stops and removes
    `~/.amq/board/serve.json`.
