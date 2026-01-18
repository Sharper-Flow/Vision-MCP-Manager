#!/usr/bin/env bash
#
# Migration Script: Jarvis/MCPM -> Vision
# Converts MCPM servers.json to Vision servers.yaml
#
set -euo pipefail

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${BLUE}[INFO]${NC} $*"; }
log_success() { echo -e "${GREEN}[OK]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }

# Paths
MCPM_CONFIG_DIR="${MCPM_CONFIG_DIR:-$HOME/.mcpm}"
MCPM_SERVERS="${MCPM_CONFIG_DIR}/servers.json"
MCPM_PROFILES="${MCPM_CONFIG_DIR}/profiles.json"

VISION_CONFIG_DIR="${VISION_CONFIG_DIR:-$HOME/.config/vision}"
VISION_SERVERS="${VISION_CONFIG_DIR}/servers.yaml"

DRY_RUN=false

usage() {
    cat <<EOF
Migration Script: Jarvis/MCPM -> Vision

Converts MCPM servers.json configuration to Vision servers.yaml format.

Usage: $0 [OPTIONS]

Options:
  --dry-run         Show what would be migrated without making changes
  --mcpm-dir DIR    MCPM config directory (default: ~/.mcpm)
  --vision-dir DIR  Vision config directory (default: ~/.config/vision)
  --help            Show this help

Examples:
  $0 --dry-run      # Preview migration
  $0                # Perform migration
EOF
}

check_requirements() {
    if ! command -v jq &>/dev/null; then
        log_error "jq is required but not installed"
        log_info "Install with: sudo apt install jq (Debian/Ubuntu)"
        log_info "           or: brew install jq (macOS)"
        exit 1
    fi
}

backup_existing() {
    if [[ -f "$VISION_SERVERS" ]]; then
        local backup="${VISION_SERVERS}.backup.$(date +%Y%m%d_%H%M%S)"
        log_info "Backing up existing Vision config to $backup"
        if ! $DRY_RUN; then
            cp "$VISION_SERVERS" "$backup"
        fi
    fi
}

# Convert MCPM server entry to Vision YAML format
# Arguments: server_name, port, server_json
convert_server() {
    local name="$1"
    local port="$2"
    local server_json="$3"
    
    local command args env autostart
    
    command=$(echo "$server_json" | jq -r '.command // empty')
    
    # Handle args array
    args=$(echo "$server_json" | jq -r '.args // [] | @json')
    
    # Handle env object
    env=$(echo "$server_json" | jq -r '.env // {} | to_entries | .[] | "      \(.key): \"\(.value)\""')
    
    # Determine autostart from profile_tags or default to true
    autostart="true"
    
    echo "  ${name}:"
    echo "    port: ${port}"
    echo "    command: ${command}"
    
    # Format args
    if [[ "$args" != "[]" ]]; then
        echo "    args: ${args}"
    fi
    
    # Format env if present
    if [[ -n "$env" ]]; then
        echo "    env:"
        echo "$env"
    fi
    
    echo "    autostart: ${autostart}"
    echo ""
}

migrate() {
    log_info "Starting migration from Jarvis/MCPM to Vision"
    
    if [[ ! -f "$MCPM_SERVERS" ]]; then
        log_error "MCPM servers.json not found at $MCPM_SERVERS"
        exit 1
    fi
    
    log_info "Reading MCPM config from $MCPM_SERVERS"
    
    # Parse MCPM servers
    local servers
    servers=$(jq -r '.servers // {}' "$MCPM_SERVERS")
    
    if [[ "$servers" == "{}" || -z "$servers" ]]; then
        log_warn "No servers found in MCPM config"
        exit 0
    fi
    
    local server_names
    server_names=$(echo "$servers" | jq -r 'keys[]')
    local count=$(echo "$server_names" | wc -l)
    
    log_info "Found $count servers to migrate"
    
    # Backup existing Vision config
    backup_existing
    
    # Create Vision config directory
    if ! $DRY_RUN; then
        mkdir -p "$VISION_CONFIG_DIR"
    fi
    
    # Build Vision config
    local vision_config=""
    vision_config+="# Vision MCP Server Configuration\n"
    vision_config+="# Migrated from Jarvis/MCPM on $(date)\n"
    vision_config+="# Original: ${MCPM_SERVERS}\n"
    vision_config+="\n"
    vision_config+="supervision:\n"
    vision_config+="  shutdown_timeout: 30s\n"
    vision_config+="  restart_delay: 1s\n"
    vision_config+="  max_restart_delay: 5m\n"
    vision_config+="  health_check_interval: 30s\n"
    vision_config+="\n"
    vision_config+="servers:\n"
    
    # Assign ports starting at 6276
    local port=6276
    
    while IFS= read -r name; do
        local server_json
        server_json=$(echo "$servers" | jq --arg name "$name" '.[$name]')
        
        log_info "  Migrating: $name -> port $port"
        
        local server_yaml
        server_yaml=$(convert_server "$name" "$port" "$server_json")
        vision_config+="$server_yaml"
        
        ((port++))
    done <<< "$server_names"
    
    # Output or write
    if $DRY_RUN; then
        echo ""
        echo "========== DRY RUN: Would write to $VISION_SERVERS =========="
        echo -e "$vision_config"
        echo "============================================================="
    else
        echo -e "$vision_config" > "$VISION_SERVERS"
        log_success "Vision config written to $VISION_SERVERS"
    fi
    
    # Migration summary
    echo ""
    log_success "Migration complete!"
    echo ""
    echo "Next steps:"
    echo "  1. Review config: cat $VISION_SERVERS"
    echo "  2. Stop Jarvis:   docker compose -f ~/dev/MCP/docker-compose.yaml down"
    echo "  3. Start Vision:  vision daemon start"
    echo ""
    
    if [[ -f "$MCPM_PROFILES" ]]; then
        log_warn "Note: Vision does not use profiles. Each server runs independently."
        log_info "Your MCPM profiles were: $(jq -r 'keys | join(", ")' "$MCPM_PROFILES" 2>/dev/null || echo "unknown")"
    fi
}

main() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --dry-run) DRY_RUN=true; shift ;;
            --mcpm-dir) MCPM_CONFIG_DIR="$2"; MCPM_SERVERS="${MCPM_CONFIG_DIR}/servers.json"; MCPM_PROFILES="${MCPM_CONFIG_DIR}/profiles.json"; shift 2 ;;
            --vision-dir) VISION_CONFIG_DIR="$2"; VISION_SERVERS="${VISION_CONFIG_DIR}/servers.yaml"; shift 2 ;;
            --help) usage; exit 0 ;;
            *) log_error "Unknown option: $1"; usage; exit 1 ;;
        esac
    done
    
    check_requirements
    migrate
}

main "$@"
