# MCP Transport Architecture

This document describes how Vision bridges MCP transports: accepting Streamable HTTP from upstream clients and managing stdio subprocesses downstream, with strict per-session subprocess isolation.

## MCP Transport Types

The MCP specification defines two standard transports:

### 1. stdio (Standard I/O)

- Client spawns the MCP server as a subprocess
- Communication via stdin/stdout using newline-delimited JSON-RPC
- Server writes logs to stderr (not protocol messages)
- One client per server process (1:1)
- Server exits when client disconnects

**Go SDK types:**
- Server-side: `mcp.StdioTransport{}`
- Client-side: `mcp.CommandTransport{Command: exec.Command("server")}`

### 2. Streamable HTTP

- Server runs as HTTP service on a dedicated port
- Client sends JSON-RPC via HTTP POST to `/mcp`
- Server responds with `application/json` or `text/event-stream` (SSE)
- Supports multiple concurrent sessions via `Mcp-Session-Id` header
- Session teardown via `DELETE /mcp`

**Endpoints:**
- `POST /mcp` - Send requests/notifications, receive responses
- `GET /mcp` - Open SSE stream for server-initiated messages
- `DELETE /mcp` - Close session and release resources

**Go SDK types:**
- Server-side: `mcp.NewStreamableHTTPHandler(getServer, opts)`
- Client-side: `&mcp.StreamableClientTransport{Endpoint: url}`

> **Note:** `StreamableClientTransport` uses the `Endpoint` URL as-is. When the server's mux mounts the handler at `/mcp`, clients must include the path: `Endpoint: "http://host:port/mcp"`.

## Vision's Architecture

Vision bridges these two transports: it exposes **Streamable HTTP** to upstream clients (AI agents) and manages **stdio subprocesses** downstream. Each upstream MCP session gets its own isolated subprocess.

```
                    AI Agent (e.g. Claude Code)
                    Speaks: Streamable HTTP
                            |
                            | HTTP POST/GET/DELETE to :6276/mcp
                            v
  +----------------------------------------------------------+
  |                    Vision Daemon                          |
  |                                                          |
  |  Admin MCP Server (:6275)                                |
  |    go-sdk mcp.Server + StreamableHTTPHandler             |
  |    Tools: vision_list, vision_add, vision_remove, ...    |
  |                                                          |
  |  Per-Server Proxy (one port per configured server):      |
  |                                                          |
  |  :6276/mcp ──[session A]──> subprocess A (stdio)         |
  |            ──[session B]──> subprocess B (stdio)          |
  |                                                          |
  |  :6277/mcp ──[session C]──> subprocess C (stdio)         |
  |                                                          |
  |  Components:                                             |
  |    StreamableHTTPHandler  (go-sdk, per-server port)      |
  |    session.Manager        (per-server, spawns processes) |
  |    proxy.NewProxyHandler  (wires it all together)        |
  +----------------------------------------------------------+
```

### Per-Session Subprocess Model

Every upstream MCP session results in a **dedicated downstream subprocess**. There is no process sharing between sessions.

**Session lifecycle:**

1. Client sends `initialize` POST to `:port/mcp`
2. `StreamableHTTPHandler` invokes the `getServer` callback (runs synchronously before `server.Connect()`)
3. Inside `getServer`:
   - A new `mcp.Server` is created with `HasTools: true`
   - `session.Manager.SpawnSession()` creates a `CommandTransport` and calls `Client.Connect()` which starts the subprocess and completes the MCP initialize handshake
   - `downstream.ListTools()` discovers available tools
   - Each tool is registered on the upstream `mcp.Server` via `Server.AddTool(tool, handler)` with a proxy handler that forwards `CallTool` to the downstream `ClientSession`
4. The upstream client sees all downstream tools in its initialize response
5. Subsequent `tools/call` requests are proxied to the downstream subprocess
6. On `DELETE /mcp` or session close, `Manager.RemoveSession()` calls `ClientSession.Close()` which triggers `CommandTransport.Close()` following the MCP spec teardown: close stdin, wait, SIGTERM, wait, SIGKILL

### Notification Relay

Downstream-to-upstream notifications are relayed via the `proxySession` struct:

| Downstream Notification | Upstream Action |
|------------------------|-----------------|
| `tools/list_changed` | Re-discover tools via `ListTools`, re-register on upstream `mcp.Server` (auto-notifies clients) |
| `logging/message` | Relay via `ServerSession.Log()` |
| `progress` | Relay via `ServerSession.NotifyProgress()` |

Notification handlers are set via `ClientOptions` at `mcp.NewClient()` time. The upstream `ServerSession` is captured via the `InitializedHandler` callback in `ServerOptions`.

### Admission Control

`session.Manager` enforces a configurable `MaxSessions` limit per server. When the limit is reached, new `initialize` requests fail with an admission error. Existing sessions remain unaffected.

## Key SDK Types Used

| Purpose | Type | Package |
|---------|------|---------|
| Upstream HTTP handler | `mcp.StreamableHTTPHandler` | `go-sdk/mcp` |
| Upstream server | `mcp.Server` | `go-sdk/mcp` |
| Upstream session | `mcp.ServerSession` | `go-sdk/mcp` |
| Downstream transport | `mcp.CommandTransport` | `go-sdk/mcp` |
| Downstream client | `mcp.Client` | `go-sdk/mcp` |
| Downstream session | `mcp.ClientSession` | `go-sdk/mcp` |
| HTTP client transport | `mcp.StreamableClientTransport` | `go-sdk/mcp` |

## Server Configuration

### stdio Servers (Currently Supported)

```yaml
servers:
  time:
    port: 6276
    command: npx
    args: ["-y", "@anthropic/mcp-time"]
    max_sessions: 10  # Optional admission limit
    env:
      TZ: "UTC"
```

Vision:
1. Allocates the configured port for Streamable HTTP
2. On each new client session, spawns a subprocess with `command` + `args`
3. Proxies MCP requests between the upstream HTTP session and the downstream stdio subprocess
4. On session teardown, terminates only that session's subprocess

### Transport Detection

When `transport` is not specified, Vision infers it from the config:

| Config Has | Inferred Transport |
|------------|-------------------|
| `command` | `stdio` |

> **Future:** HTTP and SSE proxy transports may be added for remote/legacy MCP servers.

## Internal Packages

| Package | Responsibility |
|---------|---------------|
| `internal/session` | `Manager` — per-session subprocess lifecycle (spawn, track, teardown) |
| `internal/mcp` | `NewProxyHandler` — creates `StreamableHTTPHandler` with per-session proxy; `PortManager` — manages HTTP listeners per server |
| `internal/admin` | Admin MCP server on port 6275 with `vision_*` management tools |
| `internal/daemon` | Orchestrates config, registry, supervisor, and proxy setup |

## References

- [MCP Spec: Transports](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
- [Go SDK: mcp package](https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk/mcp)
