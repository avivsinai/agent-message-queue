---
version: 1
slug: "internal-board-web-page-html"
primary_target: "internal/board/web/page.html"
related_targets: []
---

## Scope and mode

The static page that `amq-board --html --out <file>` writes. Mode: Operate.
A later live `serve` view inherits this surface.

## Task, content, constraints

- Task: answer "what needs me" first, then why, since when, and how complete
  and current the view is; turn a glance into one copied command.
- Content: the amq/board/v1 model (header, sources with coverage, four columns,
  operator gates, roster, usage and limits). The information architecture is
  fixed: header line, sources strip, Waiting on you at full width, In flight,
  Blocked, Unclaimed, roster, footer.
- Constraints: one page, no navigation, no modal or floating menu, ordinary Tab,
  Enter, Space only; light and dark from `prefers-color-scheme`; no external
  fonts or scripts; no `innerHTML`; restrained color with one accent reserved for
  Waiting on you and focus. 390 px stays usable: strips may stack their cells,
  but the page never scrolls sideways.
- Memorable moment: the cocked strip.
- Build path: code-first (sponsor). The decision comp B is the finish-review
  reference only; its red frame on Blocked is not kept.

## Direction contract

THESIS: Every item is a flight-progress strip with fixed field cells; the
owner's strips form the top bay, full width. It refuses free-floating cards: a
strip moves only when a field says so.
OWN-WORLD: Buff strip stock on a cool grey rack; one amber accent on the
Waiting on you bay and focus, nowhere else (no red frame on Blocked); condensed
sans small-caps cell labels once per bay, monospace refs and commands, tabular
figures; cell positions repeat on every strip so values align down the bay.
STORY: The owner scans the top bay row by row, reads reason, since, and
checked age in fixed cells, sees cocked strips as the ones that need a look,
and copies the command from the strip's last cell.
FIRST VIEWPORT: Header line; square status lamps with words; YOUR BAY with one
single-row strip per item (ref, title, reason and since, owner, checked age,
command and Copy); three bays below with fewer cells; roster bay right.
Signature: the cocked strip for stale or action-required items, with its word.
FORM: Air-traffic flight-progress strips, position 1 of 7, seed key 0a8db146.
FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance

## Decision comp (critique reference)

internal/board/web/.impeccable/mocks/decision/B.png (synthetic content,
gitignored).
