# Agent Message Queue (AMQ)

File-based message queue for AI agent coordination. Enables Claude Code and Codex CLI to communicate on the same machine using atomic Maildir-style delivery.

## Installation

### 1. Install Binary (macOS/Linux)

```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
```

Installs to `~/.local/bin` or `~/go/bin` (no sudo required). Verify: `amq --version`

### 2. Install Skill

**Claude Code** (via marketplace):
```
/plugin marketplace add avivsinai/skills-marketplace
/plugin install amq-cli@avivsinai-marketplace
```

**Codex CLI** (Codex chat command; not a shell command):
```
$skill-installer install https://github.com/avivsinai/agent-message-queue/tree/main/skills/amq-cli
```
Restart Codex after installing.

For manual download, build from source, or troubleshooting, see [INSTALL.md](INSTALL.md).

## Quick Start

### 1. Initialize Project

```bash
amq init --root .agent-mail --agents claude,codex
```

### 2. Start Agent Session

**Claude Code:**
```bash
export AM_ME=claude AM_ROOT=.agent-mail
amq wake &
claude
```

**Codex CLI:**
```bash
export AM_ME=codex AM_ROOT=.agent-mail
amq wake &
codex
```

### 3. Send & Receive

```bash
# Send a message
amq send --to codex --body "Hello from Claude!"

# Check inbox
amq list --new

# Read all messages (one-shot, moves to cur, auto-acks)
amq drain --include-body
```

## Co-op Mode

For real-time Claude Code + Codex CLI collaboration, see [COOP.md](COOP.md).

**One-liner setup:**
```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/setup-coop.sh | bash
```

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
