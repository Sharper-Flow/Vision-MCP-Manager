# Configuration Templates

This directory contains **example configurations** and **schemas** for Vision.

These files are templates - they are NOT used directly by Vision at runtime.

## Files

| File | Purpose | Runtime Location |
|------|---------|------------------|
| `servers.example.yaml` | Server registry template | `~/.config/vision/servers.yaml` |
| `claude-code.example.json` | Claude Code client config | `~/.claude.json` (global) or `.claude/settings.json` (project) |
| `opencode.example.json` | OpenCode client config | `~/.config/opencode/opencode.json` or `./opencode.json` |

## Usage

### Option 1: Use `vision init` (Recommended)

```bash
# Generate global config (all projects)
vision init --global

# Generate project-specific config
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

# Claude Code (global)
cp configs/claude-code.example.json ~/.claude.json

# Claude Code (project)
mkdir -p .claude
cp configs/claude-code.example.json .claude/settings.json
```

## Important Notes

- **Never commit `servers.yaml`** - It may contain API keys and secrets
- **Never commit `.claude.json` or `.claude/`** - User-specific configuration
- Environment variables like `${CONTEXT7_API_KEY}` are expanded at runtime
- Port numbers in client configs must match the server registry
