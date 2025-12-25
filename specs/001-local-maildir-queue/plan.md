# Implementation Plan

## Language choice

Recommendation: Go

Rationale:
- Single static binary for macOS/Linux without runtime installs.
- Fast startup and low overhead for CLI workflows.
- Strong standard library for file IO, path handling, and JSON.
- Simple concurrency primitives if we later add watches or polling.

Alternatives:
- Rust: excellent performance and safety, but higher implementation overhead for a small CLI.
- TypeScript/Node: fast to iterate but requires runtime and has heavier startup for short-lived CLI calls.

## Modules

- cmd/amq: CLI entry point
- internal/cli: argument parsing and command routing
- internal/fsq: filesystem layout and atomic delivery
- internal/format: message frontmatter parsing/serialization
- internal/ack: ack creation
- internal/thread: thread reconstruction
- internal/presence: presence read/write
- internal/config: config load/write

## Command design

Global flags:
- --root <path> or AM_ROOT
- --me <handle> or AM_ME
- --json for machine output

Commands:
- init
- send
- list
- read
- ack
- thread
- presence set|list
- cleanup

## Delivery algorithm (send)

1. Validate inputs (handles, thread id, body source).
2. Generate message id and filename.
3. Create tmp file in receiver inbox/tmp with O_EXCL.
4. Write frontmatter + body.
5. fsync file, close.
6. fsync tmp directory.
7. Rename tmp -> new (same filesystem).
8. fsync new directory.
9. Write copy to sender outbox/sent (same atomic flow).

Note: If rename fails with EXDEV, report error (tmp and new must be same filesystem).

## Read algorithm

1. Locate message by id in inbox/new or inbox/cur.
2. If found in new, rename to cur.
3. Return body (optionally header in --json).

## List algorithm

1. Enumerate inbox/new or inbox/cur.
2. Parse minimal metadata (id, created, subject, from).
3. Sort by created timestamp (fallback to filename).

## Ack algorithm

1. Parse message frontmatter to get from/to/thread.
2. Build ack JSON payload.
3. Write ack to receiver acks/sent and sender acks/received.

## Thread reconstruction

1. Enumerate both agents' message files (inbox new/cur + outbox sent).
2. Parse frontmatter for thread id and created timestamp.
3. Filter by thread id.
4. Sort by created, then id.
5. Output merged list.

## Presence

- `presence set` writes presence.json for handle.
- `presence list` reads all agents and returns their presence files if present.

## Cleanup

- Remove tmp files older than duration.
- Prompt for confirmation unless --yes.

## Testing

- Unit tests for id generation, frontmatter parse/format.
- Integration tests for tmp -> new -> cur delivery.
- Concurrency tests: multiple senders writing to same inbox.
