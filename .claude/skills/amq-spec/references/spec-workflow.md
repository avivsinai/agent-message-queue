# Spec Workflow

Collaborative specification workflow for multi-agent design tasks. The word "spec" was chosen specifically to avoid collision with Claude Code's plan mode.

**Core principle**: Each agent researches independently with no biases, then they exchange, discuss architecture, and converge on a plan — which is presented to the USER for approval before any implementation.

This is a **skill-managed protocol** — agents follow these instructions using standard AMQ primitives (`amq send`, `amq drain`, `amq thread`).

## DO NOT SKIP PHASES

You MUST follow every phase in order. The most common failure mode is an agent doing all the research itself, composing a full spec, and dumping it to the partner as a finished document. **That defeats the entire purpose.**

**What you MUST NOT do:**
- Research alone and send a finished spec to partner
- Use `--kind brainstorm` or any non-spec kind for spec messages
- Skip the discussion phase
- Send a draft before both agents have exchanged research
- Implement anything before the user approves the plan

## Phases Overview

```
1. RESEARCH (parallel)  →  2. EXCHANGE + DISCUSS (ping-pong)  →  3. PLAN (main agent drafts)  →  4. REVIEW (partner)  →  5. PRESENT TO USER  →  6. EXECUTE
```

| # | Phase | Who | What happens | Gate to proceed |
|---|-------|-----|-------------|-----------------|
| 1 | **Research** | Both agents, in parallel | Each independently explores codebase, reads docs, searches. Submits findings as `spec_research`. | Both agents have submitted `spec_research` |
| 2 | **Exchange + Discuss** | Both agents, ping-pong | Read each other's research. Discuss architecture decisions, trade-offs, open questions. Multiple rounds. | Agents have aligned on approach |
| 3 | **Plan** | Main agent (usually Claude) | Drafts a concrete implementation plan based on the discussion. Sends to partner as `spec_draft`. | Partner has received the draft |
| 4 | **Review** | Partner agent | Reviews the plan, flags issues, suggests changes. Sends `spec_review`. Main agent revises if needed. | Plan is agreed upon |
| 5 | **Present** | Main agent | Presents the final plan to the **user**. Waits for questions, feedback, and explicit approval. | **User approves** |
| 6 | **Execute** | Per user instructions | Implement the plan. Only after user says go. | — |

## CRITICAL RULES

### Research Independence (Phase 1)
- **NEVER read your partner's research before submitting your own.** This eliminates bias.
- **Do NOT enter plan mode** during research — you need full tool access to explore the codebase, read files, search code, check docs.
- Research in **normal mode** with all tools available.
- Submit your findings, THEN wait for partner.

### Discussion Is Required (Phase 2)
- After exchanging research, you MUST discuss architecture decisions.
- This is a **real back-and-forth** — not a single message. Expect 2-4 rounds minimum.
- Discuss: trade-offs, approach differences, open questions, risks, which solution direction to take.
- Do NOT skip to drafting a plan until you've actually discussed and aligned.

### User Approval Gate (Phase 5)
- The plan is for the **user**, not just the partner agent.
- Present the plan clearly with: problem statement, proposed solution, file changes, trade-offs considered, risks.
- **WAIT for the user to approve.** The user may ask questions, request changes, or redirect.
- Do NOT implement anything until the user explicitly says to proceed.

## Thread Convention

All spec messages use thread `spec/<topic>` (e.g., `spec/auth-redesign`). This groups the entire workflow into a single reviewable thread via `amq thread --id spec/<topic>`.

## Agent Protocol (step by step)

### Phase 1: Research (parallel)

**Initiating agent (you START the spec):**
```bash
# 1. Send research request to partner — just the problem, NOT your findings
amq send --to <partner> --kind spec_research --thread spec/<topic> \
  --subject "Spec: <topic>" --body "Problem: <describe what needs to be designed>..."

# 2. IMMEDIATELY start your OWN research — do NOT wait for partner
#    - Explore the codebase (read files, grep, glob)
#    - Check existing patterns, dependencies, constraints
#    - Research external docs if needed
#    - Stay in NORMAL MODE (not plan mode) — you need tool access

# 3. Submit YOUR independent findings
amq send --to <partner> --kind spec_research --thread spec/<topic> \
  --subject "Research: <topic>" --body "<your findings using template below>"

# 4. Wait for partner's research
amq watch --timeout 120s
```

**Receiving agent (you GOT a `spec_research` message):**
```bash
# 1. Do your OWN independent research FIRST
#    - Do NOT read the sender's body yet — it may bias you
#    - Explore the codebase yourself
#    - Form your own conclusions

# 2. Submit YOUR findings
amq send --to <partner> --kind spec_research --thread spec/<topic> \
  --subject "Research: <topic>" --body "<your findings>"

# 3. NOW read partner's research from the thread
amq thread --id spec/<topic> --include-body
```

### Phase 2: Exchange + Discuss (ping-pong)

```bash
# Read the full thread to see both research submissions
amq thread --id spec/<topic> --include-body

# Start the discussion — compare findings, ask questions, debate approach
amq send --to <partner> --thread spec/<topic> \
  --subject "Discussion: <topic>" --body "I noticed our approaches differ on X..."

# Continue back-and-forth until aligned on architecture
# Expect 2-4 rounds minimum. Topics to cover:
#   - Where do our findings agree/differ?
#   - Which approach should we take and why?
#   - What are the trade-offs?
#   - What are the open risks?
#   - What's the scope (what's in/out)?

# Wait for partner's response between rounds
amq watch --timeout 120s
amq drain --include-body
```

### Phase 3: Plan (main agent drafts)

```bash
# Main agent (usually Claude) drafts the plan based on discussion
# Use the Spec Draft Template below

amq send --to <partner> --kind spec_draft --thread spec/<topic> \
  --subject "Plan: <topic>" --body "<plan using template below>"
```

### Phase 4: Review (partner reviews)

```bash
# Partner reviews the plan, flags issues, suggests changes
amq send --to <partner> --kind spec_review --thread spec/<topic> \
  --subject "Review: <topic>" --body "<review feedback>"

# If changes needed: main agent revises and re-sends spec_draft
# If approved: proceed to present to user
```

### Phase 5: Present to User

**This is the critical gate.** The main agent:
1. Synthesizes the final plan from the discussion + review
2. Presents it to the user in the conversation (NOT via AMQ — directly in chat)
3. Waits for the user's questions, feedback, and explicit approval
4. Does NOT proceed until the user says go

The plan presented to the user should include:
- Problem statement
- Proposed solution (clear and concrete)
- File changes (what will be created/modified)
- Architecture decisions made (and why)
- Trade-offs considered
- Risks and mitigations

Optionally send the final agreed plan to partner:
```bash
amq send --to <partner> --kind spec_decision --thread spec/<topic> \
  --subject "Final: <topic>" --body "<final plan>"
```

### Phase 6: Execute

Only after user approval. Follow the user's instructions on how to proceed — they may want to implement incrementally, adjust scope, or assign different parts to different agents.

## Tracking Progress

Use the thread to see all messages and determine the current phase:
```bash
amq thread --id spec/<topic> --include-body
```

Phase indicators by kind:
- `spec_research` messages → research/exchange phase
- Discussion messages (no spec kind) → exchange phase
- `spec_draft` → planning phase
- `spec_review` → review phase
- `spec_decision` → converged, ready for user

## Research Summary Template

```markdown
## Problem Understanding
<your interpretation of the problem — in your own words>

## Codebase Findings
- <what you found by exploring the code — file paths, patterns, constraints>
- <existing implementations that are relevant>
- <dependencies and integration points>

## Proposed Approach
<high-level solution direction based on what you found>

## Open Questions
- <things you couldn't determine from research alone>
- <decisions that need discussion>

## Risks
- <potential issues with this approach>
```

## Spec Draft Template (Plan)

```markdown
## Problem Statement
<clear problem definition>

## Proposed Solution
<concrete solution — what will be built>

## Architecture
<system design / component interactions>

## File Changes
- `path/to/file.ext` — what changes and why

## Decisions Made
- <decision 1>: <chosen option> because <rationale>
- <decision 2>: <chosen option> because <rationale>

## Trade-offs Considered
- <option A vs option B — why we chose A>

## Edge Cases
- <case 1 — how handled>

## Testing Strategy
- <how to verify this works>

## Risks
- <risk 1 — mitigation>
```
