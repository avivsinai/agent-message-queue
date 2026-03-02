# Spec Workflow

Structured collaborative specification workflow. Both agents research **independently and in parallel**, exchange findings, draft specs, review, and converge — all via AMQ messaging primitives (`amq send`, `amq drain`, `amq thread`).

This is a **skill-managed protocol** — the workflow lives in these instructions, not in compiled commands. Agents follow the phases below using standard AMQ messaging.

## CRITICAL: Research Independence

The research phase requires **unbiased, independent work**. Follow these rules strictly:

1. **NEVER draft before both agents have submitted research.** Do your own research first, submit it, then wait.
2. **After starting, immediately do your OWN research and submit it.** Do NOT wait for your partner. Do NOT read your partner's research first. Work in parallel.
3. **After submitting research, WAIT for partner.** Use `amq watch` or check `amq list --new --kind spec_research`. Only after your partner also submits should you read each other's findings.

Later phases (draft, review) are also parallel — both agents must submit — but agents are informed by prior exchange. Only research demands true independence.

## Thread Convention

All spec messages use thread `spec/<topic>` (e.g., `spec/auth-redesign`). This groups the entire workflow into a single reviewable thread via `amq thread --id spec/<topic>`.

## Agent Protocol (step by step)

**When you START a spec:**
```bash
# 1. Send research request to partner
amq send --to <partner> --kind spec_research --thread spec/<topic> \
  --subject "Spec: <topic>" --body "Problem description..."

# 2. Immediately research the codebase yourself — do NOT wait for partner
# 3. Submit YOUR findings
amq send --to <partner> --kind spec_research --thread spec/<topic> \
  --subject "Research: <topic>" --body "Your independent findings..."
# 4. Wait for partner's research
amq watch --timeout 120s
```

**When you RECEIVE a `spec_research` message (via drain/watch):**
```bash
# 1. Do your OWN independent research — do NOT read the sender's findings yet
# 2. Submit YOUR findings
amq send --to <partner> --kind spec_research --thread spec/<topic> \
  --subject "Research: <topic>" --body "Your independent findings..."
# 3. NOW read partner's research from the thread
amq thread --id spec/<topic> --include-body
```

**Exchange phase (both researches submitted):**
```bash
# Read your partner's research from the thread
amq thread --id spec/<topic> --include-body
# Discuss differences, open questions via messages on the spec/<topic> thread
amq send --to <partner> --thread spec/<topic> --body "Questions about..."
# When ready, submit your draft
amq send --to <partner> --kind spec_draft --thread spec/<topic> \
  --subject "Draft: <topic>" --body @your-draft.md
```

**Review phase (both drafts submitted):**
```bash
# Read your partner's draft from the thread
amq thread --id spec/<topic> --include-body
# Submit review feedback
amq send --to <partner> --kind spec_review --thread spec/<topic> \
  --subject "Review: <topic>" --body "Feedback on partner's draft..."
```

**Converge phase (both reviews submitted):**
```bash
# Synthesize both drafts + reviews into a final spec
amq send --to <partner> --kind spec_decision --thread spec/<topic> \
  --subject "Final: <topic>" --body @final-spec.md
```

## Phases

```
research → exchange → draft → review → converge → done
```

| Phase | You can proceed when | What to send | Rule |
|-------|---------------------|-------------|------|
| `research` | Partner also sent `spec_research` | `--kind spec_research` | Work independently, no peeking |
| `exchange` | You've read partner's research | `--kind spec_draft` | Discuss, then draft |
| `draft` | Partner also sent `spec_draft` | (wait for reviews) | Draft independently |
| `review` | Both drafts visible in thread | `--kind spec_review` | Review partner's draft |
| `converge` | Both reviews visible in thread | `--kind spec_decision` | Synthesize everything |

## Tracking Progress

Use the thread to see all messages and determine the current phase:
```bash
amq thread --id spec/<topic> --include-body
```

Count messages by kind to determine phase:
- 0 `spec_research` → still in research
- 1 `spec_research` → partner waiting for your research
- 2 `spec_research` → exchange phase (read each other's)
- `spec_draft` messages → drafting/review phase
- `spec_decision` message → done

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
