# Archive: Migrate Vision to Go MCP SDK with Streamable HTTP and per-session subprocess isolation

**Change ID:** migrateVisionToGoMcpSdkWithStr
**Archived:** 2026-02-07T21:55:27.923Z
**Created:** 2026-02-07T18:37:15.878Z

## Tasks Completed

- ✅ Add github.com/modelcontextprotocol/go-sdk v1.0.0 dependency to go.mod and verify it builds
- ✅ Migrate admin MCP server from hand-rolled HTTP to SDK mcp.Server + StreamableHTTPHandler: register all vision_* tools via mcp.AddTool, serve on :6275
- ✅ Write integration tests for admin server: verify initialize handshake, tool listing, tool calls, and session lifecycle via StreamableHTTPHandler
- ✅ Create session-aware subprocess spawner: on each new upstream session, spawn a downstream subprocess via suture, connect SDK StdioClientTransport, perform initialize handshake, and map session-ID to process
- ✅ Implement session teardown: on DELETE /mcp or session timeout, gracefully terminate the downstream subprocess (SIGTERM then SIGKILL) and remove from session map
- ✅ Replace PortManager to use StreamableHTTPHandler per-server: update daemon.setupProxyForServer() to create SDK handler with getServer callback that invokes the session-aware spawner
- ✅ Implement server-to-client notification relay: route server-initiated messages from downstream stdout to the correct upstream SSE stream via SDK ServerSession
- ✅ Remove internal/bridge/ package (jsonrpc.go, stdio_http.go, and tests); remove dead mcp.Handler type; verify no remaining hand-rolled JSON-RPC code
- ✅ Write end-to-end tests: two concurrent clients connect to same server port, each gets independent session/subprocess, notifications route correctly, session teardown kills only that client's subprocess
- ✅ Verify full daemon lifecycle: start, connect clients, reload config (SIGHUP), session survival across reload, graceful shutdown (SIGTERM) kills all sessions and subprocesses
- ✅ Update docs/MCP_TRANSPORTS.md to reflect implemented state: remove aspirational language, document actual session lifecycle, SDK types used, and per-session subprocess model
- ✅ Add Streamable HTTP security middleware for all MCP endpoints: enforce bearer auth, strict Origin allowlist, and reject wildcard CORS
- ✅ Implement admission and timeout controls: max concurrent sessions/subprocesses, per-session idle timeout, absolute session TTL, and deterministic cleanup on timeout/DELETE
- ✅ Harden HTTP server defaults for Streamable handlers: localhost bind by default, request body size cap, read/header/idle timeouts, and rate limiting for POST/GET/DELETE /mcp
- ✅ Add interoperability regression tests for known Streamable edge cases (reconnect/session 404, malformed session header, request cancellation/timeouts) against go-sdk v1.0.0
- ✅ Add structured observability for session lifecycle and security events: log session create/delete/timeout, subprocess spawn/kill outcome, auth/origin rejection, and admission-limit denials with session/server identifiers
- ✅ Add backward-compatibility verification for daemon/health contracts: validate vision daemon start|status|reload|stop and /health,/healthz behavior remain unchanged after Streamable migration
- ✅ Run concurrency hardening checks for migrated handlers and session maps: add race-focused tests for concurrent initialize/call/delete flows and require go test -race on touched packages

## Specs Modified

- **mcp-streamable-transport**: 4 delta(s)
