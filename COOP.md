# Co-op Mode: Phased Parallel Work

Co-op mode enables multiple agents (e.g., Claude Code and Codex CLI) to work **in parallel where safe, coordinate where risky**, leveraging cognitive diversity (different models = different training = different blind spots) to catch errors that same-model review would miss.

## Swarm vs Co-op

- **Co-op**: lightweight, peer-to-peer messaging between agents via AMQ threads (`p2p/...`).
- **Swarm**: join Claude Code Agent Teams and coordinate via the shared task list (`amq swarm ...`).
- **Messaging**: swarm bridge delivers task notifications only. Claude Code teammates can `amq send` to external agents, but external agents cannot DM a specific Claude Code teammate directly. External→team messages must go to the leader's AMQ inbox, then the leader drains and forwards via Claude Code internal messaging.

For swarm command reference, see [CLAUDE.md](CLAUDE.md).

## Quick Start

### Prerequisites (One-Time)

1. **Install amq CLI** ([releases](https://github.com/avivsinai/agent-message-queue/releases)):
   ```bash
   curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
   ```

2. **Install amq-cli skill** for your agents:

   **Via skills** (recommended):
   ```bash
   npx skills add avivsinai/agent-message-queue -g -y
   ```

   **Or via skild:**
   ```bash
   npx skild install @avivsinai/amq-cli -t claude -y
   ```

   See [INSTALL.md](INSTALL.md) for manual installation or troubleshooting.

### Running Co-op Mode

**Terminal 1 - Claude Code:**
```bash
amq coop exec claude -- --dangerously-skip-permissions
```

**Terminal 2 - Codex CLI:**
```bash
amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox
```

That's it. `coop exec` auto-initializes the project if needed, sets `AM_ROOT`/`AM_ME`, starts wake notifications, and execs into the agent.

To disable auto-wake (e.g., in CI or non-TTY environments):
```bash
amq coop exec --no-wake claude
```

### Multiple Pairs (Isolated Sessions)

Run multiple agent pairs on different features using `--session`:

```bash
# Pair A: auth feature
amq coop exec --session auth claude               # Terminal 1
amq coop exec --session auth codex                # Terminal 2

# Pair B: api refactor
amq coop exec --session api claude                # Terminal 3
amq coop exec --session api codex                 # Terminal 4
```

Each pair has isolated inboxes and threads. Messages stay within their root.
Equivalent explicit root form: `--root .agent-mail/<session>`.

### For Scripts/CI

When you can't use `exec` (non-interactive environments):
```bash
amq coop init
eval "$(amq env --me claude)"
```

### Fallback: Notify Hook (if wake unavailable)

`amq wake` uses TIOCSTI which may be unavailable on:
- Hardened Linux (CONFIG_LEGACY_TIOCSTI=n)
- Windows (use WSL)

If wake fails, configure the notify hook for desktop notifications:

```toml
# ~/.codex/config.toml
notify = ["python3", "/path/to/repo/scripts/codex-amq-notify.py"]
```

### Plan Mode Prompt Hook (Claude)

When Claude is in plan mode, it cannot run shell tools directly. Use the
`UserPromptSubmit` hook to inject AMQ context before prompt processing:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "AMQ_PROMPT_HOOK_MODE=plan AMQ_PROMPT_HOOK_ACTION=list python3 $CLAUDE_PROJECT_DIR/scripts/claude-amq-user-prompt-submit.py"
          }
        ]
      }
    ]
  }
}
```

Set `AMQ_PROMPT_HOOK_ACTION=drain` to auto-drain on submit (instead of list/peek).

## Roles

- **Initiator** = whoever starts the task (agent or human). Owns decisions and receives all updates.
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

## Core Principles

1. **Parallel where safe** - Research, design, review, and test phases run in parallel.
2. **Split where risky** - Code phase divides files/modules to avoid conflicts.
3. **Never branch** - Always work on same branch (joined work).
4. **Leader coordinates** - The initiator or designated leader handles phase transitions and final decisions.

## Initiator Rule

- The **initiator** is whoever started the task (agent or human).
- Always report progress and completion to the initiator.
- Ask questions only to the initiator. Do not ask a third party.

## Progress Protocol (Start / Heartbeat / Done)

- **Start**: send `kind=status` with an ETA to the initiator as soon as you begin.
- **Heartbeat**: update on phase boundaries or every 10-15 minutes.
- **Done**: send Summary / Changes / Tests / Notes to the initiator.
- **Blocked**: send `kind=question` to the initiator with options and a recommendation.

## Modes of Collaboration

Pick one mode per task; the initiator decides.

- **Leader + Worker**: leader decides, worker executes; best default.
- **Co-workers**: peers decide together; if no consensus, ask the initiator.
- **Duplicate**: independent solutions or reviews; initiator merges results.
- **Driver + Navigator**: driver codes, navigator reviews/tests and can interrupt.
- **Spec + Implementer**: one writes spec/tests, the other implements.
- **Reviewer + Implementer**: one codes, the other focuses on review and risk detection.

## CLI Commands

### Send

```bash
amq send --to codex --subject "Review: New parser" --kind review_request --body "..."
amq send --to codex --priority urgent --kind question --body "Blocked on API"
```

### Receive

```bash
amq drain --include-body                          # One-shot, silent when empty
amq watch --timeout 60s                           # Block until message arrives
amq list --new                                    # Peek without side effects
```

### Reply (Auto Thread/Refs)

```bash
amq reply --id "msg_123" --kind review_response --body "LGTM with minor suggestions..."
```

## Wake Command (Optional)

> Co-op works without wake. `coop exec` starts it automatically.

`amq wake` uses TIOCSTI to inject notifications into your terminal.

**Options:**
- `--inject-mode auto|raw|paste` - Injection strategy
- `--bell` - Ring terminal bell on new messages
- `--debounce 250ms` - Batch rapid messages
- `--interrupt` / `--interrupt=false` - Enable/disable Ctrl+C for urgent messages

**Platform support:**
- macOS: Works
- Linux: May be disabled by kernel hardening (CONFIG_LEGACY_TIOCSTI)
- Windows: Not supported (use WSL)

## Message Format

```json
{
  "schema": 1,
  "from": "codex",
  "to": ["claude"],
  "thread": "p2p/claude__codex",
  "subject": "Code review needed",
  "priority": "urgent",
  "kind": "review_request",
  "labels": ["parser"],
  "context": {"paths": ["internal/cli/drain.go"], "focus": "sorting"}
}
```

### Priority Levels

| Priority | Behavior |
|----------|----------|
| `urgent` | Interrupt current work |
| `normal` | Add to TODO list |
| `low` | Batch for later |

### Message Kinds

| Kind | Reply Kind | Default Priority |
|------|------------|------------------|
| `review_request` | `review_response` | normal |
| `question` | `answer` | normal |
| `decision` | — | normal |
| `todo` | — | normal |
| `status` | — | low |
| `brainstorm` | — | low |
| `spec_research` | `spec_research` | normal |
| `spec_draft` | `spec_review` | normal |
| `spec_review` | — | normal |
| `spec_decision` | — | normal |

## Spec Workflow

Structured collaborative specification workflow where both agents research, exchange findings, draft specs, review, and converge on a final design — all via AMQ messaging.

### Phases

```
research ──(all submit)──→ exchange ──(first draft)──→ draft ──(all submit)──→ review ──(all submit)──→ converge ──(any submit final)──→ done
```

| Phase | Advances when | Submit value |
|-------|--------------|-------------|
| `research` | All agents submitted research | `research` |
| `exchange` | First draft submitted | `draft` |
| `draft` | All agents submitted drafts | `draft` |
| `review` | All agents submitted reviews | `review` |
| `converge` | Any agent submits final | `final` |

### Commands

```bash
amq coop spec start --topic <name> --partner <agent> [--body <problem>]
amq coop spec status --topic <name>
amq coop spec submit --topic <name> --phase <research|draft|review|final> [--body <text|@file>]
amq coop spec present --topic <name>
```

### Example Workflow

```bash
# Claude starts a spec topic
amq coop spec start --topic auth-redesign --partner codex --body "We need to redesign auth"

# Both agents submit research
amq coop spec submit --topic auth-redesign --phase research --body "My findings..."
# (partner does the same → auto-advances to exchange)

# Agents discuss via regular messages on spec/auth-redesign thread
# When ready, submit drafts
amq coop spec submit --topic auth-redesign --phase draft --body @draft.md

# Both submit reviews
amq coop spec submit --topic auth-redesign --phase review --body "Feedback..."

# Any agent submits the converged final spec
amq coop spec submit --topic auth-redesign --phase final --body @final-spec.md

# View the final spec
amq coop spec present --topic auth-redesign
```

Artifacts are stored in `<root>/specs/<topic>/` (e.g., `claude-research.md`, `codex-draft.md`, `final.md`).

## Context Object Schema

```json
{
  "paths": ["internal/cli/send.go"],
  "symbols": ["Header", "runSend"],
  "focus": "error handling in validation",
  "commands": ["go test ./internal/cli/..."]
}
```

## Troubleshooting

### Wake not working
```bash
amq wake --me claude                              # Watch for warnings
amq drain --include-body                          # Manual fallback
```

### Messages not appearing
```bash
amq list --me claude --new --json                 # Check inbox directly
amq watch --me claude --poll                      # Force poll mode
```
