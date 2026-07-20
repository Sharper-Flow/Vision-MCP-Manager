# ADR 0001: Managed Native HTTP for Playwright

- Status: accepted
- Date: 2026-07-20
- Change: `fixPlaywrightSessionIsolation`

## Context

Vision previously multiplexed independent OpenCode clients through one stdio Playwright MCP connection. `--isolated` kept that process profile ephemeral, but did not create independent downstream MCP sessions. Clients therefore shared one browser backend/context and could interfere through pages, cookies, navigation, and element references. Transport reconnects also kept abandoned sessions consuming capacity.

OpenCode must remain unchanged. Vision must preserve external port `6287`, bearer/Origin enforcement, six active sessions, 30-minute application-idle expiry, and at-most-once browser mutations.

## Decision

Vision owns one loopback-only native Streamable HTTP `@playwright/mcp` process and exposes it through a lifecycle-aware gateway:

```text
OpenCode -> Vision :6287 security/admission/leases -> ReverseProxy
         -> 127.0.0.1:<internal>/mcp -> Playwright MCP
```

- Playwright generates `Mcp-Session-Id`; Vision preserves it one-to-one.
- Native Playwright HTTP creates one BrowserContext per MCP client. `--isolated` shares one browser process while creating a new context for every session.
- Vision reserves capacity before initialize and commits the lease only after a valid downstream session ID.
- Only structurally valid non-`ping` JSON-RPC requests refresh application activity. SSE, notifications, responses, and keepalives do not.
- Unknown, expired, and process-lost IDs receive `404` before downstream dispatch.
- Every HTTP request, including DELETE, is forwarded at most once. Ambiguous results trigger backend-wide drain/recycle; Vision never replays browser mutations.
- Recycle rejects new dispatch, waits for admitted application requests, replaces the process, invalidates old IDs, and requires initialize/delete readiness before reopening.
- Active diagnostics are capped at 100 rows; closed history is capped at 1,000 rows. Only hashed session IDs are exposed.

## Browser Selection

Canonical host arguments:

```text
@playwright/mcp@0.0.77
--browser chromium
--headless
--isolated
--host 127.0.0.1
--allowed-hosts 127.0.0.1:<internal-port>
--port <internal-port>
```

`@playwright/mcp` otherwise defaults to branded Chrome and expects `/opt/google/chrome/chrome` on Linux. `--browser chromium` maps to Playwright's version-managed `chrome-for-testing` channel; headless execution uses the matching cached Chrome headless shell. This is preferable to hardcoding `--executable-path`, which bypasses Playwright's browser-version selection. Ubuntu 26.04 is supported by the verified Playwright line.

## Evidence

The opt-in integration test `TestManagedPlaywrightNativeHTTP` pins:

- `@playwright/mcp` `0.0.77`
- `playwright-core` `1.62.0-alpha-2026-06-29`

It verifies 10 alternating isolation rounds per client, cookie/page/reference separation, six-session admission, in-flight idle-boundary protection, transport-independent idle expiry, forced process restart, stale-ID failure, replacement readiness, and secret-safe diagnostics. The verified normal run completed in 21.58 seconds; the race-enabled repeat completed in 21.74 seconds. Run:

```bash
VISION_PLAYWRIGHT_REAL_TEST=1 go test -race ./internal/integration \
  -run '^TestManagedPlaywrightNativeHTTP$' -count=1
```

## Consequences

### Positive

- Independent contexts without six browser processes.
- Crash-safe capacity and deterministic cleanup.
- No OpenCode changes or client endpoint migration.
- Real lifecycle state visible through `vision_list` and `/v1/servers/{name}`.

### Tradeoffs

- Shared browser-process loss invalidates every Playwright session.
- Native HTTP host protection requires the exact loopback host and port.
- A Playwright package/browser revision upgrade requires rerunning the pinned real verification.

## Rollback

Rollback uses Vision's existing stateful stdio model, not shared stdio:

1. Restore the pre-change `playwright` entry with `transport: stdio`, `stateful: true`, `max_sessions: 6`, and `--browser chromium --headless --isolated`.
2. Keep external port `6287`; OpenCode configuration remains unchanged.
3. Run `vision config validate`.
4. Run `vision daemon reload`.
5. Confirm `vision status` and one isolated Playwright session.

Rollback target: under 10 minutes. The pinned stateful-stdio rehearsal reached a navigable real browser in 1.921 seconds (`TestPlaywrightStatefulStdioRollback`). Never roll back to shared stdio, shared browser context, profile/lock deletion, or periodic restart cleanup.
