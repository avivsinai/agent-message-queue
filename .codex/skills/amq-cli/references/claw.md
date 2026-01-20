# CLAW Loop (Co-op Loop and Wake)

Use this loop whenever you receive or expect messages.

1. Drain: `amq drain --include-body`.
2. Classify: identify the message kind and intent (review_request, question, decision, todo).
3. Act: do the work in the shared workspace (read files locally, run tests).
4. Write back: reply with results; send a `status` update if work is long-running.
5. Wait: if blocked, message the partner; if waiting, `amq watch --timeout 60s`.

Guidelines:
- Act without asking the user what to do with incoming AMQ messages.
- Prefer file paths and commands over code snippets in messages.
- Keep the partner updated when scope or ETA changes.
