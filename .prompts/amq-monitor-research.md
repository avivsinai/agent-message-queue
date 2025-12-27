# AMQ Background Monitor: Research & Implementation Prompt

## Context

We're building a workflow where Claude Code (and Codex CLI) sessions automatically monitor an Agent Message Queue (AMQ) for incoming messages from other agents. The goal is:

1. **Non-blocking monitoring**: Main agent works normally while a background watcher monitors the queue
2. **Automatic surfacing**: When messages arrive, they're surfaced to the main agent
3. **Priority-based handling**: Urgent messages interrupt; normal messages queue as TODOs

## What We Know Works (Claude Code Dec 2025)

### Async Subagents
- Subagents can run in background via `Ctrl+B` or `run_in_background: true`
- `/tasks` command shows all background agents with status, progress, token usage
- `AgentOutputTool` automatically surfaces results when agents complete
- Background agents can **wake the main agent** when they finish

### Natural Language Spawning
```
"Run the queue-watcher in the background while I work on this feature"
"Have the amq-monitor check for messages every 30 seconds"
```

### The AMQ CLI
```bash
# Block until message arrives (or timeout)
amq watch --me claude --timeout 60s --json
# Output: {"event":"new_message","messages":[{"id":"...","from":"codex","subject":"..."}]}

# One-shot drain (read + move to cur + ack)
amq drain --me claude --include-body --json
```

## Research Questions

1. **Wake mechanism**: How exactly does a background subagent "wake" the main agent? Is it automatic when the agent completes, or does it require specific output/signaling?

2. **Continuous monitoring**: Can a background agent loop indefinitely (watch → process → watch again), or does it need to complete and be re-spawned?

3. **Interruption**: Can a background agent interrupt the main agent mid-task for urgent messages, or only when the main agent is idle/between turns?

4. **Codex CLI parity**: Does Codex have equivalent async agent capabilities? The experimental background terminals feature suggests partial support.

5. **Context injection**: When a background agent surfaces results, how much context can it inject? Can it directly modify the main agent's TodoWrite list?

## Proposed Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Claude Code / Codex Session                                │
│                                                             │
│  Main Agent (foreground)     AMQ Watcher (background)       │
│  ┌─────────────────────┐     ┌─────────────────────────┐   │
│  │ Working on user     │     │ amq watch --timeout 60s │   │
│  │ requested task...   │     │         ↓               │   │
│  │                     │     │ Message arrives         │   │
│  │                     │  ←──│ AgentOutputTool wakes   │   │
│  │                     │     │ main with result        │   │
│  │ Receives message:   │     └─────────────────────────┘   │
│  │ {priority: urgent}  │                                    │
│  │         ↓           │                                    │
│  │ Decides: interrupt  │                                    │
│  │ or queue to TODOs   │                                    │
│  └─────────────────────┘                                    │
└─────────────────────────────────────────────────────────────┘
```

## Message Format Extension

```yaml
---
schema: amq/v1
id: msg_abc123
from: codex
to: claude
subject: "Code review needed"
priority: urgent    # NEW: urgent | normal | low
thread: p2p/claude__codex
created: 2025-12-26T10:00:00Z
---

Body of the message...
```

## Implementation Options

### Option A: Session-Start Watcher
Add to CLAUDE.md:
```markdown
At session start, spawn a background AMQ watcher:
- Run `amq watch --me $AM_ME --json` in background
- When messages arrive, surface them
- For urgent: interrupt current work
- For normal: add to TodoWrite
- Re-spawn watcher after processing
```

### Option B: Custom Subagent Definition
Create `.claude/agents/amq-watcher.md`:
```markdown
---
name: amq-watcher
description: Monitors AMQ for incoming messages
tools: Bash, TodoWrite
model: haiku
---

Monitor the AMQ inbox using `amq watch`. When messages arrive:
1. Parse priority from message metadata
2. If urgent: return immediately to wake main agent
3. If normal: add to todo list, continue watching
```

### Option C: Hook-Based (Notification)
```json
{
  "hooks": {
    "Notification": [{
      "hooks": [{
        "type": "command",
        "command": "amq list --me $AM_ME --new --json | jq -e 'length > 0'"
      }]
    }]
  }
}
```

## What We Need Clarified

1. Is `run_in_background: true` on the Task tool actually shipped, or still a feature request?
2. Can background agents use TodoWrite to modify the main agent's todo list?
3. What's the actual mechanism for "waking" - is it polling, event-driven, or completion-triggered?
4. For Codex: what's the equivalent pattern?

## Test Plan

1. Spawn background watcher manually with Ctrl+B
2. Send test message via `amq send --to claude --subject "test" --body "hello"`
3. Verify main agent receives notification
4. Test priority handling (urgent vs normal)
5. Verify continuous monitoring (respawn after message processed)

## References

- [Claude Code Async Subagents (Dec 2025)](https://www.zerotopete.com/p/christmas-came-early-async-subagents)
- [Async Workflows Guide](https://claudefa.st/blog/guide/agents/async-workflows)
- [Background Commands Wiki](https://github.com/ruvnet/claude-flow/wiki/background-commands)
- [Feature Request #9905](https://github.com/anthropics/claude-code/issues/9905) - Task tool async support
- [AMQ CLI Documentation](./CLAUDE.md)
