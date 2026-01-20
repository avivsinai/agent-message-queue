# Edge Cases and Recovery

Common issues and what to do:

- `amq` not found: install and verify `amq --version`.
- Missing environment: run `amq env --me <handle> --wake` or export `AM_ROOT`/`AM_ME`.
- Unknown handles with `--strict`: re-run `amq init` or update handles in the root.
- No TTY for `amq wake`: run in a real terminal or use `amq watch --timeout 60s`.
- Duplicate or stale wake: re-run `amq wake`; each terminal gets its own per-TTY lock and stale locks are cleaned. Avoid manual file edits.
- Corrupt messages: check DLQ with `amq dlq list` and `amq dlq read`, then `amq dlq retry`.
- Permissions problems: ensure dirs are 0700 and files are 0600 (owned by current user).
- Waiting too long: use `amq watch --timeout 60s`, then `amq drain --include-body`.

Do not edit message files directly; always use the CLI.
