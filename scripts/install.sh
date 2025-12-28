#!/bin/bash
# AMQ Binary Installer
# Usage: curl -sL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
#
# Options:
#   VERSION=v0.7.3 ./install.sh   # Install specific version
#   INSTALL_DIR=~/bin ./install.sh # Install to custom directory

set -e

REPO="avivsinai/agent-message-queue"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
VERSION="${VERSION:-latest}"

GREEN='\033[0;32m'
RED='\033[0;31m'
BLUE='\033[0;34m'
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
    VERSION=$(curl -sL "https://api.github.com/repos/$REPO/releases/latest" | grep '"tag_name"' | cut -d'"' -f4)
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

echo "Downloading: $ASSET"

# Create temp directory
TMP_DIR=$(mktemp -d)
trap "rm -rf $TMP_DIR" EXIT

# Download and extract
if ! curl -sL "$URL" -o "$TMP_DIR/$ASSET"; then
    echo -e "${RED}Error: Download failed${NC}"
    echo "URL: $URL"
    exit 1
fi

cd "$TMP_DIR"
tar xzf "$ASSET"

if [ ! -f "amq" ]; then
    echo -e "${RED}Error: Binary not found in archive${NC}"
    exit 1
fi

# Install
echo "Installing to: $INSTALL_DIR/amq"

if [ -w "$INSTALL_DIR" ]; then
    mv amq "$INSTALL_DIR/amq"
else
    echo "Requires sudo for $INSTALL_DIR"
    sudo mv amq "$INSTALL_DIR/amq"
fi

chmod +x "$INSTALL_DIR/amq"

echo ""
echo -e "${GREEN}Installation complete!${NC}"
echo ""

# Verify
if command -v amq &> /dev/null; then
    echo "Installed: $(amq --version)"
else
    echo -e "${RED}Warning: amq not in PATH${NC}"
    echo "Add $INSTALL_DIR to your PATH or run: export PATH=\"\$PATH:$INSTALL_DIR\""
fi

echo ""
echo "Next: Initialize mailboxes with 'amq init --root .agent-mail --agents claude,codex'"
