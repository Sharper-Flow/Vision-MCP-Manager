# Vision Configuration Reference

This document provides a complete reference for Vision's configuration files.

## Configuration Files

Vision uses two types of configuration:

1. **Server Registry** (`~/.config/vision/servers.yaml`) - Machine-wide server definitions
2. **Client Configs** (`.opencode.json`, `.claude/settings.json`) - Per-project or global client settings

## Server Registry

### Location

The server registry is located at `~/.config/vision/servers.yaml` by default.

You can override this with:
- `VISION_CONFIG` environment variable
- `--config` CLI flag

### Schema

```yaml
# Server definitions
servers:
  <server-name>:
    # HTTP port for this server (required, range: 6276-6300)
    port: 6276
    
    # Transport type (auto-detected if not specified)
    transport: stdio
    
    # For stdio transport: command to execute
    command: npx
    
    # Command arguments
    args:
      - "-y"
      - "@anthropic/mcp-time"
    
    # Environment variables passed to the server process
    env:
      API_KEY: "your-api-key-here"
      DEBUG: "true"
    
    # Start on daemon launch
    autostart: true
    
    # Restart policy: always, on-failure, never
    restart_policy: on-failure
    
    # Maximum restart attempts (default: 5)
    max_restarts: 5
    
    # Idle session timeout (default: 5m).
    # When a session is reaped, the next tool call automatically
    # respawns a fresh subprocess — no error is returned to the agent.
    # Increase for servers where agents pause between calls (e.g. 30m).
    session_timeout: 5m
    
    # Absolute session TTL (default: 0, no limit)
    session_ttl: 1h
    
    # Maximum concurrent sessions per server (default: 100)
    max_sessions: 100

# Security settings (applied to all Streamable HTTP endpoints)
security:
  # Bearer token for authentication (optional)
  bearer_token: "your-secret-token"
  
  # Allowed origins for CORS (optional, wildcards rejected)
  allowed_origins:
    - "http://localhost:3000"

# Global supervision settings
supervision:
  # Health check interval (default: 30s)
  health_check_interval: 30s
  
  # Graceful shutdown timeout (default: 10s)
  shutdown_timeout: 10s
  
  # Initial restart delay (default: 1s)
  restart_delay: 1s
  
  # Maximum restart delay (default: 60s)
  max_restart_delay: 60s
```

### Transport Types

Vision uses **Streamable HTTP** as the upstream transport for all servers. Downstream, stdio subprocesses are managed with per-session isolation.

| Transport | When to Use | Configuration |
|-----------|-------------|---------------|
| `stdio` | Most MCP servers (npx, uvx, binaries) | Set `command` and optional `args` |

Transport is auto-detected:
- Has `command` → `stdio` (exposed as Streamable HTTP on the configured port)

> **Note:** Each client session spawns an isolated subprocess. There is no shared state between sessions.

### Example Configurations

#### Basic stdio server (most common)

```yaml
servers:
  time:
    port: 6276
    command: npx
    args: ["-y", "@anthropic/mcp-time"]
    autostart: true
```

#### Server with API key

```yaml
servers:
  context7:
    port: 6277
    command: npx
    args: ["-y", "@upstash/context7-mcp", "--api-key", "your-api-key-here"]
    autostart: true
```

> **Best Practice:** Put API keys directly in `servers.yaml`. This file is local to your machine (`~/.config/vision/servers.yaml`) and is never committed to version control. Hardcoding keys avoids environment variable resolution issues and ensures the daemon always has access to credentials regardless of how it was started.
>
> Environment variable expansion (`${CONTEXT7_API_KEY}`) is supported but not recommended—keys may fail to resolve depending on how the daemon is launched.

#### Native HTTP server (proxy mode)

```yaml
servers:
  remote-api:
    port: 6278
    transport: http
    url: "http://internal-server:8080/mcp"
    headers:
      Authorization: "Bearer ${API_TOKEN}"
```

#### Session limits

```yaml
servers:
  expensive-server:
    port: 6279
    command: npx
    args: ["-y", "@example/expensive-mcp"]
    max_sessions: 5
    session_timeout: 10m
    session_ttl: 1h
```

## Client Configurations

### Claude Code

Create `.claude/settings.json` in your project or `~/.config/Claude/Claude.json` globally:

```json
{
  "mcpServers": {
    "time": {
      "type": "streamable-http",
      "url": "http://localhost:6276/mcp"
    },
    "context7": {
      "type": "streamable-http",
      "url": "http://localhost:6277/mcp"
    }
  }
}
```

Generate with: `vision init --client claude-code`

### OpenCode

Create `.opencode.json` in your project or `~/.opencode.json` globally:

```json
{
  "mcp": {
    "time": {
      "type": "remote",
      "url": "http://localhost:6276/mcp"
    },
    "context7": {
      "type": "remote",
      "url": "http://localhost:6277/mcp"
    }
  }
}
```

Generate with: `vision init --client opencode`

## Environment Variable Expansion

Vision supports environment variable expansion for values already set in your shell environment:

```yaml
servers:
  my-server:
    env:
      # Expand from shell environment
      API_KEY: "${MY_API_KEY}"
      
      # With default value if not set
      DEBUG: "${DEBUG:-false}"
```

However, the simplest approach is to put API keys directly in the config file since `~/.config/vision/servers.yaml` is never committed to version control.

## Validation

Validate your configuration:

```bash
vision config validate
```

This checks:
- YAML syntax
- Required fields
- Port range (6276-6300)
- No port conflicts
- Transport configuration consistency

## Hot Reload

Vision supports configuration hot reload:

```bash
# Send SIGHUP to reload
vision daemon reload

# Or send signal directly
kill -HUP $(cat /tmp/vision.pid)
```

On reload, Vision:
1. Re-reads `servers.yaml`
2. Starts new servers
3. Stops removed servers
4. Updates changed servers (stops then starts)

## Port Allocation

Vision allocates ports 6276-6300 for MCP servers:

| Port | Purpose |
|------|---------|
| 6275 | Management API |
| 6276-6300 | MCP servers |

Each server MUST have a unique port. The CLI will automatically allocate the next available port when adding servers.
