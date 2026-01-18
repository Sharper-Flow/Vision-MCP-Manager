# MCP Transport Types

This document describes the MCP transport types that Vision must support to work with all MCP servers.

## Official MCP Transports (Per Spec)

The MCP specification defines **two standard transports**:

### 1. stdio (Standard I/O)

**How it works:**
- Client spawns the MCP server as a subprocess
- Communication via stdin/stdout using newline-delimited JSON
- Server writes logs to stderr (not protocol messages)

**Message format:**
```
{"jsonrpc":"2.0","method":"tools/list","id":1}\n
{"jsonrpc":"2.0","result":{"tools":[...]},"id":1}\n
```

**Characteristics:**
- One client per server process (1:1)
- Server exits when client disconnects
- Most common transport for local MCP servers
- Used by: `npx`/`uvx` servers, local binaries

**Go SDK Support:**
- Server: `mcp.StdioTransport{}`
- Client: `mcp.CommandTransport{Command: exec.Command("server")}`

### 2. Streamable HTTP (Recommended for Remote)

**How it works:**
- Server runs as HTTP service on a dedicated port
- Client sends JSON-RPC via HTTP POST to `/mcp`
- Server can respond with:
  - `application/json` - Single JSON response
  - `text/event-stream` - SSE stream for multiple messages

**Endpoints:**
- `POST /mcp` - Send requests/notifications, receive responses
- `GET /mcp` - Open SSE stream for server-initiated messages
- `DELETE /mcp` - Close session (optional)

**Headers:**
- `Mcp-Session-Id` - Session identifier (server-generated)
- `Mcp-Protocol-Version` - MCP protocol version

**Characteristics:**
- Multiple clients per server (N:1 or N:N with sessions)
- Supports streaming responses
- Supports server-to-client notifications
- Session management via headers

**Go SDK Support:**
- Server: `mcp.NewStreamableHTTPHandler(getServer, opts)`
- Client: `mcp.StreamableClientTransport{Endpoint: url}`

## Legacy Transport (Deprecated)

### HTTP + SSE (Backwards Compatibility Only)

**How it works:**
- Separate endpoints for send (POST) and receive (SSE GET)
- Client opens persistent SSE connection for server messages
- Client POSTs requests to a different endpoint

**Status:** Deprecated in favor of Streamable HTTP. Only use for backwards compatibility with older servers.

**Go SDK Support:**
- Server: `mcp.SSEHandler` (legacy)
- Client: `mcp.SSEClientTransport` (legacy)

## Vision's Role

Vision bridges these transports:

```
┌─────────────────────────────────────────────────────────────┐
│                    AI Agent (OpenCode)                      │
│                 Speaks: Streamable HTTP                     │
└─────────────────┬───────────────────────────────────────────┘
                  │ HTTP POST/GET to :6276/mcp
                  ▼
┌─────────────────────────────────────────────────────────────┐
│                      Vision Daemon                          │
│                                                             │
│   Exposes: Streamable HTTP (one port per server)            │
│   Bridges to: stdio subprocess                              │
│                                                             │
│   :6276/mcp ─────────► [time server] (stdio)                │
│   :6277/mcp ─────────► [context7 server] (stdio)            │
│   :6278/mcp ─────────► [filesystem server] (stdio)          │
└─────────────────────────────────────────────────────────────┘
```

## Server Configuration by Transport Type

### stdio Servers (Most Common)

```yaml
servers:
  time:
    port: 6276
    transport: stdio          # Default, can be omitted
    command: npx
    args: ["-y", "@anthropic/mcp-time"]
```

Vision:
1. Spawns subprocess with `command` + `args`
2. Communicates via stdin/stdout
3. Exposes as Streamable HTTP on allocated port

### Native HTTP Servers (Already Remote)

Some MCP servers already expose HTTP endpoints. Vision can proxy to them:

```yaml
servers:
  remote-server:
    port: 6279
    transport: http           # Proxy to existing HTTP server
    url: "http://internal-server:8080/mcp"
```

Vision:
1. Does NOT spawn a subprocess
2. Proxies HTTP requests to the upstream URL
3. Handles session mapping if needed

### Native SSE Servers (Legacy)

For older servers using the deprecated SSE transport:

```yaml
servers:
  legacy-server:
    port: 6280
    transport: sse            # Legacy SSE transport
    url: "http://legacy:8080"
```

Vision:
1. Connects to SSE endpoint for server messages
2. POSTs requests to the server
3. Re-exposes as Streamable HTTP

## Transport Detection

When `transport` is not specified, Vision infers it:

| Config Has | Inferred Transport |
|------------|-------------------|
| `command` | `stdio` |
| `url` with `/mcp` | `http` (streamable) |
| `url` without `/mcp` | `sse` (legacy) |

## Session Management

### stdio Sessions

- Each session can reuse the same process (stateless servers)
- Or spawn a new process per session (stateful servers)
- Configured via `stateful: true/false` in server config

### HTTP Sessions

- Vision generates `Mcp-Session-Id` for clients
- Maps client sessions to upstream server sessions
- Handles session expiration and cleanup

## Implementation Priority

1. **stdio** - Required. Most MCP servers use this.
2. **Streamable HTTP** - Required. This is what Vision exposes.
3. **HTTP proxy** - Nice to have. For remote/native HTTP servers.
4. **SSE (legacy)** - Low priority. Only for backwards compatibility.

## References

- [MCP Spec: Transports](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
- [Go SDK: Transport Types](https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk/mcp)
- [TypeScript SDK: Transports](https://github.com/modelcontextprotocol/typescript-sdk)
