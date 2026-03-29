# Vision Plugin for OpenCode

OpenCode plugin that enables AI agents to discover and configure MCP servers through the Vision daemon.

## Features

- **Context Injection**: Automatically injects Vision usage instructions at session start
- **Tool Wrappers**: Exposes all Vision Admin MCP tools as OpenCode plugin tools
- **Health Check**: Detects if daemon is running and provides helpful guidance
- **Error Handling**: Clear error messages when daemon is not available

## Installation

1. Ensure Vision daemon is installed and running:

   ```bash
   vision daemon start
   ```

2. Add this plugin to your OpenCode configuration:
   ```json
   {
     "plugins": ["~/dev/vision-plugin/src/index.ts"]
   }
   ```

## Available Tools

| Tool            | Description                                 |
| --------------- | ------------------------------------------- |
| `vision_list`   | List all registered MCP servers with status |
| `vision_add`    | Add and start an MCP server                 |
| `vision_remove` | Stop and remove a server                    |
| `vision_search` | Search for servers by name or capability    |
| `vision_init`   | Generate or reconcile .opencode.json config |
| `vision_status` | Check daemon health and statistics          |

## Architecture

```
OpenCode Session
      │
      ▼
Vision Plugin (this package)
      │
      ├── Context Injection (session.created, compacting)
      │
      └── Tool Calls ──► Vision Admin MCP (port 6275)
                              │
                              ▼
                         Vision Daemon
                              │
                              ▼
                         MCP Servers
```

The plugin is a thin client that:

1. Injects context at session start to solve the "bootstrap problem"
2. Wraps Admin MCP tool calls for OpenCode tool discovery
3. Handles daemon-not-running errors gracefully

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
- Vision daemon running on port 6275
- OpenCode with plugin support

## License

MIT
