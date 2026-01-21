<p align="center">
  <img src="assets/header.png" alt="Vision" width="100%">
</p>

<p align="center">
  <strong>Unified MCP Server Management for AI Agents</strong>
</p>

<p align="center">
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.24+-00ADD8?style=flat&logo=go" alt="Go Version"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-blue.svg" alt="License"></a>
</p>

Vision is a Go-native daemon that centralizes [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) server management for AI coding agents like Claude Code, OpenCode, and Cursor. It replaces fragmented multi-tool setups with a single binary that handles process supervision, stdio-to-HTTP bridging, and configuration generation.

## Why Vision?

Managing MCP servers typically means juggling multiple configuration files, manually starting processes, and dealing with inconsistent transport protocols. Vision solves this by providing:

| Challenge | Vision's Solution |
|-----------|-------------------|
| **Scattered configs** | Single YAML registry (`~/.config/vision/servers.yaml`) |
| **Manual process management** | Erlang-style supervision with automatic restarts |
| **stdio-only servers** | HTTP bridge exposing each server on a dedicated port |
| **Per-project duplication** | Global config + project-local overrides |
| **Complex Jarvis/MCPM setups** | One-command migration path |

## Quick Start

```bash
# Install Vision
curl -fsSL https://raw.githubusercontent.com/Sharper-Flow/Vision-MCP-Manager/trunk/scripts/install.sh | bash

# Add your first MCP server
vision server add time --command uvx --args "mcp-server-time"

# Start the daemon
vision daemon start -d

# Generate client configuration
vision init --global
```

That's it. Your AI agent can now connect to `http://localhost:6282/mcp` for the time server.

## Installation

### One-Line Install (Recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/Sharper-Flow/Vision-MCP-Manager/trunk/scripts/install.sh | bash
```

The installer downloads the latest binary, places it in `/usr/local/bin`, and creates the configuration directory.

**Options:**
- `--migrate` — Automatically import servers from existing Jarvis/MCPM setups
- `--systemd` — Install and enable the systemd user service

### From Source

```bash
git clone https://github.com/Sharper-Flow/Vision-MCP-Manager.git
cd Vision-MCP-Manager
make build
sudo cp bin/vision /usr/local/bin/
```

**Requirements:** Go 1.24+ (uses `iter.Seq` from the MCP SDK)

### Running as a Service

For always-on operation, install the systemd user service:

```bash
# User service (recommended, no sudo required)
mkdir -p ~/.config/systemd/user
cp scripts/vision-user.service ~/.config/systemd/user/vision.service
systemctl --user daemon-reload
systemctl --user enable --now vision

# Check status
systemctl --user status vision
```

## Configuration

### Server Registry

Vision maintains a central registry of MCP servers at `~/.config/vision/servers.yaml`:

```yaml
servers:
  # Context7 - Library documentation lookup
  context7:
    port: 6276
    command: npx
    args: ["-y", "@upstash/context7-mcp@latest"]
    env:
      CONTEXT7_API_KEY: "${CONTEXT7_API_KEY}"
    autostart: true

  # Kagi - Web search and summarization
  kagimcp:
    port: 6279
    command: uvx
    args: ["kagimcp"]
    env:
      KAGI_API_KEY: "${KAGI_API_KEY}"
    autostart: true

  # Firecrawl - Web scraping and extraction
  firecrawl:
    port: 6281
    command: npx
    args: ["-y", "firecrawl-mcp"]
    env:
      FIRECRAWL_API_KEY: "${FIRECRAWL_API_KEY}"
    autostart: true

  # Time - Timezone and scheduling utilities
  time:
    port: 6282
    command: uvx
    args: ["mcp-server-time", "--local-timezone=America/New_York"]
    autostart: true
```

Each server entry specifies:
- **port** — Dedicated HTTP port (6276-6300 range)
- **command** — Executable (`npx`, `uvx`, or direct binary path)
- **args** — Command-line arguments
- **env** — Environment variables (supports `${VAR}` expansion from `~/.config/vision/env`)
- **autostart** — Whether to start with the daemon

### Environment Variables

Store secrets separately in `~/.config/vision/env`:

```bash
CONTEXT7_API_KEY=ctx7_xxxxxxxxxxxx
KAGI_API_KEY=xxxxxxxxxxxxxxxx
FIRECRAWL_API_KEY=fc-xxxxxxxxxxxxxxxx
```

This file is automatically loaded by Vision and never committed to version control.

### Client Configuration

Vision generates configuration for AI agents. For OpenCode:

```bash
# Global configuration (~/.opencode.json)
vision init --global

# Project-local configuration (./.opencode.json)
vision init

# Specific servers only
vision init --servers time,context7

# Extend global config with additional servers
vision init --extend --servers project-specific-server
```

Generated configuration looks like:

```json
{
  "mcp": {
    "context7": {
      "type": "remote",
      "url": "http://localhost:6276/mcp",
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

## Architecture

Vision operates as a central orchestrator between AI agents and MCP servers:

```
┌─────────────────────────────────────────────────────────────────┐
│                        AI Agents                                │
│            (Claude Code, OpenCode, Cursor, etc.)                │
└───────────────────────────┬─────────────────────────────────────┘
                            │ HTTP POST /mcp (JSON-RPC 2.0)
                            ▼
┌─────────────────────────────────────────────────────────────────┐
│                       Vision Daemon                             │
│                                                                 │
│   ┌─────────────────────────────────────────────────────────┐   │
│   │              Admin MCP Server (:6275)                   │   │
│   │    vision_list  vision_add  vision_status  vision_init  │   │
│   └─────────────────────────────────────────────────────────┘   │
│                                                                 │
│   ┌─────────────────────────────────────────────────────────┐   │
│   │              stdio-to-HTTP Bridge Layer                 │   │
│   │      :6276/mcp      :6279/mcp      :6281/mcp    ...     │   │
│   └────────────┬──────────────┬──────────────┬──────────────┘   │
│                │              │              │                  │
│   ┌────────────▼──────────────▼──────────────▼──────────────┐   │
│   │              Process Supervisor (suture)                │   │
│   │    ┌──────────┐   ┌──────────┐   ┌──────────┐           │   │
│   │    │ context7 │   │ kagimcp  │   │firecrawl │   ...     │   │
│   │    │  (stdio) │   │  (stdio) │   │  (stdio) │           │   │
│   │    └──────────┘   └──────────┘   └──────────┘           │   │
│   └─────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
```

**Key components:**

1. **Admin MCP Server** (port 6275) — Exposes Vision's management tools as MCP tools, allowing AI agents to add/remove servers, check status, and generate configs without leaving the conversation.

2. **stdio-to-HTTP Bridge** — Converts stdio-based MCP servers to HTTP endpoints. Each server receives a dedicated port, enabling parallel access and security isolation.

3. **Process Supervisor** — Uses [suture](https://github.com/thejerf/suture) for Erlang-style process supervision. Failed servers restart automatically with exponential backoff.

## CLI Reference

### Daemon Management

```bash
vision daemon start        # Start in foreground
vision daemon start -d     # Start in background (daemonize)
vision daemon stop         # Graceful shutdown
vision daemon status       # Show health, uptime, memory usage
vision daemon reload       # Hot-reload configuration (SIGHUP)
```

### Server Management

```bash
vision server add <name> --command <cmd> [--args <args>] [--env KEY=VAL]
vision server remove <name>
vision server list         # Show all servers with status
vision server info <name>  # Detailed server information
vision server start <name>
vision server stop <name>
vision server restart <name>
```

### Configuration Generation

```bash
vision init                             # Project-local config
vision init --global                    # Global config
vision init --client claude             # Target Claude Code format
vision init --servers time,context7     # Specific servers
vision init --extend --servers extra    # Extend existing config
```

### Migration

```bash
vision migrate --dry-run   # Preview changes without writing
vision migrate             # Import from Jarvis/MCPM
```

## Migrating from Jarvis/MCPM

If you're currently using Jarvis or MCPM for MCP server management, Vision provides automated migration:

```bash
# Preview what will be imported
vision migrate --dry-run

# Perform the migration
vision migrate
```

The migration:
- Converts JSON configs to Vision's YAML format
- Preserves server names, commands, and environment variables
- Assigns ports in the 6276-6300 range
- Creates a backup of your existing configuration

See [docs/MIGRATION.md](docs/MIGRATION.md) for detailed migration guidance.

## Admin MCP Tools

Vision exposes its own management interface as MCP tools on port 6275. This allows AI agents to manage servers without CLI access:

| Tool | Description |
|------|-------------|
| `vision_list` | List all servers with status (running/stopped/error) |
| `vision_add` | Add and optionally start a new server |
| `vision_remove` | Stop and remove a server |
| `vision_search` | Search the server catalog by name or capability |
| `vision_init` | Generate client configuration |
| `vision_status` | Daemon health, uptime, and memory stats |
| `vision_guidance` | Tool selection recommendations |

To use these tools, add Vision's admin server to your client config:

```json
{
  "mcp": {
    "vision": {
      "type": "remote",
      "url": "http://localhost:6275/mcp"
    }
  }
}
```

## Development

```bash
# Build the binary
make build

# Run tests with race detection
make test

# Run linter
make lint

# Run all checks (lint + test + build)
make all

# Cross-compile for all platforms
make dist

# Generate coverage report
make test-coverage
```

See [DEVELOPMENT.md](DEVELOPMENT.md) for architecture details and contribution guidelines.

## Documentation

- [Configuration Reference](docs/CONFIGURATION.md) — Complete server configuration options
- [Migration Guide](docs/MIGRATION.md) — Migrating from Jarvis/MCPM
- [MCP Transports](docs/MCP_TRANSPORTS.md) — Understanding stdio, HTTP, and SSE transports
- [Development Guide](DEVELOPMENT.md) — Building, testing, and contributing

## License

MIT License — see [LICENSE](LICENSE) for details.

---

<sub>Built with Go. Inspired by the need for simpler MCP server management.</sub>
