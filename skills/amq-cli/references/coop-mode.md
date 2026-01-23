# Co-op Mode Protocol

## Roles

- **Claude Code** = Leader + Worker (coordinates phases, merges, prepares commits, gets user approval)
- **Codex** = Worker (executes phases, reports to leader, awaits next assignment)

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

1. **Never branch** — always work on same branch (joined work)
2. **Code phase = split** — divide files/modules to avoid conflicts
3. **File overlap** — if same file unavoidable, assign one owner; other reviews/proposes via message
4. **Coordinate between phases** — sync before moving to next phase
5. **Leader decides** — Claude Code makes final calls at merge points
6. **Ask user only for** — credentials, unclear requirements

## Stay in Sync

- After completing a phase, report to leader and await next assignment
- While waiting, safe to do: review partner's work, run tests, read docs
- If no assignment comes, ask leader (not user) for next task

## Communication

- Use AMQ messages to coordinate between phases
- Don't paste code blocks — reference file paths (shared workspace)

## Message Handling

- `amq drain --include-body` — process incoming messages
- `amq send --to <partner>` — send work/findings to partner
- `amq reply --id <msg_id>` — reply in thread
