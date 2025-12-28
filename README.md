# Agent Message Queue (AMQ)

A local, file-queue mailbox for two or more coding agents running on the same machine. AMQ uses Maildir-style atomic delivery (tmp -> new -> cur) with a minimal CLI so agents can exchange messages without a server, database, or daemon.

Requires Go 1.25+ to build from source.

## Installation

### Quick Install (macOS/Linux)

```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
```

This downloads the latest binary and installs it to `/usr/local/bin`.

For manual download, building from source, or installing the Claude Code / Codex skill, see **[INSTALL.md](INSTALL.md)**.

### Verify

```bash
amq --version
```

## Quickstart

```bash
# Initialize mailboxes for two agents
amq init --root .agent-mail --agents claude,codex

# Set environment
export AM_ROOT=.agent-mail AM_ME=claude

# Send a message
amq send --to codex --body "Quick ping"

# Check inbox (as codex)
AM_ME=codex amq list --new
```

## Goals

- Minimal, robust file-based message exchange between agents on the same machine
- Atomic delivery semantics and durable writes
- No background services or network dependencies
- Human-auditable artifacts (plain text + JSON frontmatter)

## Non-goals

- Global search or indexing across repos
- Long-running daemons or background workers
- Complex auth, ACLs, or multi-tenant isolation

## Environment Variables

- `AM_ROOT`: default root directory for storage
- `AM_ME`: default agent handle

Handles must be lowercase and match `[a-z0-9_-]+`.

## CLI Reference

```bash
amq init --root <path> --agents a,b,c [--force]
amq send --to <agent> [--subject <str>] [--body <str|@file|stdin>] [--priority <p>] [--kind <k>]
amq list [--new | --cur] [--json]
amq read --id <msg_id> [--json]
amq ack --id <msg_id>
amq drain [--include-body] [--ack] [--json]
amq reply --id <msg_id> [--body <str>]
amq thread --id <thread_id> [--include-body]
amq watch --timeout <duration> [--poll] [--json]
amq monitor --timeout <duration> [--include-body] [--json]
amq presence set --status <busy|idle|...> [--note <str>]
amq presence list [--json]
amq cleanup --tmp-older-than <duration> [--dry-run]
amq --version
```

Most commands accept `--root`, `--me`, `--json`, and `--strict`.

## Multi-Agent Workflows

### Pattern 1: Active Agent Loop

When an agent is actively working, integrate quick inbox checks between steps:

```bash
export AM_ME=claude AM_ROOT=.agent-mail

# Drain inbox (moves to cur, acks, silent when empty)
amq drain --include-body

# Do work...

# Send updates
amq send --to codex --subject "Progress" --body "Completed task X"
```

### Pattern 2: Wait for Reply

When waiting for a response from another agent:

```bash
# Send request
amq send --to codex --subject "Review request" --body @file.go

# Block until message arrives (or timeout)
amq watch --timeout 120s

# Process
amq drain --include-body
```

### Watch Command

Uses fsnotify for efficient OS-native file notifications:

```bash
amq watch --timeout 60s              # Wait for messages
amq watch --timeout 60s --poll       # Polling fallback (NFS)
amq watch --timeout 60s --json       # JSON output for scripting
```

Returns: `existing`, `new_message`, or `timeout`.

## Co-op Mode

AMQ enables real-time collaboration between Claude Code and Codex CLI sessions. See [COOP.md](COOP.md) for the full protocol.

**Quick setup** (run in your project):
```bash
curl -sL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/setup-coop.sh | bash
```

This creates mailboxes, stop hook, and settings.

**Usage:**
```bash
export AM_ROOT=.agent-mail AM_ME=claude  # or codex

# Send with priority/kind
amq send --to codex --kind review_request --priority normal --body "Please review..."

# Reply (auto thread/refs)
amq reply --id <msg_id> --body "LGTM!"

# Background watch
amq monitor --timeout 0 --include-body --json
```

### References

- [Claude Code Best Practices](https://www.anthropic.com/engineering/claude-code-best-practices) - Headless mode, multi-agent workflows, `ultrathink`
- [Claude Code Hooks](https://code.claude.com/docs/en/hooks) - Stop hooks for autonomous operation
- [Codex CLI Features](https://developers.openai.com/codex/cli/features/) - Approval modes, `full-auto`, background terminals

## Security

- Directories use 0700 permissions (owner-only access)
- Files use 0600 permissions (owner read/write only)
- Unknown handles trigger a warning by default; use `--strict` to error instead
- With `--strict`, corrupt or unreadable `config.json` also causes an error

These defaults are suitable for single-user machines. For shared systems, ensure the AMQ root is in a user-owned directory.

## Build & Development

```bash
make build    # go build -o amq ./cmd/amq
make test     # go test ./...
make vet      # go vet
make lint     # golangci-lint run
make ci       # Full CI: vet + lint + test + smoke
```

`make lint` requires [golangci-lint](https://golangci-lint.run/usage/install/).

Install git hooks:
```bash
./scripts/install-hooks.sh
```

## License

MIT (see [LICENSE](LICENSE)).
