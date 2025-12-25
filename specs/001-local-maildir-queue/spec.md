# Feature Specification: Local Maildir Queue

Feature: 001-local-maildir-queue
Status: Draft
Date: 2025-12-24

## Context

We need a local, file-queue mailbox that lets multiple coding agents exchange messages on the same machine without a server, database, or daemon. The design borrows Maildir delivery semantics (tmp -> new -> cur) and uses per-message files with JSON frontmatter and Markdown bodies.

## Goals

- Safe, atomic delivery semantics using tmp/new/cur
- Human-readable message files with durable writes
- Zero background services, all actions via CLI commands
- Explicit, non-destructive cleanup only

## Non-goals

- Global search/indexing across repos
- Long-running daemons or background workers
- Complex auth, ACLs, or multi-tenant isolation

## User Stories and Acceptance

### Story 1: Initialize a shared mailbox root

As a user, I want to initialize a root directory and create mailboxes for a set of agent handles.

Acceptance:
- Given a root path, `amq init --root <path> --agents a,b` creates the full directory layout.
- A meta/config.json is created containing the agent handles and timestamp.

### Story 2: Send a message with atomic delivery

As an agent, I want to send a message to another agent and rely on atomic delivery semantics.

Acceptance:
- Given a message, CLI writes the message to receiver inbox/tmp, fsyncs it, then renames into inbox/new.
- Receiver never scans tmp; only new/cur are visible.
- Sender keeps a copy in outbox/sent.

### Story 3: List new messages

As an agent, I want to list new messages without marking them as read.

Acceptance:
- `amq list --me <handle> --new` lists message ids from inbox/new.
- No files are moved or modified.

### Story 4: Read a message and mark as seen

As an agent, I want to read a specific message by id and mark it as read.

Acceptance:
- `amq read --me <handle> --id <msg_id>` outputs the message body.
- The file is moved from inbox/new to inbox/cur atomically.

### Story 5: Acknowledge a message

As an agent, I want to acknowledge a message for coordination.

Acceptance:
- `amq ack --me <handle> --id <msg_id>` writes an ack file in receiver acks/sent and sender acks/received.
- No edits to the original message file.

### Story 6: View a thread

As an agent, I want to see a thread across both mailboxes.

Acceptance:
- `amq thread --me <handle> --id <thread_id>` scans both agents' inbox/outbox and merges messages by created timestamp.
- Output is stable and deterministic for equal timestamps.

### Story 7: Presence (optional)

As an agent, I want to set presence metadata in a simple file.

Acceptance:
- `amq presence set --me <handle> --status busy` writes presence.json with last_seen and status.
- `amq presence list` reads all presence files and returns their current values.

### Story 8: Cleanup stale tmp files

As an operator, I want to explicitly remove stale tmp files.

Acceptance:
- `amq cleanup --tmp-older-than 36h` deletes tmp files older than the duration.
- Command requires explicit confirmation unless `--yes` is provided.

## Edge Cases

- If tmp/new/cur are on different filesystems, rename must fail with an error.
- If ack is requested but sender mailbox is missing, record the ack locally and warn.
- If message id is not found, read/ack returns a clear error.
- If config.json exists and --force is not set, init fails safely.
- Handles must be normalized to lowercase and match `[a-z0-9_-]+`.

## Success Criteria

- Concurrency-safe delivery with atomic rename semantics
- No in-place edits of message files
- Clear, minimal CLI for codex/claude workflows
