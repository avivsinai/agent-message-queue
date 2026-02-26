# Spec Workflow

Structured collaborative specification workflow. Both agents research **independently and in parallel**, exchange findings, draft specs, review, and converge — all via AMQ messaging.

## CRITICAL: Research Independence

The research phase requires **unbiased, independent work**. Follow these rules strictly:

1. **NEVER draft before both agents have submitted research.** The phase state machine enforces this — you cannot submit a draft during the research phase.
2. **After `start`, immediately do your OWN research and submit it.** Do NOT wait for your partner. Do NOT read your partner's research first. Work in parallel.
3. **After submitting research, WAIT for partner.** Check `amq coop spec status` — when phase advances to `exchange`, both agents have submitted. Only THEN read each other's research.

Later phases (draft, review) are also parallel — both agents must submit — but agents are informed by prior exchange discussion. Only research demands true independence.

## Agent Protocol (step by step)

**When you START a spec:**
```bash
amq coop spec start --topic <name> --partner <agent> --body "Problem description"
# Now immediately research the codebase yourself
# Submit YOUR findings — do NOT wait for partner
amq coop spec submit --topic <name> --phase research --body "Your independent findings"
# Wait for partner to also submit (check status periodically)
```

**When you RECEIVE a spec research request (via drain/watch):**
```bash
# Do your OWN independent research — do NOT read the sender's findings yet
# Submit YOUR findings
amq coop spec submit --topic <name> --phase research --body "Your independent findings"
# Phase auto-advances to exchange when both submit
```

**Exchange phase (both researches submitted):**
```bash
# NOW read your partner's research submission
# Discuss differences, open questions via amq send on the spec/<topic> thread
# When ready to propose a design, submit your draft
amq coop spec submit --topic <name> --phase draft --body @your-draft.md
```

**Review phase (both drafts submitted):**
```bash
# Read your partner's draft
# Submit review feedback
amq coop spec submit --topic <name> --phase review --body "Feedback on partner's draft"
```

**Converge phase (both reviews submitted):**
```bash
# Synthesize both drafts + reviews into a final spec
amq coop spec submit --topic <name> --phase final --body @final-spec.md
```

## Phases

```
research → exchange → draft → review → converge → done
```

| Phase | Advances when | Submit value | Rule |
|-------|--------------|-------------|------|
| `research` | All agents submitted | `research` | Work independently, no peeking |
| `exchange` | First draft submitted | `draft` | Read partner's research, discuss, then draft |
| `draft` | All agents submitted | `draft` | Draft independently |
| `review` | All agents submitted | `review` | Review partner's draft |
| `converge` | Any agent submits final | `final` | Synthesize everything |

## Commands

```bash
amq coop spec start --topic <name> --partner <agent> [--body <problem>]
amq coop spec status --topic <name> [--json]
amq coop spec submit --topic <name> --phase <research|draft|review|final> [--body <text|@file>]
amq coop spec present --topic <name> [--json]
```

Artifacts are stored in `<root>/specs/<topic>/` (e.g., `claude-research.md`, `codex-draft.md`, `final.md`).

## Research Summary Template

```markdown
## Problem Understanding
<your interpretation of the problem>

## Key Findings
- Finding 1
- Finding 2

## Proposed Approach
<high-level solution direction>

## Open Questions
- Question 1
- Question 2

## Risks
- Risk 1
```

## Spec Draft Template

```markdown
## Problem Statement
<clear problem definition>

## Solution
<proposed solution>

## Architecture
<system design / component interactions>

## File Changes
- `path/to/file.go` — description of change

## Edge Cases
- Case 1

## Testing
- Test strategy

## Risks
- Risk 1
```
