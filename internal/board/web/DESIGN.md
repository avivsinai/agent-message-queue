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
    fontSize: "1.75rem"
    fontWeight: 650
    lineHeight: 1.2143
    letterSpacing: "-.02em"
  headline-compact:
    fontFamily: "system-ui, -apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif"
    fontSize: "1.5rem"
    fontWeight: 650
    lineHeight: 1.25
    letterSpacing: "-.02em"
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

Inline disclosures reveal context in place. Color supports written state: warnings explain missing coverage, and errors identify failed operations. Local font stacks and CSS carry the system without external assets.

**Key Characteristics:**
- One reading lane with a secondary navigation rail.
- Readable titles, body text, and metadata at distinct sizes.
- Flat surfaces, fine dividers, and restrained cobalt selection.
- Native inline disclosures and visible keyboard focus.

## Colors

### Primary
Cobalt (`accent`) identifies links, actions, focus, and the selected destination. The pale selection tone (`selected`) fills active navigation, button hover, and text selection.

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
- Headline: view names; the compact headline applies at 959px and below.
- Title: record titles and section headings.
- Body: reasons and descriptions, with a repeated maximum line length of 66ch.
- Metadata: ownership, age, source observations, and field labels.
- Control: buttons and disclosure labels; navigation has the same size and weight with a slightly taller line height.
- Code: command fields and references. Times use tabular numerals.

**The Reading Size Rule.** Compact layouts reflow rows and controls without reducing body or metadata type.

## Layout

The shell is centered and bounded at 75rem, with 2rem side gutters on wide screens. A two-column workspace uses an 11rem navigation rail and a 2rem gap. The main column is one reading lane. A view heading and search wrap when space runs out. Search is bounded at 20rem on wide screens.

At 959px and below, gutters become 1rem, navigation moves above the content, secondary destinations and Appearance use More, and search fills its row. At 600px and below, work rows become one column; copy actions keep their natural width. Metadata wraps and definition lists stack. Work rows use 1.25rem by 1.5rem padding on wide screens and 1rem on small screens. Shared spacing steps are in the frontmatter.

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
Primary destinations are Next, Work, Backlog, and Requests. Sources, Agents, and Appearance form secondary context. The current destination uses accent ink on the selected tone. Links have a minimum height of 2.75rem. More is a native disclosure on compact layouts.

### Cards / Containers
A shared list surface groups work records; each row holds a title, reason, metadata, copy action, and Details. Secondary source and agent lists use the same surface and divider treatment. Empty states remain inside the list surface; search-empty feedback sits outside it.

### Inline Disclosure
Details keep source evidence and selectable commands in the reading flow. The detail body uses subtle fill and 1rem padding. Coverage is a compact warning disclosure above work lists, with its missing scope and source link inside. Header feedback keeps refresh errors and a pending Update snapshot action close to the snapshot time.

## Do's and Don'ts

### Do:
- Do keep command text selectable and show a written copy result.
- Do use inline disclosure for evidence and secondary context.
- Do preserve readable type when the layout becomes one column.

### Don't:
- Don't use color alone to identify status.
- Don't add flight-strip cells, condensed micro-labels, or a wall of cards.
- Don't hide the keyboard focus outline.
