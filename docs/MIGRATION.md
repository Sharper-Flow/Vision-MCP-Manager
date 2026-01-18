# Migrating from Jarvis/MCPM to Vision

This guide helps you migrate from the Jarvis/MCPM architecture to Vision.

## Overview

Vision is a complete replacement for the Jarvis/MCPM stack:

| Component | Before | After |
|-----------|--------|-------|
| Daemon | mcpm-daemon (Python/Node) | Vision daemon (Go) |
| CLI | mcpm CLI | vision CLI |
| Config | servers.json, profiles.json | servers.yaml |
| Supervisor | supervisord | suture (built-in) |
| Profiles | essentials, dev-core, etc. | Flat server list |

## Prerequisites

1. Install Vision binary
2. Backup your existing MCPM configuration

## Migration Steps

### Step 1: Stop Jarvis/MCPM

```bash
# Stop the mcpm daemon
docker compose -f ~/dev/MCP/docker-compose.yaml down

# Or if running directly
mcpm daemon stop
```

### Step 2: Export Current Configuration

If using automatic migration:

```bash
# Preview what would be migrated
vision migrate --dry-run

# Perform migration
vision migrate
```

This will:
- Read `~/.mcpm/servers.json`
- Convert to `~/.config/vision/servers.yaml`
- Create a backup of the original

### Manual Migration

If you prefer manual migration, convert your servers.json:

**Before (servers.json):**
```json
{
  "servers": {
    "time": {
      "command": "npx",
      "args": ["-y", "@anthropic/mcp-time"],
      "profile_tags": ["essentials"]
    }
  }
}
```

**After (servers.yaml):**
```yaml
servers:
  time:
    port: 6276
    command: npx
    args: ["-y", "@anthropic/mcp-time"]
    autostart: true
```

### Step 3: Start Vision Daemon

```bash
# Start in foreground (for testing)
vision daemon start

# Or install as a service
sudo cp scripts/vision.service /etc/systemd/system/
sudo systemctl enable vision
sudo systemctl start vision
```

### Step 4: Update Client Configuration

**Claude Code:**

Old (Jarvis):
```json
{
  "mcpServers": {
    "essentials": {
      "type": "streamable-http",
      "url": "http://localhost:6276/mcp"
    }
  }
}
```

New (Vision):
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

Generate automatically:
```bash
# For Claude Code
vision init --client claude-code --global

# For project-specific
cd /your/project
vision init --client claude-code
```

**OpenCode:**

```bash
# For OpenCode
vision init --client opencode --global
```

### Step 5: Verify Migration

```bash
# Check daemon status
vision daemon status

# List servers
vision server list

# Check health
vision health

# Test a server
curl http://localhost:6276/mcp -X POST \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":"1","method":"tools/list","params":{}}'
```

## Key Differences

### 1. No More Profiles

Vision uses a flat server list instead of profile groupings:

- **Before**: Servers grouped into essentials, dev-core, research, data
- **After**: Each server is independent; select per-project in client config

To replicate profile behavior, use `--servers` flag:
```bash
vision init --servers time,context7,firecrawl
```

### 2. One Port Per Server

- **Before**: All servers in a profile on one port (e.g., 6276)
- **After**: Each server gets its own port (6276, 6277, 6278, ...)

This improves security isolation (same-origin policy).

### 3. YAML Configuration

- **Before**: JSON files (servers.json, profiles.json, clients.json)
- **After**: Single YAML file (servers.yaml)

YAML benefits:
- Comments allowed
- More readable
- Environment variable expansion
- Standard for Go tooling

### 4. Built-in Supervision

- **Before**: External supervisord
- **After**: Built-in suture supervisor

No need to manage separate supervisor configuration.

## Troubleshooting

### Server not starting

Check server logs:
```bash
vision server info <name>
journalctl -u vision -f
```

### Port already in use

Vision uses ports 6275-6300. If you have conflicts:
```bash
# Find what's using the port
lsof -i :6276

# Change server port in servers.yaml
```

### Client can't connect

1. Verify daemon is running: `vision daemon status`
2. Check server is started: `vision server list`
3. Test endpoint directly: `curl http://localhost:6276/health`

### Missing environment variables

Vision expands `${VAR}` in config. Ensure variables are set:
```bash
export CONTEXT7_API_KEY="your-key"
vision daemon start
```

Or add to systemd service:
```ini
[Service]
Environment="CONTEXT7_API_KEY=your-key"
```

## Rollback Plan

If migration fails:

```bash
# Stop Vision
vision daemon stop

# Restart Jarvis/MCPM
docker compose -f ~/dev/MCP/docker-compose.yaml up -d

# Restore client configs to use Jarvis endpoints
```

Your MCPM configuration was backed up during migration and can be restored.
