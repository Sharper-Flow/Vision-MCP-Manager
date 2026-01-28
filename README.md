<p align="center">
  <img src="assets/header.png" alt="Vision" width="100%">
</p>

<p align="center">
  <strong>The MCP Control Plane for Agentic Coding</strong>
</p>

<p align="center">
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.24+-00ADD8?style=flat&logo=go" alt="Go Version"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-blue.svg" alt="License"></a>
</p>

Vision turns MCP servers from a configuration nightmare into a supervised, agent-accessible control plane. One daemon. One config. Full agent autonomy with complete operator oversight.

## The Problem

Using MCP servers with AI coding agents today means:

- **Scattered configurations** — Each project has its own MCP setup, duplicated across machines
- **Manual process management** — Servers crash silently, requiring manual restarts
- **No visibility** — Which tools are running? What's failing? No central place to look
- **Static tooling** — Agents can't adapt their capabilities; humans must edit configs
- **Transport chaos** — stdio, HTTP, SSE... each client expects something different

## The Solution

Vision is a Go-native daemon that provides:

| Capability | What It Means |
|------------|---------------|
| **Centralized Registry** | One YAML file (`~/.config/vision/servers.yaml`) defines all your MCP servers |
| **Process Supervision** | Erlang-style supervision with automatic restarts and exponential backoff |
| **stdio-to-HTTP Bridge** | Every server gets a dedicated HTTP port—no more transport incompatibilities |
| **Agent Self-Management** | AI agents can add, remove, and restart servers through the Admin MCP API |
| **Hot Reload** | Update configuration without restarting your coding session |

## Why Vision + OpenCode?

[OpenCode](https://opencode.ai) is an open-source AI coding agent with native support for Vision's `remote` MCP transport. Together, they enable **high-agency coding with guardrails**:

```
You: "Research best practices for React Server Components"

Agent: [Calls vision_list — no documentation server available]
Agent: [Calls vision_search("documentation") — finds context7]
Agent: [Calls vision_add("context7", start=true)]
Agent: [Now has Context7 available — proceeds with research]
```

No human intervention. No config file edits. The agent adapts to what it needs.

**But you stay in control:**
- All servers are defined in your central config
- The Admin API only exposes servers you've pre-approved in the catalog
- Every process is supervised and logged
- One `vision daemon status` shows everything

## Comparison

| Feature | Vision | MCP Gateway | Direct in OpenCode |
|---------|--------|-------------|-------------------|
| Agent self-management | Yes | No | No |
| Process supervision | Yes (Erlang-style) | Varies | No |
| Automatic restarts | Yes | Varies | No |
| Hot reload | Yes | No | No |
| Central config | Yes | Yes | No (per-project) |
| stdio-to-HTTP bridge | Yes | Some | No |
| Admin MCP API | Yes | No | N/A |
| Single binary | Yes | Varies | N/A |

## Quick Start

```bash
# Install Vision
curl -fsSL https://raw.githubusercontent.com/Sharper-Flow/Vision-MCP-Manager/trunk/scripts/install.sh | bash

# Start the daemon (with example servers)
vision daemon start -d

# Generate OpenCode configuration
vision init --global
```

Your AI agent can now connect to Vision-managed servers at `http://localhost:627X/mcp`.

## Installation

### One-Line Install (Recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/Sharper-Flow/Vision-MCP-Manager/trunk/scripts/install.sh | bash
```

The installer downloads the latest binary, places it in `/usr/local/bin`, and creates the configuration directory.

**Options:**
- `--systemd` — Install and enable the systemd user service
- `--opencode` — Auto-configure OpenCode integration

### From Source

```bash
git clone https://github.com/Sharper-Flow/Vision-MCP-Manager.git
cd Vision-MCP-Manager
make build
sudo cp bin/vision /usr/local/bin/
```

**Requirements:** Go 1.24+

### Running as a Service

For always-on operation, install the systemd user service:

```bash
mkdir -p ~/.config/systemd/user
cp scripts/vision-user.service ~/.config/systemd/user/vision.service
systemctl --user daemon-reload
systemctl --user enable --now vision
```

## Configuration

### Server Registry

Vision maintains a central registry at `~/.config/vision/servers.yaml`:

```yaml
servers:
  # Context7 - Library documentation lookup
  context7:
    port: 6276
    command: npx
    args: ["-y", "@upstash/context7-mcp@latest"]
    env:
      CONTEXT7_API_KEY: "your-api-key-here"
    autostart: true

  # Kagi - Web search and summarization
  kagi:
    port: 6284
    command: uvx
    args: ["kagimcp"]
    env:
      KAGI_API_KEY: "your-kagi-api-key-here"
    autostart: true

  # Time - Timezone utilities
  time:
    port: 6282
    command: uvx
    args: ["mcp-server-time", "--local-timezone=America/New_York"]
    autostart: true
```

> **Best Practice:** Put API keys directly in `servers.yaml`. This file is local (`~/.config/vision/`) and never committed to git.

### OpenCode Integration

Add Vision to your OpenCode config (`~/.config/opencode/opencode.jsonc`):

```json
{
  "mcp": {
    "vision": {
      "type": "remote",
      "url": "http://localhost:6275/mcp",
      "enabled": true
    },
    "context7": {
      "type": "remote",
      "url": "http://localhost:6276/mcp",
      "enabled": true
    },
    "kagi": {
      "type": "remote",
      "url": "http://localhost:6284/mcp",
      "enabled": true
    }
  }
}
```

Or generate automatically:

```bash
vision init --global --client opencode
```

## Agent Self-Management

Vision's killer feature: **agents can manage their own tools**.

The Admin MCP Server (port 6275) exposes these tools to your AI agent:

| Tool | What It Does |
|------|--------------|
| `vision_list` | Show all servers with status (running/stopped/error) |
| `vision_add` | Provision and start a new server from the catalog |
| `vision_remove` | Stop and remove a server |
| `vision_search` | Find servers by name or capability |
| `vision_status` | Daemon health, uptime, memory usage |
| `vision_guidance` | Get recommendations for which tool to use |

**Example workflow:**

1. Agent needs web search capability
2. Calls `vision_search("web search")` → finds `kagi`
3. Calls `vision_add("kagi", start=true)` → server starts
4. Agent now has web search available
5. You see it in `vision_list` — full visibility

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        AI Agents                                │
│                  (OpenCode, Claude Code, etc.)                  │
└───────────────────────────────┬─────────────────────────────────┘
                                │ HTTP POST /mcp
                                ▼
┌─────────────────────────────────────────────────────────────────┐
│                       Vision Daemon                             │
│                                                                 │
│   ┌─────────────────────────────────────────────────────────┐   │
│   │           Admin MCP Server (:6275)                      │   │
│   │   vision_list  vision_add  vision_status  vision_search │   │
│   └─────────────────────────────────────────────────────────┘   │
│                                                                 │
│   ┌─────────────────────────────────────────────────────────┐   │
│   │              stdio-to-HTTP Bridge Layer                 │   │
│   │      :6276/mcp      :6284/mcp      :6282/mcp    ...     │   │
│   └────────────┬──────────────┬──────────────┬──────────────┘   │
│                │              │              │                  │
│   ┌────────────▼──────────────▼──────────────▼──────────────┐   │
│   │         Process Supervisor (Erlang-style)               │   │
│   │    ┌──────────┐   ┌──────────┐   ┌──────────┐           │   │
│   │    │ context7 │   │   kagi   │   │   time   │   ...     │   │
│   │    │  (stdio) │   │  (stdio) │   │  (stdio) │           │   │
│   │    └──────────┘   └──────────┘   └──────────┘           │   │
│   └─────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
```

**Key components:**

1. **Admin MCP Server** — Lets agents manage their own tooling without human intervention
2. **stdio-to-HTTP Bridge** — Converts any stdio MCP server to HTTP, each on a dedicated port
3. **Process Supervisor** — Uses [suture](https://github.com/thejerf/suture) for automatic restarts with backoff

## Trust Through Visibility

High-agency doesn't mean black-box. Vision gives you:

- **Centralized config** — Know exactly what tools are available (`~/.config/vision/servers.yaml`)
- **Process supervision** — Every server is monitored; crashes trigger automatic restarts
- **Single daemon** — One place to check status: `vision daemon status`
- **Dedicated ports** — Each server isolated on its own port for security and debugging
- **Hot reload** — Update config without disrupting active sessions: `vision daemon reload`

## CLI Reference

### Daemon

```bash
vision daemon start        # Start in foreground
vision daemon start -d     # Start daemonized
vision daemon stop         # Graceful shutdown
vision daemon status       # Health, uptime, memory
vision daemon reload       # Hot-reload config (SIGHUP)
```

### Servers

```bash
vision server list                    # Show all servers
vision server add <name> --command <cmd> [--args <args>]
vision server remove <name>
vision server start <name>
vision server stop <name>
```

### Configuration

```bash
vision init                           # Project-local config
vision init --global                  # Global config
vision init --client opencode         # Target OpenCode format
vision init --servers time,context7   # Specific servers only
```

## Troubleshooting

| Symptom | Solution |
|---------|----------|
| Server not responding | `vision daemon status` — check if daemon is running |
| Server keeps crashing | Check logs: `journalctl --user -u vision -f` |
| Config changes not applied | Run `vision daemon reload` |
| Port already in use | Check for conflicts: `lsof -i :6276` |

## Documentation

- [Configuration Reference](docs/CONFIGURATION.md) — Complete server options
- [AI Agents Guide](docs/agents.md) — Deep dive into agent integration
- [MCP Transports](docs/MCP_TRANSPORTS.md) — Understanding stdio, HTTP, SSE
- [Development Guide](DEVELOPMENT.md) — Building and contributing

## Roadmap

- [ ] Audit logging for all MCP traffic
- [ ] Per-tool permission controls
- [ ] Usage analytics dashboard
- [ ] Multi-machine sync

## License

MIT License — see [LICENSE](LICENSE) for details.

---

<p align="center">
  <sub>Built with Go. Designed for agentic coding.</sub>
</p>
