#!/usr/bin/env bash
#
# Vision MCP Daemon Installation Script
# https://github.com/Sharper-Flow/Vision-MCP-Manager
#
set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Defaults
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
CONFIG_DIR="${HOME}/.config/vision"
SYSTEMD_USER_DIR="${HOME}/.config/systemd/user"
VERSION="${VERSION:-latest}"
REPO="Sharper-Flow/Vision-MCP-Manager"

log_info() { echo -e "${BLUE}[INFO]${NC} $*"; }
log_success() { echo -e "${GREEN}[OK]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }

resolved_service_path() {
    local extra_path="$1"
    local base_path="${INSTALL_DIR}:/usr/local/bin:/usr/bin:/bin"
    local combined="${extra_path}:${base_path}"
    local deduped=""
    local part

    IFS=':' read -r -a path_parts <<< "$combined"
    for part in "${path_parts[@]}"; do
        [[ -z "$part" ]] && continue
        case ":${deduped}:" in
            *":${part}:"*) continue ;;
        esac
        if [[ -z "$deduped" ]]; then
            deduped="$part"
        else
            deduped="${deduped}:$part"
        fi
    done

    printf '%s' "$deduped"
}

configure_service_path() {
    local service_path="$1"
    local current_path
    current_path=$(resolved_service_path "$PATH")

    python3 - "$service_path" "$current_path" <<'PY'
from pathlib import Path
import sys

service_file = Path(sys.argv[1])
path_value = sys.argv[2]
content = service_file.read_text()
content = content.replace('__VISION_PATH__', path_value)
service_file.write_text(content)
PY
}

usage() {
    cat <<EOF
Vision MCP Daemon Installer

Usage: $0 [OPTIONS]

Options:
  --system          Install system-wide (requires sudo)
  --user            Install for current user only (default)
  --no-service      Skip systemd service installation
  --migrate         Migrate from Jarvis/MCPM after install
  --opencode        Configure OpenCode to use Vision MCP servers
  --version VER     Install specific version (default: latest)
  --help            Show this help message

Examples:
  $0                    # Install for current user
  $0 --system           # Install system-wide
  $0 --migrate          # Install and migrate from Jarvis/MCPM
  $0 --opencode         # Install and configure OpenCode integration
EOF
}

detect_platform() {
    local os arch
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    arch=$(uname -m)
    
    case "$arch" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *) log_error "Unsupported architecture: $arch"; exit 1 ;;
    esac
    
    case "$os" in
        linux) os="linux" ;;
        darwin) os="darwin" ;;
        *) log_error "Unsupported OS: $os"; exit 1 ;;
    esac
    
    echo "${os}_${arch}"
}

get_latest_version() {
    curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | \
        grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/'
}

download_binary() {
    local platform="$1"
    local version="$2"
    local dest="$3"
    
    if [[ "$version" == "latest" ]]; then
        version=$(get_latest_version)
        log_info "Latest version: $version"
    fi
    
    local url="https://github.com/${REPO}/releases/download/${version}/vision_${platform}.tar.gz"
    local tmpdir=$(mktemp -d)
    
    log_info "Downloading Vision ${version} for ${platform}..."
    
    if ! curl -fsSL "$url" -o "${tmpdir}/vision.tar.gz"; then
        log_error "Failed to download from $url"
        rm -rf "$tmpdir"
        exit 1
    fi
    
    tar -xzf "${tmpdir}/vision.tar.gz" -C "$tmpdir"
    
    mkdir -p "$(dirname "$dest")"
    mv "${tmpdir}/vision" "$dest"
    chmod +x "$dest"
    
    rm -rf "$tmpdir"
    log_success "Binary installed to $dest"
}

build_from_source() {
    local dest="$1"
    
    log_info "Building from source..."
    
    if ! command -v go &>/dev/null; then
        log_error "Go is required to build from source. Install Go or use a pre-built release."
        exit 1
    fi
    
    local script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    local project_dir="$(dirname "$script_dir")"
    
    cd "$project_dir"
    
    go build -o "$dest" ./cmd/vision
    chmod +x "$dest"
    
    log_success "Built and installed to $dest"
}

install_config() {
    local config_file="${CONFIG_DIR}/servers.yaml"
    
    mkdir -p "$CONFIG_DIR"
    
    if [[ -f "$config_file" ]]; then
        log_warn "Config file already exists: $config_file"
        return
    fi
    
    log_info "Creating default config at $config_file"
    
    cat > "$config_file" <<'YAML'
# Vision MCP Server Configuration
# See: https://github.com/Sharper-Flow/Vision-MCP-Manager

supervision:
  shutdown_timeout: 30s
  restart_delay: 1s
  max_restart_delay: 5m
  health_check_interval: 30s

servers:
  # Example: Time server
  # time:
  #   port: 6276
  #   command: npx
  #   args: ["-y", "@anthropic/mcp-time"]
  #   autostart: true
  
  # Example: Context7 documentation server
  # context7:
  #   port: 6277
  #   command: npx
  #   args: ["-y", "@context7/mcp-server"]
  #   env:
  #     CONTEXT7_API_KEY: "${CONTEXT7_API_KEY}"
  #   autostart: true
YAML
    
    log_success "Created default config"
}

install_aux_scripts() {
    local script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    local dest_dir="$INSTALL_DIR"
    local src="$script_dir/vision-clean"
    local dest="$dest_dir/vision-clean"

    if [[ ! -f "$src" ]]; then
        log_warn "Aux script not found: $src"
        return
    fi

    mkdir -p "$dest_dir"
    chmod +x "$src"
    ln -sfn "$src" "$dest"
    log_success "Installed auxiliary script to $dest"
}

install_systemd_user() {
    local script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    local service_file="${script_dir}/vision-user.service"
    
    if [[ ! -f "$service_file" ]]; then
        log_warn "User service file not found: $service_file"
        return
    fi
    
    mkdir -p "$SYSTEMD_USER_DIR"
    cp "$service_file" "${SYSTEMD_USER_DIR}/vision.service"
    
    # Update path to binary
    sed -i "s|%h/.local/bin/vision|${INSTALL_DIR}/vision|g" "${SYSTEMD_USER_DIR}/vision.service"
    configure_service_path "${SYSTEMD_USER_DIR}/vision.service"
    
    systemctl --user daemon-reload
    
    log_success "Systemd user service installed"
    log_info "To enable: systemctl --user enable vision"
    log_info "To start:  systemctl --user start vision"
}

install_systemd_system() {
    local script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    local service_file="${script_dir}/vision.service"
    
    if [[ ! -f "$service_file" ]]; then
        log_warn "System service file not found: $service_file"
        return
    fi
    
    sudo cp "$service_file" /etc/systemd/system/vision@.service
    local current_path
    current_path=$(resolved_service_path "$PATH")
    sudo python3 - /etc/systemd/system/vision@.service "$current_path" <<'PY'
from pathlib import Path
import sys

service_file = Path(sys.argv[1])
path_value = sys.argv[2]
content = service_file.read_text()
content = content.replace('__VISION_PATH__', path_value)
service_file.write_text(content)
PY
    sudo systemctl daemon-reload
    
    log_success "Systemd system service installed"
    log_info "To enable: sudo systemctl enable vision@${USER}"
    log_info "To start:  sudo systemctl start vision@${USER}"
}

migrate_from_mcpm() {
    log_info "Checking for Jarvis/MCPM configuration..."
    
    local mcpm_servers="${HOME}/.mcpm/servers.json"
    
    if [[ ! -f "$mcpm_servers" ]]; then
        log_warn "No MCPM servers.json found at $mcpm_servers"
        log_info "Skipping migration"
        return
    fi
    
    log_info "Found MCPM config. Running migration..."
    
    if command -v vision &>/dev/null; then
        vision migrate --dry-run
        
        read -p "Proceed with migration? [y/N] " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            vision migrate
            log_success "Migration complete"
        else
            log_info "Migration skipped"
        fi
    else
        log_warn "Vision binary not in PATH. Run migration manually after install."
    fi
}

configure_opencode() {
    log_info "Configuring OpenCode integration..."
    
    local opencode_config="${HOME}/.config/opencode/opencode.json"
    local opencode_dir="${HOME}/.config/opencode"
    
    # Create directory if needed
    mkdir -p "$opencode_dir"
    
    # Check if config exists
    if [[ -f "$opencode_config" ]]; then
        # Config exists - check if jq is available for merging
        if command -v jq &>/dev/null; then
            log_info "Merging Vision MCP servers into existing OpenCode config..."
            
            # Create temp file with Vision MCP config
            local vision_mcp='{
                "vision": {"type": "remote", "url": "http://localhost:6275/mcp", "enabled": true}
            }'
            
            # Merge into existing config
            local tmp_config=$(mktemp)
            jq --argjson vision "$vision_mcp" '.mcp = (.mcp // {}) + $vision' "$opencode_config" > "$tmp_config"
            mv "$tmp_config" "$opencode_config"
            
            log_success "Added Vision admin server to OpenCode config"
            log_info "Note: Add individual servers (context7, kagimcp, etc.) based on your servers.yaml"
        else
            log_warn "jq not found - cannot merge config automatically"
            log_info "Add the following to your OpenCode config manually:"
            echo '    "vision": {"type": "remote", "url": "http://localhost:6275/mcp", "enabled": true}'
        fi
    else
        # Create new config with Vision
        log_info "Creating new OpenCode config with Vision..."
        cat > "$opencode_config" <<'JSON'
{
  "mcp": {
    "vision": {
      "type": "remote",
      "url": "http://localhost:6275/mcp",
      "enabled": true
    }
  }
}
JSON
        log_success "Created OpenCode config with Vision admin server"
    fi
    
    log_info "Vision admin tools available: vision_list, vision_add, vision_remove, vision_status"
    log_info "Use vision_list to see available MCP servers and their ports"
}

main() {
    local system_install=false
    local skip_service=false
    local do_migrate=false
    local from_source=false
    local configure_oc=false
    
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --system) system_install=true; shift ;;
            --user) system_install=false; shift ;;
            --no-service) skip_service=true; shift ;;
            --migrate) do_migrate=true; shift ;;
            --opencode) configure_oc=true; shift ;;
            --version) VERSION="$2"; shift 2 ;;
            --from-source) from_source=true; shift ;;
            --help) usage; exit 0 ;;
            *) log_error "Unknown option: $1"; usage; exit 1 ;;
        esac
    done
    
    echo ""
    echo "========================================"
    echo "  Vision MCP Daemon Installer"
    echo "========================================"
    echo ""
    
    local platform=$(detect_platform)
    log_info "Detected platform: $platform"
    
    # Set install directory based on mode
    if $system_install; then
        INSTALL_DIR="/usr/local/bin"
        log_info "Installing system-wide to $INSTALL_DIR"
    else
        INSTALL_DIR="${HOME}/.local/bin"
        log_info "Installing for user to $INSTALL_DIR"
    fi
    
    # Install binary
    if $from_source; then
        build_from_source "${INSTALL_DIR}/vision"
    else
        download_binary "$platform" "$VERSION" "${INSTALL_DIR}/vision"
    fi
    
    # Install default config
    install_config

    # Install helper scripts
    install_aux_scripts
    
    # Install systemd service
    if ! $skip_service; then
        if $system_install; then
            install_systemd_system
        else
            install_systemd_user
        fi
    fi
    
    # Add to PATH reminder
    if [[ ":$PATH:" != *":${INSTALL_DIR}:"* ]]; then
        log_warn "${INSTALL_DIR} is not in your PATH"
        log_info "Add this to your shell profile:"
        echo "    export PATH=\"\${PATH}:${INSTALL_DIR}\""
    fi
    
    # Migration
    if $do_migrate; then
        migrate_from_mcpm
    fi
    
    # OpenCode configuration
    if $configure_oc; then
        configure_opencode
    fi
    
    echo ""
    log_success "Installation complete!"
    echo ""
    echo "Next steps:"
    echo "  1. Edit config: $CONFIG_DIR/servers.yaml"
    echo "  2. Start daemon: vision daemon start"
    echo "  3. Check status: vision daemon status"
    echo ""
}

main "$@"
