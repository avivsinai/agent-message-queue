#!/bin/bash
# AMQ Binary Installer
# Usage: curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
#
# Installs to user-local directory (no sudo required):
#   - $GOBIN if set
#   - ~/.local/bin if exists
#   - ~/go/bin if exists
#   - ~/.local/bin (created if needed)
#
# Options:
#   curl ... | bash -s -- --skill        # Also install Claude Code/Codex skill
#   curl ... | VERSION=v0.7.3 bash       # Specific version
#   curl ... | INSTALL_DIR=~/bin bash    # Custom install dir

set -e

REPO="avivsinai/agent-message-queue"
VERSION="${VERSION:-latest}"
INSTALL_SKILL=false

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --skill)
            INSTALL_SKILL=true
            shift
            ;;
        *)
            shift
            ;;
    esac
done

# Determine install directory (user-local, no sudo needed)
# Priority: explicit INSTALL_DIR > GOBIN > ~/.local/bin > ~/go/bin
determine_install_dir() {
    if [ -n "$INSTALL_DIR" ]; then
        echo "$INSTALL_DIR"
    elif [ -n "$GOBIN" ]; then
        echo "$GOBIN"
    elif [ -d "$HOME/.local/bin" ]; then
        echo "$HOME/.local/bin"
    elif [ -d "$HOME/go/bin" ]; then
        echo "$HOME/go/bin"
    else
        # Default to ~/.local/bin (XDG standard)
        echo "$HOME/.local/bin"
    fi
}

INSTALL_DIR=$(determine_install_dir)

GREEN='\033[0;32m'
RED='\033[0;31m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0m'

echo -e "${BLUE}=== AMQ Binary Installer ===${NC}"
echo ""

# Detect platform
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    arm64|aarch64) ARCH="arm64" ;;
    *)
        echo -e "${RED}Error: Unsupported architecture: $ARCH${NC}"
        exit 1
        ;;
esac

case "$OS" in
    darwin|linux) ;;
    mingw*|msys*|cygwin*)
        echo -e "${RED}Error: Windows detected. Please use WSL or download manually.${NC}"
        echo "See: https://github.com/$REPO/releases"
        exit 1
        ;;
    *)
        echo -e "${RED}Error: Unsupported OS: $OS${NC}"
        exit 1
        ;;
esac

echo "Platform: ${OS}_${ARCH}"

# Get version
if [ "$VERSION" = "latest" ]; then
    echo "Fetching latest version..."
    VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | grep '"tag_name"' | cut -d'"' -f4)
    if [ -z "$VERSION" ]; then
        echo -e "${RED}Error: Could not determine latest version${NC}"
        exit 1
    fi
fi

echo "Version: $VERSION"

# Build asset name (format: amq_0.7.3_darwin_arm64.tar.gz)
VERSION_NUM="${VERSION#v}"  # Remove 'v' prefix
ASSET="amq_${VERSION_NUM}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET"
CHECKSUMS_URL="https://github.com/$REPO/releases/download/$VERSION/checksums.txt"

echo "Downloading: $ASSET"

# Create temp directory
TMP_DIR=$(mktemp -d)
STAGED_BINARY=""
cleanup() {
    if [ -n "$STAGED_BINARY" ]; then
        rm -f "$STAGED_BINARY" || true
    fi
    rm -rf "$TMP_DIR" || true
}
trap cleanup EXIT

# Download (curl -f fails on HTTP errors like 404)
if ! curl -fsSL "$URL" -o "$TMP_DIR/$ASSET"; then
    echo -e "${RED}Error: Download failed (asset not found or network error)${NC}"
    echo "URL: $URL"
    echo "Check available releases: https://github.com/$REPO/releases"
    exit 1
fi

# Verify the selected release asset before extracting or installing it.
CHECKSUMS_FILE="$TMP_DIR/checksums.txt"
if ! curl -fsSL "$CHECKSUMS_URL" -o "$CHECKSUMS_FILE"; then
    echo -e "${RED}Error: failed to download checksums${NC}"
    exit 1
fi
if [ ! -r "$CHECKSUMS_FILE" ]; then
    echo -e "${RED}Error: checksums.txt is missing or unreadable${NC}"
    exit 1
fi

if ! CHECKSUM_RESULT=$(awk -v asset="$ASSET" '
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
' "$CHECKSUMS_FILE"); then
    echo -e "${RED}Error: checksums.txt is unreadable${NC}"
    exit 1
fi

case "$CHECKSUM_RESULT" in
    missing)
        echo -e "${RED}Error: checksum entry not found for $ASSET${NC}"
        exit 1
        ;;
    duplicate)
        echo -e "${RED}Error: duplicate checksum entries found for $ASSET${NC}"
        exit 1
        ;;
    malformed)
        echo -e "${RED}Error: malformed checksum entry for $ASSET${NC}"
        exit 1
        ;;
    ok:*)
        EXPECTED="${CHECKSUM_RESULT#ok:}"
        ;;
    *)
        echo -e "${RED}Error: could not parse checksum entry for $ASSET${NC}"
        exit 1
        ;;
esac

if command -v sha256sum &> /dev/null; then
    (cd "$TMP_DIR" && printf '%s  %s\n' "$EXPECTED" "$ASSET" | sha256sum -c -) || {
        echo -e "${RED}Error: checksum verification failed${NC}"
        exit 1
    }
elif command -v shasum &> /dev/null; then
    if ! ACTUAL_LINE=$(shasum -a 256 "$TMP_DIR/$ASSET"); then
        echo -e "${RED}Error: checksum verification failed${NC}"
        exit 1
    fi
    ACTUAL="${ACTUAL_LINE%%[[:space:]]*}"
    EXPECTED_NORMALIZED=$(printf '%s' "$EXPECTED" | tr '[:upper:]' '[:lower:]')
    ACTUAL_NORMALIZED=$(printf '%s' "$ACTUAL" | tr '[:upper:]' '[:lower:]')
    if [ "$EXPECTED_NORMALIZED" != "$ACTUAL_NORMALIZED" ]; then
        echo -e "${RED}Error: checksum verification failed${NC}"
        exit 1
    fi
else
    echo -e "${RED}Error: sha256sum or shasum is required to verify the download${NC}"
    exit 1
fi

cd "$TMP_DIR"
if ! tar xzf "$ASSET" 2>/dev/null; then
    echo -e "${RED}Error: Failed to extract archive (corrupted download?)${NC}"
    exit 1
fi

if [ ! -f "amq" ]; then
    echo -e "${RED}Error: Binary not found in archive${NC}"
    exit 1
fi

# Install to user-local directory (no sudo needed)
echo "Installing to: $INSTALL_DIR/amq"

# Ensure install directory exists
mkdir -p "$INSTALL_DIR"

# Build the replacement beside the target, then atomically publish it.
if ! STAGED_BINARY=$(mktemp "$INSTALL_DIR/.amq.install.XXXXXX"); then
    echo -e "${RED}Error: could not create staged install file${NC}"
    exit 1
fi
if ! install -m 0755 amq "$STAGED_BINARY"; then
    echo -e "${RED}Error: failed to stage binary${NC}"
    exit 1
fi
if ! chmod 0755 "$STAGED_BINARY" ||
   [ ! -f "$STAGED_BINARY" ] ||
   [ ! -s "$STAGED_BINARY" ] ||
   [ ! -x "$STAGED_BINARY" ]; then
    echo -e "${RED}Error: staged binary validation failed${NC}"
    exit 1
fi
if ! mv -f "$STAGED_BINARY" "$INSTALL_DIR/amq"; then
    echo -e "${RED}Error: failed to publish binary${NC}"
    exit 1
fi
STAGED_BINARY=""

echo ""
echo -e "${GREEN}Installation complete!${NC}"
echo ""

# Verify installation
if command -v amq &> /dev/null; then
    echo "Installed: $(amq --version)"
else
    echo -e "${RED}Warning: $INSTALL_DIR is not in your PATH${NC}"
    echo ""
    echo "Add it to your shell config:"
    if [ -n "$ZSH_VERSION" ] || [ -f "$HOME/.zshrc" ]; then
        echo "  echo 'export PATH=\"\$PATH:$INSTALL_DIR\"' >> ~/.zshrc && source ~/.zshrc"
    else
        echo "  echo 'export PATH=\"\$PATH:$INSTALL_DIR\"' >> ~/.bashrc && source ~/.bashrc"
    fi
    echo ""
    echo "Or run directly: $INSTALL_DIR/amq --version"
fi

echo ""

# Install skill if requested
if [ "$INSTALL_SKILL" = true ]; then
    echo -e "${BLUE}Installing Claude Code / Codex skill...${NC}"
    if command -v npx &> /dev/null; then
        if npx skills add avivsinai/agent-message-queue -g -y; then
            echo -e "${GREEN}Skill installed successfully!${NC}"
        else
            echo -e "${YELLOW}Warning: Skill installation failed. Try manually:${NC}"
            echo "  npx skills add avivsinai/agent-message-queue -g -y"
        fi
    else
        echo -e "${YELLOW}Warning: npx not found. Install Node.js, then run:${NC}"
        echo "  npx skills add avivsinai/agent-message-queue -g -y"
    fi
    echo ""
fi

echo "Next steps:"
echo "  1. Start agent: amq coop exec claude"
echo "  Tip: eval \"\$(amq shell-setup)\" to add co-op aliases to your shell"
echo ""
