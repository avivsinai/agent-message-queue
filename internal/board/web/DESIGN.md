---
name: amq-board
description: A fleet board printed as flight-progress strips on a cool grey rack; one amber bay holds what waits on the owner.
colors:
  rack: "#dde2e6"
  rack-rule: "#b9c1c8"
  rack-ink: "#1b2026"
  rack-muted: "#47505a"
  stock: "#f8f2e2"
  stock-cell: "#efe6cf"
  stock-rule: "#ddd1b3"
  ink: "#221e17"
  muted: "#5c5444"
  accent: "#e9a63a"
  accent-ink: "#1f1a10"
  accent-wash: "#f6dfae"
  accent-text: "#8a4d00"
  focus: "#a35c00"
  live: "#2c6a32"
  warn: "#7a3d8c"
  error: "#a6261c"
  quiet: "#6b6457"
  rack-dark: "#13171b"
  rack-rule-dark: "#343c44"
  rack-ink-dark: "#e3e7eb"
  rack-muted-dark: "#a3adb7"
  stock-dark: "#29251d"
  stock-cell-dark: "#332e24"
  stock-rule-dark: "#463f31"
  ink-dark: "#f2ead8"
  muted-dark: "#c1b69c"
  accent-dark: "#e3a23d"
  accent-ink-dark: "#1c1609"
  accent-wash-dark: "#4a3818"
  accent-text-dark: "#f2bb5e"
  focus-dark: "#f2bb5e"
  live-dark: "#8ed39a"
  warn-dark: "#dcaeea"
  error-dark: "#ff958a"
  quiet-dark: "#aaa28f"
typography:
  headline:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", Roboto, \"Helvetica Neue\", sans-serif"
    fontSize: "1.125rem"
    fontWeight: 650
    letterSpacing: "-0.01em"
  title:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", Roboto, \"Helvetica Neue\", sans-serif"
    fontSize: "0.9375rem"
    fontWeight: 600
    lineHeight: 1.3
  body:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", Roboto, \"Helvetica Neue\", sans-serif"
    fontSize: "0.8125rem"
    fontWeight: 400
    lineHeight: 1.45
  small:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", Roboto, \"Helvetica Neue\", sans-serif"
    fontSize: "0.694rem"
    fontWeight: 400
    lineHeight: 1.45
  count:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", Roboto, \"Helvetica Neue\", sans-serif"
    fontSize: "1.125rem"
    fontWeight: 650
    fontFeature: "\"tnum\""
  count-waiting:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", Roboto, \"Helvetica Neue\", sans-serif"
    fontSize: "1.35rem"
    fontWeight: 650
    fontFeature: "\"tnum\""
  label:
    fontFamily: "\"Avenir Next Condensed\", \"Roboto Condensed\", \"Arial Narrow\", \"Helvetica Neue\", system-ui, sans-serif"
    fontSize: "0.694rem"
    fontWeight: 600
    letterSpacing: "0.08em"
  label-bay:
    fontFamily: "\"Avenir Next Condensed\", \"Roboto Condensed\", \"Arial Narrow\", \"Helvetica Neue\", system-ui, sans-serif"
    fontSize: "0.8125rem"
    fontWeight: 700
    letterSpacing: "0.1em"
  mono:
    fontFamily: "ui-monospace, \"SF Mono\", Menlo, Consolas, \"Liberation Mono\", monospace"
    fontSize: "0.694rem"
    lineHeight: 1.5
  mono-since:
    fontFamily: "ui-monospace, \"SF Mono\", Menlo, Consolas, \"Liberation Mono\", monospace"
    fontSize: "0.8125rem"
    fontWeight: 600
    fontFeature: "\"tnum\""
  control:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", Roboto, \"Helvetica Neue\", sans-serif"
    fontSize: "0.694rem"
    fontWeight: 600
    lineHeight: 1
rounded:
  square: "0px"
  strip: "2px"
  control: "3px"
spacing:
  rack-gap: "0.375rem"
  cell-y: "0.4rem"
  cell-x: "0.625rem"
  bay-y: "0.45rem"
  bay-x: "0.75rem"
  cock: "1.5rem"
  cock-phone: "0.75rem"
  page-gutter: "clamp(1rem, 2.5vw, 2rem)"
  bay-gap: "1.5rem"
  bay-gap-wide: "1.25rem"
  section: "2rem"
  footer: "2.5rem"
components:
  strip:
    backgroundColor: "{colors.stock}"
    textColor: "{colors.ink}"
    rounded: "{rounded.strip}"
    padding: "0.4rem 0.625rem"
  strip-cell:
    backgroundColor: "{colors.stock-cell}"
    textColor: "{colors.ink}"
    typography: "{typography.mono}"
  strip-cell-waiting:
    backgroundColor: "{colors.accent-wash}"
    textColor: "{colors.ink}"
    typography: "{typography.mono}"
  bay-head:
    backgroundColor: "{colors.rack}"
    textColor: "{colors.rack-ink}"
    typography: "{typography.label-bay}"
    padding: "0.45rem 0.75rem"
  bay-head-waiting:
    backgroundColor: "{colors.accent}"
    textColor: "{colors.accent-ink}"
    typography: "{typography.label-bay}"
    rounded: "{rounded.control}"
    padding: "0.45rem 0.75rem"
  gates:
    backgroundColor: "{colors.accent-wash}"
    textColor: "{colors.ink}"
    typography: "{typography.body}"
    padding: "0.45rem 0.75rem"
  command-code:
    backgroundColor: "{colors.stock-cell}"
    textColor: "{colors.ink}"
    typography: "{typography.mono}"
    rounded: "{rounded.strip}"
    padding: "0.2rem 0.4rem"
  copy-button:
    backgroundColor: "{colors.stock}"
    textColor: "{colors.ink}"
    typography: "{typography.control}"
    rounded: "{rounded.control}"
    padding: "0.3rem 0.6rem"
    height: "1.75rem"
    width: "4.25rem"
  copy-button-active:
    backgroundColor: "{colors.stock-cell}"
    textColor: "{colors.ink}"
  copy-button-copied:
    backgroundColor: "{colors.live}"
    textColor: "{colors.stock}"
  copy-button-failed:
    backgroundColor: "{colors.stock}"
    textColor: "{colors.error}"
  filter-input:
    backgroundColor: "{colors.stock}"
    textColor: "{colors.ink}"
    typography: "{typography.body}"
    rounded: "{rounded.control}"
    padding: "0.35rem 0.55rem"
  mark:
    textColor: "{colors.warn}"
    typography: "{typography.label}"
    rounded: "{rounded.strip}"
    padding: "0 0.3rem"
  token:
    backgroundColor: "{colors.stock-cell}"
    textColor: "{colors.ink}"
    typography: "{typography.mono}"
    rounded: "{rounded.strip}"
    padding: "0.05rem 0.35rem"
  source-lamp:
    rounded: "{rounded.square}"
    size: "0.625rem"
  agent-lamp:
    rounded: "{rounded.square}"
    size: "0.5625rem"
---

# Design System: amq-board

## Overview

**Creative North Star: "The Flight-Progress Rack"**

The board is an air-traffic flight-progress rack. Every card is a strip of buff card stock with fixed field cells, slotted into a cool grey rack. The owner's strips form the top bay at full width; the other three bays and the roster sit below it. A strip never floats free and never moves for decoration: it changes position only when a field says so. The one strip that leaves its slot is the cocked strip, and that displacement is the system's signature.

The world is dense and quiet. Two materials do almost all the work: the grey rack carries the chrome (masthead, source lamps, bay heads, roster, limits, footer), and the buff stock carries the items. One amber accent marks the Waiting on you bay and the focus ring. Status is told with square lamps and words, never with color alone. Small condensed uppercase labels name fields once; monospace carries refs, ages, and commands; every number uses tabular figures so values align down a bay.

The page follows the system color scheme only (`prefers-color-scheme`); there is no manual theme switch. The frontmatter records both themes as sibling keys: a plain key (`rack`) is the light value and its `-dark` sibling (`rack-dark`) is the dark value of the same CSS custom property in `internal/board/web/page.css`. All fonts are local system stacks; the page loads nothing external.

**Key Characteristics:**
- Two materials: cool grey rack for chrome, buff stock for items.
- One amber bay at full width, first on every viewport.
- Fixed field cells that repeat on every strip of a bay.
- The cocked strip: a strip pushed out of its slot, with its empty slot edge drawn dashed in the rack gutter.
- Square status lamps, each with its status word.
- Condensed uppercase labels, monospace refs and commands, tabular figures.

## Colors

A cool, low-chroma grey rack and a warm buff card stock, with one amber accent and three small status hues (green, violet, red).

### Primary
- **Bay Amber** (`accent`): the fill of the Waiting on you bay head only. Text on it is **Amber Ink** (`accent-ink`).
- **Amber Wash** (`accent-wash`): the gates row under the Waiting bay head, the ref and since cells of Waiting strips, the Waiting empty state, and text selection.
- **Amber Text** (`accent-text`): the kind word in Waiting strip refs, the Waiting count in the masthead, and the filter caret.
- **Focus Amber** (`focus`): the 2px focus outline on every focusable element. In light it is its own amber, distinct from `accent-text`, and holds at least 3:1 against both rack and stock; in dark it equals `accent-text`.

### Tertiary (status)
- **Signal Green** (`live`): an `ok` source lamp, a live agent lamp and its state word, and the Copied state of the copy button (stock-colored text on a green fill).
- **Caution Violet** (`warn`): degraded and partial sources, the "not assessed" key, marks on strips, a stale page age, and reached limits or usage.
- **Fault Red** (`error`): an unavailable source lamp and status word, and the failed copy state.
- **Quiet Grey** (`quiet`): the default lamp face and the outline of a quiet agent lamp.

### Neutral
- **Rack Grey** (`rack`): the page background and the plain bay-head background.
- **Rack Rule** (`rack-rule`): 1px rules between masthead, sources, bays, roster, limits, and footer; the filter input border; the scrollbar thumb.
- **Rack Ink** (`rack-ink`): primary text on the rack; the human agent lamp fill.
- **Rack Muted** (`rack-muted`): labels, meta lines, lamp status words, roster secondary lines, the footer, and the dashed slot edge of a cocked strip.
- **Strip Stock** (`stock`): the strip body, the filter input, the copy button, and command code that sits directly on the rack (roster and footer).
- **Cell Stock** (`stock-cell`): the ref and since cells of lower strips, command code on a strip, and ref tokens.
- **Stock Rule** (`stock-rule`): cell dividers in wide Waiting strips, the dashed detail divider, the copy button border, and related-token outlines.
- **Strip Ink** (`ink`) and **Strip Muted** (`muted`): primary and secondary text on stock. Secondary text on stock uses `muted`, never `rack-muted`.

### Named Rules
**The Two-Material Rule.** Rack text uses `rack-ink` and `rack-muted`; stock text uses `ink` and `muted`. A surface's text pair never crosses to the other material.

**The One Amber Rule.** The amber family belongs to the Waiting on you bay (its head, gates, empty state, and ref and since cells), the masthead Waiting count, focus, selection, the filter caret, and native control accents. No other bay, column, or status takes amber.

**The Violet Caution Rule.** Caution is violet, never amber or orange, so a warning can never be read as the owner's queue. Blocked has no color treatment at all: it is a plain bay like In flight and Unclaimed.

## Typography

**Body Font:** system-ui (with -apple-system, "Segoe UI", Roboto, "Helvetica Neue", sans-serif)
**Label Font:** "Avenir Next Condensed" (with "Roboto Condensed", "Arial Narrow", "Helvetica Neue", system-ui), set `font-stretch: condensed`
**Mono Font:** ui-monospace (with "SF Mono", Menlo, Consolas, "Liberation Mono", monospace)

**Character:** A plain system sans for reading, a narrow uppercase label face for field names, and monospace for anything the owner copies or compares. There is no display role: the largest text is the Waiting count.

### Hierarchy
- **Headline** (650, 1.125rem): the project name in the masthead.
- **Title** (600, 0.9375rem, line-height 1.3): the strip title, clamped to three lines.
- **Body** (400, 0.8125rem, line-height 1.45): the page base; reason, owner, gates, and meta lines.
- **Small** (400, 0.694rem): secondary lines in lower strips, roster counts and usage, lamp notes, evidence, and the footer.
- **Count** (650, 1.125rem, tabular): the masthead and bay counts. The Waiting bay count steps up to 1.35rem.
- **Label** (600, 0.694rem, 0.08em tracking, uppercase, condensed): section labels (Sources, Fleet, Filter, Limits, Regenerate), cell labels, count names, roster session names, ref kinds (700), and marks (700).
- **Bay Title** (700, 0.8125rem, 0.1em tracking, uppercase, condensed): the bay heads.
- **Mono** (0.694rem, line-height 1.5): refs, commands, tokens, and evidence facts. The since cell uses mono at the body size, weight 600.
- **Control** (600, 0.694rem, line-height 1): the copy button.

### Named Rules
**The Tabular Rule.** Every count, age, time, and limit uses tabular figures (`.num`, `time`, counts, gates, limits).

**The Label Once Rule.** A field label is printed once per bay, not once per strip. Where a bay has no label row, the strip carries a short lowercase cue word (`since`, `owner`) in the body face instead.

## Layout

The page is one column, `max-width: 120rem`, centered, with a side gutter of `clamp(1rem, 2.5vw, 2rem)`. The order is fixed: masthead, sources strip, the Waiting on you bay at full width, the lower grid (In flight, Blocked, Unclaimed, Roster), limits, footer. Horizontal rules in `rack-rule` separate the bands; there are no panels around them.

The lower grid has one column below 52rem, two columns from 52rem, and from 84rem three equal bay columns plus a roster column of `minmax(17rem, 0.85fr)`. Gaps are 1.5rem, tightening to 1.25rem at the widest layout.

Inside a bay, strips stack in a rack with a 0.375rem gap. Cells pad 0.4rem by 0.625rem; bay heads pad 0.45rem by 0.75rem.

The Waiting rack is a size container (`rack`) and its strip changes shape with the container, not the viewport:
- Narrow: ref and since share the top row, then title, reason, owner, command, and details stack.
- From 40rem: a 7rem ref column runs down the left edge; title and since, then reason and owner, then the command and details fill the right.
- From 68rem: one row per strip under a label row (Ref, Title, Reason, Since, Owner, Checked, Command) on the grid `7rem minmax(10rem, 1.2fr) minmax(9rem, 1fr) 6rem minmax(9rem, 1fr) 7.5rem minmax(22rem, 2.6fr)`. The command cell is the last cell, and only the first command sits on the strip; the rest go under details.

Lower strips keep one shape everywhere: a designator band (ref and since), the title, the owner, and a closing row with the reason on the left and Details on the right.

Below 40rem, bay heads stick to the top while their strips scroll, the sources and limits stack their label above the list, the filter takes the full row, and the cock offset halves. At 390px wide the page never scrolls sideways: long text breaks anywhere (`overflow-wrap: anywhere`) and cells stack instead of shrinking past legibility.

## Elevation & Depth

The rack is flat. The only shadow is the strip shadow, which lifts the card stock a hair off the rack; everything else separates by material (rack versus stock), cell tint (`stock-cell`), and 1px rules.

### Shadow Vocabulary
- **Strip lift** (`box-shadow: 0 1px 2px rgb(27 32 38 / 0.24)` in light, `0 1px 2px rgb(0 0 0 / 0.6)` in dark): every strip, and nothing else.

### Named Rules
**The Stock Lifts, The Rack Stays Rule.** Only strips cast a shadow. Bay heads, the roster, inputs, and buttons are flat.

## Shapes

Corners are nearly square. Strips, marks, tokens, and code blocks round at 2px; the input, the copy button, and the Waiting bay head (top corners) round at 3px. Status lamps are true squares with a 1.5px border: a filled square is a positive state, a hollow square (border only) is disabled or partial. Dashed lines mean "not here": the slot edge of a cocked strip, the divider above an open details region, and related (not merged) ref tokens.

## Components

### Strip (signature)
The unit of the board: a buff card with fixed cells.
- **Body:** `stock` with `ink` text, 2px corners, the strip lift shadow.
- **Ref cell:** mono at the small size, with the kind word in the condensed label face (700, `muted`) before the id (600). Tinted `stock-cell` in lower bays and `accent-wash` in the Waiting bay, where the kind word turns `accent-text`.
- **Since cell:** mono 600, right-aligned, never wraps; a `since` cue precedes the age except where the wide label row names the column.
- **Title:** title style, three-line clamp; marks sit beneath it.
- **Command cell:** a small `muted` label, then the command in a `stock-cell` code block that wraps anywhere and draws a `$ ` prompt in `muted` before it, then the Copy button.
- **Details:** a native disclosure closing the strip; its summary reads "More commands and evidence" when a command is on the strip and "Details" otherwise. Open, it shows the remaining commands, the full title, ref tokens, related tokens (dashed outline), and evidence lines with source, field, value, and age.
- **Wide Waiting strip:** cells are divided by 1px `stock-rule` lines, and the since cell drops its tint.

### Cocked Strip
- **Trigger:** a strip carries the mark `stale` or `owners disagree` (`internal/board/html.go`).
- **Form:** the strip shifts inline by `cock` (1.5rem; 0.75rem below 40rem). In the gutter it vacated, a 1px dashed `rack-muted` bracket (open on the strip side, inset 0.25rem top and bottom) marks the empty slot. The mark word on the strip names why.

### Bay Head
- **Plain:** `rack` background, a 1px `rack-rule` bottom rule, the bay title in the condensed label face, and the count beside it.
- **Waiting on you:** `accent` fill with `accent-ink` text, 3px top corners, no rule, and the larger count. A gates row in `accent-wash` follows it with per-session unread lines for the owner.

### Source Lamp
- **Shape:** a 0.625rem square with a 1.5px border, followed by the source name (650), its status word, and its age.
- **States:** `ok` filled `live`; `degraded` filled `warn`; `unavailable` filled `error`; `disabled` hollow `quiet`; `ok` but partial hollow `warn` with "· partial" after the status. A note line under the lamp names what was not assessed; a detail longer than 60 characters goes into a "why <status>" disclosure.

### Roster Agent
- **Lamp:** a 0.5625rem square: `live` fill for live agents, `rack-ink` fill for the human, hollow `quiet` otherwise.
- **Lines:** handle (650), state word right-aligned (live in `live`, 600), then counts, usage (`warn` when a limit is reached), and a Details disclosure. Quiet handles collapse into one "N quiet" disclosure per session.

### Buttons (Copy)
- **Shape:** 3px corners, at least 4.25rem by 1.75rem, `stock` fill, 1px `stock-rule` border.
- **Hover:** the border darkens to `muted`. **Active:** `stock-cell` fill.
- **Copied:** `live` fill and border, `stock` text, label "Copied".
- **Failed:** `error` border and text, label "Selected"; the command is selected for a keyboard copy. The button never claims a copy that did not happen, and every result is announced through a polite live region. Both states reset after 1.5s.
- **Motion:** background, color, and border transition over 150ms ease-out; reduced motion removes all transitions.

### Chips (Marks and Tokens)
- **Mark:** condensed uppercase label text in `warn` with a 1px `currentColor` border and 2px corners.
- **Token:** mono small text on `stock-cell`, 2px corners. A related token is transparent with a dashed `stock-rule` border and `muted` text.

### Inputs / Fields (Filter)
- **Style:** `stock` fill, `ink` text, 1px `rack-rule` border, 3px corners, `muted` placeholder, `accent-text` caret.
- **Focus:** the shared 2px `focus` outline with a 2px offset.
- **Behavior:** filtering hides strips by handle, ref, or title, and each bay states "N hidden by filter" so nothing disappears silently.

### Masthead
The project name, session line, and generation time with a live age that turns `warn` and says "stale: regenerate" after five minutes; counts for each bay as in-page links (Waiting count in `accent-text`); the fleet usage line and the filter.

## Do's and Don'ts

### Do:
- **Do** put every item on a strip with the same cells in the same positions as its bay neighbours.
- **Do** keep Waiting on you first and full width on every viewport.
- **Do** pair every status color with its word: lamp plus status text, mark plus its word, Copied or Selected on the button.
- **Do** use square lamps: filled for a positive state, hollow for disabled or partial.
- **Do** set refs, ages, and commands in monospace and every number in tabular figures.
- **Do** put secondary content (extra commands, evidence, long source details, quiet handles) in native `details` disclosures.
- **Do** cock a strip only for the `stale` or `owners disagree` mark, and always draw the dashed slot edge it leaves.

### Don't:
- **Don't** use amber outside the Waiting on you bay, its masthead count, focus, selection, the caret, and native control accents.
- **Don't** give Blocked a red frame or any color treatment; it is a plain bay.
- **Don't** use orange or amber for caution; caution is `warn` violet.
- **Don't** add shadows to anything but strips.
- **Don't** let any layout scroll sideways at 390px; stack cells instead.
- **Don't** load external fonts, scripts, or stylesheets.
- **Don't** put a label above each strip's cells; label a bay once, or use a lowercase cue word.
