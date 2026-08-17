# Vision Configuration Reference

This document provides a complete reference for Vision's configuration files.

## Configuration Files

Vision uses two types of configuration:

1. **Server Registry** (`~/.config/vision/servers.yaml`) - Machine-wide server definitions
2. **Client Configs** (`opencode.json[c]`, `.opencode/opencode.json[c]`, `.claude/settings.json`) - Per-project or global client settings

## Server Registry

### Location

The server registry is located at `~/.config/vision/servers.yaml` by default.

You can override this with:
- `VISION_CONFIG` environment variable
- `--config` CLI flag

### Schema

```yaml
# Server definitions
servers:
  <server-name>:
    # HTTP port for this server (required, range: 6276-6325)
    port: 6276
    
    # Transport type (auto-detected if not specified)
    transport: stdio
    
    # For stdio transport: command to execute
    command: npx
    
    # Command arguments
    args:
      - "-y"
      - "@anthropic/mcp-time"
    
    # Environment variables passed to the server process
    env:
      API_KEY: "your-api-key-here"
      DEBUG: "true"
    
    # Start on daemon launch
    autostart: true
    
    # Restart policy: always, on-failure, never
    restart_policy: on-failure
    
    # Maximum automatic restarts in a five-minute window (default: 5).
    # Set 0 to disable automatic restarts while keeping the chosen policy.
    max_restarts: 5
    
    # Idle session timeout (default: 5m).
    # When a session is reaped, the next tool call automatically
    # respawns a fresh subprocess — no error is returned to the agent.
    # Increase for servers where agents pause between calls (e.g. 30m).
    session_timeout: 5m
    
    # Absolute session TTL (default: 0, no limit)
    session_ttl: 1h
    
    # Maximum concurrent sessions per server (default: 100)
    max_sessions: 100

    # Optional stronger resilience defaults for upstream network-backed tools.
    # Current values: networked
    availability_profile: networked

    # Explicitly safe read-only tools that may share results across sessions.
    shared_read_only_tools:
      - kagi_search_fetch
      - kagi_summarizer

    # Cache successful shared read-only results for this long (default: 10s for networked)
    shared_result_cache_ttl: 10s

    # Max cached shared results per server (default: 128 for networked)
    shared_result_cache_size: 128

    # Max concurrent downstream tool calls per server (default: 4 for networked)
    max_in_flight_requests: 4

# Security settings (applied to all Streamable HTTP endpoints)
security:
  # Bearer token for authentication (optional)
  bearer_token: "your-secret-token"
  
  # Allowed origins for CORS (optional, wildcards rejected)
  allowed_origins:
    - "http://localhost:3000"

# Global supervision settings
supervision:
  # Reachability probe cadence for managed servers (default: 30s;
  # end-to-end probes run at 10x this interval)
  health_check_interval: 30s

  # Graceful shutdown timeout (default: 10s)
  shutdown_timeout: 10s
  
  # Initial restart delay (default: 1s)
  restart_delay: 1s
  
  # Maximum restart delay (default: 60s)
  max_restart_delay: 60s
```

Reachability probing covers `stdio` and `managed-http` servers, whose listeners
Vision owns. `http` and `sse` servers are externally hosted through `url`; Vision
does not front them with a listener, so their status is reported from process
and backend state rather than reachability evidence.

### Managed process restart policy

Vision applies restart settings to each managed process. The first launch does
not consume the restart budget.

| `restart_policy` | Clean exit | Failed exit |
|---|---|---|
| `never` | Stop | Fail without restarting |
| `on-failure` | Stop | Restart while budget remains |
| `always` | Restart while budget remains | Restart while budget remains |

Each automatic restart waits `restart_delay * 2^(attempt-1)`, capped by
`max_restart_delay`. `max_restarts` limits automatic restarts in a rolling
five-minute window. When the budget is exhausted, status becomes `error`, the
raw process state becomes `failed`, and the reason names restart exhaustion.
`vision_restart` creates a fresh managed process and resets this budget.

For `managed-http`, Vision also stores a private ownership lease for each
process generation. After an abrupt daemon exit, the next Linux daemon reclaims
the old process group only when every live member matches that lease. Unknown,
mixed, or unverifiable groups are never killed; startup fails with a conflict.
Automatic managed-process recovery is not available on non-Linux systems and
fails closed before publishing the backend as running.

### Multi-agent / ADV workloads

The default `session_timeout` is five minutes. That is appropriate for short,
interactive calls, but code-intelligence agents often pause while they plan,
delegate work, run tests, or wait for another sub-agent. If an idle session is
reaped during that pause, the next tool call transparently spawns a fresh
downstream process. No error reaches the agent, but startup and tool discovery
add latency.

When several ADV sub-agents resume together, a short timeout can also create a
burst of simultaneous respawns. Use a longer timeout for MCP servers that are
reused throughout a change:

```yaml
servers:
  context7:
    # ...command, args, env, and port...
    session_timeout: 30m

  lgrep:
    # ...command, args, env, and port...
    session_timeout: 30m
```

Keep the five-minute default for rarely used or resource-heavy servers. A
longer timeout trades resident process resources for lower tail latency; it is
not a keepalive and does not bypass `session_ttl` or `max_sessions`.

### Transport Types

Vision uses **Streamable HTTP** as the upstream transport for all servers.

| Transport | When to Use | Configuration |
|-----------|-------------|---------------|
| `stdio` | Most command-based MCP servers | Set `command` and optional `args`; choose shared or `stateful` mode |
| `managed-http` | Vision-owned native HTTP process requiring lifecycle/admission control | Set explicit transport, `command`, and exact loopback `/mcp` `url` |
| `http` | Externally owned native Streamable HTTP server | Set `url` ending in `/mcp` |
| `sse` | Externally owned legacy SSE server | Set legacy URL |

Transport is auto-detected for ordinary servers: `command` implies `stdio`, a URL ending in `/mcp` implies `http`, and another URL implies legacy `sse`. `managed-http` is always explicit because it intentionally owns both a command and URL.

> **Note:** `stateful: true` gives each stdio client an isolated subprocess. Shared stdio is the default for ordinary stateless tools.

### Example Configurations

#### Basic stdio server (most common)

```yaml
servers:
  time:
    port: 6276
    command: npx
    args: ["-y", "@anthropic/mcp-time"]
    autostart: true
```

#### Server with API key

```yaml
servers:
  kagi:
    port: 6279
    command: uvx
    args: ["kagimcp"]
    autostart: true
    availability_profile: networked
    shared_read_only_tools: ["kagi_search_fetch", "kagi_summarizer"]
```

> **Best Practice:** Put API keys directly in `servers.yaml`. This file is local to your machine (`~/.config/vision/servers.yaml`) and is never committed to version control. Hardcoding keys avoids environment variable resolution issues and ensures the daemon always has access to credentials regardless of how it was started.
>
> Environment variable expansion (`${CONTEXT7_API_KEY}`) is supported but not recommended—keys may fail to resolve depending on how the daemon is launched.

#### Managed Playwright (system default)

Use one Vision-supervised native HTTP process with six independent BrowserContexts:

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

Rules:

- Keep external port `6287`; OpenCode remains pointed at `http://127.0.0.1:6287/mcp`.
- Internal port `16287` is loopback-only and must match URL, `--port`, and exact `--allowed-hosts` host:port.
- `--browser chromium` maps to Playwright-managed Chrome for Testing. With `--headless`, Playwright selects its matching headless shell. Do not rely on branded Chrome at `/opt/google/chrome/chrome`, and do not hardcode `--executable-path` unless version-managed channel selection is impossible.
- `--isolated` keeps profiles ephemeral and creates a separate BrowserContext for every native HTTP session. Never add `--shared-browser-context`.
- Capacity is six. Idle means 30 minutes without application requests; SSE reconnects and keepalives do not refresh it.

After editing:

```bash
vision config validate
vision daemon reload
vision daemon status
```

The admin `vision_list` result and `GET /v1/servers/playwright` include
`session_lifecycle`: backend state, capacity, up to 100 active rows, up to 1,000
closed rows, omitted counts, safe ID, age, application idle, in-flight, SSE
count, and lifecycle reason. Raw MCP session IDs are never exposed.

`vision_list.status` is the effective managed status. The V1 endpoint preserves
its existing raw `state` field and adds `process_state`, `effective_status`, and
an optional `effective_reason`. A recycling or restarting backend reports
effective `error`, never `running`; a ready backend with a running process
reports `running`.

#### Native HTTP server (proxy mode)

```yaml
servers:
  remote-api:
    port: 6278
    transport: http
    url: "http://internal-server:8080/mcp"
    headers:
      Authorization: "Bearer ${API_TOKEN}"
```

#### Session limits

```yaml
servers:
  expensive-server:
    port: 6279
    command: npx
    args: ["-y", "@example/expensive-mcp"]
    max_sessions: 5
    session_timeout: 10m
    session_ttl: 1h
```

### Slot Groups

Slot groups remain supported for process-per-session servers. For Playwright they are now the **stateful-stdio rollback architecture**, not the default: managed native HTTP provides per-client BrowserContexts inside one supervised browser process with lower process overhead and application-activity leases.

Slot groups let you define a pool of identical MCP servers behind a single virtual endpoint. Vision transparently routes each incoming session to the least-loaded healthy slot — agents connect to one port and never know how many backing processes exist.

**When to use:** Servers that require process-per-session isolation and cannot provide safe native HTTP session isolation. For Playwright this is a rollback option; managed native HTTP is the normal default.

#### How it works

1. You declare a `slot_groups` section in `servers.yaml` with a template name, base port, count, and a group port.
2. Vision expands each group into `count` flat server entries at load time, naming them `<template>-1`, `<template>-2`, …, `<template>-<count>`.
3. Each slot gets a sequential port starting from `base_port`.
4. A virtual listener on `group_port` receives all MCP traffic. The multiplexer selects the slot with the fewest active sessions (pending + established), skipping unhealthy or at-capacity slots. Ties break by slot index (lower wins).
5. Once an upstream session is bound to a slot, it stays on that slot for its entire lifetime (sticky routing).

#### Schema

```yaml
slot_groups:
  <group-name>:
    # Prefix for synthesized server names (<template>-1, <template>-2, ...).
    # Must not collide with any key in the servers: map.
    template: <string>           # required

    # Port assigned to the first slot. Subsequent slots get
    # base_port+1, base_port+2, etc. All must fall within 6276-6325.
    base_port: <int>             # required

    # Number of identical slots to create. Minimum: 2.
    count: <int>                 # required, >= 2

    # Port for the virtual group endpoint that agents connect to.
    # Also must be within 6276-6325 and not overlap with any
    # slot ports or other servers.
    group_port: <int>            # required

    # Defaults applied to every synthesized server. Supports all
    # ServerConfig fields except port (set automatically).
    defaults:                    # optional
      command: npx
      args: ["-y", "@anthropic/mcp-server"]
      autostart: true
      stateful: true
      max_sessions: 1
      # ... any valid ServerConfig field
```

#### Example: 8-slot Playwright pool

This creates 8 Playwright MCP processes (`playwright-1` through `playwright-8` on ports 6284–6291) behind a single virtual endpoint on port 6283. Agents connect to `http://localhost:6283/mcp` and Vision routes each session to the least-loaded slot.

```yaml
slot_groups:
  playwright:
    template: playwright
    base_port: 6284
    count: 8
    group_port: 6283
    defaults:
      command: npx
      args:
        - "-y"
        - "@playwright/mcp@0.0.77"
        - "--browser"
        - "chromium"
        - "--headless"
        - "--isolated"
      autostart: true
      stateful: true
      max_sessions: 1
      session_timeout: 30m
      session_ttl: 2h
```

In the OpenCode client config, point Playwright at the virtual group port:

```json
{
  "mcp": {
    "playwright": {
      "type": "remote",
      "url": "http://localhost:6283/mcp"
    }
  }
}
```

#### Migration: Playwright stdio or slot group to managed HTTP

1. Preserve the existing external `playwright` port (`6287`).
2. Select an unused loopback internal port, conventionally `16287`.
3. Replace the Playwright entry with the canonical `managed-http` configuration above.
4. Remove the old Playwright slot-group entry only after confirming no unrelated client uses its virtual or slot ports.
5. Run the pinned real verification before deployment:

   ```bash
   VISION_PLAYWRIGHT_REAL_TEST=1 go test -race ./internal/integration \
     -run '^TestManagedPlaywrightNativeHTTP$' -count=1
   ```

6. Run `vision config validate`, reload Vision, and confirm backend state `ready` before browser work.
7. Observe lifecycle/admission/restart diagnostics during the 48-hour canary.

No OpenCode source, SDK, wrapper, dependency, or MCP endpoint change is required.

#### Rollback: managed HTTP to isolated stateful stdio

Target: restore service on port `6287` in under 10 minutes.

The pinned isolated rehearsal `TestPlaywrightStatefulStdioRollback` reached a navigable browser through the rollback path in **1.921 seconds** on the target host.

```yaml
servers:
  playwright:
    port: 6287
    transport: stdio
    command: npx
    args: ["-y", "@playwright/mcp@0.0.77", "--browser", "chromium", "--headless", "--isolated"]
    stateful: true
    max_sessions: 6
    session_timeout: 30m
    autostart: true
```

1. Restore the isolated stateful-stdio entry above (or the pre-change backup).
2. Run `vision config validate`.
3. Run `vision daemon reload`.
4. Confirm `vision daemon status` and initialize one Playwright session through `http://127.0.0.1:6287/mcp`.
5. Retain the failed managed configuration and diagnostics for investigation.

Do not roll back to shared stdio, `--shared-browser-context`, profile/lock deletion, or periodic restarts.

Rehearse independently with:

```bash
VISION_PLAYWRIGHT_REAL_TEST=1 go test ./internal/integration \
  -run '^TestPlaywrightStatefulStdioRollback$' -count=1 -v
```

#### Routing semantics

| Behavior | Detail |
|----------|--------|
| **Selection** | Least-loaded healthy slot (sessions + pending in-flight initializations) |
| **Tie-breaking** | Lowest slot index wins |
| **Stickiness** | An upstream session is bound to one slot for its full lifetime |
| **Health awareness** | Slots whose backing process is not `running` are skipped; slots that recently failed initialization are quarantined for 2 seconds |
| **Capacity** | If every bounded slot has reached its `max_sessions` and no unlimited slot exists, new sessions receive a max-sessions error |
| **Direct access** | Individual slot ports remain accessible — you can connect directly to `playwright-3` on port 6286 for debugging |

#### Observability

**MCP tool** — Call `vision_slot_status` from the admin MCP server (port 6275) to see per-slot session counts:

```json
{
  "groups": [
    {
      "group_name": "playwright",
      "slot_count": 8,
      "slots": [
        { "name": "playwright-1", "port": 6284, "active_sessions": 1, "max_sessions": 1 },
        { "name": "playwright-2", "port": 6285, "active_sessions": 0, "max_sessions": 1 }
      ]
    }
  ]
}
```

**HTTP endpoints** — The admin HTTP surface also exposes slot status:

| Endpoint | Returns |
|----------|---------|
| `GET /v1/slots` | All slot groups with per-slot detail |
| `GET /v1/slots/{group}` | Single group detail (404 if unknown) |

#### Validation rules

- `count` must be ≥ 2.
- Synthesized server names (`<template>-1`, etc.) must not collide with existing `servers:` keys.
- All generated ports (`base_port` through `base_port + count - 1`) must be unique and within 6276–6325.
- `group_port` must not overlap with any slot port or other server port.

#### Save / round-trip behavior

When `vision config save` (or any write-back path) persists the config, it preserves the `slot_groups:` section and omits the synthesized slot entries. On the next load, expansion runs again. This means you can safely edit the `slot_groups:` block and reload — your hand-written `servers:` entries are never overwritten.

## Client Configurations

### Claude Code

Create `.claude/settings.json` in your project or `~/.config/Claude/Claude.json` globally:

```json
{
  "mcpServers": {
    "time": {
      "type": "streamable-http",
      "url": "http://localhost:6276/mcp"
    },
    "context7": {
      "type": "streamable-http",
      "url": "http://localhost:6277/mcp"
    }
  }
}
```

Configure Claude Code manually in the Claude Code settings file; the Vision CLI
does not generate client configuration.

### OpenCode

Create `opencode.json` or `opencode.jsonc` in your project, optionally under
`.opencode/`, or use `~/.config/opencode/opencode.jsonc` globally:

```json
{
  "mcp": {
    "time": {
      "type": "remote",
      "url": "http://localhost:6276/mcp",
      "enabled": true
    },
    "context7": {
      "type": "remote",
      "url": "http://localhost:6277/mcp",
      "enabled": true
    },
    "gh_grep": {
      "type": "remote",
      "url": "https://mcp.grep.app",
      "enabled": true,
      "timeout": 20000
    }
  }
}
```

The OpenCode plugin's `vision_init` tool reconciles an existing OpenCode JSON
configuration instead of blindly replacing it. When `path` is omitted, the
plugin detects a sole existing recognized project config: root
`opencode.jsonc`/`opencode.json`, or `.opencode/opencode.jsonc`/
`.opencode/opencode.json`. If none exists, it uses the project root's
`opencode.jsonc`; if multiple recognized configs exist, an explicit path is
required. Explicit relative plugin paths resolve against the project directory.
Existing JSONC comments and trailing commas are preserved during reconciliation.
Direct Admin MCP callers must provide an absolute `path`; stale Vision-managed
MCP endpoint mappings can then be repaired while unrelated configuration keys
are preserved.

## Environment Variable Expansion

Vision supports `${VAR}` and `${VAR:-default}` expansion in `servers.yaml`:

```yaml
servers:
  my-server:
    env:
      # Expand from .env file or shell environment
      API_KEY: "${MY_API_KEY}"
      
      # With default value if not set
      DEBUG: "${DEBUG:-false}"
```

### Secrets Management

Store API keys and tokens in `~/.config/vision/.env` (dot-prefixed, per dotenv convention). Vision loads this file before parsing the config, so `${VAR}` expansion works regardless of how the daemon is started — foreground, background, or systemd.

After updating `.env`, prefer restarting the daemon (or the affected downstream server/session) before validation. A config reload updates future spawns, but already-running MCP subprocesses may continue using the old environment until they are restarted.

```bash
# ~/.config/vision/.env
CONTEXT7_API_KEY=your-context7-key
KAGI_API_KEY=your-kagi-key
FIRECRAWL_API_KEY=your-firecrawl-key
```

```bash
# Set restrictive permissions
chmod 600 ~/.config/vision/.env
```

Reference them in `servers.yaml`:

```yaml
servers:
  context7:
    env:
      CONTEXT7_API_KEY: "${CONTEXT7_API_KEY}"
```

For producer-owned remote MCPs like Grep by Vercel, prefer configuring the official remote endpoint directly in the client:

```json
{
  "mcp": {
    "gh_grep": {
      "type": "remote",
      "url": "https://mcp.grep.app",
      "timeout": 20000
    }
  }
}
```

If you intentionally want Vision to proxy a remote MCP, use native HTTP proxy mode instead of a local wrapper:

```yaml
servers:
  gh_grep:
    port: 6288
    transport: http
    url: "https://mcp.grep.app"
```

## File Permissions

Vision enforces owner-only permissions on sensitive files:

- **`servers.yaml`** — Written with `0600` permissions when saved via `vision add` or `vision config`. Prevents other users from reading `bearer_token` values or other config secrets.
- **`.env`** — Should be manually set to `0600` since it contains API keys.

```bash
chmod 600 ~/.config/vision/servers.yaml
chmod 600 ~/.config/vision/.env
```

## Validation

Validate your configuration:

```bash
vision config validate
```

This checks:
- YAML syntax
- Required fields
- Port range (6276-6325)
- No port conflicts
- Transport configuration consistency

## Hot Reload

Vision supports configuration hot reload:

```bash
# Send SIGHUP to reload
vision daemon reload

# Or send signal directly
kill -HUP $(cat /tmp/vision.pid)
```

On reload, Vision:
1. Re-reads `servers.yaml`
2. Starts new servers
3. Stops removed servers
4. Updates changed servers (stops then starts)

## Port Allocation

Vision allocates ports 6276-6325 for MCP servers:

| Port | Purpose |
|------|---------|
| 6275 | Management API |
| 6276-6325 | MCP servers |

Each server MUST have a unique port. The CLI will automatically allocate the next available port when adding servers.
