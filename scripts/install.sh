#!/bin/bash
# AMQ Binary Installer
# Usage: curl -fsSL https://raw.githubusercontent.com/avivsinai/agent-message-queue/main/scripts/install.sh | bash
#
# Options (set before piping to bash):
#   curl ... | VERSION=v0.7.3 bash
#   curl ... | INSTALL_DIR=~/bin bash

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

echo "Downloading: $ASSET"

# Create temp directory
TMP_DIR=$(mktemp -d)
trap "rm -rf $TMP_DIR" EXIT

# Download (curl -f fails on HTTP errors like 404)
if ! curl -fsSL "$URL" -o "$TMP_DIR/$ASSET"; then
    echo -e "${RED}Error: Download failed (asset not found or network error)${NC}"
    echo "URL: $URL"
    echo "Check available releases: https://github.com/$REPO/releases"
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

# Install using install command (handles permissions correctly)
echo "Installing to: $INSTALL_DIR/amq"

# Ensure install directory exists
if [ ! -d "$INSTALL_DIR" ]; then
    if [ -w "$(dirname "$INSTALL_DIR")" ]; then
        mkdir -p "$INSTALL_DIR"
    else
        echo "Creating $INSTALL_DIR (requires sudo)"
        sudo mkdir -p "$INSTALL_DIR"
    fi
fi

# Install binary with correct permissions
if [ -w "$INSTALL_DIR" ]; then
    install -m 0755 amq "$INSTALL_DIR/amq"
else
    echo "Requires sudo for $INSTALL_DIR"
    sudo install -m 0755 amq "$INSTALL_DIR/amq"
fi

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
