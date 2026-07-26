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
# AMQ_MANUAL_INSTALL_BEGIN
(
# Replace X.Y.Z and darwin_arm64 with the release and platform you downloaded.
TAG=vX.Y.Z
ASSET=amq_X.Y.Z_darwin_arm64.tar.gz
TARGET_DIR="$HOME/.local/bin"
ARCHIVE_SOURCE="$PWD/$ASSET"
DOWNLOAD_DIR=""
EXTRACT_DIR=""
STAGE_DIR=""

cleanup_manual_install() {
  if [ -n "$STAGE_DIR" ]; then
    rm -rf "$STAGE_DIR" || true
  fi
  if [ -n "$EXTRACT_DIR" ]; then
    rm -rf "$EXTRACT_DIR" || true
  fi
  if [ -n "$DOWNLOAD_DIR" ]; then
    rm -rf "$DOWNLOAD_DIR" || true
  fi
}
trap cleanup_manual_install EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

[ -f "$ARCHIVE_SOURCE" ] && [ -r "$ARCHIVE_SOURCE" ] || {
  echo "Archive is missing, unreadable, or not a regular file: $ARCHIVE_SOURCE" >&2
  exit 1
}
DOWNLOAD_DIR=$(mktemp -d "${TMPDIR:-/tmp}/.amq.download.XXXXXX") || {
  echo "Could not create a private download directory" >&2
  exit 1
}
EXTRACT_DIR=$(mktemp -d "${TMPDIR:-/tmp}/.amq.extract.XXXXXX") || {
  echo "Could not create a private extraction directory" >&2
  exit 1
}
cp "$ARCHIVE_SOURCE" "$DOWNLOAD_DIR/$ASSET" || {
  echo "Could not snapshot $ASSET" >&2
  exit 1
}
curl -fsSL \
  "https://github.com/avivsinai/agent-message-queue/releases/download/$TAG/checksums.txt" \
  -o "$DOWNLOAD_DIR/checksums.txt" || {
  echo "Could not download checksums.txt" >&2
  exit 1
}
[ -r "$DOWNLOAD_DIR/checksums.txt" ] || {
  echo "Downloaded checksums.txt is unreadable" >&2
  exit 1
}

CHECKSUM_RESULT=$(
  awk -v asset="$ASSET" '
    {
      sub(/\r$/, "")
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
        hash = $1
      } else if (last == asset) {
        count++
        malformed = 1
      }
    }
    END {
      if (count == 0) {
        print "missing"
      } else if (count > 1) {
        print "duplicate"
      } else if (malformed) {
        print "malformed"
      } else {
        print "ok:" hash
      }
    }
  ' "$DOWNLOAD_DIR/checksums.txt"
) || {
  echo "Could not read checksums.txt" >&2
  exit 1
}
case "$CHECKSUM_RESULT" in
  ok:*) EXPECTED="${CHECKSUM_RESULT#ok:}" ;;
  *)
    echo "Expected exactly one well-formed checksum entry for $ASSET" >&2
    exit 1
    ;;
esac

RECORD_FILE="$DOWNLOAD_DIR/selected.sha256"
printf '%s  %s\n' "$EXPECTED" "$ASSET" >"$RECORD_FILE" || {
  echo "Could not create the private verifier record" >&2
  exit 1
}
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$DOWNLOAD_DIR" && sha256sum -c "$RECORD_FILE") || {
    echo "Checksum verification failed" >&2
    exit 1
  }
elif command -v shasum >/dev/null 2>&1; then
  (cd "$DOWNLOAD_DIR" && shasum -a 256 -c "$RECORD_FILE") || {
    echo "Checksum verification failed" >&2
    exit 1
  }
else
  echo "sha256sum or shasum is required" >&2
  exit 1
fi

tar xzf "$DOWNLOAD_DIR/$ASSET" -C "$EXTRACT_DIR" || {
  echo "Archive extraction failed" >&2
  exit 1
}
[ ! -L "$EXTRACT_DIR/amq" ] && [ -f "$EXTRACT_DIR/amq" ] &&
  [ -s "$EXTRACT_DIR/amq" ] && [ -x "$EXTRACT_DIR/amq" ] || {
  echo "Archive did not contain a regular executable amq binary" >&2
  exit 1
}
mkdir -p "$TARGET_DIR" || {
  echo "Could not create $TARGET_DIR" >&2
  exit 1
}
STAGE_DIR=$(mktemp -d "$TARGET_DIR/.amq.install.XXXXXX") || {
  echo "Could not create a staged install directory" >&2
  exit 1
}
install -m 0755 "$EXTRACT_DIR/amq" "$STAGE_DIR/amq" || {
  echo "Could not stage amq" >&2
  exit 1
}
chmod 0755 "$STAGE_DIR/amq" || {
  echo "Could not set staged amq permissions" >&2
  exit 1
}
[ ! -L "$STAGE_DIR/amq" ] && [ -f "$STAGE_DIR/amq" ] &&
  [ -s "$STAGE_DIR/amq" ] && [ -x "$STAGE_DIR/amq" ] || {
  echo "Staged amq validation failed" >&2
  exit 1
}
"$STAGE_DIR/amq" --version >/dev/null || {
  echo "Staged amq failed its version check" >&2
  exit 1
}
mv -f "$STAGE_DIR/amq" "$TARGET_DIR/" || {
  echo "Could not publish amq" >&2
  exit 1
}
[ ! -e "$STAGE_DIR/amq" ] && [ ! -L "$STAGE_DIR/amq" ] &&
  [ ! -L "$TARGET_DIR/amq" ] && [ -f "$TARGET_DIR/amq" ] &&
  [ -s "$TARGET_DIR/amq" ] && [ -x "$TARGET_DIR/amq" ] &&
  cmp -s "$EXTRACT_DIR/amq" "$TARGET_DIR/amq" || {
  echo "Published amq validation failed" >&2
  exit 1
}
rmdir "$STAGE_DIR" 2>/dev/null || true
STAGE_DIR=""
"$TARGET_DIR/amq" --version || {
  echo "Installed amq failed its version check" >&2
  exit 1
}
)
# AMQ_MANUAL_INSTALL_END
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
