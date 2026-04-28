# Mcp Streamable Transport

> **Version:** 1.0.0
> **Updated:** 2026-02-07

## Purpose

Capability: Mcp Streamable Transport

## Requirements

### Serve MCP over streamable HTTP with session semantics

**ID:** `rq-mcpstr01` | **Priority:** **[MUST]**

Vision MUST expose MCP endpoints using the go-sdk StreamableHTTPHandler per server port. The server MUST support initialize, GET event streaming, and session teardown via DELETE while honoring Mcp-Session-Id semantics.

**Tags:** `mcp`, `transport`, `streamable-http`

#### Scenarios

**Initialize returns a server session id** (`rq-mcpstr01.1`)

**Given:**
- Vision daemon is running with a configured MCP server port
- A client sends a valid initialize request

**When:** The initialize request is handled by the streamable endpoint

**Then:**
- Response is successful and includes Mcp-Session-Id
- The session id can be used by follow-up requests

**Session teardown invalidates further use** (`rq-mcpstr01.2`)

**Given:**
- A client has an active streamable session
- The client sends DELETE for that session

**When:** A later request reuses the deleted session id

**Then:**
- The request is rejected as an unknown or expired session
- No previous session resources remain attached

---

### Enforce subprocess isolation per MCP session (stateful) or shared subprocess (stateless)

**ID:** `rq-mcpstr02` | **Priority:** **[MUST]**

Vision MUST maintain strict 1:1 mapping between each upstream MCP session and a supervised downstream subprocess when the server is configured with `stateful: true`. When the server is configured with `stateful: false` (default), multiple upstream sessions MAY share a single downstream subprocess managed by a SharedSessionManager. Session teardown or timeout MUST terminate only the owning subprocess (stateful) or decrement the shared refcount (stateless).

**Tags:** `sessions`, `supervision`, `isolation`

#### Scenarios

**Concurrent clients receive isolated subprocesses** (`rq-mcpstr02.1`)

**Given:**
- A server is configured with `stateful: true`
- Two clients connect concurrently to the same server port
- Both establish independent MCP sessions

**When:** Each client performs tool calls and receives notifications

**Then:**
- Each session maps to a different subprocess
- Notifications and responses are routed to the correct session only

**Session timeout cleans up deterministicly** (`rq-mcpstr02.2`)

**Given:**
- A session exceeds idle timeout or absolute TTL

**When:** Timeout handling executes

**Then:**
- The owned subprocess is terminated gracefully then forcefully if needed
- The session mapping is removed and cannot be reused

**Concurrent clients share a single subprocess for stateless servers** (`rq-mcpstr02.3`)

**Given:**
- A server is configured with `stateful: false` (default)
- Two clients connect concurrently to the same server port
- Both establish independent MCP sessions

**When:** Both clients perform tool calls

**Then:**
- Both sessions route tool calls through the same shared downstream subprocess
- The shared subprocess is alive as long as at least one upstream session is active
- When the last upstream session disconnects, the shared subprocess is eligible for teardown

---

### Apply security and admission controls to all streamable endpoints

**ID:** `rq-mcpstr03` | **Priority:** **[MUST]**

Vision MUST require bearer authentication when configured, enforce a strict Origin allowlist, reject wildcard CORS, and enforce configured limits for max concurrent sessions, request sizes, and HTTP timeouts on streamable MCP endpoints.

**Tags:** `security`, `limits`, `hardening`

#### Scenarios

**Unauthorized and disallowed origin requests are denied** (`rq-mcpstr03.1`)

**Given:**
- A request is missing valid credentials or has a non-allowlisted Origin

**When:** The request hits a streamable MCP endpoint

**Then:**
- The request is rejected
- No session or subprocess is created

**Admission limits reject excess sessions** (`rq-mcpstr03.2`)

**Given:**
- Configured max concurrent sessions is reached

**When:** Another initialize request is submitted

**Then:**
- The request is rejected with a clear limit error
- Existing sessions remain healthy

---

### Preserve operational compatibility and emit migration observability

**ID:** `rq-mcpstr04` | **Priority:** **[SHOULD]**

Vision MUST preserve existing daemon command and health endpoint behavior while adding structured lifecycle/security logs for session creation, teardown, timeout, subprocess spawn/kill, and request denials.

**Tags:** `compatibility`, `operations`, `observability`

#### Scenarios

**Daemon and health contracts remain stable** (`rq-mcpstr04.1`)

**Given:**
- The Streamable migration is deployed

**When:** Operators run daemon lifecycle commands and health checks

**Then:**
- Daemon start/status/reload/stop remain functional
- /health and /healthz behavior remains backward-compatible

**Lifecycle and denial events are observable** (`rq-mcpstr04.2`)

**Given:**
- Sessions are created, timed out, denied, and torn down during normal operation

**When:** System logs are inspected

**Then:**
- Logs include session and server identifiers for lifecycle and denial events
- Operators can correlate subprocess lifecycle with session lifecycle

---
