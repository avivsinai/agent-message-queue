# Installation

AMQ has two components:
1. **Binary** (`amq`) - the CLI tool (required)
2. **Skill** - teaches Claude Code / Codex how to use it (required for AI workflows)

## Step 1: Install the Binary

### One-liner (macOS/Linux)

```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
```

Options:
```bash
curl -fsSL .../install.sh | VERSION=v0.7.3 bash      # Specific version
curl -fsSL .../install.sh | INSTALL_DIR=~/bin bash   # Custom directory
```

### Manual Download

Download from [Releases](https://github.com/avivsinai/agent-message-queue/releases):

| Platform | Asset |
|----------|-------|
| macOS (Apple Silicon) | `amq_*_darwin_arm64.tar.gz` |
| macOS (Intel) | `amq_*_darwin_amd64.tar.gz` |
| Linux (x86_64) | `amq_*_linux_amd64.tar.gz` |
| Linux (ARM64) | `amq_*_linux_arm64.tar.gz` |
| Windows | `amq_*_windows_amd64.zip` (use in WSL) |

Extract and move to PATH:
```bash
tar xzf amq_*.tar.gz
sudo mv amq /usr/local/bin/
```

### Build from Source

Requires Go 1.25+:
```bash
git clone https://github.com/avivsinai/agent-message-queue.git
cd agent-message-queue
make build
sudo mv amq /usr/local/bin/
```

### Verify

```bash
amq --version
```

## Step 2: Install the Skill

The skill teaches Claude Code and Codex CLI how to use AMQ. Required for AI agent workflows.

### Claude Code

```
/plugin marketplace add avivsinai/skills-marketplace
/plugin install amq-cli@avivsinai-marketplace
```

### Codex CLI

```bash
mkdir -p ~/.codex/skills/amq-cli
curl -o ~/.codex/skills/amq-cli/SKILL.md \
  https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/skills/amq-cli/SKILL.md
```

Restart Codex after installing.

## Quick Test

```bash
# Initialize mailboxes
amq init --root .agent-mail --agents claude,codex

# Send a test message
export AM_ROOT=.agent-mail AM_ME=claude
amq send --to codex --body "Hello from Claude!"

# Check inbox as codex
AM_ME=codex amq list --new
```

## Upgrading

Re-run the install script to upgrade to the latest version:
```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
```

Or specify a version:
```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | VERSION=v0.8.0 bash
```
