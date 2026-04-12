# Token-Efficient Review Loops

When a `review_request` may take multiple rounds, do not keep the whole loop in the main conversation. Use your host's background worker or subagent primitive so the AMQ exchange runs in isolated context and only the final verdict returns.

## Host Mapping

- Claude Code: use a subagent or background agent.
- Codex-based agents: use a spawned/background Codex worker or task.
- Tool names vary by host. The invariant is the same: intermediate AMQ rounds stay off the main thread.

## Pattern

- The background agent sends the initial `review_request` via `amq send`.
- It waits for replies with `amq drain --include-body`.
- If the reviewer finds issues, it applies fixes and re-sends for review.
- It stops when the reviewer says the change is green or a max round count is hit.
- It returns one line to the main context, for example: `codex signed off after 3 rounds, 5 findings fixed`.

## Why

- Intermediate review rounds stay out of the main context.
- Repeated diffs, logs, and review notes do not accumulate as stale history.
- The main conversation keeps only the durable outcome.

## Examples

```text
Claude Code:
Agent({
  run_in_background: true,
  task: `
    Send: amq send --to codex --kind review_request --body "Please review: src/foo.go"
    Loop up to 3 rounds:
    - amq drain --include-body
    - if codex is green, stop
    - apply the requested fixes
    - amq send --to codex --kind review_request --body "Updated: src/foo.go"
    Return one line only:
    "codex signed off after 3 rounds, 5 findings fixed"
  `
})
```

```text
Codex-style host:
Start a background worker/subagent for the AMQ review loop
- amq send --to codex --kind review_request --body "Please review: src/foo.go"
- amq drain --include-body
- apply fixes and re-send until green or max rounds
Return one line only:
"codex signed off after 3 rounds, 5 findings fixed"
```

This is behavioral guidance for agents using AMQ, not a CLI feature or protocol change.
