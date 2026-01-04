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

**Claude Code:**
```
/plugin marketplace add avivsinai/skills-marketplace
/plugin install amq-cli@avivsinai-marketplace
```

**Codex CLI** (Codex chat command; not a shell command):
```
$skill-installer install https://github.com/avivsinai/agent-message-queue/tree/main/skills/amq-cli
```

Restart Codex after installing.

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

### Skill: Manual Install (Codex)

If skill-installer isn't available:

```bash
mkdir -p ~/.codex/skills/amq-cli
curl -o ~/.codex/skills/amq-cli/SKILL.md \
  https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/skills/amq-cli/SKILL.md
```

Restart Codex after installing.

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
