# Installation and distribution

The [README getting started guide](README.md#getting-started) is the canonical
first-run path. This page is the one home for binary installs, skill installs,
companions, upgrades, and platform limits.

## Binary installation

### Homebrew (macOS)

```sh
brew install avivsinai/tap/amq
```

The formula installs `amq` and the companions `amq-acp`, `amq-remote`,
`amq-owner-sign`, and `amq-bridge`.

### Installer script (macOS and Linux)

Review [the script](scripts/install.sh), then run it:

```sh
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
```

The script installs the core `amq` binary without `sudo`:

1. It selects the latest release, or the tag in `VERSION=vX.Y.Z`.
2. It downloads the archive for your OS and architecture and the release
   `checksums.txt`, requires exactly one well-formed entry for that archive,
   and verifies it before extraction.
3. It installs to `INSTALL_DIR` when set, otherwise `$GOBIN`, an existing
   `~/.local/bin` or `~/go/bin`, or a new `~/.local/bin`. It stages the binary,
   runs its version check, and then moves it into place.
4. It warns when the directory is not on `PATH`, or when `PATH` resolves `amq`
   to a different executable, and prints the fix.

```sh
curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | VERSION=vX.Y.Z bash
```

The script refuses native Windows; use the ZIP or WSL with the Linux asset.

### Manual release download

Release assets and `checksums.txt` are on the
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

The [installer script](#installer-script-macos-and-linux) is the recommended
way to install a release on macOS and Linux; it performs the verification
below for you. For a manual install, verify the downloaded asset against
`checksums.txt` before you extract it, as in the recipe below. Companions have
their own recipe under [Companion binaries](#companion-binaries). On Windows,
extract the ZIP and put the `.exe` at a stable path.

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

Install the AMQ skills for Claude Code or Codex:

```sh
npx skills add avivsinai/agent-message-queue -g -y
```

Without npm, copy the canonical skill directory from a clone:

```sh
git clone https://github.com/avivsinai/agent-message-queue.git amq-src
for target in ~/.claude/skills/amq-cli ~/.codex/skills/amq-cli; do
  if [ -e "$target" ] || [ -L "$target" ]; then
    echo "Skill target already exists: $target (remove it to replace it)" >&2
  else
    mkdir -p "$(dirname "$target")" && cp -R amq-src/skills/amq-cli "$target"
  fi
done
```

Restart the agent after installing a skill.

## Companion binaries

Homebrew installs `amq-acp`, `amq-remote`, `amq-owner-sign`, and `amq-bridge`
with `amq`. Each is also a release asset for `linux` and `darwin` on `amd64`
and `arm64`. `amq-keepalive` is only a release asset.

| Companion | Purpose |
| --- | --- |
| `amq-keepalive` | On macOS, attaches and supervises registered wake targets. On native Windows (`amq-keepalive_*_windows_*.zip`), only the direct `inject` adapters; `attach`, `reattach`, and `supervise` are not supported. See [amq-keepalive](docs/amq-keepalive.md). |
| `amq-bridge` | Signed cross-host courier. See [amq-bridge](cmd/amq-bridge/README.md). |
| `amq-acp` | Preview ACP v2 stdio companion. See [amq-acp](cmd/amq-acp/README.md). |
| `amq-remote` | Remote-session companion. See [amq-remote](cmd/amq-remote/README.md). |
| `amq-owner-sign` | The owner's signer for a strict relay share; the owner key stays in its process. See [the strict share](cmd/amq-remote/README.md#strict-share-in-one-owner-step). |

To install a companion from its release asset, verify its entry in
`checksums.txt` before you extract it. Asset names follow
`<companion>_<version>_<os>_<arch>.tar.gz`:

```sh
TAG=vX.Y.Z
BIN=amq-keepalive
ASSET=amq-keepalive_X.Y.Z_darwin_arm64.tar.gz
base="https://github.com/avivsinai/agent-message-queue/releases/download/$TAG"
curl -fsSLO "$base/$ASSET" && curl -fsSLO "$base/checksums.txt"
grep "  $ASSET\$" checksums.txt | shasum -a 256 -c   # or: sha256sum -c
tar -xzf "$ASSET" "$BIN"
mkdir -p ~/.local/bin && install -m 0755 "$BIN" ~/.local/bin/"$BIN"
~/.local/bin/"$BIN" --version
```

Install `amq-keepalive` at a stable path, not a versioned package-manager path:
wake identity includes the resolved injector executable. Replacing that file is
atomic; a running supervisor keeps its current image and adopts a strictly
newer image on its next supervise pass. A rollback, an equal-version
replacement, or a supervisor started with `--no-self-upgrade` needs a
service-manager restart. Do not move the executable while registered wakes
identify it.

## Platform capability matrix

| Platform | Core queue (`send`, `drain`, `read`, threads) | `coop init` | `coop exec` | Wake notifications | Installer script |
| --- | --- | --- | --- | --- | --- |
| macOS | Supported | Supported | Supported | Supported | Supported |
| Linux | Supported | Supported | Supported | Supported; raw TTY injection may be disabled by kernel hardening | Supported |
| WSL | Supported via Linux binary | Supported | Supported | Same constraints as Linux | Supported |
| Native Windows | Verified subset only (below) | Supported | Not supported natively ([#891](https://github.com/avivsinai/agent-message-queue/issues/891)) | Not supported natively ([#891](https://github.com/avivsinai/agent-message-queue/issues/891)) | Use the ZIP; the script rejects Windows ([#891](https://github.com/avivsinai/agent-message-queue/issues/891)) |

Native Windows CI (`windows-claim-test`) verifies one subset: exclusive drain
claim (`internal/fsq` claim tests and `internal/cli` drain claim), the callable
Windows launch surface (`launchapi`), and the Windows Codex and Claude
keepalive adapters (`internal/keepalive/adapter`). The rest of `go test ./...`
is not run there; the broader suite has known failures
([#891](https://github.com/avivsinai/agent-message-queue/issues/891)).
`amq wake`, `coop exec`, and terminal supervision are not native Windows
contracts. Direct submitted injection via `amq-keepalive.exe` is the supported
injection path. Use WSL with a Linux asset for the complete co-op workflow.
`amq-bridge`, `amq-acp`, `amq-remote`, and `amq-owner-sign` are published for
Linux and macOS, not Windows.

## Verify and upgrade

```sh
amq --version
brew upgrade amq   # Homebrew: amq and its companions
amq upgrade        # direct installs
```

`amq upgrade --all` also upgrades directly installed `amq-keepalive`,
`amq-bridge`, and `amq-acp` when their targets are unambiguous and match the
release build identity. Reinstall a directly installed `amq-remote` or
`amq-owner-sign` from its release asset. Package-managed installs are delegated
to their package manager. On Windows, `--all` upgrades only
`amq-keepalive.exe`.

Disable upgrade notifications for one command or the environment:

```sh
amq --no-update-check ...
export AMQ_NO_UPDATE_CHECK=1
```
