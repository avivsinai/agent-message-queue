# Co-op Mode Protocol

## Roles

- **Initiator** = whoever starts the task (agent or human). Owns decisions and receives updates.
- **Leader/Coordinator** = coordinates phases, merges, and final decisions (often the initiator).
- **Worker** = executes assigned phases and reports back to the initiator.

**Default pairing note**: Claude is often faster and more decisive, while Codex tends to be deeper but slower. That commonly makes Claude a natural coordinator and Codex a strong worker. This is a default, not a rule — roles are set per task by the initiator.

## Phased Flow

| Phase | Mode | Description |
|-------|------|-------------|
| **Research** | Parallel | Both explore codebase, read docs, search. No conflicts. |
| **Design** | Parallel → Merge | Both propose approaches. Leader merges/decides. |
| **Code** | Split | Divide by file/module. Never edit same file. |
| **Review** | Parallel | Both review each other's code. Leader decides disputes. |
| **Test** | Parallel | Both run tests, report results to leader. |

```
Research (parallel) → sync findings
    ↓
Design (parallel) → leader merges approach
    ↓
Code (split: divide files/modules)
    ↓
Review (parallel: each reviews other's code)
    ↓
Test (parallel: both run tests)
    ↓
Leader prepares commit → user approves → push
```

## Key Rules

1. **Initiator rule** — reply to the initiator and ask the initiator for clarifications
2. **Never branch** — always work on same branch (joined work)
3. **Code phase = split** — divide files/modules to avoid conflicts
4. **File overlap** — if same file unavoidable, assign one owner; other reviews/proposes via message
5. **Coordinate between phases** — sync before moving to next phase
6. **Leader decides** — initiator or designated leader makes final calls

## Stay in Sync

- After completing a phase, report to the initiator and await next assignment
- While waiting, safe to do: review partner's work, run tests, read docs
- If no assignment comes, ask the initiator (not a third party) for next task

## Progress Protocol (Start / Heartbeat / Done)

- **Start**: send `kind=status` with an ETA to the initiator as soon as you begin.
- **Heartbeat**: update on phase boundaries or every 10-15 minutes.
- **Done**: send Summary / Changes / Tests / Notes to the initiator.
- **Blocked**: send `kind=question` to the initiator with options and a recommendation.

## Modes of Collaboration (Modus Operandi)

- **Leader + Worker**: leader decides, worker executes; best default.
- **Co-workers**: peers decide together; if no consensus, ask the initiator.
- **Duplicate**: independent solutions or reviews; initiator merges results.
- **Driver + Navigator**: driver codes, navigator reviews/tests and can interrupt.
- **Spec + Implementer**: one writes spec/tests, the other implements.
- **Reviewer + Implementer**: one codes, the other focuses on review and risk detection.

## Communication

- Use AMQ messages to coordinate between phases and report to the initiator
- Don't paste code blocks — reference file paths (shared workspace)

## Interrupts

- Urgent messages labeled `interrupt` trigger wake Ctrl+C injection + an interrupt notice (when wake is running).

## Message Handling

- `amq drain --include-body` — process incoming messages
- `amq send --to <partner>` — send work/findings to partner
- `amq reply --id <msg_id>` — reply in thread


## Spec Workflow

Structured pre-implementation specification workflow via `amq coop spec`.

Phases: `research` → `exchange` → `draft` → `review` → `converge` → `done`

```bash
amq coop spec start --topic <name> --partner <agent> --body "Problem"
amq coop spec submit --topic <name> --phase research --body "Findings"
amq coop spec status --topic <name>
amq coop spec submit --topic <name> --phase draft --body @draft.md
amq coop spec submit --topic <name> --phase review --body "Feedback"
amq coop spec submit --topic <name> --phase final --body @final.md
amq coop spec present --topic <name>
```

See SKILL.md for templates and detailed phase transition rules.
