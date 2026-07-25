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
The installer requires a readable `checksums.txt` with exactly one valid entry for the selected asset. It uses `sha256sum` or `shasum` and stops before extraction if verification cannot be completed.

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
cp -r /tmp/amq/.agents/skills/amq-cli ~/.codex/skills/
rm -rf /tmp/amq
```

**Grok CLI** (optional peer; same skill contents, Grok's own discovery path):
```bash
git clone https://github.com/avivsinai/agent-message-queue.git /tmp/amq
mkdir -p ~/.grok/skills
cp -r /tmp/amq/.agents/skills/amq-cli ~/.grok/skills/
rm -rf /tmp/amq
```
A project-local copy under `.grok/skills/amq-cli` in the repo works the same way if you prefer per-project installs. Inside a checkout of this repository, Grok discovers the bundled `.claude/skills/amq-cli` through its Claude Code compatibility with no copying at all; it also supports user-level `~/.agents/skills`. See the [xAI skill discovery docs](https://docs.x.ai/build/features/skills-plugins-marketplaces) for the authoritative list of paths Grok CLI checks.

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

For manual installs, verify the selected asset against `checksums.txt` before extracting it:

```bash
# Replace X.Y.Z and darwin_arm64 with the release and platform you downloaded.
TAG=vX.Y.Z
ASSET=amq_X.Y.Z_darwin_arm64.tar.gz
curl -fsSL "https://github.com/avivsinai/agent-message-queue/releases/download/$TAG/checksums.txt" -o checksums.txt
CHECKSUM_LINE=$(
  awk -v asset="$ASSET" '
    {
      field = $2
      candidate = field
      sub(/^\*/, "", candidate)
      last = $NF
      sub(/^\*/, "", last)
      if (candidate == asset) {
        count++
        if (NF != 2 ||
            length($1) != 64 ||
            $1 !~ /^[0-9A-Fa-f]+$/ ||
            (field != asset && field != "*" asset)) {
          malformed = 1
        }
        line = $0
      } else if (last == asset) {
        count++
        malformed = 1
      }
    }
    END {
      if (count != 1 || malformed) exit 1
      print line
    }
  ' checksums.txt
) || { echo "Expected exactly one well-formed checksum entry for $ASSET" >&2; exit 1; }
printf '%s\n' "$CHECKSUM_LINE" > "$ASSET.sha256"
verify_amq_checksum() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c "$ASSET.sha256" || return 1
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 -c "$ASSET.sha256" || return 1
  else
    echo "sha256sum or shasum is required" >&2
    return 1
  fi
}
if verify_amq_checksum; then
  tar xzf "$ASSET"
  mkdir -p ~/.local/bin
  mv amq ~/.local/bin/
else
  echo "Checksum verification failed; archive was not extracted." >&2
  false
fi
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
