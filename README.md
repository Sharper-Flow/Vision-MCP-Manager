# Vision

A Go-native MCP (Model Context Protocol) server daemon that provides centralized management of MCP servers for AI agents.

## Overview

Vision replaces complex multi-layer setups (Jarvis/MCPM) with a single Go binary that:

- **Manages MCP servers** via a centralized YAML registry
- **Bridges stdio to HTTP** - Converts stdio-based MCP servers to HTTP endpoints
- **Supervises processes** - Erlang-style process supervision with automatic restarts
- **Isolates sessions** - Process-per-session for stateful servers
- **Allocates ports** - Each server gets a dedicated port (6276-6300) for security isolation

## Quick Start

```bash
# Build
make build

# Add a server to the registry
./bin/vision server add time --command npx --args "-y @anthropic/mcp-time"

# Start the daemon
./bin/vision daemon start

# Initialize Claude Code config (global)
./bin/vision init --global

# Or for a specific project
cd /path/to/project
./bin/vision init
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

### Client Configuration

Vision generates client configs for AI agents:

**Claude Code** (`~/.claude.json` or `.claude/settings.json`):
```json
{
  "mcpServers": {
    "time": {
      "url": "http://localhost:6276/mcp",
      "transport": "streamable-http"
    },
    "context7": {
      "url": "http://localhost:6277/mcp",
      "transport": "streamable-http"
    }
  }
}
```

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      AI Agents                              │
│  (Claude Code, OpenCode, etc.)                              │
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

# Client configuration
vision init --global                    # Configure ~/.claude.json
vision init                             # Configure .claude/settings.json
vision init --servers time,context7     # Specific servers only
vision init --extend --servers extra    # Extend global config
vision init --client opencode           # For OpenCode instead

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
