# Vision Plugin for OpenCode

OpenCode plugin that enables AI agents to discover and configure MCP servers through the Vision daemon.

## Features

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
   systemctl --user restart vision.service
   ```

   The `opencode_mcp_*` tools need no daemon.

## Real-host V2 qualification

Run `VISION_V2_LAB=/path/to/dev/opencode-v2-lab/lab bash opencode-plugin/scripts/e2e-v2-runtime.sh`
from the repository root. `VISION_V2_LAB` points at the toolbox OpenCode V2
lab, which installs and hash-pins the OpenCode binary and runs it offline in
bubblewrap. The qualified host version is the exact `@opencode/plugin` pin in
`package.json`, currently 2.4.1; the runner fails unless the lab is pinned to
that version. The runner needs Linux, bubblewrap, Python 3, npm and network
access for npm downloads. Tool-list and MCP-control checks use the lab's
scripted loopback provider (`--fixture`). Isolation uses the lab's
`check-isolation` with its negative control, and `snapshot-live` /
`compare-live` hash comparison of the live install.

A private bwrap sandbox, with the real home hidden and private PID, IPC and
network namespaces, covers the checks the lab cannot express: the HTTP MCP
session-expiry probe, the reconnect probe, and a read-only bind of the
repository plugin directory. It uses the lab's pinned binary unless
`VISION_V2_BIN` overrides it.

| Variable                   | Effect                                                                                    |
| -------------------------- | ----------------------------------------------------------------------------------------- |
| `VISION_V2_LOADER_PROBE=1` | Adds the directory-entry probes.                                                          |
| `VISION_V2_NEGATIVE=1`     | Runs only the broken-plugin negative control; exits 0 when the tool assertions reject it. |
| `VISION_V1_BIN`            | Adds OpenCode 1.18.35 checks on the private sandbox.                                      |
| `VISION_V2_KEEP=1`         | Keeps the fixture and lab instance for inspection.                                        |

Known lab gaps that keep those checks on the private route: an `--exec`
helper cannot run the pinned binary (`/opencode` is bound only when the run
executable is `/opencode`), `--fixture` serves only
`/v1/chat/completions` so no HTTP MCP server can share the sandbox, the
fixture script cannot select responses by request content, and read-only
bind sources must be inside the toolbox repository.

On 2.4.1, per-server Code Mode defaults on independently of the environment
variable described below. The runner checks the server and environment matrix.

## Available Tools

The plugin exposes two groups of tools: the first needs the Vision daemon, the
second does not. Registration of the first group depends on the host version.

### Daemon-backed (require `systemctl --user restart vision.service`)

These wrap Vision Admin MCP calls on port 6275. They return a structured error if
the daemon is not running.

On OpenCode 2.x, the plugin registers all eight unless an enabled `vision` MCP
server is declared. The host exposes that server's tools in the catalog or
directly, depending on its Code Mode setting. A server under another key does
not suppress plugin tools. The decision reads the merged `vision` entry,
including edits by `opencode_mcp_connect`, `opencode_mcp_disconnect` and other
plugins. It takes effect when OpenCode rebuilds its tool list after MCP tool
changes; a change that leaves the MCP tool set unchanged may wait until the
next rebuild.

On OpenCode 1.x, the plugin omits all eight when
`OPENCODE_EXPERIMENTAL_CODE_MODE=true`; the `vision` MCP server supplies them
through the Code Mode catalog. The V1 decision is made at plugin-factory time.

| Tool                 | Description                                    |
| -------------------- | ---------------------------------------------- |
| `vision_list`        | List all registered MCP servers with status    |
| `vision_remove`      | Stop and remove a server                       |
| `vision_restart`     | Restart a server in-place, preserving its port |
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

On 1.x with Code Mode enabled, daemon capability depends on the `vision` MCP
server being declared and connected. On 2.x, a declared, enabled `vision`
server suppresses plugin daemon tools even if the server fails to connect.

Consequences worth stating plainly:

- Keep the `vision` entry for 1.x Code Mode sessions; without it, daemon tools
  are unavailable. On 2.x, an absent or disabled entry enables plugin tools.
- On 1.x, `opencode_mcp_connect` can reconnect a failed `vision` server. On 2.x,
  it re-enables a disabled server; if an enabled server fails, daemon tools are
  absent until it connects. Fix the connection or restart OpenCode.
- If the Vision daemon itself is down, both paths fail regardless. The pairing
  changes nothing for that case.

## Architecture

```
OpenCode Session
      │
      ▼
Vision Plugin (this package)
      │
      ├── vision_* tools ──────► Vision Admin MCP (port 6275)
      │   [conditional]               │
      │                               ▼
      │                          Vision Daemon
      │                               │
      │                               ▼
      │                          MCP Servers
      │
      └── opencode_mcp_* tools ─► opencode's own MCP registry
          [both modes]              (in-process, via the injected plugin client)

Through a declared `vision` MCP server the daemon path is:

OpenCode Session ──► tools.vision.*() or vision_vision_*() ──► `vision` MCP server ──► Vision Daemon
```

The `vision_*` path is a thin client over the Admin MCP. The `opencode_mcp_*`
path never leaves the opencode process: opencode injects a client into the plugin
whose transport dispatches straight into its own HTTP app when no server port is
bound, which is the default for the TUI.

The plugin:

1. Wraps Admin MCP tool calls for OpenCode tool discovery when registered
2. Handles daemon-not-running errors gracefully
3. Connects and disconnects opencode MCP servers at runtime, independent of the daemon

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

- Node.js 24+ for plugin development and checks
- OpenCode with plugin support
- Vision daemon running on port 6275 — required for the `vision_*` tools only.
  The `opencode_mcp_*` tools work with the daemon stopped.

## License

MIT
