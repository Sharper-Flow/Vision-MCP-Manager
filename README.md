# Vision

A Go-native MCP (Model Context Protocol) server daemon that provides centralized management of MCP servers for AI agents.

## Overview

Vision replaces complex multi-layer setups (Jarvis/MCPM) with a single Go binary that:

- **Manages MCP servers** via a centralized YAML registry
- **Bridges stdio to HTTP** - Converts stdio-based MCP servers to HTTP endpoints
- **Supervises processes** - Erlang-style process supervision with automatic restarts
- **Isolates sessions** - Process-per-session for stateful servers
- **Allocates ports** - Each server gets a dedicated port (6276-6300) for security isolation

## Installation

### Quick Install (Recommended)

```bash
# Download and run the installer
curl -fsSL https://raw.githubusercontent.com/Sharper-Flow/Vision-MCP-Manager/trunk/scripts/install.sh | bash

# Or with options
curl -fsSL https://raw.githubusercontent.com/Sharper-Flow/Vision-MCP-Manager/trunk/scripts/install.sh | bash -s -- --migrate
```

### From Source

```bash
git clone https://github.com/Sharper-Flow/Vision-MCP-Manager.git
cd Vision-MCP-Manager
make build
sudo cp bin/vision /usr/local/bin/
```

### Systemd Service (Optional)

```bash
# User service (no sudo)
cp scripts/vision-user.service ~/.config/systemd/user/vision.service
systemctl --user daemon-reload
systemctl --user enable --now vision

# System service (requires sudo)
sudo cp scripts/vision.service /etc/systemd/system/vision@.service
sudo systemctl daemon-reload
sudo systemctl enable --now vision@$USER
```

## Quick Start

```bash
# Add a server to the registry
vision server add time --command npx --args "-y @anthropic/mcp-time"

# Start the daemon
vision daemon start

# Initialize OpenCode/Claude Code config (global)
vision init --global

# Or for a specific project
cd /path/to/project
vision init
```

### Migrating from Jarvis/MCPM

```bash
# Preview what would be migrated
vision migrate --dry-run

# Perform migration
vision migrate

# Or use the standalone script
./scripts/migrate-from-mcpm.sh --dry-run
```

## Configuration

### Server Registry (`~/.config/vision/servers.yaml`)

```yaml
servers:
  time:
    port: 6276
    command: npx
    args: ["-y", "@anthropic/mcp-time"]
    autostart: true
    
  context7:
    port: 6277
    command: npx
    args: ["-y", "@upstash/context7-mcp"]
    env:
      CONTEXT7_API_KEY: "${CONTEXT7_API_KEY}"
    autostart: true
```

### Client Configuration (OpenCode)

Vision generates OpenCode configs. OpenCode searches for `.opencode.json` in:

1. `./.opencode.json` (project-local, highest priority)
2. `$XDG_CONFIG_HOME/opencode/.opencode.json`
3. `$HOME/.opencode.json` (global)

```json
{
  "mcpServers": {
    "time": {
      "type": "sse",
      "url": "http://localhost:6276/mcp"
    },
    "context7": {
      "type": "sse",
      "url": "http://localhost:6277/mcp"
    }
  }
}
```

> **Note**: Vision exposes MCP servers via HTTP/SSE endpoints, so use `type: "sse"` with a `url`.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      AI Agents                              │
│                      (OpenCode)                             │
└─────────────────┬───────────────────────────────────────────┘
                  │ HTTP (Streamable HTTP transport)
                  ▼
┌─────────────────────────────────────────────────────────────┐
│                   Vision Daemon                             │
│  ┌─────────────────────────────────────────────────────┐    │
│  │              HTTP Bridge Layer                      │    │
│  │   :6276/mcp    :6277/mcp    :6278/mcp    ...       │    │
│  └────────┬────────────┬────────────┬──────────────────┘    │
│           │            │            │                       │
│  ┌────────▼────────────▼────────────▼──────────────────┐    │
│  │           Process Supervisor (suture)               │    │
│  │  ┌─────────┐  ┌─────────┐  ┌─────────┐              │    │
│  │  │  time   │  │context7 │  │  ...    │              │    │
│  │  │ (stdio) │  │ (stdio) │  │ (stdio) │              │    │
│  │  └─────────┘  └─────────┘  └─────────┘              │    │
│  └─────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────┘
```

## CLI Commands

```bash
# Daemon management
vision daemon start        # Start the daemon (foreground)
vision daemon start -d     # Start the daemon (background)
vision daemon stop         # Stop the daemon
vision daemon status       # Show daemon status

# Server management
vision server add <name> --command <cmd> [--args <args>] [--env KEY=VAL]
vision server remove <name>
vision server list
vision server info <name>
vision server start <name>
vision server stop <name>
vision server restart <name>

# Client configuration (OpenCode)
vision init --global                    # Configure ~/.opencode.json (global)
vision init                             # Configure ./.opencode.json (project)
vision init --servers time,context7     # Specific servers only
vision init --extend --servers extra    # Extend global config

# Migration (from Jarvis/MCPM)
vision migrate --dry-run   # Preview migration
vision migrate             # Perform migration
```

## Development

```bash
# Build
make build

# Run tests
make test

# Run linter
make lint

# Run all checks
make all

# Cross-compile
make dist
```

## Requirements

- Go 1.24+ (for `iter.Seq` in MCP SDK)
- MCP servers must be installed via `npx`, `uvx`, or direct binaries

## License

MIT
