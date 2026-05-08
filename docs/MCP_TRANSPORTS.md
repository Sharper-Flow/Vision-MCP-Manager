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

### Downstream Respawn

When a downstream subprocess becomes unavailable — reaped by idle timeout, crashed, or otherwise closed — the proxy transparently respawns a new subprocess instead of returning `ErrDownstreamUnavailable` to the upstream client.

**Respawn triggers:**
- Tool call (`tools/call`) hits a closed downstream
- Tool list refresh (`tools/list_changed` notification relay) hits a closed downstream

**Respawn flow:**
1. Handler detects `downstreamClosed == true` or `downstream == nil`
2. Calls `proxySession.respawnDownstream()` which:
   - Acquires `respawnMu` to serialize concurrent attempts (only one subprocess spawns)
   - Double-checks state (another goroutine may have already respawned)
   - Spawns a new subprocess via `session.Manager.SpawnSession()`
   - Re-discovers tools via `ListTools` and re-registers proxy handlers
   - Atomically swaps the downstream pointer and resets the closed flag
3. If respawn succeeds, the original operation proceeds against the new downstream
4. If respawn fails, the error propagates as before

**Concurrency:** `respawnMu` ensures only one goroutine spawns a subprocess. Other concurrent callers block, then see the already-respawned downstream via double-check.

**Overhead:** ~20ms for subprocess spawn + initialize handshake (Node.js servers). Transparent to the upstream client.

### Structured Fallback Suggestions

Availability failures are returned as visible tool results, not JSON-RPC
protocol errors. This follows the MCP SDK guidance that tool-originated errors
should use `CallToolResult{IsError: true}` so the LLM can see the failure and
self-correct.

When `callDownstreamTool` classifies an upstream failure as `AvailabilityError`,
`makeProxyToolHandler` converts it into a `CallToolResult` whose text content
includes:

- failure category: `config_drift`, `provider_timeout`, `retry_exhausted`, or
  `circuit_open`
- failed server and tool name
- human-readable cause/guidance
- up to three fallback suggestions ranked by catalog capability overlap

Fallback suggestions are advisory only. Vision **does not** retry on another
server or auto-route the tool call. The agent must explicitly choose and invoke
any alternative.

Suggestions are supplied through `ProxyConfig.SuggestionProvider`. The daemon
wires this to a catalog-backed provider that compares the failed server's
capabilities with other catalog entries and annotates each suggestion with:

- server name
- overlapping capabilities
- installed/configured status
- catalog source URL when available
- match reason

If `SuggestionProvider` is nil, availability failures are still visible as
`IsError` tool results, but the suggestions list is empty.

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

### Subprocess Lifecycle Ownership

Vision has **two independent subprocess lifecycles** for the same configured server. The design branches by transport type to avoid unnecessary accumulation:

**stdio transport:**
- `registry.Start()` skips the supervisor — no daemon-scoped subprocess is created
- `session.Manager` is the **sole lifecycle owner** for stdio subprocesses
- Each upstream session gets its own isolated subprocess via `SpawnSession()`
- Subprocesses are spawned **lazily** on first HTTP session connect, not at daemon startup
- Subprocesses are cleaned up by the session reaper (idle timeout / TTL) or on session close

**HTTP/SSE transport:**
- `registry.Start()` registers the server with the supervisor
- The supervisor owns the subprocess lifecycle, with automatic restart on crash
- `session.Manager` forwards requests to the existing server process (no per-session subprocess)

This distinction matters for process accounting: stdio servers report `PID=0` in registry status because no daemon-scoped process exists. This is **correct and expected** — it means "the proxy endpoint is active, but subprocess lifecycle is managed per-session by the session manager."

### Transport Detection

When `transport` is not specified, Vision infers it from the config:

| Config Has | Inferred Transport |
|------------|-------------------|
| `command` | `stdio` |

> **Current support:** Vision already supports HTTP and SSE proxy transports for remote/native MCP servers. Use `transport: http` or `transport: sse` with `url:` when you intentionally want Vision in front of an upstream MCP endpoint.

## Internal Packages

| Package | Responsibility |
|---------|---------------|
| `internal/server` | `Registry` — server registration; `Start()`/`Stop()` branch by transport type to skip supervisor for stdio servers; `ManagedServer` — per-server state |
| `internal/session` | `Manager` — per-session subprocess lifecycle (spawn, track, teardown), reaper |
| `internal/mcp` | `NewProxyHandler` — creates `StreamableHTTPHandler` with per-session proxy; `PortManager` — manages HTTP listeners per server |
| `internal/admin` | Admin MCP server on port 6275 with `vision_*` management tools |
| `internal/daemon` | Orchestrates config, registry, supervisor, and proxy setup |
| `internal/supervisor` | Suture-based process supervisor for non-stdio transports (HTTP/SSE); not used for stdio servers |

## References

- [MCP Spec: Transports](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
- [Go SDK: mcp package](https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk/mcp)
