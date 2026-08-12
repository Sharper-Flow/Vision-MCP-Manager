# Vision Plugin for OpenCode

OpenCode plugin that enables AI agents to discover and configure MCP servers through the Vision daemon.

## Features

- **Context Injection**: Automatically injects Vision usage instructions at session start
- **Tool Wrappers**: Exposes all Vision Admin MCP tools as OpenCode plugin tools
- **Runtime MCP Control**: Connect and disconnect opencode's own MCP servers mid-session, without the Vision daemon
- **Health Check**: Detects if daemon is running and provides helpful guidance
- **Error Handling**: Clear error messages when daemon is not available

## Installation

1. Add this plugin to your OpenCode configuration, pointing at the package
   directory:

   ```json
   {
     "plugin": ["/path/to/vision/opencode-plugin"]
   }
   ```

2. For the `vision_*` tools, ensure the Vision daemon is installed and running:

   ```bash
   vision daemon start
   ```

   The `opencode_mcp_*` tools need no daemon.

## Available Tools

The plugin exposes two groups of tools. The distinction matters: the first group
needs the Vision daemon, the second does not.

### Daemon-backed (require `vision daemon start`)

These wrap Vision Admin MCP calls on port 6275. They return a structured error if
the daemon is not running.

| Tool                 | Description                                    |
| -------------------- | ---------------------------------------------- |
| `vision_list`        | List all registered MCP servers with status    |
| `vision_add`         | Add and start an MCP server                    |
| `vision_remove`      | Stop and remove a server                       |
| `vision_restart`     | Restart a server in-place, preserving its port |
| `vision_search`      | Search for servers by name or capability       |
| `vision_init`        | Generate or reconcile an OpenCode config       |
| `vision_status`      | Check daemon health and statistics             |
| `vision_guidance`    | Get ranked tool-selection guidance             |
| `vision_slot_status` | Inspect slot-group routing health              |
| `vision_metrics`     | Inspect daemon and tool-call metrics           |

`vision_init` is an OpenCode-plugin convenience. When `path` is omitted, the
plugin detects recognized project configs in this order: a sole existing root
`opencode.jsonc`/`opencode.json`, or a sole existing
`.opencode/opencode.jsonc`/`.opencode/opencode.json`. If none exists, it uses
the project root's `opencode.jsonc`; if multiple recognized configs exist, an
explicit path is required. Explicit relative plugin paths resolve against the
project directory. Existing JSONC comments and trailing commas are preserved
during reconciliation. Direct Admin MCP callers must provide an absolute
`path`; they do not use plugin path detection.

### Daemon-independent (work with the daemon stopped)

These drive opencode's **own** MCP registry in-process. No Vision daemon, no TCP,
no port.

| Tool                      | Description                                           |
| ------------------------- | ----------------------------------------------------- |
| `opencode_mcp_connect`    | Connect an MCP server declared in opencode config     |
| `opencode_mcp_disconnect` | Disconnect an MCP server, releasing its prompt budget |

Both operate only on servers already declared in the opencode config `mcp` block —
they cannot add a server that is not declared. Two things to know:

- **Next turn, not this one.** A connected server's tools appear on the following
  turn. A disconnected server's tools disappear from the following turn.
- **Session-lifetime, not configuration.** Neither tool writes to any config file.
  The connection ends with the opencode process, and the server's config entry is
  left untouched, so it stays reconnectable.

This is what makes it practical to leave a context-expensive server
`enabled: false` by default: an agent that turns out to need it can connect it
mid-session instead of asking for a config edit and a restart.

## Architecture

```
OpenCode Session
      │
      ▼
Vision Plugin (this package)
      │
      ├── Context Injection (session.created, compacting)
      │
      ├── vision_* tools ──────► Vision Admin MCP (port 6275)
      │                               │
      │                               ▼
      │                          Vision Daemon
      │                               │
      │                               ▼
      │                          MCP Servers
      │
      └── opencode_mcp_* tools ─► opencode's own MCP registry
                                  (in-process, via the injected plugin client)
```

The `vision_*` path is a thin client over the Admin MCP. The `opencode_mcp_*`
path never leaves the opencode process: opencode injects a client into the plugin
whose transport dispatches straight into its own HTTP app when no server port is
bound, which is the default for the TUI.

The plugin:

1. Injects context at session start to solve the "bootstrap problem"
2. Wraps Admin MCP tool calls for OpenCode tool discovery
3. Handles daemon-not-running errors gracefully
4. Connects and disconnects opencode MCP servers at runtime, independent of the daemon

## Development

```bash
# Install dependencies
npm install

# Type check
npm run typecheck

# Run tests
npm test

# Lint and format
npm run lint
npm run format
```

## Requirements

- Node.js 18+
- OpenCode with plugin support
- Vision daemon running on port 6275 — required for the `vision_*` tools only.
  The `opencode_mcp_*` tools work with the daemon stopped.

## License

MIT
