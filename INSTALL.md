# Installation

## Quick Install

### 1. Binary (macOS/Linux)

```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
```

Installs to user-local directory (no sudo required):
- `$GOBIN` if set
- `~/.local/bin` if exists
- `~/go/bin` if exists
- `~/.local/bin` (created if needed)
The installer verifies release checksums when possible.

### 2. Skill

Install the skill to enable co-op mode guidance in Claude Code or Codex. Try methods in order until one works.

#### Method 1: skild (Recommended)

```bash
npx skild install @avivsinai/amq-cli
```

#### Method 2: Skills Marketplace

**Claude Code:**
```
/plugin marketplace add avivsinai/skills-marketplace
/plugin install amq-cli@avivsinai-marketplace
```

> **Note**: If you get an SSH "Permission denied" error, this is a Claude Code bug when SSH keys aren't configured. Use Method 1 or Method 3 instead.

**Codex CLI** (Codex chat command; not a shell command):
```
$skill-installer install https://github.com/avivsinai/agent-message-queue/tree/main/skills/amq-cli
```

#### Method 3: Manual (Fallback)

If npm tools fail (network issues, corporate firewalls, etc.):

**Claude Code:**
```bash
git clone https://github.com/avivsinai/agent-message-queue.git /tmp/amq
mkdir -p ~/.claude/skills
cp -r /tmp/amq/.claude/skills/amq-cli ~/.claude/skills/
rm -rf /tmp/amq
```

**Codex CLI:**
```bash
git clone https://github.com/avivsinai/agent-message-queue.git /tmp/amq
mkdir -p ~/.codex/skills
cp -r /tmp/amq/.codex/skills/amq-cli ~/.codex/skills/
rm -rf /tmp/amq
```

Restart your agent after installing.

---

## Alternative Methods

### Binary: Manual Download

Download from [Releases](https://github.com/avivsinai/agent-message-queue/releases):

| Platform | Asset |
|----------|-------|
| macOS (Apple Silicon) | `amq_*_darwin_arm64.tar.gz` |
| macOS (Intel) | `amq_*_darwin_amd64.tar.gz` |
| Linux (x86_64) | `amq_*_linux_amd64.tar.gz` |
| Linux (ARM64) | `amq_*_linux_arm64.tar.gz` |
| Windows | `amq_*_windows_amd64.zip` (use in WSL) |

```bash
tar xzf amq_*.tar.gz
mkdir -p ~/.local/bin
mv amq ~/.local/bin/
```

Optionally verify checksums:

```bash
curl -fsSL https://github.com/avivsinai/agent-message-queue/releases/download/<TAG>/checksums.txt | grep amq_<VERSION>_<OS>_<ARCH>
```

### Binary: Build from Source

Requires Go 1.25+:

```bash
git clone https://github.com/avivsinai/agent-message-queue.git
cd agent-message-queue
make build
mkdir -p ~/.local/bin
mv amq ~/.local/bin/
```

### Binary: Install Script Options

```bash
# Specific version
curl -fsSL .../install.sh | VERSION=v0.8.0 bash

# Custom directory
curl -fsSL .../install.sh | INSTALL_DIR=~/bin bash
```

---

## Verify

```bash
amq --version
```

## Upgrading

Re-run the install script:

```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
```
