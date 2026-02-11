#!/bin/bash
set -e

# Configuration
OPENCODE_VERSION="${OPENCODE_VERSION:-latest}"
OPENCODE_INSTALL_DIR="${OPENCODE_INSTALL_DIR:-$HOME/.local/bin}"
OPENCODE_BIN="$OPENCODE_INSTALL_DIR/opencode"
MCP_ADDR="${MCP_ADDR:-:9876}"
MCP_TIMEOUT_SEC="${MCP_TIMEOUT_SEC:-120}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[OK]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Detect OS and architecture
detect_platform() {
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    ARCH=$(uname -m)

    case "$ARCH" in
        x86_64|amd64)
            ARCH="amd64"
            ;;
        aarch64|arm64)
            ARCH="arm64"
            ;;
        *)
            log_error "Unsupported architecture: $ARCH"
            exit 1
            ;;
    esac

    case "$OS" in
        linux)
            OS="linux"
            ;;
        darwin)
            OS="darwin"
            ;;
        mingw*|msys*|cygwin*)
            OS="windows"
            ;;
        *)
            log_error "Unsupported OS: $OS"
            exit 1
            ;;
    esac

    log_info "Detected platform: ${OS}/${ARCH}"
}

# Check if opencode is already available in PATH or common locations
find_existing_opencode() {
    # Check PATH first (both names)
    if command -v opencode &> /dev/null; then
        EXISTING_BIN=$(command -v opencode)
        log_success "Found opencode in PATH: $EXISTING_BIN"
        return 0
    fi
    
    if command -v opencode-cli &> /dev/null; then
        EXISTING_BIN=$(command -v opencode-cli)
        log_success "Found opencode-cli in PATH: $EXISTING_BIN"
        return 0
    fi

    # Check common locations (both names)
    local COMMON_PATHS=(
        "$HOME/.local/bin/opencode"
        "$HOME/.local/bin/opencode-cli"
        "/usr/local/bin/opencode"
        "/usr/local/bin/opencode-cli"
        "/usr/bin/opencode"
        "/usr/bin/opencode-cli"
        "$HOME/go/bin/opencode"
        "$HOME/.opencode/bin/opencode"
    )

    for path in "${COMMON_PATHS[@]}"; do
        if [ -x "$path" ]; then
            EXISTING_BIN="$path"
            log_success "Found opencode at: $EXISTING_BIN"
            return 0
        fi
    done

    return 1
}

# Get latest version from GitHub
get_latest_version() {
    if [ "$OPENCODE_VERSION" = "latest" ]; then
        log_info "Fetching latest opencode version..."
        
        # Try GitHub API with timeout
        OPENCODE_VERSION=$(curl -sL --connect-timeout 5 --max-time 10 \
            "https://api.github.com/repos/opencode-ai/opencode/releases/latest" 2>/dev/null | \
            grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')
        
        if [ -z "$OPENCODE_VERSION" ]; then
            log_warn "Failed to fetch latest version from GitHub API"
            
            # Try parsing from releases page as fallback
            OPENCODE_VERSION=$(curl -sL --connect-timeout 5 --max-time 10 \
                "https://github.com/opencode-ai/opencode/releases/latest" 2>/dev/null | \
                grep -oE 'tag/v[0-9]+\.[0-9]+\.[0-9]+' | head -1 | sed 's/tag\///')
        fi
        
        if [ -z "$OPENCODE_VERSION" ]; then
            log_warn "Could not determine latest version, using fallback: v0.5.0"
            OPENCODE_VERSION="v0.5.0"
        fi
    fi
    log_info "Using opencode version: $OPENCODE_VERSION"
}

# Download and install opencode
install_opencode() {
    # First check if already installed somewhere
    if find_existing_opencode; then
        OPENCODE_BIN="$EXISTING_BIN"
        CURRENT_VERSION=$("$OPENCODE_BIN" --version 2>/dev/null | head -1 || echo "unknown")
        log_info "Version: $CURRENT_VERSION"
        
        if [ "$1" != "--force" ]; then
            log_info "Use --force to reinstall"
            return 0
        fi
        log_info "Forcing reinstall..."
    fi

    # Check target location
    if [ -f "$OPENCODE_BIN" ]; then
        CURRENT_VERSION=$("$OPENCODE_BIN" --version 2>/dev/null | head -1 || echo "unknown")
        log_info "opencode already installed: $CURRENT_VERSION"
        
        if [ "$1" != "--force" ]; then
            log_info "Use --force to reinstall"
            return 0
        fi
        log_info "Forcing reinstall..."
    fi

    detect_platform
    get_latest_version

    # Remove 'v' prefix if present for download URL
    VERSION_NUM="${OPENCODE_VERSION#v}"

    # Construct download URL
    FILENAME="opencode_${VERSION_NUM}_${OS}_${ARCH}"
    if [ "$OS" = "windows" ]; then
        FILENAME="${FILENAME}.zip"
    else
        FILENAME="${FILENAME}.tar.gz"
    fi

    DOWNLOAD_URL="https://github.com/opencode-ai/opencode/releases/download/${OPENCODE_VERSION}/${FILENAME}"

    log_info "Downloading from: $DOWNLOAD_URL"

    # Create install directory
    mkdir -p "$OPENCODE_INSTALL_DIR"

    # Download to temp file
    TEMP_DIR=$(mktemp -d)
    TEMP_FILE="$TEMP_DIR/$FILENAME"

    if command -v curl &> /dev/null; then
        curl -fsSL "$DOWNLOAD_URL" -o "$TEMP_FILE"
    elif command -v wget &> /dev/null; then
        wget -q "$DOWNLOAD_URL" -O "$TEMP_FILE"
    else
        log_error "Neither curl nor wget found. Please install one of them."
        exit 1
    fi

    # Extract
    log_info "Extracting..."
    if [ "$OS" = "windows" ]; then
        unzip -q "$TEMP_FILE" -d "$TEMP_DIR"
    else
        tar -xzf "$TEMP_FILE" -C "$TEMP_DIR"
    fi

    # Find and move binary
    if [ "$OS" = "windows" ]; then
        BINARY_NAME="opencode.exe"
    else
        BINARY_NAME="opencode"
    fi

    # Look for the binary in extracted files
    EXTRACTED_BIN=$(find "$TEMP_DIR" -name "$BINARY_NAME" -type f | head -1)
    if [ -z "$EXTRACTED_BIN" ]; then
        log_error "Binary not found in archive"
        rm -rf "$TEMP_DIR"
        exit 1
    fi

    mv "$EXTRACTED_BIN" "$OPENCODE_BIN"
    chmod +x "$OPENCODE_BIN"

    # Cleanup
    rm -rf "$TEMP_DIR"

    log_success "opencode installed to: $OPENCODE_BIN"
    
    # Verify installation
    if "$OPENCODE_BIN" --version &> /dev/null; then
        log_success "Verification: $($OPENCODE_BIN --version | head -1)"
    else
        log_warn "Binary installed but version check failed"
    fi
}

# Build MCP server
build_server() {
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    cd "$SCRIPT_DIR"

    if [ ! -f "go.mod" ]; then
        log_error "go.mod not found. Are you in the project directory?"
        exit 1
    fi

    log_info "Building MCP server..."
    
    # Check if Go is installed
    if ! command -v go &> /dev/null; then
        log_error "Go is not installed. Please install Go 1.22 or later."
        exit 1
    fi

    go build -o ./mcpserver ./cmd/mcpserver
    log_success "MCP server built: ./mcpserver"
}

# Start the server
start_server() {
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    cd "$SCRIPT_DIR"

    if [ ! -f "./mcpserver" ]; then
        build_server
    fi

    # Find opencode - check multiple locations
    OPENCODE_PATH=""
    
    # Check if we found it during install
    if [ -n "$EXISTING_BIN" ] && [ -x "$EXISTING_BIN" ]; then
        OPENCODE_PATH="$EXISTING_BIN"
    # Check configured install location
    elif [ -x "$OPENCODE_BIN" ]; then
        OPENCODE_PATH="$OPENCODE_BIN"
    # Check PATH (both names)
    elif command -v opencode &> /dev/null; then
        OPENCODE_PATH=$(command -v opencode)
    elif command -v opencode-cli &> /dev/null; then
        OPENCODE_PATH=$(command -v opencode-cli)
    # Check common locations
    else
        for path in "$HOME/.local/bin/opencode" "$HOME/.local/bin/opencode-cli" "/usr/local/bin/opencode" "/usr/local/bin/opencode-cli" "/usr/bin/opencode" "/usr/bin/opencode-cli" "$HOME/go/bin/opencode"; do
            if [ -x "$path" ]; then
                OPENCODE_PATH="$path"
                break
            fi
        done
    fi

    if [ -z "$OPENCODE_PATH" ]; then
        log_error "opencode not found. Run: $0 install"
        exit 1
    fi

    log_info "Starting MCP server..."
    log_info "  Address: $MCP_ADDR"
    log_info "  Timeout: ${MCP_TIMEOUT_SEC}s"
    log_info "  opencode: $OPENCODE_PATH"
    echo ""

    export MCP_ADDR
    export MCP_TARGET="$OPENCODE_PATH"
    export MCP_TIMEOUT_SEC

    exec ./mcpserver
}

# Show help
show_help() {
    echo "OpenCode MCP Server Starter"
    echo ""
    echo "Usage: $0 [command] [options]"
    echo ""
    echo "Commands:"
    echo "  install [--force]  Download and install opencode CLI"
    echo "  build              Build the MCP server"
    echo "  start              Start the MCP server (default)"
    echo "  run                Install (if needed), build, and start"
    echo "  help               Show this help message"
    echo ""
    echo "Environment Variables:"
    echo "  OPENCODE_VERSION      Version to install (default: latest)"
    echo "  OPENCODE_INSTALL_DIR  Install directory (default: ~/.local/bin)"
    echo "  MCP_ADDR              Server listen address (default: :9876)"
    echo "  MCP_TIMEOUT_SEC       Command timeout (default: 120)"
    echo ""
    echo "Examples:"
    echo "  $0 install                    # Install latest opencode"
    echo "  $0 install --force            # Force reinstall"
    echo "  OPENCODE_VERSION=v0.5.0 $0 install  # Install specific version"
    echo "  $0 run                        # Full setup and start"
    echo "  MCP_ADDR=:8080 $0 start       # Start on custom port"
}

# Main
main() {
    case "${1:-run}" in
        install)
            install_opencode "$2"
            ;;
        build)
            build_server
            ;;
        start)
            start_server
            ;;
        run)
            install_opencode
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
