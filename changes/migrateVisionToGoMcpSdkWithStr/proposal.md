# Migrate Vision to Go MCP SDK with Streamable HTTP and Per-Session Subprocess Isolation

## Why

Vision currently implements the MCP protocol from scratch using hand-rolled JSON-RPC types, a custom `StdioHTTPBridge`, and plain HTTP POST/response transport. This creates three problems:

1. **Spec violation**: A single `StdioHTTPBridge` is shared across all clients for a given MCP server. The MCP spec requires per-session isolation -- capability negotiation is per-session, server-initiated notifications must route to specific clients, and sampling/elicitation requests must target specific sessions. Sharing one stdio pipe across N clients entangles all of this.

2. **No Streamable HTTP**: Vision exposes plain HTTP POST/response endpoints. The MCP spec's Streamable HTTP transport (SSE streaming, `Mcp-Session-Id` headers, `GET /mcp` for server push, `DELETE /mcp` for session teardown) is documented in `docs/MCP_TRANSPORTS.md` but not implemented. OpenCode and other agents cannot receive server-initiated notifications.

3. **Maintenance burden**: ~620 lines of hand-rolled JSON-RPC parsing (`bridge/jsonrpc.go` + `bridge/stdio_http.go`) and ~300 lines of HTTP handler code (`mcp/handler.go`) replicate what the official Go MCP SDK provides with spec compliance, session management, and Streamable HTTP built in.

## What Changes

Replace Vision's custom MCP implementation with the official Go MCP SDK (`github.com/modelcontextprotocol/go-sdk v1.0.0`):

- **Upstream (client-facing)**: Replace `mcp.Handler` + `PortManager` with SDK's `StreamableHTTPHandler` per server port, supporting SSE streaming, session IDs, and server-to-client push.
- **Downstream (subprocess-facing)**: Replace `StdioHTTPBridge` with SDK's `StdioClientTransport` + `Client`, spawning one subprocess per client session (strict 1:1 isolation).
- **Admin server**: Migrate the admin MCP server (`:6275`) from hand-rolled HTTP to an SDK `mcp.Server` served via `StreamableHTTPHandler`.
- **Remove**: `internal/bridge/` package (jsonrpc.go, stdio_http.go), custom `mcp.Handler`, hand-rolled JSON-RPC types.
- **Keep**: Port-per-server isolation model, suture-based process supervision, config/catalog/registry packages.

## Architecture

### Current (shared bridge, plain HTTP)

```
OpenCode-A ──POST /mcp──> :6276 ──> mcp.Handler ──> StdioHTTPBridge ──> single subprocess
OpenCode-B ──POST /mcp──> :6276 ──/                (shared pipe)    ──/
```

### Target (per-session, Streamable HTTP)

```
OpenCode-A ──Streamable HTTP──> :6276/mcp ──> StreamableHTTPHandler ──> Session A ──> stdio subprocess A
OpenCode-B ──Streamable HTTP──> :6276/mcp ──/                       ──> Session B ──> stdio subprocess B
```

### SDK Integration Points

| Vision Component         | Current                              | Target                                         |
| ------------------------ | ------------------------------------ | ---------------------------------------------- |
| Upstream transport       | `mcp.Handler` (plain HTTP POST)      | `mcp.StreamableHTTPHandler`                    |
| Downstream transport     | `bridge.StdioHTTPBridge` (raw pipes) | `mcp.StdioClientTransport` + `mcp.Client`      |
| JSON-RPC types           | `bridge.Request/Response/Error`      | SDK-provided (removed)                         |
| Session management       | None                                 | SDK `ServerSession` + `Mcp-Session-Id` header  |
| Admin server             | Hand-rolled `admin.Server`           | `mcp.Server` + `StreamableHTTPHandler`         |
| Protocol handshake       | `bridge.Initialize()` (custom)       | SDK `Client.Connect()` (automatic)             |
| Server notifications     | Logged and dropped                   | Relayed to upstream SSE stream                 |
| Process supervision      | suture (unchanged)                   | suture (unchanged)                             |

### Session Lifecycle (New)

1. Client sends POST to `:6276/mcp` with `initialize` request
2. SDK's `StreamableHTTPHandler` calls `getServer(r)` callback
3. Vision's callback spawns a new subprocess via suture, creates `StdioClientTransport`, connects `mcp.Client`
4. SDK creates `ServerSession` with unique `Mcp-Session-Id`, returns in response header
5. All subsequent requests from this client include `Mcp-Session-Id` and route to the same subprocess
6. Client sends `DELETE /mcp` or session times out -- Vision kills the subprocess, suture cleans up

### Port-Per-Server Model (Retained)

Each MCP server definition in `servers.yaml` continues to get a dedicated port:
- `:6275` -- Admin MCP server (Vision management tools)
- `:6276` -- Server A (e.g., context7)
- `:6277` -- Server B (e.g., kagi)
- etc.

The `PortManager` is retained but simplified: instead of creating a `mcp.Handler` + bridge per port, it creates a `StreamableHTTPHandler` per port.

## Success Criteria

1. [ ] Vision daemon starts and serves MCP over Streamable HTTP on per-server ports, with `Mcp-Session-Id` headers in responses
2. [ ] Each client session (`initialize` through `DELETE /mcp`) maps to exactly one downstream subprocess; subprocess is terminated when session ends
3. [ ] Server-initiated notifications from downstream subprocesses are relayed to the correct upstream client's SSE stream
4. [ ] The admin MCP server (`:6275`) serves its tools via `StreamableHTTPHandler` with session support
5. [ ] The `internal/bridge/` package is removed; no hand-rolled JSON-RPC types remain
6. [ ] `go.mod` adds `github.com/modelcontextprotocol/go-sdk` as a dependency
7. [ ] Existing CLI commands (`vision daemon start/stop/reload/status`, `vision health`) continue to work
8. [ ] Configuration format (`servers.yaml`) remains backward-compatible; no user-facing config changes required
9. [ ] All existing tests pass or are updated; new tests cover session lifecycle (create, use, teardown, timeout)
10. [ ] Multiple OpenCode instances can connect to the same MCP server port simultaneously with independent sessions
11. [ ] Streamable MCP endpoints enforce bearer authentication and strict Origin allowlist (no wildcard CORS)
12. [ ] Vision enforces explicit session and subprocess limits (max concurrent sessions, idle timeout, absolute TTL)
13. [ ] HTTP hardening defaults are in place (`127.0.0.1` bind by default, body size limits, read/header/idle timeouts, rate limiting)
14. [ ] Interop regression tests cover streamable reconnect/session-404, malformed session headers, and cancellation/timeout behavior

## Affected Code

### Removed
- `internal/bridge/jsonrpc.go` -- Hand-rolled JSON-RPC types
- `internal/bridge/stdio_http.go` -- Custom StdioHTTPBridge
- `internal/bridge/bridge_test.go` -- Bridge tests
- `internal/bridge/stdio_http_test.go` -- Bridge tests

### Heavily Modified
- `internal/mcp/handler.go` -- Replace `Handler` with SDK `StreamableHTTPHandler` wrapper; update `PortManager`
- `internal/admin/server.go` -- Replace hand-rolled MCP server with SDK `mcp.Server`
- `internal/admin/tools.go` -- Adapt tool registration to SDK's `mcp.AddTool` pattern
- `internal/daemon/daemon.go` -- Replace `setupProxyForServer()` with session-aware subprocess spawning
- `internal/supervisor/process.go` -- Adapt process management for per-session lifecycle
- `go.mod` -- Add `github.com/modelcontextprotocol/go-sdk` dependency

### Unchanged
- `internal/config/` -- Config loading, schema (already has `stateful`/`max_sessions` fields)
- `internal/catalog/` -- Server catalog
- `internal/server/registry.go` -- Server registry (lifecycle events may need minor updates)
- `internal/supervisor/supervisor.go` -- Suture supervisor wrapper
- `internal/supervisor/health.go` -- Health checking
- `internal/daemon/pidfile.go` -- PID file management
- `internal/daemon/shutdown.go` -- Signal handling
- `cmd/vision/main.go` -- CLI entry point

## Constraints

- **MUST**: Use the official Go MCP SDK (`github.com/modelcontextprotocol/go-sdk`) v1.0.0 or later
- **MUST**: Maintain backward-compatible `servers.yaml` configuration format
- **MUST**: Keep port-per-server isolation model
- **MUST**: Support strict 1:1 session-to-subprocess mapping (no shared processes)
- **MUST**: Preserve suture-based process supervision for subprocess lifecycle
- **MUST NOT**: Introduce SSE/WebSocket for admin API without also serving plain JSON responses (SDK supports `JSONResponse` mode)
- **MUST NOT**: Change CLI command interface (`vision daemon start/stop/status/reload`)
- **SHOULD**: Preserve existing health check endpoints (`/health`, `/healthz`)
- **SHOULD**: Implement daemon-managed session timeout controls (idle + absolute TTL), since go-sdk v1.0.0 Streamable options do not provide built-in session timeout configuration
- **SHOULD**: Support SDK's `Stateless` mode for future optimization of stateless servers

## Research Validation

## Summary

External validation of this change against MCP spec and go-sdk documentation confirms the core direction is correct (official SDK + per-session isolation). The main gaps are security hardening tasks and explicit runtime limits for streamable sessions/subprocesses.

## Validated Decisions

- Adopt `github.com/modelcontextprotocol/go-sdk` instead of custom JSON-RPC/transport code
- Use strict 1:1 upstream session to downstream subprocess as the initial implementation
- Keep port-per-server topology as the near-term operational default

## Simplification Opportunities

| Current | Simpler Alternative | Effort | Recommendation |
|---------|---------------------|--------|----------------|
| Consider early shared-process mode for stateless servers | Ship strict 1:1 first, defer optimization | Low | Defer shared-process mode to follow-up after correctness baseline |
| Keep custom security behavior per handler | Centralize auth/origin/rate/body/timeouts in shared middleware | Medium | Implement one hardened middleware stack for admin + server handlers |
| Future pressure to consolidate ports now | Keep port-per-server now; add optional gateway mode later | Low | Preserve current topology and revisit only if ops friction grows |

## Concerns

- go-sdk streamable transport has active edge-case issues across versions; migration should include targeted regression tests around reconnect, cancellation, and session invalidation
- Per-session subprocess model can cause resource exhaustion without admission controls
- Current Vision codebase uses permissive wildcard CORS and bind-all listeners for managed server ports; this must not carry forward unchanged

## Anti-Patterns Detected

- Using `Mcp-Session-Id` as an auth mechanism (must remain correlation only)
- Relying on wildcard CORS (`*`) for MCP endpoints that may be browser-reachable
- Assuming SDK defaults alone provide full DoS protection without outer server limits

## Over-Engineering Flags

- Implementing stateless shared-process optimization during initial migration would add routing complexity before establishing a correct baseline
- Introducing single-port gateway routing in the same change would increase scope and risk with limited immediate value

## Detailed Findings

### SDK Adoption
**Current:** Replace hand-rolled protocol code with go-sdk v1.0.0.
**Research:** Official SDK is stable at v1.0.0 and designed for streamable sessions; this reduces protocol drift and maintenance burden.
**Simpler Option:** None materially better than official SDK for long-term maintenance.
**Recommendation:** Keep this decision and pin/validate on v1.0.0 with interop tests.
**Sources:** https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.0.0, https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk/mcp

### Session Mapping Strategy
**Current:** Strict 1:1 upstream session -> downstream subprocess.
**Research:** This is the simplest spec-aligned baseline, especially for server-initiated requests/notifications.
**Simpler Option:** Shared-process stateless mode is simpler operationally but loses protocol capabilities and increases routing complexity when mixed with full-session semantics.
**Recommendation:** Keep strict 1:1 now; evaluate optional stateless mode in a follow-up change.
**Sources:** https://modelcontextprotocol.io/specification/2025-06-18/basic/transports, https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/v1.0.0/docs/protocol.md

### Endpoint Topology
**Current:** Retain port-per-server.
**Research:** Port-per-server is operationally boring and clear for localhost daemon usage; single-port routing is a valid later option when ingress management complexity dominates.
**Simpler Option:** None required now.
**Recommendation:** Keep port-per-server in this change and document optional gateway mode for future migration.
**Sources:** https://modelcontextprotocol.io/specification/2025-06-18/basic/transports, https://github.com/modelcontextprotocol/go-sdk/blob/main/mcp/streamable.go

### Security Hardening
**Current:** Proposal lacked explicit auth/origin/rate/limits requirements.
**Research:** MCP guidance requires request verification and Origin validation for HTTP transports. Session IDs are not auth. SDK does not replace perimeter hardening.
**Simpler Option:** Apply one shared middleware policy to all streamable handlers.
**Recommendation:** Add concrete hardening criteria and implementation tasks for auth, origin checks, body/time limits, rate limits, and session/subprocess caps.
**Sources:** https://modelcontextprotocol.io/specification/draft/basic/security_best_practices, https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/v1.0.0/auth/auth.go

## Action Items

- [ ] Add security middleware task (auth + origin allowlist + no wildcard CORS)
- [ ] Add admission/timeout controls task (session + subprocess limits)
- [ ] Add HTTP hardening defaults task (bind, body limits, timeouts, rate limits)
- [ ] Add streamable interop regression task for reconnect/cancel/session-invalid cases
- [ ] Keep shared-process stateless optimization out of this change scope

## Prep Gap Analysis Notes

### Cross-Cutting Concerns Coverage

- **Error handling**: Covered by teardown, timeout, and streamable interop regression tasks.
- **Logging/monitoring**: Expanded with explicit observability task for session/security lifecycle events.
- **Validation**: Covered by malformed-session interoperability tests and admission-control tasks.
- **Security**: Covered by auth/origin/CORS and HTTP hardening tasks.
- **Performance/concurrency**: Expanded with race-focused concurrent session test task.
- **Config compatibility**: Covered by backward-compatibility daemon and health contract verification task.

### Out of Scope (This Change)

- **Caching**: Not applicable to protocol transport migration.
- **Persistence/data model**: Session/process state remains in-memory lifecycle state only.
- **i18n/l10n**: Not applicable for daemon transport internals.
- **Privacy**: No new user data classes introduced beyond existing request metadata.

## Confidence

- High: SDK adoption, strict 1:1 session model, and port-per-server baseline
- Low: Resource behavior under high concurrency until limits/benchmarks are implemented and tested

## Impact

- **Breaking changes**: Yes -- clients currently sending plain `POST /mcp` with `application/json` responses will now receive Streamable HTTP responses (SSE or JSON depending on SDK negotiation). OpenCode's remote MCP transport already supports Streamable HTTP, so this should be transparent.
- **New dependency**: `github.com/modelcontextprotocol/go-sdk v1.0.0` (official, well-maintained)
- **Resource impact**: Per-session subprocess spawning increases memory/process count proportional to active sessions. Session timeout (5m default) provides natural cleanup.
- **Affected specs**: None (no specs exist yet; this change establishes the architectural baseline)
