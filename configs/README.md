# Configuration Templates

This directory contains **example configurations** and **schemas** for Vision.

These files are templates - they are NOT used directly by Vision at runtime.

## Files

| File | Purpose | Runtime Location |
|------|---------|------------------|
| `servers.example.yaml` | Server registry template | `~/.config/vision/servers.yaml` |
| `opencode.example.json` | OpenCode client config | See locations below |

## OpenCode Configuration Locations

OpenCode searches for `.opencode.json` in this order (first found wins):

1. **Project-local**: `./.opencode.json` (current directory)
2. **XDG Config**: `$XDG_CONFIG_HOME/opencode/.opencode.json`
3. **Home**: `$HOME/.opencode.json`

**Recommendation**: Use `$HOME/.opencode.json` for global config, `./.opencode.json` for project overrides.

## Usage

### Option 1: Use `vision init` (Recommended)

```bash
# Generate global config (~/.opencode.json)
vision init --global

# Generate project-specific config (./.opencode.json)
cd /path/to/project
vision init

# Generate for specific servers only
vision init --servers time,context7
```

### Option 2: Copy and Modify

```bash
# Server registry
mkdir -p ~/.config/vision
cp configs/servers.example.yaml ~/.config/vision/servers.yaml
# Edit to add your servers and API keys

# OpenCode (global)
cp configs/opencode.example.json ~/.opencode.json

# OpenCode (project-local)
cp configs/opencode.example.json ./.opencode.json
```

## MCP Server Types in OpenCode

OpenCode supports two MCP transport types:

| Type | Use Case | Config |
|------|----------|--------|
| `stdio` | Local process (direct spawn) | `command`, `args`, `env` |
| `sse` | Remote HTTP/SSE endpoint | `url`, `headers` (optional) |

**Vision uses `sse` type** because it exposes MCP servers as HTTP endpoints:

```json
{
  "mcpServers": {
    "time": {
      "type": "sse",
      "url": "http://localhost:6276/mcp"
    }
  }
}
```

## Important Notes

- **Never commit `servers.yaml`** - It may contain API keys and secrets
- **Never commit `.opencode.json`** - User-specific configuration
- Environment variables like `${CONTEXT7_API_KEY}` are expanded at runtime
- Port numbers in client configs must match the server registry
- Each Vision-managed server gets a dedicated port (6276-6300)
