# Configuration Templates

This directory contains **example configurations** and **schemas** for Vision.

These files are templates - they are NOT used directly by Vision at runtime.

## Files

| File | Purpose | Runtime Location |
|------|---------|------------------|
| `servers.example.yaml` | Server registry template | `~/.config/vision/servers.yaml` |
| `opencode.example.json` | OpenCode client config | See locations below |

## OpenCode Configuration Locations

Use one of OpenCode's current configuration paths:

- **Project**: `./opencode.json` or `./opencode.jsonc`
- **Project subdirectory**: `./.opencode/opencode.json` or `./.opencode/opencode.jsonc`
- **Global**: `~/.config/opencode/opencode.jsonc`

The project configuration takes precedence over the global configuration. The
current schema uses a top-level `mcp` object with `remote` entries.

## Usage

### Copy and Modify

```bash
# Server registry
mkdir -p ~/.config/vision
cp configs/servers.example.yaml ~/.config/vision/servers.yaml
# Edit to add your servers and API keys

# OpenCode (global)
cp configs/opencode.example.json ~/.config/opencode/opencode.jsonc

# OpenCode (project-local)
cp configs/opencode.example.json ./opencode.jsonc
```

## MCP Server Types in OpenCode

OpenCode supports two MCP transport types:

| Type | Use Case | Config |
|------|----------|--------|
| `stdio` | Local process (direct spawn) | `command`, `args`, `env` |
| `remote` | Remote HTTP endpoint | `url`, `headers` (optional) |

**Vision uses `remote` type** because it exposes MCP servers as HTTP endpoints:

```json
{
  "mcp": {
    "time": {
      "type": "remote",
      "url": "http://localhost:6276/mcp",
      "enabled": true
    }
  }
}
```

## Important Notes

- **Never commit `servers.yaml`** - It may contain API keys and secrets
- Keep machine-specific OpenCode configuration out of source control unless it is
  an intentional project declaration.
- Environment variables like `${CONTEXT7_API_KEY}` are expanded at runtime
- Port numbers in client configs must match the server registry
- Each Vision-managed server gets a dedicated port (6276-6325)
