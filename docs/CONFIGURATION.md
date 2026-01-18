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
    transport: stdio | http | sse
    
    # For stdio transport: command to execute
    command: npx
    
    # Command arguments
    args:
      - "-y"
      - "@anthropic/mcp-time"
    
    # Environment variables (supports ${VAR} expansion)
    env:
      API_KEY: "${MY_API_KEY}"
      DEBUG: "true"
    
    # For http/sse transport: server URL
    url: "http://localhost:8080/mcp"
    
    # HTTP headers for http transport
    headers:
      Authorization: "Bearer ${TOKEN}"
    
    # Start on daemon launch
    autostart: true
    
    # Restart policy: always, on-failure, never
    restart_policy: on-failure
    
    # Maximum restart attempts (default: 5)
    max_restarts: 5
    
    # Process-per-session mode for stateful servers
    stateful: false
    
    # Idle session timeout (default: 5m)
    session_timeout: 5m
    
    # Maximum concurrent sessions (default: 100)
    max_sessions: 100

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

| Transport | When to Use | Configuration |
|-----------|-------------|---------------|
| `stdio` | Most MCP servers (npx, uvx, binaries) | Set `command` and optional `args` |
| `http` | Native HTTP MCP servers | Set `url` pointing to `/mcp` endpoint |
| `sse` | Legacy SSE-based MCP servers | Set `url` to the SSE endpoint |

Transport is auto-detected:
- Has `command` → `stdio`
- Has `url` ending with `/mcp` → `http`
- Has `url` without `/mcp` → `sse`

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

#### Server with environment variables

```yaml
servers:
  context7:
    port: 6277
    command: npx
    args: ["-y", "@upstash/context7-mcp"]
    env:
      CONTEXT7_API_KEY: "${CONTEXT7_API_KEY}"
    autostart: true
```

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

#### Stateful server (process-per-session)

```yaml
servers:
  stateful-db:
    port: 6279
    command: npx
    args: ["-y", "@example/stateful-mcp"]
    stateful: true
    session_timeout: 10m
    max_sessions: 50
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

Generate with: `vision init --client opencode`

## Environment Variable Expansion

Vision supports environment variable expansion in configuration values:

```yaml
servers:
  my-server:
    env:
      # Simple expansion
      API_KEY: "${MY_API_KEY}"
      
      # With default value
      DEBUG: "${DEBUG:-false}"
      
      # Nested expansion
      URL: "https://${HOST}:${PORT:-8080}/api"
```

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
