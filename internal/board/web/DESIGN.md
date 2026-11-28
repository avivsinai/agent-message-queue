---
name: AMQ Work Navigator
description: A readable work list with inline context and safe commands.
colors:
  page: "#f5f6f8"
  surface: "#fff"
  subtle: "#eef1f5"
  ink: "#18212f"
  muted: "#566273"
  divider: "#d8dee7"
  control: "#7c8797"
  accent: "#254edb"
  selected: "#eaf0ff"
  warning: "#825400"
  warning-bg: "#fff4d6"
  error: "#b42318"
  page-dark: "#14171c"
  surface-dark: "#1d222a"
  subtle-dark: "#272e39"
  ink-dark: "#f1f4f8"
  muted-dark: "#b2bdcb"
  divider-dark: "#394250"
  control-dark: "#7f8da1"
  accent-dark: "#a8c0ff"
  selected-dark: "#253451"
  warning-dark: "#ffd17d"
  warning-bg-dark: "#382d18"
  error-dark: "#ffb4ac"
typography:
  headline:
    fontFamily: "system-ui, -apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif"
    fontSize: "2rem"
    fontWeight: 700
    lineHeight: 1.1875
    letterSpacing: "-.03em"
  headline-compact:
    fontFamily: "system-ui, -apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif"
    fontSize: "1.5rem"
    fontWeight: 700
    lineHeight: 1.25
    letterSpacing: "-.03em"
  title:
    fontFamily: "system-ui, -apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif"
    fontSize: "1.125rem"
    fontWeight: 600
    lineHeight: 1.4445
  body:
    fontFamily: "system-ui, -apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif"
    fontSize: "1rem"
    fontWeight: 400
    lineHeight: 1.5
  metadata:
    fontFamily: "system-ui, -apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif"
    fontSize: ".875rem"
    fontWeight: 400
    lineHeight: 1.4286
  control:
    fontFamily: "system-ui, -apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif"
    fontSize: ".9375rem"
    fontWeight: 600
    lineHeight: 1.3334
  code:
    fontFamily: "ui-monospace, \"SF Mono\", Menlo, Consolas, monospace"
    fontSize: ".875rem"
    lineHeight: 1.5
rounded:
  control: ".5rem"
  list: ".75rem"
spacing:
  tight: ".25rem"
  small: ".5rem"
  related: ".75rem"
  base: "1rem"
  row-y: "1.25rem"
  section: "1.5rem"
  workspace: "2rem"
  view: "3rem"
components:
  button:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.accent}"
    typography: "{typography.control}"
    rounded: "{rounded.control}"
    padding: ".625rem .875rem"
    height: "2.75rem"
  button-hover:
    backgroundColor: "{colors.selected}"
  button-active:
    backgroundColor: "{colors.subtle}"
  button-disabled:
    backgroundColor: "{colors.subtle}"
    textColor: "{colors.muted}"
  button-failed:
    textColor: "{colors.error}"
  search:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    typography: "{typography.body}"
    rounded: "{rounded.control}"
    padding: ".625rem .75rem"
    height: "2.75rem"
  navigation:
    textColor: "{colors.muted}"
    rounded: "{rounded.control}"
    padding: ".6875rem .875rem"
    height: "2.75rem"
  navigation-selected:
    backgroundColor: "{colors.selected}"
    textColor: "{colors.accent}"
  work-list:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    rounded: "{rounded.list}"
  disclosure-content:
    backgroundColor: "{colors.subtle}"
    textColor: "{colors.ink}"
    rounded: "{rounded.control}"
    padding: "1rem"
  coverage:
    backgroundColor: "{colors.warning-bg}"
    textColor: "{colors.warning}"
    rounded: "{rounded.control}"
    padding: ".875rem 1rem"
  command:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    typography: "{typography.code}"
    rounded: "{rounded.control}"
    padding: ".75rem"
---
# Design System: AMQ Work Navigator

## Overview

**Creative North Star: "The Work Navigator"**

The Work Navigator uses a cool neutral canvas, a clear list surface, and restrained cobalt selection. A system sans supports reading; monospace identifies commands and references. Fine dividers and generous rows keep separate records easy to scan.

Inline disclosures reveal context in place. Suggestion quotes preserve line breaks and tabs; other control characters appear as escapes. Color supports written state: warnings explain missing coverage, and errors identify failed operations. Local font stacks and CSS carry the system without external assets.

**Key Characteristics:**
- One navigation bar under the header on every view: Inbox, Flow, Floor, Cost, and a closed More menu; there is no navigation rail.
- Readable titles, body text, and metadata at distinct sizes.
- Flat surfaces, fine dividers, and restrained cobalt selection.
- Native inline disclosures and visible keyboard focus.
- Four primary destinations, Inbox, Flow, Floor, and Cost; separate destinations under More.
- Cobalt is reserved for what needs you, links, and navigation; stage tones stay neutral.
- Stage marks that fill as work advances: an empty circle for Backlog, a quarter, half, and three quarters for Ready, In progress, and In review, a full disc for Shipped, and a square for Blocked.
- Station plates that state each agent seat's state as a glyph and a word, repeated by a 6px state bar.

## Colors

### Primary
Cobalt (`accent`) identifies links, actions, focus, the selected destination, and work that needs you. It is not a stage tone. The pale selection tone (`selected`) fills active navigation, button hover, and text selection; on surfaces already on that tone, selected text uses accent with surface ink.

### Secondary
Warning ink and its pale background (`warning`, `warning-bg`) identify incomplete coverage and old observations. Error ink (`error`) marks failed copy and refresh states. These states also use words.

### Neutral
The cool page (`page`) holds a light list surface (`surface`); inset context uses `subtle`. Primary and secondary text use `ink` and `muted`. Fine separators use `divider`; input and button borders use the stronger `control` tone. Dark siblings map the same roles to charcoal surfaces and lighter ink. Appearance offers System, Light, and Dark; System follows the browser preference.

**The Written State Rule.** State is stated in words; color reinforces the words.

## Typography

**Body Font:** system-ui with local platform fallbacks.
**Label/Mono Font:** ui-monospace with SF Mono, Menlo, and Consolas fallbacks.

The type is plain and readable. View headings lead; record titles support scanning; body text explains the reason; metadata supplies context. There is no display type role or condensed label face.

### Hierarchy
- Headline: view names at 2rem, weight 700, tracking -.03em; the compact headline (1.5rem) applies at 959px and below.
- Title: record titles and section headings.
- Body: reasons and descriptions, with a repeated maximum line length of 66ch.
- Metadata: ownership, age, source observations, and field labels.
- Control: buttons and disclosure labels; navigation has the same size and weight with a slightly taller line height.
- Code: command fields and references. Times, counts, and ages use tabular numerals. Conventional-commit prefixes use Code as a small chip. Scrollbars and native controls take the theme (scrollbar colors, accent color).

**The Reading Size Rule.** Compact layouts reflow rows and controls without reducing body or metadata type.

## Layout

The shell gives every view one left edge: the header, the navigation bar and each view's heading and content start at the same x. Reading views are bounded at 75rem from that edge, with 2rem side gutters on wide screens; Flow and Floor use the window up to 120rem. One navigation bar sits under the header on every view: the four main destinations and the More menu in one row, over a divider rule. A view heading and search wrap when space runs out. Search is bounded at 20rem on wide screens.

At 959px and below, gutters become 1rem, the bar stays one row down to 360px, and search fills its row. Snapshot age remains visible at compact widths. At 600px and below, work rows become one column; copy actions keep their natural width. Metadata wraps and definition lists stack. Work rows use 1.25rem by 1.5rem padding on wide screens and 1rem on small screens. Shared spacing steps are in the frontmatter.

Flow and Floor are the exceptions to the reading lane. With script, both views release the shell to the window width (2rem gutters, at most 120rem) under the same bar. On Flow, six stage columns share the width; at 1279px and below the board has three columns; at 600px and below the stages stack as sections under their headings. On Floor, sessions flow side by side, each holding its station plates at 16.5rem; a session wraps its plates, and at 600px and below the plates and sessions become one column. Without script, both render inside the normal shell.

Without script, all views remain readable in document order. With script, one destination is shown at a time. Print shows all views and removes navigation and controls.

## Elevation & Depth

Depth comes from tone and borders. Work and source lists have a 1px divider border. Rows share one list container and are separated by 1px rules. Disclosed context uses a subtle inset surface. No shadows are defined.

**The Flat Surface Rule.** Use surface tones and fine dividers for grouping; the system has no shadows.

## Shapes

Controls and inset context have gently rounded corners (`control`); list containers have a larger radius (`list`). Inputs and buttons use a 1px control border. Focus uses a 3px accent outline with a 3px offset. Controls have a minimum height of 2.75rem; compact coverage summaries retain their native disclosure shape.

## Components

### Buttons
Outlined surface buttons use accent text. Hover uses the selected tone, active uses the subtle tone, and disabled uses muted text. Copy success changes the label to Copied; failure uses error ink and opens the selectable command. There is no green success fill. Button and navigation color changes take 150ms ease-out only when reduced motion is not requested.

### Inputs / Fields
Search has a visible label, surface fill, control border, and full-size body type. Commands use a readonly, resizable monospace textarea with a minimum height of 4.5rem. Both retain visible focus.

### Navigation
Primary destinations are Inbox, Flow (the default), Floor, and Cost, one bar under the header, the same on every view and width. The bar comes before the content in tab order. Next, Work, Backlog, Requests, Older requests, Suggestions, History, Sources, Agents, and Appearance sit under More, in that order. Inbox carries its count after the word when it is above zero, in accent ink in every state, and its accessible name says the count in words ("Inbox, 2 waiting"). The current destination uses accent ink on the selected tone. Links have a minimum height of 2.75rem. More is a native disclosure that starts closed on every view; it opens with Enter or Space and closes with Esc, which returns focus to its summary, or with a click outside. Disclosure markers are drawn chevrons (a 1.5px stroke that turns when open), not browser triangles.

### Cards / Containers
A shared list surface groups work records; each row holds a title, a one-sentence summary clamped to two lines when the record has one, the reason, metadata, copy action, and Details. A conventional-commit prefix (feat, fix, and the like) is a quiet monospace chip on the subtle tone before the title, and the rest of the title is sentence-cased. Secondary source and agent lists use the same surface and divider treatment. Agents lists sessions with a live wake or recent presence first; the other sessions fold into one closed disclosure at the end of the view, "N sessions without recent presence". Empty states remain inside the list surface; search-empty feedback sits outside it.

### Flow Board
Each stage column has a 3px top rule in its stage tone (control for Backlog, muted for Ready, ink for In progress and In review, warning for Blocked, muted for Shipped), its stage mark, its word, and its count. Stages that hold cards share the width; an empty stage is a narrow vertical rail (2.75rem, rotated heading with its count) above 600px and a short heading at 600px and below. Cards are flat list-surface tiles with a 1px divider border and the control radius: the title (conventional-commit chip first, clamped to three lines), a one-sentence summary clamped to four lines, then metadata: owner, time in stage, "Needs you" when it applies, and the PR number with its CI word. When the stage time is unknown, a pull request card shows "open Nd" from its open date. There is no flag row above the title. A card that needs you has an accent border. The flag is a drawn diamond and the words Needs you in accent ink. CI is a small drawn shape and a word: a filled disc for CI green, an error-ink diamond for CI red, a ring for CI running, a dash for no CI.

The Needs you section sits above the board. Its heading holds the flag and a count. Each card that needs you is one tile on the selected tone with an accent border: the title as a link to its item in the Inbox view (three lines), the summary (four lines), then the reason and that card's own wait in accent. The panel also lists up to 25 open obligations owed by the owner as tiles, each a link to its Inbox item, with their deadlines and an omitted count. The heading counts waiting cards and all open owner obligations. Unread gate lines follow apart from the tiles on one quiet muted line, led by "Unread for you" and the oldest age. Coverage follows the section as the same compact coverage disclosure every view uses (see Inline Disclosure). The legend and the stage rules are one disclosure whose summary is the legend and "How stages are decided"; search sits inline beside it on the same row, and an open disclosure takes the full row.

A card is a native disclosure: with script, its body opens as a side sheet fixed to the right edge (34rem, full height, a 1px control rule on its left, no shadow), Esc or Close returns focus to the card, and opening one sheet closes the others. The sheet slides 1.5rem in 150ms only when reduced motion is not requested. Without script, the body opens in place under the card.

### Station Plate
The Floor shows one plate per agent seat with recent evidence, grouped under its session name and seat count. A plate is a flat tile with a 1px divider border, the control radius, and a 6px state bar on its left edge. From the top: the state glyph and word at title size and weight in the state ink, the handle and its harness, the state's reason, then Now (the most advanced owned card, clamped to two lines, with its stage and the count of the rest), Says or Status (the agent's own words in quotation marks, never styled as fact), Limit (warning ink, only when an account limit was read before later activity on the seat or its account), and Inbox; a divider rule separates the wake and last activity at the foot. The six states and their tones: ■ Limit reached in warning ink on the warning background, ▲ Stalled in error ink on the error background, ● Working in accent, ○ Idle in ink, – Off in muted ink with a control bar, and ? Unknown in muted ink with a dashed bar. The headline repeats the state counts with the same glyphs and ink; dormant mailboxes fold into one disclosure. A plate is a native disclosure with the same side sheet as a Flow card: state and reason, wake, inbox, last activity and its source, account limit, spend, quoted self-reports, owned work, and the message command. The board never claims a process is running or has stopped; state words name the evidence.

### Inline Disclosure
Details use the drawn chevron and keep source evidence and selectable commands in the reading flow. The detail body uses subtle fill and 1rem padding. Coverage is a compact warning disclosure above each view's content, as wide as its summary while closed: the summary is "Incomplete coverage:" and each source named with a plain status (read failed, unavailable, partly read). The body names each source again with its status (a partly read source adds its coverage gap in the source's words) and that source's owner step: a detected command in the standard command field with its Copy button, or words such as Install sqlite3. "Nothing for you to do." follows only when the board knows no step exists, then "Sources says why." Raw read errors stay in Sources.

### Header
One row: the brand and project at the left; at the right the snapshot time with its age and one button. Idle, the button reads Refresh and no status shows. When a newer snapshot waits, the button reads Update and one short status sentence sits beside the time; a failed refresh puts its sentence there in error ink. At 959px and below the header is two lines: the button sits beside the brand and the time takes the second line. There the Update label carries a waiting update and its sentence is read only by assistive technology; a failed refresh puts its sentence on a line of its own. A failed check keeps a waiting update. A view heading takes focus after a view change; it shows the focus ring only for keyboard focus, drawn around the heading's words.

### Request Rows
Requests is a reading lane. The headline sentence, the count of Inbox items as a link to the Inbox view, sits above the groups at title size, with the reminder that it is not a count of unfinished work and the link to older requests in muted metadata under it. One inline disclosure, "How this is counted", holds the count for requests to agents (one sentence that calls it the agents' bookkeeping), the coverage notes, and the recent and older partition. Each scoped addressee is a section heading with its count, holding one shared list surface of request rows. A row is a strip: the subject as title, then one observation per recipient as a drawn mark and words (a ring for no direct reply, in ink and medium weight; a filled disc for direct reply; a dashed ring for not assessed, both muted), with later thread messages after it, then any opinions, then kind, sender and recipients, and send time. An opinion sits apart from the observations on the subtle inset: the value in ink and weight 600, the classifier name after it, and the exact quote under a 2px control rule with its sender and age. The copy action is Copy thread command. A request without a direct reply has the copy action and Details: the message ID, bound tasks with their recorded states, text mentions, the attach control, and the owner commands. A replied request is a compact row with no commands. Each addressee shows up to 25 requests without a reply and the newest five replied ones, with a count of the rest. Older requests use the same list surface with compact rows (subject, kind, people, and time), the view's search, and the newest 200 with a count of the rest. No row prints a namespace path outside a command.

### Inbox
Inbox is a reading lane, the owner's queue. The view heading is followed by one sentence at title size: "2 items wait on you, 1 blocks work." or "Nothing waits on you." One shared list surface holds a row per item, blocking first, then oldest first: the first 25, with a count of the rest that points to `--json`; a tile whose item is not shown links to the view. A row, top to bottom: the title, with the drawn diamond and the words Blocks work in accent when the item blocks work; one metadata line (kind or bead, sender, age, and reply by, in warning ink with "(overdue)" once the time has passed); the summary sentence; the options as a definition list, one entry per option divided by fine rules, the label in ink and weight 600 with "Recommended" under it in muted words, then the option's summary and its "For:" and "Against:" lists in metadata size; "If you answer later: …" in muted metadata; the copy buttons, one per option labelled "Copy answer: <option>", the recommended first, then "Copy free answer"; and Details holding every command of the row as a command field, so a failed copy has a field to select. A plain question shows one "Copy answer"; a decision bead shows "Copy show bead"; a request without an answer command says why in muted words. There are no cards inside a row and no icons. After the list, "Mail for you" lists one compact row per session with its unread count, oldest age, and "Copy read command", under one muted sentence that mail is not counted as a decision. The empty state sits inside the list surface: "Nothing waits on you. Agents ask here when a decision is yours." A Needs you tile on Flow links to its row: the Inbox view opens and the row's title takes focus.

### Cost
Cost is a reading lane of sentences and two real tables. The heading is followed by the list-price sentence at title size, with the price date, "A subscription is not billed this way.", and the unpriced tokens in muted metadata. "By model" and "By seat" (over the longest window, 7 d) are `<table>` elements with a caption at title size, column header cells with scope, and a row header cell for the model or seat, on the list surface with fine row rules. Numbers are right-aligned tabular figures. The Share column is a number with a 4px bar under it: the subtle track and a fill in the neutral `control` tone, drawn as an inline SVG rectangle so no style attribute is needed. A share is a measurement, so it is never accent, and a row with no price says "not counted". A model id or a handle never breaks inside itself. The seat cell holds the handle and, under it, one muted metadata line with the session and harness; seat rows list by cost, and the account's other sessions follow last in muted ink, their line naming the session count and the account. Limits is one list surface with one entry per account, the account in ink and weight 600 and each limit sentence on its own line; a reached limit uses warning ink with its words. There is no chart, sparkline, or ring. A table wider than the lane scrolls inside its own container; at 600px and below each table row becomes a definition list, each cell under its column's name.

### Suggestion Review
Suggestions use the shared white list surface and fine dividers. Each entry shows its state, quoted source span, and proposed interpretations; line breaks and tabs remain in the quote, while other control characters appear as escapes. Native Details reveals the source message, exact span, and available terminal commands. Suggestions remain a separate review surface, outside task counts.

## Do's and Don'ts

### Do:
- Do keep command text selectable and show a written copy result.
- Do use inline disclosure for evidence and secondary context.
- Do preserve readable type when the layout becomes one column.

### Don't:
- Don't use color alone to identify status.
- Don't spend cobalt on stage tones; it marks what needs you, links, and navigation.
- Don't use font glyphs for flag and observation marks; draw them in CSS with one 1.5px stroke so they match the stage marks and CI shapes. Floor station glyphs are the exception.
- Don't add flight-strip cells or condensed micro-labels. Cards appear only on the owner-ratified Flow board, where each stage column is a short sorted list, and plates only on the owner-ratified Floor; other views stay as reading rows.
- Don't hide the keyboard focus outline.
