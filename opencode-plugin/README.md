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

The plugin exposes two groups of tools. The distinction matters twice over: the
first group needs the Vision daemon, the second does not — and the first group is
registered **only when Code Mode is off**.

### Daemon-backed (require `vision daemon start`)

These wrap Vision Admin MCP calls on port 6275. They return a structured error if
the daemon is not running.

**Registered only when Code Mode is off.** When
`OPENCODE_EXPERIMENTAL_CODE_MODE=true`, the plugin omits all ten. They remain
reachable as `tools.vision.<name>()` through the `vision` MCP server, and Code
Mode collapses MCP tool schemas into one `execute` tool plus a bounded catalog.
Plugin-registered schemas get no such collapse, so registering them under Code
Mode would carry ten full schemas in every prompt for capability the session can
already reach. The decision is made once, at plugin-factory time.

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

**Registered in both Code Mode states.** Unlike the daemon-backed group, these
two have no `tools.*` equivalent — the Code Mode catalog is built from MCP tools
only, so a plugin-registered tool never appears in it. Suppressing them under
Code Mode would make them unreachable rather than deduplicated.

| Tool                      | Description                                           |
| ------------------------- | ----------------------------------------------------- |
| `opencode_mcp_connect`    | Connect an MCP server declared in opencode config     |
| `opencode_mcp_disconnect` | Disconnect an MCP server, releasing its prompt budget |

Both operate only on servers already declared in the opencode config `mcp` block —
they cannot add a server that is not declared. Two things to know:

- **Takes effect immediately.** The registry change lands within the same turn:
  a tool resolved at call time is usable right away. Only the turn's _advertised_
  tool list is fixed at turn start, so a newly connected server may not appear in
  that listing until the next turn. An earlier version of this document claimed
  availability began on the following turn; that was measured false and
  `src/registration.test.ts` now pins the corrected wording.
- **Session-lifetime, not configuration.** Neither tool writes to any config file.
  The connection ends with the opencode process, and the server's config entry is
  left untouched, so it stays reconnectable.

This is what makes it practical to leave a context-expensive server
`enabled: false` by default: an agent that turns out to need it can connect it
mid-session instead of asking for a config edit and a restart.

### Precondition: the plugin and the `vision` mcp entry are paired

Under Code Mode, daemon capability depends on the `vision` MCP server being both
**declared** in the opencode config `mcp` block and **connected**. Before
conditional registration the plugin's direct HTTP to port 6275 survived a failed
or absent MCP connection; under Code Mode it no longer does, because the daemon
tools are not registered at all.

Consequences worth stating plainly:

- Do **not** delete the `vision` entry from the `mcp` block as "redundant with
  the plugin". It is load-bearing for every Code Mode session.
- If the `vision` server is declared but disconnected, `opencode_mcp_connect` is
  the in-band recovery path — which is exactly why that tool stays registered in
  both modes.
- If the Vision daemon itself is down, both paths fail regardless. The pairing
  changes nothing for that case.

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
      │   [Code Mode OFF only]        │
      │                               ▼
      │                          Vision Daemon
      │                               │
      │                               ▼
      │                          MCP Servers
      │
      └── opencode_mcp_* tools ─► opencode's own MCP registry
          [both modes]              (in-process, via the injected plugin client)

Under Code Mode the daemon path is reached instead as:

OpenCode Session ──► tools.vision.*() ──► `vision` MCP server ──► Vision Daemon
```

The `vision_*` path is a thin client over the Admin MCP. The `opencode_mcp_*`
path never leaves the opencode process: opencode injects a client into the plugin
whose transport dispatches straight into its own HTTP app when no server port is
bound, which is the default for the TUI.

The plugin:

1. Injects context at session start to solve the "bootstrap problem", rendering
   `tools.vision.*()` or bare tool names depending on Code Mode
2. Wraps Admin MCP tool calls for OpenCode tool discovery, when Code Mode is off
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
