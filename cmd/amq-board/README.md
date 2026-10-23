# amq-board

`amq-board` shows a fleet of coding agents on one board. Run it in a project
that has an AMQ queue:

```sh
amq-board            # text
amq-board --json     # the board model (schema amq/board/v1)
amq-board --refresh  # ignore the GitHub cache and fetch once
```

It prints four columns, in this order:

- **Waiting on you**: requests owed by the reserved `user` handle, and work
  that waits on you or that you own. Above the cards, each session shows its
  count of unread messages for `user`; that count overlaps the cards and is
  never added to them.
- **In flight**: work an owner holds (an AMQ request owed by an agent, a bead
  in progress or assigned).
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

## Sources

| Source | Read | Notes |
| --- | --- | --- |
| AMQ | `amq env --json`, `amq session list --json`, `amq who --json`, per session `amq doctor --ops --json --root <session>`, `amq thread open --all-sessions --json` | Always on. AMQ resolves the root from the current directory. `doctor --ops` counts unread mail only for agents in a session's `config.json`; other mailboxes show "unread not counted". |
| beads | `bd --readonly --sandbox list --json --brief --status open,in_progress,blocked,deferred --limit 0` | When `bd` is on `PATH` and the project has `.beads/`. `--limit 0` reads every row (bd stops at 50 otherwise). Deferred beads are read so that a deferred blocker still blocks; they are not shown. A bead id mentioned elsewhere and not open is looked up with one `bd --readonly --sandbox list --all --id ...` call, at most 200 ids per run. |

| GitHub | `git remote get-url origin`, `gh config get user -h github.com`, and one `gh api graphql` call for every repository in scope | When `gh` is on `PATH` and the `origin` remote is on github.com, or `forges.github.repos` names repositories. One call per refresh because a gh call that reads the token can prompt the keychain; the response is cached for the TTL (default 10 min) under `~/.amq/board/cache/`, keyed by host, repo set, account, and query version, with no token in it. A second run within the TTL makes no `gh api` call. The first 50 open PRs and issues per repository are read; more are reported as "N more PRs not fetched", and PRs that request your review are found by a search even past that page. |
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
  "forges": {"github": {"repos": [], "ttl": "10m",
                        "ready_to_merge_waits_on_human": true}},
  "providers": [{"name": "tracker", "exec": "/abs/path", "args": [], "ttl": "10m"}]
}
```

A project may override `liveness_window`, `stale_after`,
`obligation_max_age`, `sources`, `forges.github.repos`, and
`forges.github.ready_to_merge_waits_on_human` in `.amq/board.json`, and turn
a registered provider on or off with `{"name": "tracker", "enabled": false}`.
Executables, arguments, humans, and identities come only from the user file;
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
