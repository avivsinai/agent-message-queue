# Installation

## Quick Install

### 1. Binary

**macOS (Homebrew — recommended):**
```bash
brew install avivsinai/tap/amq
```

**macOS/Linux (script):**
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

Install the skill to enable co-op mode guidance in Claude Code or Codex.

#### Method 1: skills (Recommended)

Using [Vercel's skills CLI](https://github.com/vercel-labs/add-skill):

```bash
npx skills add avivsinai/agent-message-queue -g -y
```

#### Method 2: skild

Using [skild registry](https://skild.sh):

```bash
# For Claude Code
npx skild install @avivsinai/amq-cli -t claude -y

# For Codex CLI
npx skild install @avivsinai/amq-cli -t codex -y
```

Or directly from GitHub:

```bash
npx skild install avivsinai/agent-message-queue -t claude -y
npx skild install avivsinai/agent-message-queue -t codex -y
```

#### Method 3: Skills Marketplace

> **Known Issue**: Claude Code uses SSH to clone marketplace repos, which fails without SSH keys configured. See [issue #14485](https://github.com/anthropics/claude-code/issues/14485). Use Method 1 or 2 instead.

**Claude Code:**
```
/plugin marketplace add avivsinai/skills-marketplace
/plugin install amq-cli@avivsinai-marketplace
```

**Codex CLI** (Codex chat command; not a shell command):
```
$skill-installer install https://github.com/avivsinai/agent-message-queue/tree/main/skills/amq-cli
```

#### Method 4: Manual (Always Works)

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

**Homebrew:**
```bash
brew upgrade amq
```

**Other installs:**
```bash
amq upgrade
```

Or re-run the install script:

```bash
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
```

### Disabling Update Notifications

For CI or offline environments:
```bash
amq --no-update-check ...      # Per-command
export AMQ_NO_UPDATE_CHECK=1   # Global
```
