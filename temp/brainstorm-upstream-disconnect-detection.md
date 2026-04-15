# Proposal Draft — Detect Upstream Client Disconnect to Reap Orphaned Subprocesses

**Status:** Draft awaiting ADV tools. When `adv_change_create` etc. are available, feed this file into `/adv-proposal` to create the official change.

**Proposed change ID (suggestion):** `detectUpstreamDisconnectForSessionCleanup`

**Proposed title:** Detect upstream client disconnect in Vision proxy to reap orphaned MCP subprocesses

**Related drafts:** `temp/brainstorm-shared-session-mode.md` — the shared-session-mode proposal makes this bug ~free even when it happens; this proposal fixes the leak itself. They compose; ship both.

---

## Why

Vision currently has **no upstream-disconnect detection**. Every MCP client that exits without sending `DELETE /mcp` leaves its downstream subprocess alive until the idle-timeout reaper fires — 5 minutes default, 30 minutes in operator tunings. Measured against the operator's real workload:

- `/tmp/vision-daemon.log` shows **52 svelte-mcp spawns vs 40 reaps**, a 12-session gap during normal usage.
- **All 358 reaps across all servers** in the log were `idle timeout` or `health check failed`; **zero** were client-initiated.
- The current Vision daemon has **86 direct subprocess children** vs. 16 OpenCode-related processes on the host — overwhelming evidence of accumulated orphans.
- Code audit of `internal/mcp/handler.go` confirms zero TCP-close detection: no `r.Context().Done()`, no `CloseNotify`, no `ConnState` callback, no `Hijack`, no ping/liveness. The listener calls `srv.ListenAndServe()` and hands entirely off to go-sdk's `StreamableHTTPHandler`, which requires an explicit `DELETE /mcp` for session teardown.

OpenCode — Vision's primary client — does not send `DELETE /mcp` on TUI exit, `/exit`, `kill`, tmux pane close, SIGHUP, or crash. This behavior is consistent with every other MCP client I surveyed (Claude Desktop, Cursor, Continue) and is unlikely to change at the client layer in the near term.

The result: every ungraceful client exit leaks a subprocess for up to 30 minutes, multiplied across N configured servers per session × the operator's 10+ concurrent OpenCode sessions. On a 64 GB host this has repeatedly pushed the operator into RAM pressure.

Tier A mitigation (shortening idle timeouts in `~/.config/vision/servers.yaml`) reduces leak window but cannot eliminate the leak. The real fix is detecting upstream disconnect in the proxy and reaping the downstream subprocess when all connections for a session have closed and not reconnected within a short grace window.

## What Changes

### 1. Connection-aware session tracker (new middleware)

Add a middleware in `internal/mcp/handler.go` that wraps the streamable handler and tracks the count of in-flight HTTP connections per `Mcp-Session-Id`:

- On each request carrying a session ID (after `initialize`), increment an atomic counter for that session and register a `r.Context().Done()` watcher.
- On request completion or context cancel, decrement the counter and stamp `last-connection-closed-at` for the session.
- Expose a new `session.Manager.TouchConnection(sessionID)` / `ConnectionClosed(sessionID)` API so the middleware can signal lifecycle events without reaching into manager internals.

### 2. Grace-period reaper for connection-closed sessions

Extend `internal/session/manager.go`'s existing reaper goroutine with a second sweep:

- For every tracked session where `active_connections == 0` AND `last-connection-closed-at > connection_grace_period`, call `RemoveSession(sessionID, reason: "client disconnected")`.
- Default `connection_grace_period`: **30 seconds**. Configurable per-server via new `connection_grace_period` field in `config.ServerConfig`.
- The grace period is required because streamable HTTP clients routinely open and close individual HTTP connections per request; a single connection closing is not equivalent to a session ending.

### 3. New log reason + metrics dimension

- Reaper emits `reason="client disconnected"` (distinct from `idle timeout`).
- Prometheus metric (when the observability proposal lands): `vision_sessions_reaped_total{server, reason}` with new `client_disconnected` value.
- Operator can finally distinguish real leaks from idle sessions.

### 4. Feature flag + staged rollout

- New per-server config flag `disconnect_detection: enabled | disabled` (default `enabled`).
- Reason: this changes behavior for currently-leaky but intentionally long-lived sessions (e.g. an agent that legitimately idles >30 s between tool calls with a stable TCP connection — rare, but possible).
- Existing transparent respawn (`proxy.go:1071-1193`) already handles the case where a session is reaped and a subsequent request arrives: the proxy spawns a fresh subprocess and the caller sees only a small latency hit. So aggressive reaping is safe.

### 5. Out of scope (explicitly)

- Sending server-initiated `ping` notifications to probe liveness over idle connections. Useful but requires client cooperation; MCP spec doesn't mandate ping response. Revisit later if the connection-close signal proves insufficient.
- Changing the go-sdk's `StreamableHTTPHandler` behavior. This proposal wraps it, doesn't modify it.
- Migrating MCP session state across daemon restarts.
- Supervisor-level restart circuit-breaker for catalog entries that are misconfigured (see "Related issue" below).

## Success Criteria

Quantitative:

- Under typical operator workload (10 OpenCode sessions, 5+ MCP servers), **reap-reason distribution over 24h shows >70% of reaps attributed to `client_disconnected`** (not `idle timeout`) — proving the new detection path is firing on the real cause.
- **Active subprocess count within 60 s of an OpenCode exit returns to baseline** (current behavior: takes `session_timeout` + up to `reaper_interval` minutes).
- Memory attributable to MCP subprocesses stays within ~10% of "number of live OpenCode sessions × number of configured servers × ~75 MB" over a 24h window, measured by `vision_subprocesses_active` metric.

Qualitative:

- Existing transparent-respawn integration tests continue to pass — reaping a session and then making a new request MUST transparently spawn a fresh subprocess.
- No false-positive reaps during legitimate idle agent workflows (operator validation: leave an OpenCode session idle for 5 min between tool calls, subprocess survives).
- Default behavior is safer than today without operator config changes.

## Affected Code

- `internal/mcp/handler.go` — new connection-aware middleware, session-connection tracker data structures.
- `internal/mcp/handler_test.go` — new tests for TCP-close → reap signaling (closeable `httptest.Server`, test `httptest.ResponseRecorder` with cancellable context, verify grace-period behavior).
- `internal/session/manager.go` — new `TouchConnection`, `ConnectionClosed`, `setLastConnectionClosedAt` APIs on `TrackedSession`; extend reaper to check `active_connections == 0` + grace period.
- `internal/session/manager_test.go` — new tests for connection-close reap path (with clock injection for deterministic grace-period expiry).
- `internal/config/schema.go` — new `DisconnectDetection bool` (default true) + `ConnectionGracePeriod Duration` (default 30 s) fields on `ServerConfig`.
- `internal/config/schema_test.go` — defaults + YAML round-trip tests.
- `internal/mcp/proxy.go` — verify existing respawn path composes with aggressive reaping (read-only walkthrough + integration test).
- `internal/mcp/interop_test.go` — new test: client opens initialize → closes TCP without DELETE → verify subprocess reaped within grace period + next client gets fresh session.
- `docs/MCP_TRANSPORTS.md` — document disconnect-detection behavior, grace period semantics, interaction with transparent respawn.
- `docs/CONFIGURATION.md` — document new config fields.
- `configs/servers.example.yaml` — show `disconnect_detection` and `connection_grace_period` in examples.

## Related Repositories

None — all changes local to `Vision-MCP-Manager`. Confirms OpenCode behavior is unchanged; Vision accommodates the actual-observed client pattern instead of relying on clients to send DELETE frames they never send.

## Constraints

- **MUST NOT** regress transparent respawn: existing integration tests in `interop_test.go` and `proxy_test.go` must pass unchanged.
- **MUST NOT** reap sessions while upstream requests are in flight — connection counter is the guard; must be decremented strictly after request completes.
- **MUST** handle concurrent connections for the same session ID correctly — streamable HTTP allows multiple in-flight SSE + POST connections per session. Counter must be atomic.
- **MUST** handle the `initialize`-then-immediate-disconnect race: the connection closes before the `Mcp-Session-Id` header is on the response. Middleware tracks connections only after a session ID is known; sessions that never completed initialize are cleaned up by the existing idle path (this edge case is rare and low-value to optimize).
- **MUST** remain MCP-spec compliant: `DELETE /mcp` still honored; new detection is additive.
- **SHOULD** be safe to enable by default — the grace period absorbs benign per-request TCP closes; only genuinely-dead sessions get reaped.
- **SHOULD** compose with the shared-session-mode proposal — when shared mode lands, `TouchConnection`/`ConnectionClosed` still mean the same thing (a specific upstream subscriber disconnected); the shared-backend refcount decrements and the backend stays alive as long as any subscriber is active.

## Impact

- **Operator RAM**: subprocess count tracks live-client count with ~30 s lag, eliminating the 5-30 min leak window that currently causes RAM pressure under normal operation.
- **Cold-start latency**: unchanged for active sessions. Sessions that are reaped due to disconnect-detection and later reused pay the same respawn cost as today's idle-timeout reaps (~20 ms for Node subprocesses).
- **Debuggability**: operators can finally tell from `reason="client disconnected"` logs whether a reap was because the client exited or because it was idle — a distinction that matters for tuning.
- **Risk**: low. Transparent respawn is already production-tested. Feature flag per-server allows opt-out if a specific server's workload is incompatible. Grace period is tunable.

## Context

Primary evidence collected during `/build` diagnostic for svelte-mcp leak investigation:

- `/tmp/vision-daemon.log` analysis: 52 spawn / 40 reap imbalance for svelte-mcp; all 358 reaps across 8 active servers were daemon-initiated (idle or health-check), zero client-initiated.
- `ps -p <vision_pid> --ppid` shows 86 direct subprocess children vs. 16 OpenCode-related host processes.
- Code audit of `internal/mcp/handler.go:1-265`: no connection-lifetime tracking. `internal/mcp/proxy.go` tracks downstream session lifecycle but not upstream connection lifecycle.
- `internal/session/manager.go:287` reaper logs `"reaping expired session"` but only from the `isExpired(LastActivity) || isExpired(CreatedAt + SessionTTL)` branches — there is no connection-close branch to reach.

Related issue uncovered in the same investigation (separate proposal needed):

- `grep-app` server (previously configured) recorded **724 exit-status-1 events and 400+ restart attempts in ~4 minutes** in the daemon log before being removed from the registry. The supervisor's backoff re-entered but never circuit-breaker-gave-up. This is a separate bug: the supervisor needs a "stop restarting after N failures in T seconds" policy. **Operator has since removed `grep-app` from the Vision registry**, but the stale references remained in `~/.opencode.json` and `~/.config/opencode/opencode.json` until cleaned up during this diagnostic. The supervisor bug itself deserves its own proposal.

Prior art / ecosystem: this fix is absent from every MCP gateway surveyed (MCPJungle, 1MCP, Microsoft mcp-gateway, IBM ContextForge). Every one of them relies on either a `DELETE /mcp` or an idle timer. Vision has an opportunity to lead the ecosystem here; the fix is ~1 day of focused work and pays back immediately for every operator running many concurrent clients.

## Discovery Agenda

Unresolved items carried forward to `/adv-discover`:

### Codebase

- Exact interface contract between the connection-tracking middleware and `session.Manager` — does `Manager` grow new methods, or does the middleware post events to an `OnConnectionLifecycle` callback registered at listener-add time? Prefer the callback pattern for symmetry with existing `OnSessionRemoved`.
- How does the tracker handle streamable HTTP's long-lived `GET /mcp` SSE stream? That connection is expected to persist for the lifetime of the session; its close IS a strong signal. Distinguish it from POST `/mcp` request-response churn.
- Does go-sdk expose any hooks for per-session connection state already? Audit before rolling our own.

### Ecosystem

- Does any upstream MCP client emit `DELETE /mcp` correctly on graceful shutdown? OpenCode, Claude Desktop, Cursor, Continue — audit actual wire traffic (could use a small capture proxy) to see if anyone ever cleanly disconnects.
- Is the `notifications/ping` mechanism defined in any recent MCP spec draft? If yes, we should also implement it and let clients opt in to cleaner shutdowns.

### Domain

- Operator's preferred `connection_grace_period` default — 30 s is a guess; might be tightened to 10-15 s if false positives are a non-issue, or loosened if they are. Validate against real workload.
- Should `disconnect_detection` default on or off in the first release? Conservative recommendation: on by default with the feature flag as an emergency off-switch; more aggressive: ship it enabled and remove the flag in a follow-up.

### Integration

- Interaction with health-check reaping: if both paths converge on the same session at the same time, `RemoveSession` must be idempotent. Audit and test.
- Interaction with stale-session-recovery path (`proxy.go:301-398`): a reaped session that receives a late request via a reconnecting client should recover cleanly via the existing tombstone-aware recovery logic. Audit and test.

---

## Follow-up when ADV tools return

1. Run `adv_change_list` to detect overlap; look specifically at `temp/brainstorm-shared-session-mode.md` — these two changes compose and should probably share an `/adv-discover` session even if they're separate changes.
2. `adv_change_create` with the proposed title + the "Why" section verbatim.
3. `adv_change_update` to populate the rest of this proposal.
4. `adv_gate_complete gateId: proposal`.
5. Hand off to `/adv-discover` with the Discovery Agenda above as the input.
