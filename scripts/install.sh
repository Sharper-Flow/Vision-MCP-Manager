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
  --opencode        Configure OpenCode to use Vision MCP servers
  --version VER     Install specific version (default: latest)
  --help            Show this help message

Examples:
  $0                    # Install for current user
  $0 --system           # Install system-wide
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

configure_opencode() {
    log_info "Configuring OpenCode integration..."
    
    local opencode_dir="${HOME}/.config/opencode"
    local opencode_jsonc="${opencode_dir}/opencode.jsonc"
    local opencode_json="${opencode_dir}/opencode.json"
    
    # Create directory if needed
    mkdir -p "$opencode_dir"
    
    if [[ -e "$opencode_jsonc" && -e "$opencode_json" ]]; then
        log_error "Ambiguous OpenCode configuration: both existing config files are present: $opencode_jsonc and $opencode_json"
        return 1
    fi

    local existing_config=""
    if [[ -e "$opencode_jsonc" ]]; then
        existing_config="$opencode_jsonc"
    elif [[ -e "$opencode_json" ]]; then
        existing_config="$opencode_json"
    fi

    if [[ -n "$existing_config" ]]; then
        log_warn "Existing config found at $existing_config; leaving it unchanged."
        log_info "Add the Vision MCP declaration manually to the existing config."
        log_info "Call the Vision Admin MCP tool vision_init with path: $existing_config, then restart OpenCode."
        return 0
    fi

    log_info "Creating new OpenCode config with Vision..."
    (
        local tmp_config
        tmp_config=$(mktemp "${opencode_dir}/.opencode.jsonc.XXXXXX")
        cleanup_tmp_config() {
            rm -f -- "$tmp_config"
        }
        trap cleanup_tmp_config EXIT HUP INT TERM
        chmod 600 "$tmp_config"
        cat > "$tmp_config" <<'JSON'
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "vision": {
      "type": "remote",
      "url": "http://localhost:6275/mcp",
      "enabled": true
    }
  }
}
JSON
        mv -f -- "$tmp_config" "$opencode_jsonc"
        trap - EXIT HUP INT TERM
    )
    log_success "Created OpenCode config with Vision admin server at $opencode_jsonc"
    log_info "Restart OpenCode to load the new configuration."
    
    log_info "Vision admin tools available: vision_list, vision_add, vision_remove, vision_status"
    log_info "Use vision_list to see available MCP servers and their ports"
}

main() {
    local system_install=false
    local skip_service=false
    local from_source=false
    local configure_oc=false
    
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --system) system_install=true; shift ;;
            --user) system_install=false; shift ;;
            --no-service) skip_service=true; shift ;;
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

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi
