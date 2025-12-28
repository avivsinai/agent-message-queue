# Agent Message Queue (AMQ)

File-based message queue for AI agent coordination. Enables Claude Code and Codex CLI to communicate on the same machine using atomic Maildir-style delivery.

## Installation

### 1. Install Binary

```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
```

Verify: `amq --version`

### 2. Install Skill

**Claude Code** (via marketplace):
```
/plugin marketplace add avivsinai/skills-marketplace
/plugin install amq-cli@avivsinai-marketplace
```

**Codex CLI** (via skill-installer):
```
$skill-installer install https://github.com/avivsinai/agent-message-queue/tree/main/skills/amq-cli
```
Restart Codex after installing.

For manual download, build from source, or troubleshooting, see [INSTALL.md](INSTALL.md).

## Quick Start

```bash
# Initialize mailboxes
amq init --root .agent-mail --agents claude,codex

# Set environment
export AM_ROOT=.agent-mail AM_ME=claude

# Send a message
amq send --to codex --body "Hello from Claude!"

# Check inbox (as codex)
AM_ME=codex amq list --new
```

## Co-op Mode

For real-time Claude Code + Codex CLI collaboration:

```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/setup-coop.sh | bash
```

See [COOP.md](COOP.md) for the full protocol.

## Documentation

- [INSTALL.md](INSTALL.md) - Alternative installation methods
- [COOP.md](COOP.md) - Co-op mode protocol
- [CLAUDE.md](CLAUDE.md) - Agent instructions and CLI reference

## Development

```bash
git clone https://github.com/avivsinai/agent-message-queue.git
cd agent-message-queue
make build   # Build binary
make test    # Run tests
make ci      # Full CI: vet + lint + test + smoke
```

## License

MIT
