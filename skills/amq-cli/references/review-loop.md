# Token-Efficient Review Loops

When a `review_request` may take multiple rounds, do not keep the whole loop in the main conversation. Spawn a background agent to own the AMQ exchange and return only the final verdict.

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

## Example

```text
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

This is behavioral guidance for agents using AMQ, not a CLI feature or protocol change.
