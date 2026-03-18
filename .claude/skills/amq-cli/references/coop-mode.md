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
| **Design** | Parallel -> Merge | Both propose approaches. Leader merges/decides. |
| **Code** | Split | Divide by file/module. Never edit same file. |
| **Review** | Parallel | Both review each other's code. Leader decides disputes. |
| **Test** | Parallel | Both run tests, report results to leader. |

```
Research (parallel) -> sync findings
    v
Design (parallel) -> leader merges approach
    v
Code (split: divide files/modules)
    v
Review (parallel: each reviews other's code)
    v
Test (parallel: both run tests)
    v
Leader prepares commit -> user approves -> push
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

## Cross-Session & Cross-Project Messaging

When running multiple sessions on the same project, agents can communicate across session boundaries:

```bash
# Send to an agent in another session
amq send --to codex@auth --body "Auth middleware ready"

# Broadcast to a channel (all subscribed agents across sessions)
amq announce --channel ci --kind status --body "CI green on main"

# See who is working where
amq who
```

Use `--topic` and `--claim` flags on `coop exec` to declare what each session is working on:
```bash
amq coop exec --session auth --topic "Auth rewrite" --claim "internal/auth/" claude
```

Channel membership is advisory metadata for fan-out discovery, not authorization. Join channels with:
```bash
amq channel join --name ci
```

For cross-project messaging (sibling projects on disk), use project-qualified addresses:
```bash
amq send --to claude@infra-lib:collab --body "Need updated types"
```

## Spec Workflow

The spec workflow is a skill-managed protocol that uses standard AMQ kinds plus labels (`workflow:spec`, `phase:*`) on thread `spec/<topic>`.

Canonical spec phases are:
`Research -> Discuss -> Draft -> Review -> Present -> Execute`

For the full spec protocol, see the amq-spec skill's `spec-workflow.md`.
