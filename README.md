# Agent Message Queue (AMQ)

A local, file-queue mailbox for two or more coding agents running on the same machine. AMQ uses Maildir-style atomic delivery (tmp -> new -> cur) with a minimal CLI so agents can exchange messages without a server, database, or daemon.

Status: implementation-ready. The detailed plan is in `specs/001-local-maildir-queue`.

Requires Go 1.25+.

## Goals

- Minimal, robust file-based message exchange between agents on the same machine
- Atomic delivery semantics and durable writes
- No background services or network dependencies
- Human-auditable artifacts (plain text + JSON frontmatter)

## Non-goals

- Global search or indexing across repos
- Long-running daemons or background workers
- Complex auth, ACLs, or multi-tenant isolation

## Quickstart

```bash
# Build the CLI

go build -o amq ./cmd/amq

# Initialize a root with two agent mailboxes
./amq init --root .agent-mail --agents codex,cloudcode

# Send a message
./amq send --me codex --to cloudcode --body "Quick ping"
```

## Environment variables

- `AM_ROOT`: default root directory for storage
- `AM_ME`: default agent handle

Handles must be lowercase and match `[a-z0-9_-]+`.

## CLI

- `amq init --root <path> --agents a,b,c`
- `amq send --me codex --to cloudcode --subject "Review notes" --thread p2p/codex__cloudcode --body @notes.md`
- `amq list --me cloudcode --new`
- `amq list --me cloudcode --new --limit 50 --offset 50`
- `amq read --me cloudcode --id <msg_id>`
- `amq ack --me cloudcode --id <msg_id>`
- `amq thread --id p2p/codex__cloudcode --include-body`
- `amq thread --id p2p/codex__cloudcode --limit 50`
- `amq presence set --me codex --status busy`
- `amq cleanup --tmp-older-than 36h`
- `amq watch --me cloudcode --timeout 60s`
- `amq --version`

All commands accept `--root` and `--json`.

See `specs/001-local-maildir-queue/quickstart.md` for the full contract.

## Multi-Agent Workflows

AMQ supports two primary coordination patterns:

### Pattern 1: Active Agent Loop

When an agent is actively working, integrate quick inbox checks between steps:

```bash
# Agent work loop (pseudocode):
# 1. Check inbox (non-blocking)
amq list --me claude --new --json
# 2. Process any messages
# 3. Do work
# 4. Send updates to other agents
amq send --to codex --subject "Progress" --body "Completed task X"
# 5. Repeat
```

### Pattern 2: Explicit Wait for Reply

When waiting for a response from another agent:

```bash
# Send request
amq send --me claude --to codex --subject "Review request" --body @file.go

# Wait for reply (blocks until message arrives or timeout)
amq watch --me claude --timeout 120s

# Returns immediately when message arrives (<1ms latency via fsnotify)
amq read --me claude --id <msg_id>
```

### Watch Command Details

The `watch` command uses OS-level file notifications (fsnotify/inotify) for instant response:

```bash
# Wait up to 60 seconds for new messages
amq watch --me claude --timeout 60s

# Use polling fallback for network filesystems (NFS, etc.)
amq watch --me claude --timeout 60s --poll

# JSON output for scripting
amq watch --me claude --timeout 60s --json
```

Returns:
- `existing` - Found messages already in inbox
- `new_message` - Message arrived during watch
- `timeout` - No messages within timeout period

## Build, lint, test

```bash
make build
make test
make vet
make lint
make ci
make smoke
```

`make lint` expects `golangci-lint` to be installed. See https://golangci-lint.run/usage/install/

## Skills

This repo includes ready-to-use skills for AI coding assistants.

### Claude Code

```bash
# Add the central marketplace (one-time)
/plugin marketplace add avivsinai/skills-marketplace

# Install this plugin
/plugin install amq-cli@avivsinai-marketplace
```

### Codex CLI

```bash
# Install from the skills marketplace
$skill-installer install https://github.com/avivsinai/skills-marketplace/tree/main/skills/amq-cli

# Or install directly from this repo
$skill-installer install https://github.com/avivsinai/agent-message-queue/tree/main/.codex/skills/amq-cli
```

### Manual Installation

```bash
git clone https://github.com/avivsinai/agent-message-queue
ln -s $(pwd)/agent-message-queue/.claude/skills/amq-cli ~/.claude/skills/amq-cli
ln -s $(pwd)/agent-message-queue/.codex/skills/amq-cli ~/.codex/skills/amq-cli
```

## License

MIT (see `LICENSE`).
