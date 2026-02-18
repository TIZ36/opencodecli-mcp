#!/bin/bash
set -e

# Configuration
OPENCODE_VERSION="${OPENCODE_VERSION:-latest}"
OPENCODE_INSTALL_DIR="${OPENCODE_INSTALL_DIR:-$HOME/.local/bin}"
OPENCODE_OFFICIAL_DIR="$HOME/.opencode/bin"
OPENCODE_BIN="$OPENCODE_INSTALL_DIR/opencode"
MCP_ADDR="${MCP_ADDR:-:9876}"
MCP_TIMEOUT_SEC="${MCP_TIMEOUT_SEC:-120}"
# Use official installer by default: https://opencode.ai/install
USE_OFFICIAL_INSTALL="${USE_OFFICIAL_INSTALL:-true}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[OK]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Paths that may contain OLD opencode (opencode-ai, v0.x) - will be removed when upgrading
OLD_OPENCODE_PATHS=(
    "$HOME/.local/bin/opencode"
    "$HOME/.local/bin/opencode-cli"
    "/usr/local/bin/opencode"
    "/usr/local/bin/opencode-cli"
    "$HOME/go/bin/opencode"
)

COMMON_OPENCODE_PATHS=(
    "$HOME/.opencode/bin/opencode"
    "$HOME/.local/bin/opencode"
    "$HOME/.local/bin/opencode-cli"
    "/usr/local/bin/opencode"
    "/usr/local/bin/opencode-cli"
    "/usr/bin/opencode"
    "/usr/bin/opencode-cli"
    "$HOME/go/bin/opencode"
)

# Check if version is old (0.x from opencode-ai, incompatible CLI)
is_old_opencode_version() {
    local ver="$1"
    # 0, 0.x, 0.0.x = old; 1.x = new (opencode.ai)
    [[ "$ver" =~ ^0(\.[0-9]+)*$ ]] && return 0
    return 1
}

# Uninstall old opencode from known locations (avoid PATH conflicts with official)
uninstall_old_opencode() {
    local removed=0
    for path in "${OLD_OPENCODE_PATHS[@]}"; do
        if [ -e "$path" ]; then
            rm -f "$path"
            log_info "Removed old opencode: $path"
            removed=1
        fi
    done
    [ "$removed" -eq 1 ] && log_success "Old opencode uninstalled"
}

find_opencode() {
    # Prefer official installer path (~/.opencode/bin) - opencode 1.2+ has correct CLI (run, models)
    if [ -x "$HOME/.opencode/bin/opencode" ]; then
        EXISTING_BIN="$HOME/.opencode/bin/opencode"
        log_success "Found opencode (official): $EXISTING_BIN"
        return 0
    fi

    if command -v opencode &>/dev/null; then
        EXISTING_BIN=$(command -v opencode)
        log_success "Found opencode in PATH: $EXISTING_BIN"
        return 0
    fi
    if command -v opencode-cli &>/dev/null; then
        EXISTING_BIN=$(command -v opencode-cli)
        log_success "Found opencode-cli in PATH: $EXISTING_BIN"
        return 0
    fi

    for path in "${COMMON_OPENCODE_PATHS[@]}"; do
        if [ -x "$path" ]; then
            EXISTING_BIN="$path"
            log_success "Found opencode at: $EXISTING_BIN"
            return 0
        fi
    done
    return 1
}

# Install via official script (recommended)
install_official() {
    log_info "Installing opencode via official installer (https://opencode.ai/install)..."
    if ! curl -fsSL https://opencode.ai/install | bash; then
        log_error "Official installer failed"
        return 1
    fi
    # Ensure PATH includes official dir for current session
    export PATH="$OPENCODE_OFFICIAL_DIR:$PATH"
    return 0
}

# Install from GitHub (fallback)
install_from_github() {
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64|amd64) ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) log_error "Unsupported architecture: $ARCH"; exit 1 ;;
    esac
    case "$OS" in
        linux) OS="linux" ;;
        darwin) OS="darwin" ;;
        mingw*|msys*|cygwin*) OS="windows" ;;
        *) log_error "Unsupported OS: $OS"; exit 1 ;;
    esac

    RELEASE_OS="$OS"
    [ "$OS" = "darwin" ] && RELEASE_OS="mac"
    [ "$OS" = "windows" ] && RELEASE_OS="windows"
    RELEASE_ARCH="$ARCH"
    [ "$ARCH" = "amd64" ] && RELEASE_ARCH="x86_64"

    if [ "$OPENCODE_VERSION" = "latest" ]; then
        OPENCODE_VERSION=$(curl -sL --connect-timeout 5 --max-time 10 \
            "https://api.github.com/repos/opencode-ai/opencode/releases/latest" 2>/dev/null | \
            grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')
        [ -z "$OPENCODE_VERSION" ] && OPENCODE_VERSION="v0.5.0"
    fi

    if [ "$OS" = "windows" ]; then
        FILENAME="opencode-${RELEASE_OS}-${RELEASE_ARCH}.zip"
    else
        FILENAME="opencode-${RELEASE_OS}-${RELEASE_ARCH}.tar.gz"
    fi
    DOWNLOAD_URL="https://github.com/opencode-ai/opencode/releases/download/${OPENCODE_VERSION}/${FILENAME}"

    log_info "Downloading from: $DOWNLOAD_URL"
    mkdir -p "$OPENCODE_INSTALL_DIR"
    TEMP_DIR=$(mktemp -d)
    TEMP_FILE="$TEMP_DIR/$FILENAME"

    if command -v curl &>/dev/null; then
        curl -fsSL "$DOWNLOAD_URL" -o "$TEMP_FILE"
    elif command -v wget &>/dev/null; then
        wget -q "$DOWNLOAD_URL" -O "$TEMP_FILE"
    else
        log_error "curl or wget required"; exit 1
    fi

    if [ "$OS" = "windows" ]; then
        unzip -q "$TEMP_FILE" -d "$TEMP_DIR"
    else
        tar -xzf "$TEMP_FILE" -C "$TEMP_DIR"
    fi

    BINARY_NAME="opencode.exe"
    [ "$OS" != "windows" ] && BINARY_NAME="opencode"
    EXTRACTED_BIN=$(find "$TEMP_DIR" -name "$BINARY_NAME" -type f | head -1)
    [ -z "$EXTRACTED_BIN" ] && { log_error "Binary not found in archive"; rm -rf "$TEMP_DIR"; exit 1; }

    mv "$EXTRACTED_BIN" "$OPENCODE_BIN"
    chmod +x "$OPENCODE_BIN"
    [ "$OS" != "windows" ] && ln -sf "$OPENCODE_BIN" "$OPENCODE_INSTALL_DIR/opencode-cli"
    rm -rf "$TEMP_DIR"
    log_success "opencode installed to: $OPENCODE_BIN"
}

install_opencode() {
    if find_opencode; then
        OPENCODE_BIN="$EXISTING_BIN"
        CURRENT_VER=$("$OPENCODE_BIN" -v 2>/dev/null | tail -1 | grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' || echo "0")
        log_info "Current version: $CURRENT_VER"

        if [ "$1" != "--force" ]; then
            # If old version (0.x), auto-upgrade to latest
            if is_old_opencode_version "$CURRENT_VER"; then
                log_warn "Detected old opencode ($CURRENT_VER), upgrading to latest..."
            else
                log_info "Use --force to reinstall"
                return 0
            fi
        else
            log_info "Forcing reinstall..."
        fi
    fi

    if [ "$USE_OFFICIAL_INSTALL" = "true" ] && [ "$OPENCODE_VERSION" = "latest" ]; then
        uninstall_old_opencode
        if install_official; then
            export PATH="$OPENCODE_OFFICIAL_DIR:$PATH"
            if [ -x "$OPENCODE_OFFICIAL_DIR/opencode" ]; then
                EXISTING_BIN="$OPENCODE_OFFICIAL_DIR/opencode"
                log_success "Installed via official script: $EXISTING_BIN"
            fi
            return 0
        fi
        log_warn "Falling back to GitHub install..."
        uninstall_old_opencode
    fi

    install_from_github

    if "$OPENCODE_BIN" --version &>/dev/null; then
        log_success "Verification: $($OPENCODE_BIN --version | head -1)"
    fi
}

build_server() {
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    cd "$SCRIPT_DIR"
    [ ! -f "go.mod" ] && { log_error "go.mod not found"; exit 1; }
    [ ! -x "$(command -v go)" ] && { log_error "Go not installed"; exit 1; }
    log_info "Building MCP server..."
    go build -o ./mcpserver ./cmd/mcpserver
    log_success "MCP server built: ./mcpserver"
}

resolve_opencode_path() {
    OPENCODE_PATH=""
    [ -n "${EXISTING_BIN:-}" ] && [ -x "$EXISTING_BIN" ] && OPENCODE_PATH="$EXISTING_BIN"
    [ -z "$OPENCODE_PATH" ] && [ -x "$OPENCODE_BIN" ] && OPENCODE_PATH="$OPENCODE_BIN"
    [ -z "$OPENCODE_PATH" ] && command -v opencode &>/dev/null && OPENCODE_PATH=$(command -v opencode)
    [ -z "$OPENCODE_PATH" ] && command -v opencode-cli &>/dev/null && OPENCODE_PATH=$(command -v opencode-cli)
    if [ -z "$OPENCODE_PATH" ]; then
        for p in "${COMMON_OPENCODE_PATHS[@]}"; do
            [ -x "$p" ] && { OPENCODE_PATH="$p"; break; }
        done
    fi
    echo "$OPENCODE_PATH"
}

start_server() {
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    cd "$SCRIPT_DIR"
    [ ! -f "./mcpserver" ] && build_server

    find_opencode 2>/dev/null || true
    OPENCODE_PATH=$(resolve_opencode_path)
    [ -z "$OPENCODE_PATH" ] && { log_error "opencode not found. Run: $0 install"; exit 1; }

    log_info "Starting MCP server..."
    log_info "  Address: $MCP_ADDR"
    log_info "  Timeout: ${MCP_TIMEOUT_SEC}s"
    log_info "  opencode: $OPENCODE_PATH"
    echo ""

    export MCP_ADDR MCP_TIMEOUT_SEC
    export MCP_TARGET="$OPENCODE_PATH"
    exec ./mcpserver
}

show_help() {
    cat <<EOF
OpenCode MCP Server Starter

Usage: $0 [command] [options]

Commands:
  install [--force]  Install opencode (official installer by default)
  build             Build the MCP server
  start             Start the MCP server
  run [--skip-install]  Install (if needed), build, and start

Options:
  --force           Force reinstall opencode
  --skip-install    Skip install step when using 'run'

Note: Old opencode (0.x from opencode-ai) is auto-uninstalled when upgrading to latest.

Environment:
  USE_OFFICIAL_INSTALL   Use opencode.ai/install (default: true)
  OPENCODE_VERSION       Version for GitHub install (default: latest)
  OPENCODE_INSTALL_DIR   Install dir for GitHub (default: ~/.local/bin)
  MCP_ADDR               Listen address (default: :9876)
  MCP_TIMEOUT_SEC        Command timeout (default: 120)

Examples:
  $0 run                      # Full setup and start
  $0 run --skip-install       # Build and start only (opencode must exist)
  USE_OFFICIAL_INSTALL=false $0 install  # Use GitHub install
  $0 install --force          # Force reinstall
EOF
}

main() {
    case "${1:-run}" in
        install)
            install_opencode "$2"
            ;;
        build)
            build_server
            ;;
        start)
            find_opencode || true
            start_server
            ;;
        run)
            if [ "${2:-}" != "--skip-install" ]; then
                install_opencode
            else
                find_opencode || { log_error "opencode not found"; exit 1; }
            fi
            build_server
            start_server
            ;;
        help|--help|-h)
            show_help
            ;;
        *)
            log_error "Unknown command: $1"
            show_help
            exit 1
            ;;
    esac
}

main "$@"
