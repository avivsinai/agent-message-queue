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

## Security

- Directories use 0700 permissions (owner-only access)
- Files use 0600 permissions (owner read/write only)
- Unknown handles trigger a warning by default; use `--strict` to error instead
- With `--strict`, corrupt or unreadable `config.json` also causes an error

These defaults are suitable for single-user machines. For shared systems, ensure the AMQ root is in a user-owned directory.

## CLI

- `amq init --root <path> --agents a,b,c`
- `amq send --me codex --to cloudcode --subject "Review notes" --thread p2p/cloudcode__codex --body @notes.md`
- `amq list --me cloudcode --new`
- `amq list --me cloudcode --new --limit 50 --offset 50`
- `amq read --me cloudcode --id <msg_id>`
- `amq ack --me cloudcode --id <msg_id>`
- `amq thread --id p2p/cloudcode__codex --include-body`
- `amq thread --id p2p/cloudcode__codex --limit 50`
- `amq presence set --me codex --status busy`
- `amq cleanup --tmp-older-than 36h [--dry-run]`
- `amq watch --me cloudcode --timeout 60s`
- `amq --version`

Most commands accept `--root`, `--json`, and `--strict` (except `init` which has its own flags).

See `specs/001-local-maildir-queue/quickstart.md` for the full contract.

## Multi-Agent Workflows

AMQ supports two primary coordination patterns:

### Pattern 1: Active Agent Loop

When an agent is actively working, integrate quick inbox checks between steps.
Commands below assume `AM_ME` is set (e.g., `export AM_ME=claude`):

```bash
# Agent work loop (pseudocode):
# 1. Check inbox (non-blocking)
amq list --new --json
# 2. Process any messages
# 3. Do work
# 4. Send updates to other agents
amq send --to codex --subject "Progress" --body "Completed task X"
# 5. Repeat
```

### Pattern 2: Explicit Wait for Reply

When waiting for a response from another agent (assumes `AM_ME` is set):

```bash
# Send request
amq send --to codex --subject "Review request" --body @file.go

# Wait for reply (blocks until message arrives or timeout)
amq watch --timeout 120s

# Process the reply
amq read --id <msg_id>
```

### Watch Command Details

The `watch` command uses fsnotify for efficient OS-native file notifications:

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

Install git hooks to enforce checks before push:

```bash
./scripts/install-hooks.sh
```

## Co-op Mode

AMQ enables real-time collaboration between Claude Code and Codex CLI sessions. See [COOP.md](COOP.md) for the full protocol.

```bash
# Setup
export AM_ROOT=.agent-mail AM_ME=claude  # or codex
./amq init --root .agent-mail --agents claude,codex

# Send with priority/kind
./amq send --to codex --kind review_request --priority normal --body "Please review..."

# Reply (auto thread/refs)
./amq reply --id <msg_id> --body "LGTM!"

# Background watch
./amq monitor --timeout 0 --include-body --json
```

## Installing Skills

### Claude Code (via marketplace)

```bash
# Add marketplace (one-time)
/plugin marketplace add avivsinai/skills-marketplace

# Install plugin
/plugin install amq-cli@avivsinai-marketplace
```

### Codex CLI

**Via skill-installer** (recommended):
```
$skill-installer install https://github.com/avivsinai/agent-message-queue/tree/main/skills/amq-cli
```
Restart Codex after installing. To update, re-run the same command.

**Manual install**:
```bash
mkdir -p ~/.codex/skills/amq-cli
curl -o ~/.codex/skills/amq-cli/SKILL.md \
  https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/skills/amq-cli/SKILL.md
```

### Development

When working in this repo, project skills in `.claude/skills/` and `.codex/skills/` take precedence over installed skills.

Source of truth: `.claude/skills/amq-cli/SKILL.md`. After changes:

```bash
make sync-skills  # syncs to .codex/skills/ and skills/
```

## License

MIT (see `LICENSE`).
