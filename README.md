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
  context7:
    port: 6276
    command: npx
    args: ["-y", "@upstash/context7-mcp@latest"]
    env:
      CONTEXT7_API_KEY: "${CONTEXT7_API_KEY}"
    autostart: true

  kagimcp:
    port: 6279
    command: uvx
    args: ["kagimcp"]
    env:
      KAGI_API_KEY: "${KAGI_API_KEY}"
    autostart: true

  firecrawl:
    port: 6281
    command: npx
    args: ["-y", "firecrawl-mcp"]
    env:
      FIRECRAWL_API_KEY: "${FIRECRAWL_API_KEY}"
    autostart: true

  time:
    port: 6282
    command: uvx
    args: ["mcp-server-time", "--local-timezone=America/New_York"]
    autostart: true
```

### Environment Variables (`~/.config/vision/env`)

Store API keys and secrets separately from the server config:

```bash
CONTEXT7_API_KEY=your-key-here
KAGI_API_KEY=your-key-here
FIRECRAWL_API_KEY=your-key-here
```

### Client Configuration (OpenCode)

Vision generates OpenCode configs. OpenCode searches for config in:

1. `./.opencode.json` (project-local, highest priority)
2. `$XDG_CONFIG_HOME/opencode/opencode.json`
3. `$HOME/.opencode.json` (global)

```json
{
  "mcp": {
    "context7": {
      "type": "remote",
      "url": "http://localhost:6276/mcp",
      "enabled": true
    },
    "kagimcp": {
      "type": "remote",
      "url": "http://localhost:6279/mcp",
      "enabled": true
    },
    "firecrawl": {
      "type": "remote",
      "url": "http://localhost:6281/mcp",
      "enabled": true
    },
    "time": {
      "type": "remote",
      "url": "http://localhost:6282/mcp",
      "enabled": true
    }
  }
}
```

> **Note**: Vision exposes MCP servers via HTTP endpoints. Use `type: "remote"` with a `url` pointing to the server's allocated port.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      AI Agents                              │
│              (OpenCode, Claude Code, etc.)                  │
└─────────────────┬───────────────────────────────────────────┘
                  │ HTTP POST /mcp (JSON-RPC 2.0)
                  ▼
┌─────────────────────────────────────────────────────────────┐
│                   Vision Daemon                             │
│                                                             │
│  ┌─────────────────────────────────────────────────────┐   │
│  │            Admin MCP Server (:6275)                 │   │
│  │   vision_list, vision_status, vision_add, ...       │   │
│  └─────────────────────────────────────────────────────┘   │
│                                                             │
│  ┌─────────────────────────────────────────────────────┐   │
│  │         stdio-to-HTTP Bridge (PortManager)          │   │
│  │   :6276/mcp    :6279/mcp    :6280/mcp    ...        │   │
│  └────────┬────────────┬────────────┬──────────────────┘   │
│           │            │            │                      │
│  ┌────────▼────────────▼────────────▼──────────────────┐   │
│  │           Process Supervisor (suture)               │   │
│  │  ┌─────────┐  ┌─────────┐  ┌─────────┐              │   │
│  │  │context7 │  │ kagimcp │  │firecrawl│  ...         │   │
│  │  │ (stdio) │  │ (stdio) │  │ (stdio) │              │   │
│  │  └─────────┘  └─────────┘  └─────────┘              │   │
│  └─────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

### How the Bridge Works

1. **Subprocess Management**: Vision spawns MCP servers as subprocesses with stdio pipes
2. **HTTP Exposure**: Each server gets a dedicated HTTP port (6276+)
3. **Request Routing**: HTTP POST to `/mcp` is forwarded to the subprocess via stdin
4. **Response Handling**: JSON-RPC responses from stdout are returned to the HTTP client
5. **Health Checks**: Each port exposes `/health` for monitoring
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
