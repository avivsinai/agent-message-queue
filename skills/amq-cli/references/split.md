# Work Split Protocol

Use this when a task can be parallelized or will take more than ~10-20 minutes.

1. Define scopes: break work into non-overlapping chunks.
2. Assign ownership: one agent per chunk; avoid touching the same files.
3. Send a `todo` or `review_request` message with:
   - Scope summary
   - Files or paths
   - Commands to run
   - Definition of done
   - ETA (if known)
4. Acknowledge with a `status` message when you start.
5. When done, message the partner with what changed and where (file paths only).

Template:
```
Subject: Split: <short title>
Body:
- Scope: <what you will do>
- Files: <paths>
- Commands: <tests/commands>
- Done when: <criteria>
- ETA: <estimate>
```
