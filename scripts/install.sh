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
trap 'rm -rf "$TMP_DIR"' EXIT

# Download (curl -f fails on HTTP errors like 404)
if ! curl -fsSL "$URL" -o "$TMP_DIR/$ASSET"; then
    echo -e "${RED}Error: Download failed (asset not found or network error)${NC}"
    echo "URL: $URL"
    echo "Check available releases: https://github.com/$REPO/releases"
    exit 1
fi

# Verify checksums when possible
CHECKSUMS_FILE="$TMP_DIR/checksums.txt"
if curl -fsSL "$CHECKSUMS_URL" -o "$CHECKSUMS_FILE"; then
    CHECKSUM_LINE=$(grep " $ASSET$" "$CHECKSUMS_FILE" || true)
    if [ -z "$CHECKSUM_LINE" ]; then
        echo -e "${YELLOW}Warning: checksum entry not found for $ASSET${NC}"
    elif command -v sha256sum &> /dev/null; then
        (cd "$TMP_DIR" && echo "$CHECKSUM_LINE" | sha256sum -c -) || {
            echo -e "${RED}Error: checksum verification failed${NC}"
            exit 1
        }
    elif command -v shasum &> /dev/null; then
        EXPECTED=$(echo "$CHECKSUM_LINE" | awk '{print $1}')
        ACTUAL=$(shasum -a 256 "$TMP_DIR/$ASSET" | awk '{print $1}')
        if [ "$EXPECTED" != "$ACTUAL" ]; then
            echo -e "${RED}Error: checksum verification failed${NC}"
            exit 1
        fi
    else
        echo -e "${YELLOW}Warning: sha256 tool not found; skipping checksum verification${NC}"
    fi
else
    echo -e "${YELLOW}Warning: failed to download checksums; skipping verification${NC}"
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

# Install binary with correct permissions
install -m 0755 amq "$INSTALL_DIR/amq"

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
