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

    # Optional stronger resilience defaults for upstream network-backed tools.
    # Current values: networked
    availability_profile: networked

    # Explicitly safe read-only tools that may share results across sessions.
    shared_read_only_tools:
      - kagi_search_fetch
      - kagi_summarizer

    # Cache successful shared read-only results for this long (default: 10s for networked)
    shared_result_cache_ttl: 10s

    # Max cached shared results per server (default: 128 for networked)
    shared_result_cache_size: 128

    # Max concurrent downstream tool calls per server (default: 4 for networked)
    max_in_flight_requests: 4

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
  kagi:
    port: 6279
    command: uvx
    args: ["kagimcp"]
    autostart: true
    availability_profile: networked
    shared_read_only_tools: ["kagi_search_fetch", "kagi_summarizer"]
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

`vision_init` now reconciles existing OpenCode JSON instead of blindly replacing it, so stale Vision-managed MCP endpoint mappings can be repaired while preserving unrelated configuration keys.

## Environment Variable Expansion

Vision supports `${VAR}` and `${VAR:-default}` expansion in `servers.yaml`:

```yaml
servers:
  my-server:
    env:
      # Expand from .env file or shell environment
      API_KEY: "${MY_API_KEY}"
      
      # With default value if not set
      DEBUG: "${DEBUG:-false}"
```

### Secrets Management

Store API keys and tokens in `~/.config/vision/.env` (dot-prefixed, per dotenv convention). Vision loads this file before parsing the config, so `${VAR}` expansion works regardless of how the daemon is started — foreground, background, or systemd.

```bash
# ~/.config/vision/.env
CONTEXT7_API_KEY=your-context7-key
KAGI_API_KEY=your-kagi-key
FIRECRAWL_API_KEY=your-firecrawl-key
```

```bash
# Set restrictive permissions
chmod 600 ~/.config/vision/.env
```

Reference them in `servers.yaml`:

```yaml
servers:
  context7:
    env:
      CONTEXT7_API_KEY: "${CONTEXT7_API_KEY}"
```

For tools that resolve tokens dynamically from CLI tools (e.g. `gh auth token`), use a bash wrapper:

```yaml
servers:
  grep-app:
    command: bash
    args: ["-c", "export GITHUB_TOKEN=$(gh auth token) && exec node /path/to/server.js"]
```

## File Permissions

Vision enforces owner-only permissions on sensitive files:

- **`servers.yaml`** — Written with `0600` permissions when saved via `vision add` or `vision config`. Prevents other users from reading `bearer_token` values or other config secrets.
- **`.env`** — Should be manually set to `0600` since it contains API keys.

```bash
chmod 600 ~/.config/vision/servers.yaml
chmod 600 ~/.config/vision/.env
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
