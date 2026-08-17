# MCP Transport Architecture

This document describes how Vision exposes Streamable HTTP to clients and owns three downstream models: per-session stdio, shared stdio, and supervised managed native HTTP.

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
- Client sends JSON-RPC requests and notifications via HTTP POST to `/mcp`
- Client opens a long-lived GET/SSE receive stream for server-initiated messages
- Server responds to POST with `application/json` or `text/event-stream` (SSE)
- Supports multiple concurrent sessions via `Mcp-Session-Id` header
- Session teardown via `DELETE /mcp`

**Endpoints:**
- `POST /mcp` - Send requests/notifications, receive responses; normal completion is not a session disconnect signal
- `GET /mcp` - Open SSE stream for server-initiated messages; stream closure is the disconnect signal used for shared-session and managed-http lease grace cleanup
- `DELETE /mcp` - Close session and release resources

**Go SDK types:**
- Server-side: `mcp.NewStreamableHTTPHandler(getServer, opts)`
- Client-side: `&mcp.StreamableClientTransport{Endpoint: url}`

> **Note:** `StreamableClientTransport` uses the `Endpoint` URL as-is. When the server's mux mounts the handler at `/mcp`, clients must include the path: `Endpoint: "http://host:port/mcp"`.

## Vision's Architecture

Vision exposes **Streamable HTTP** to upstream clients (AI agents). Most servers use stdio downstream. `managed-http` servers instead retain native Streamable HTTP and are supervised by Vision.

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

### Managed Native HTTP Model

`transport: managed-http` is for a loopback native HTTP MCP server whose process and public gateway are both owned by Vision. Playwright is the canonical use case:

```text
OpenCode -> Vision :6287/mcp -> security -> reservation/lease gateway
         -> fixed 127.0.0.1:<internal-port>/mcp -> @playwright/mcp
```

- Vision launches and restarts the configured command through Suture.
- The internal URL must be `http`, loopback-only, and exactly `/mcp`.
- Readiness is an at-most-once initialize/delete probe, not PID existence.
- Backend-generated `Mcp-Session-Id` values pass through unchanged. Vision stores raw IDs only internally and exposes bounded hashes.
- One opaque reservation owns capacity during initialize; a lease owns it after a valid response ID.
- Only structurally valid non-`ping` JSON-RPC requests refresh application activity. Notifications, responses, GET/SSE reconnects, and keepalives do not.
- Expiry waits for session in-flight count zero. Cleanup sends one DELETE; success or `404` proves disposal.
- Unknown, expired, and process-lost IDs receive pre-dispatch `404`.
- Ambiguous initialize, application, or DELETE results are never replayed. Vision drains admitted application requests, recycles the shared backend process, invalidates old IDs, and probes the replacement before reopening.

Playwright uses one shared browser process with one BrowserContext per MCP session. The required `--isolated` option makes profiles ephemeral; do not pass `--shared-browser-context`.

### Per-Session and Shared Subprocess Models

Stateful servers (`stateful: true`) give every upstream MCP session a **dedicated downstream subprocess**. Shared-mode servers (`stateful: false`, the default) route multiple upstream sessions through one shared downstream subprocess and remove only the upstream session/refcount on session cleanup.

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
6. On `DELETE /mcp` or session cleanup, Vision removes the upstream session. Stateful cleanup terminates that session's subprocess; shared-mode cleanup decrements the shared refcount and leaves the downstream subprocess healthy for other sessions.

### Disconnect Tracking and Lease Reaping

Disconnect tracking applies to shared-mode stdio sessions and managed-http leases. It tracks session-bound GET/SSE receive streams, not generic inactivity. In shared mode, when the last tracked stream closes and the grace period expires, Vision removes the upstream session/refcount while leaving the shared downstream subprocess healthy for other sessions. In managed-http, the last stream closure starts a lease disconnect window; if no application activity re-arms it before expiry, the lease becomes eligible for cleanup and the gateway sends one downstream `DELETE`; successful disposal or `404` releases admission capacity. SSE connectivity is not counted as in-flight work, but every managed-http lease reap still requires `inFlight == 0`.

Normal POST completion does **not** start a disconnect grace window. An HTTP request context ends when `ServeHTTP` returns, but that does not mean the logical MCP session disconnected. Treating POST completion as a disconnect would create false `session.disconnect_detected` / `session.disconnect_cancelled` churn. A structurally valid application request can re-arm an already-open managed-http disconnect window; it never opens a window when no stream-closure deadline exists. Notifications, responses, GET/SSE reconnects, and keepalives do not count as application activity.

The expiry sweep checks eligible leases in strict first-match order:

1. `client_disconnected` — the disconnect grace deadline elapsed after the last GET/SSE stream closed.
2. `never_streamed` — the lease never opened a stream and exceeded its derived bound.
3. `idle_timeout` — application inactivity exceeded `session_timeout`.

For managed-http lease sweeps, all three rules require `inFlight == 0`, including protocol-maintenance POSTs that do not count as application activity. The never-streamed bound starts as `5 * grace` capped by a positive idle timeout, and is then raised to a 30s handshake floor if it would otherwise fall below it; a non-positive bound disables that rule. The floor is deliberately not capped back to `session_timeout`: the expiry sweep also runs synchronously on admission under capacity pressure, so a bound shorter than a normal `initialize`→GET handshake could reap a lease mid-connect. A short `session_timeout` is still honoured by the independent idle rule. With disconnect grace disabled, the disconnect-deadline rule is disabled while a positive `session_timeout` still supplies the idle/never-streamed fallback. The first matching rule supplies the lifecycle and reap reason.

Managed-http timing derives from these server settings:

- `disconnect_grace_period` supplies grace: unset means `60s`; a negative value disables disconnect grace. After a stream closes, application activity re-arms the existing window from the activity time.
- `session_timeout` supplies the lease idle timeout and the managed-http reap cadence. The cadence starts at `session_timeout / 2`, is capped at `30s`, is further capped by `disconnect_grace_period / 2` when grace is positive, and is floored at `1s`.
- `request_timeout` supplies the managed-http hung-request bound as `10 * request_timeout`. The gateway uses a clone of `http.DefaultTransport` with `ResponseHeaderTimeout` set to that bound: it limits waiting for response headers only. Response bodies, including a POST response served as `text/event-stream`, are not bounded by it. A header timeout returns `504` and increments `BackendHeaderTimeout`; other transport failures return `502`.

`disconnect_grace_period` and `request_timeout` were previously dead configuration on the managed-http path. They are now live: the former controls stream-disconnect lease cleanup, and the latter controls the outbound response-header bound.

### Notification Relay

Downstream-to-upstream notifications are relayed via the `proxySession` struct:

| Downstream Notification | Upstream Action |
|------------------------|-----------------|
| `tools/list_changed` | Re-discover tools via `ListTools`, re-register on upstream `mcp.Server` (auto-notifies clients) |
| `logging/message` | Relay via `ServerSession.Log()` |
| `progress` | Relay via `ServerSession.NotifyProgress()` |

Notification handlers are set via `ClientOptions` at `mcp.NewClient()` time. The upstream `ServerSession` is captured via the `InitializedHandler` callback in `ServerOptions`.

### Downstream Respawn

For stdio proxy sessions, Vision may replace an unavailable downstream **only before the application operation was dispatched**. A closed subprocess detected before dispatch can be recreated and then receive the operation once.

Vision does not replay a tool call after uncertain downstream execution. Stale/unknown upstream session IDs return pre-dispatch `404`; managed native HTTP transport uses backend drain/recycle rather than synthetic request replay. This distinction prevents duplicate browser mutations.

### Managed Playwright Server

```yaml
servers:
  playwright:
    port: 6287
    transport: managed-http
    command: npx
    args:
      - "-y"
      - "@playwright/mcp@0.0.77"
      - "--browser"
      - "chromium"
      - "--headless"
      - "--isolated"
      - "--host"
      - "127.0.0.1"
      - "--allowed-hosts"
      - "127.0.0.1:16287"
      - "--port"
      - "16287"
    url: "http://127.0.0.1:16287/mcp"
    autostart: true
    restart_policy: on-failure
    max_sessions: 6
    session_timeout: 30m
```

`--browser chromium` selects Playwright-managed Chrome for Testing; headless mode uses the matching headless shell. The default MCP browser channel is branded Chrome and fails when `/opt/google/chrome/chrome` is absent. Prefer channel selection over hardcoded `--executable-path`.

### Structured Fallback Suggestions

Availability failures are returned as visible tool results, not JSON-RPC
protocol errors. This follows the MCP SDK guidance that tool-originated errors
should use `CallToolResult{IsError: true}` so the LLM can see the failure and
self-correct.

When `classifyToolCallError` classifies an upstream failure as `AvailabilityError`,
`finishToolCall` (called by `makeProxyToolHandler`) converts it into a `CallToolResult` whose text content
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

Vision branches lifecycle ownership by transport type to avoid duplicate or orphaned processes:

**stdio transport:**
- `registry.Start()` skips the supervisor — no daemon-scoped subprocess is created
- `session.Manager` is the **sole lifecycle owner** for stdio subprocesses
- Each upstream session gets its own isolated subprocess via `SpawnSession()`
- Subprocesses are spawned **lazily** on first HTTP session connect, not at daemon startup
- Subprocesses are cleaned up by the session reaper (idle timeout / TTL) or on session close

**managed-http transport:**
- `registry.Start()` registers one daemon-scoped native HTTP child with the supervisor.
- A lifecycle generation monitor invalidates all old leases on process loss.
- The public listener remains on the configured Vision port while the backend binds only its configured loopback URL.
- Requests are rejected until initialize/delete readiness succeeds.
- `LeaseManager` owns application-idle expiry, capacity, in-flight guards, and bounded diagnostics.

**HTTP/SSE transport:**
- The configured URL identifies an externally owned service; Vision does not launch its command.
- Registry/supervisor state represents the proxy service, not a child process.
- Requests forward to the configured existing server endpoint.

This distinction matters for process accounting: stdio servers report `PID=0` in registry status because no daemon-scoped process exists. This is **correct and expected** — it means "the proxy endpoint is active, but subprocess lifecycle is managed per-session by the session manager."

### Transport Detection

When `transport` is not specified, Vision infers it from the config:

| Config Has | Inferred Transport |
|------------|-------------------|
| `command` | `stdio` |
| explicit `transport: managed-http` plus `command` and loopback `url` | `managed-http` |
| URL ending in `/mcp` | `http` |
| other URL | `sse` |

> **Current support:** Vision already supports HTTP and SSE proxy transports for remote/native MCP servers. Use `transport: http` or `transport: sse` with `url:` when you intentionally want Vision in front of an upstream MCP endpoint.

## Internal Packages

| Package | Responsibility |
|---------|---------------|
| `internal/server` | `Registry` — server registration; `Start()`/`Stop()` branch by transport type to skip supervisor for stdio servers; `ManagedServer` — per-server state |
| `internal/session` | `Manager` — per-session subprocess lifecycle (spawn, track, teardown), reaper |
| `internal/mcp` | `NewProxyHandler` for stdio; `ManagedHTTPGateway` for native HTTP; lease/classification/security middleware; per-port listeners |
| `internal/admin` | Admin MCP server on port 6275 with `vision_*` management tools |
| `internal/reachability` | Probe-backed reachability evidence and probe-worker scheduling |
| `internal/daemon` | Orchestrates config, registry, supervisor, and proxy setup |
| `internal/supervisor` | Suture process ownership and backend drain/readiness coordination for managed/native transports |

## References

- [ADR 0001: Managed Native HTTP for Playwright](adr/0001-managed-native-http-playwright.md)
- [MCP Spec: Transports](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
- [Go SDK: mcp package](https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk/mcp)
