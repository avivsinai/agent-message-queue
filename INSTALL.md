# Installation and distribution

The [README getting started guide](README.md#getting-started) is the canonical
first-run path. This page covers alternative binary installs, skill installs,
companions, upgrades, and platform limits.

## Binary installation

### Homebrew (macOS)

```sh
brew install avivsinai/tap/amq
```

### Installer script (macOS and Linux)

```sh
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
```

The script installs without sudo to `$GOBIN`, an existing `~/.local/bin` or
`~/go/bin`, or a newly created `~/.local/bin`. Review a downloaded script before
running it. It requires exactly one valid checksum entry for the selected asset
and verifies that checksum before extraction.

To select a release explicitly:

```sh
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | VERSION=vX.Y.Z bash
```

Set `INSTALL_DIR` to choose another destination. The installer refuses Windows;
use the native ZIP or WSL with the Linux asset.

### Manual release download

Release assets and `checksums.txt` are published on the
[Releases page](https://github.com/avivsinai/agent-message-queue/releases).

| Platform | Core asset |
| --- | --- |
| macOS Apple Silicon | `amq_*_darwin_arm64.tar.gz` |
| macOS Intel | `amq_*_darwin_amd64.tar.gz` |
| Linux x86_64 | `amq_*_linux_amd64.tar.gz` |
| Linux ARM64 | `amq_*_linux_arm64.tar.gz` |
| Native Windows x86_64 | `amq_*_windows_amd64.zip` |
| Native Windows ARM64 | `amq_*_windows_arm64.zip` |
| WSL x86_64 | `amq_*_linux_amd64.tar.gz` |

Verify the selected asset against `checksums.txt` before extracting it. Replace
the placeholder tag, asset, and target directory in the advanced example below.

<details>
<summary>Advanced checksum-verified manual install</summary>

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

</details>

### Build from source

Requires Go 1.26 or newer:

```sh
git clone https://github.com/avivsinai/agent-message-queue.git
cd agent-message-queue
make build
mkdir -p ~/.local/bin
mv amq ~/.local/bin/
```

## Skills

Install the AMQ skill for Claude Code or Codex:

```sh
npx skills add avivsinai/agent-message-queue -g -y
```

If npm-based tooling is unavailable, clone into a private temporary directory
and copy the canonical skill directory. The example refuses an existing target;
choose a new destination or remove the old skill intentionally before retrying:

```sh
(
set -eu
skill_tmp=$(mktemp -d "${TMPDIR:-/tmp}/amq-skills.XXXXXX")
trap 'rm -rf "$skill_tmp"' EXIT
git clone https://github.com/avivsinai/agent-message-queue.git "$skill_tmp/repo"
mkdir -p ~/.claude/skills ~/.codex/skills
for target in ~/.claude/skills/amq-cli ~/.codex/skills/amq-cli; do
  if [ -e "$target" ] || [ -L "$target" ]; then
    echo "Skill target already exists: $target" >&2
    exit 1
  fi
done
cp -R "$skill_tmp/repo/skills/amq-cli" ~/.claude/skills/amq-cli
cp -R "$skill_tmp/repo/skills/amq-cli" ~/.codex/skills/amq-cli
)
```

Restart the agent after installing a skill. Other skill distribution methods are
not required for the AMQ onboarding path.

## Companion binaries

`brew install avivsinai/tap/amq` installs `amq` and `amq-acp`. The other
companions are separate release assets and are not installed by Homebrew:

- `amq-keepalive_*_darwin_{amd64,arm64}.tar.gz` and
  `amq-keepalive_*_windows_{amd64,arm64}.zip` provide the keepalive companion.
  On macOS, it can attach and supervise registered wake targets. On native
  Windows, use its direct `inject` adapters only; `attach`, `reattach`, and
  `supervise` are not supported. Install it at a stable executable path; do not
  use a versioned package-manager path for a registered macOS wake.
- `amq-bridge_*_{linux,darwin}_{amd64,arm64}.tar.gz` is the signed cross-host
  courier. See [amq-bridge](cmd/amq-bridge/README.md).
- `amq-acp` is the preview ACP v2 stdio companion. The `amq` Homebrew formula
  installs it; `amq-acp_*_{linux,darwin}_{amd64,arm64}.tar.gz` remains for a
  direct install. See [amq-acp](cmd/amq-acp/README.md).
- `amq-remote_*_{linux,darwin}_{amd64,arm64}.tar.gz` is the remote-session
  companion. See [amq-remote](cmd/amq-remote/README.md) for commands, flags,
  and exit codes. Design and pinned seams stay in the
  [remote design](docs/adr-remote-control.md) and
  [capability reference](docs/remote-compat.md).

Use the same release tag and checksum verification for each companion. A stable
path matters because wake identity includes the resolved injector executable.
Replacing that file is atomic; a running supervisor keeps its current image and
can adopt a strictly newer image on its next supervise pass. An older rollback,
an equal-version replacement, or a supervisor started with `--no-self-upgrade`
requires a service-manager restart. The registry is retained; do not move the
executable while registered wakes identify it.

For Unix companions, download the matching archive and checksum file, verify
the one selected entry, then install the extracted executable at a stable path:

```sh
(
set -eu
COMPANION=amq-keepalive
VERSION=X.Y.Z
PLATFORM=darwin_arm64
TAG=v$VERSION
ASSET="${COMPANION}_${VERSION}_${PLATFORM}.tar.gz"
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/amq-companion.XXXXXX")
companion_stage=""
trap 'rm -rf "$WORK_DIR"; if [ -n "$companion_stage" ]; then rm -rf "$companion_stage"; fi' EXIT
curl -fsSL "https://github.com/avivsinai/agent-message-queue/releases/download/$TAG/$ASSET" -o "$WORK_DIR/$ASSET"
curl -fsSL "https://github.com/avivsinai/agent-message-queue/releases/download/$TAG/checksums.txt" -o "$WORK_DIR/checksums.txt"
awk -v asset="$ASSET" '$2 == asset { print; count++ } END { if (count != 1) exit 1 }' "$WORK_DIR/checksums.txt" >"$WORK_DIR/selected.sha256"
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$WORK_DIR" && sha256sum -c selected.sha256)
else
  (cd "$WORK_DIR" && shasum -a 256 -c selected.sha256)
fi
tar -xzf "$WORK_DIR/$ASSET" -C "$WORK_DIR"
mkdir -p "$HOME/.local/bin"
companion_stage=$(mktemp -d "$HOME/.local/bin/.amq-companion.XXXXXX")
install -m 0755 "$WORK_DIR/$COMPANION" "$companion_stage/$COMPANION"
mv -f "$companion_stage/$COMPANION" "$HOME/.local/bin/$COMPANION"
"$HOME/.local/bin/$COMPANION" --version
)
```

Bridge, ACP, and Remote also have `linux_amd64` and `linux_arm64` assets. For
Windows, extract the ZIP and place `amq-keepalive.exe` at a stable path; only
the direct `inject` adapters are supported there.

`amq upgrade --all` upgrades directly installed `amq-keepalive`, `amq-bridge`,
and `amq-acp` when their targets are unambiguous and match the release build
identity. Update `amq-remote` from its release asset separately. Package-managed
core installs are delegated to their package manager. `brew upgrade amq` updates
the formula's `amq-acp`; a directly installed `amq-acp` still uses
`amq upgrade --all`. On Windows, `--all` can upgrade `amq-keepalive.exe`; bridge
and ACP are not published for Windows.

## Platform capability matrix

| Platform | Core queue (`send`, `drain`, `read`, threads) | `coop init` | `coop exec` | Wake notifications | Installer script |
| --- | --- | --- | --- | --- | --- |
| macOS | Supported | Supported | Supported | Supported | Supported |
| Linux | Supported | Supported | Supported | Supported; raw TTY injection may be disabled by kernel hardening | Supported |
| WSL | Supported via Linux binary | Supported | Supported | Same constraints as Linux | Supported |
| Native Windows | Supported via Windows ZIP | Supported | Not supported natively | Not supported natively | Use the ZIP; the script rejects Windows |

Native Windows supports core queue commands and direct submitted injection via
`amq-keepalive.exe`, but not `amq wake`, `coop exec`, or terminal supervision.
Use WSL with a Linux asset for the complete co-op workflow. `amq-bridge` and
`amq-acp` are published for Linux and macOS, not Windows.

## Verify and upgrade

```sh
amq --version
```

Homebrew:

```sh
brew upgrade amq
```

Other direct installs:

```sh
amq upgrade
```

Upgrade notifications can be disabled for one command or the environment:

```sh
amq --no-update-check ...
export AMQ_NO_UPDATE_CHECK=1
```
