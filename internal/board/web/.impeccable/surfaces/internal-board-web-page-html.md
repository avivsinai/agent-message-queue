---
version: 1
slug: "internal-board-web-page-html"
primary_target: "internal/board/web/page.html"
related_targets: ["internal/board/web/page.css","internal/board/web/page.js","internal/board/html.go","internal/board/html_views.go"]
---

# Board work navigator

Mode: Operate. Primary target: internal/board/web/page.html. Related targets: page.css, page.js, ../html.go, ../html_views.go.

## Direction contract

THESIS: The board reads like a well-set operations page: every record says what it is in one plain sentence, and the owner's queue is a row of items with their own ages, not a count. It refuses the equal-width card wall where empty stages take the same room as full ones.

OWN-WORLD: "Signal". The ratified cool neutral canvas, white or charcoal surfaces, and cobalt reserved for what needs the owner. Hierarchy and rhythm carry the beauty: a 2rem/700 view headline at -0.03em, stage heads in sentence case at 1.125rem with the count at the right, card titles at 1rem/600, a one-sentence summary in ink at .875rem, metadata muted. A conventional-commit prefix (fix(board):) becomes a quiet monospace chip, so the title starts with its noun. Flat surfaces, fine dividers, no shadows, no gradients.

STORY: The owner opens Flow, reads each Needs-you tile (title, sentence, why, how long), scans the stages for what moved, and opens a card only for provenance and commands.

FIRST VIEWPORT: 1440px: header with project and snapshot age; Needs-you as one tile per item with its age, gates as a separate quiet line, the requests link apart; coverage as one inline line naming the gaps; then the stage board where an empty stage is a narrow rail (word and 0, vertical) and full stages share the width. 390px: Needs-you directly under the header, before navigation detail and search.

FORM: Signal, a refinement inside the ratified Work Navigator world (no concept seed: extension of an established surface, owner chose Signal over Ledger and Instrument on 2026-11-02).

FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance
