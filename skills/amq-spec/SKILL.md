---
name: spec
version: 1.0.0
description: >-
  Collaborative spec workflow between two agents. Use when the user says
  "spec", "design with", "collaborative spec", "spec with codex",
  "spec with claude", or any variation of "let's design X with Y".
  This is a structured multi-phase protocol — do NOT shortcut it.
argument-hint: "<description of what to design> [with <partner>]"
metadata:
  short-description: Multi-agent collaborative spec workflow
  compatibility: claude-code, codex-cli
---

# /spec — Collaborative Specification Workflow

You are running a structured collaborative specification workflow with a partner agent.
Follow EVERY phase below IN ORDER. Do NOT skip phases. Do NOT shortcut.

## Parse Input

From the user's prompt, extract:
- **topic**: a short kebab-case name for the spec (e.g., `auth-token-rotation`)
- **partner**: the partner agent handle (default: `codex` if not specified)
- **problem**: the full problem description the user gave

If the topic or problem is unclear, ask the user to clarify before proceeding.

## Pre-flight

1. Verify AMQ is available: `which amq`
2. Verify coop is initialized: check for `.amqrc` in the project. If missing, run `amq coop init`.
3. Set the thread name: `spec/<topic>`

## PHASE 1: Research (parallel — both agents independently)

**You MUST stay in normal mode (NOT plan mode) — you need full tool access.**

### Step 1a: Send research request to partner

Send ONLY the problem description — do NOT include any of your own analysis yet.

```bash
amq send --to <partner> --kind spec_research --thread spec/<topic> \
  --subject "Spec: <topic>" --body "<problem description from user>"
```

### Step 1b: Research the codebase yourself — IMMEDIATELY, do NOT wait

While your partner works in parallel:
- Explore the codebase: read relevant files, grep for patterns, check existing implementations
- Understand constraints, dependencies, and existing conventions
- Research external docs if the user mentioned any (APIs, services, etc.)
- Form your OWN conclusions — independent of your partner

### Step 1c: Submit your research findings

```bash
amq send --to <partner> --kind spec_research --thread spec/<topic> \
  --subject "Research: <topic>" --body "<your findings — use template below>"
```

**Research template:**
```
## Problem Understanding
<your interpretation>

## Codebase Findings
- <relevant files, patterns, constraints you found>

## Proposed Approach
<your high-level direction>

## Open Questions
- <things that need discussion>

## Risks
- <potential issues>
```

### Step 1d: Wait for partner's research

```bash
amq watch --timeout 120s
```

Then drain: `amq drain --include-body`

**Do NOT proceed to Phase 2 until your partner has also submitted `spec_research`.**

---

## PHASE 2: Exchange + Discuss (ping-pong with partner)

### Step 2a: Read partner's research

```bash
amq thread --id spec/<topic> --include-body
```

### Step 2b: Start the discussion

Compare your findings with your partner's. Send a message discussing:
- Where do your findings agree or differ?
- Which approach should you take and why?
- What are the trade-offs?
- Open questions that need resolution

```bash
amq send --to <partner> --thread spec/<topic> \
  --subject "Discussion: <topic>" --body "<your analysis of both researches + questions>"
```

### Step 2c: Continue back-and-forth

Wait for partner's response, then reply. **Expect at least 2 rounds.**
Continue until you've aligned on the approach, key decisions, and scope.

```bash
amq watch --timeout 120s
amq drain --include-body
# Reply with your thoughts
amq send --to <partner> --thread spec/<topic> --body "<response>"
```

**Do NOT proceed to Phase 3 until you've had a real discussion and aligned on the approach.**

---

## PHASE 3: Draft the Plan

Based on the research and discussion, draft a concrete implementation plan.

```bash
amq send --to <partner> --kind spec_draft --thread spec/<topic> \
  --subject "Plan: <topic>" --body "<plan — use template below>"
```

**Plan template:**
```
## Problem Statement
<clear problem definition>

## Proposed Solution
<concrete solution — what will be built>

## Architecture
<system design / component interactions>

## File Changes
- `path/to/file.ext` — what changes and why

## Decisions Made
- <decision>: <chosen option> because <rationale>

## Trade-offs Considered
- <option A vs B — why we chose A>

## Testing Strategy
- <how to verify>

## Risks
- <risk — mitigation>
```

Wait for partner's review:
```bash
amq watch --timeout 120s
amq drain --include-body
```

---

## PHASE 4: Review

Read partner's review feedback. If changes are needed, revise the plan and re-send as `spec_draft`. If the plan is agreed upon, proceed.

---

## PHASE 5: Present to User — MANDATORY

**This is the critical gate. Do NOT skip this.**

Present the final plan to the user directly in the conversation. Include:
- Problem statement
- Proposed solution
- File changes (what will be created/modified)
- Key architecture decisions (and why)
- Trade-offs considered
- Risks and mitigations

Then say:

> "This is the plan we (Claude + Codex) converged on. Do you want me to proceed, or do you have questions or changes?"

**STOP HERE AND WAIT.** Do not implement anything until the user explicitly approves.

Optionally send the agreed plan to partner:
```bash
amq send --to <partner> --kind spec_decision --thread spec/<topic> \
  --subject "Final: <topic>" --body "<final plan>"
```

---

## PHASE 6: Execute

Only after the user approves. Follow their instructions — they may want:
- Full implementation
- Incremental implementation
- Scope changes
- Different agent assignments

---

## When You RECEIVE a spec_research Message

If you are the partner agent (you received a `spec_research` message):

1. **Do your OWN independent research first** — do NOT read the sender's body yet
2. Explore the codebase yourself, form your own conclusions
3. Submit your findings as `spec_research`
4. THEN read the initiator's research from the thread
5. Continue with Phase 2 (discussion) when the initiator messages you

---

## Rules — Read These

- **NEVER** research alone and send a finished spec. That defeats the purpose.
- **NEVER** enter plan mode during research — you need tool access.
- **NEVER** skip the discussion phase — it's where the real design happens.
- **NEVER** implement before the user approves the plan.
- **ALWAYS** use `--kind spec_research` / `spec_draft` / `spec_review` / `spec_decision` — not `brainstorm` or other kinds.
- **ALWAYS** use `--thread spec/<topic>` for all spec messages.
- **ALWAYS** present the plan to the user and wait for approval.

## Reference

For the full spec protocol with templates and tracking details, see:
- [references/spec-workflow.md](references/spec-workflow.md)
