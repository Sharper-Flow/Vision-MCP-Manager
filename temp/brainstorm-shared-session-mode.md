# Proposal Draft — Add `session_mode: shared` for Subprocess Reuse

**Status:** Draft awaiting ADV tools. When `adv_change_create` etc. are available, feed this file into `/adv-proposal` to create the official change.

**Proposed change ID (suggestion):** `addSharedSessionModeForMcpSubprocessReuse`

**Proposed title:** Add shared session mode with subprocess reuse for Vision MCP servers

---

## Why

Vision's current per-session subprocess architecture is MCP-spec compliant but materially inefficient for the real operator workload: 10+ concurrent OpenCode sessions × 5-7 configured MCP servers = 50-70 subprocesses, each ~50-100MB resident, totaling 3.5-7 GB of duplicated MCP server processes. This exhausts a 64 GB host in combination with normal developer workloads, and every new OpenCode session pays cold-start cost (~500ms-2s × N servers) before the first tool call returns.

The per-session model was deliberately chosen during the recent go-sdk migration (`changes/migrateVisionToGoMcpSdkWithStr/`) for strict MCP spec compliance. Research into the MCP specification and multiple production gateway projects (MCPJungle, 1MCP, Microsoft mcp-gateway, IBM ContextForge) confirms that **process sharing is spec-compliant** — capability negotiation must remain per-session and notifications must not broadcast to wrong sessions, but a single backend subprocess can legitimately serve N upstream sessions when these routing invariants are maintained.

MCPJungle ships exactly this via a per-server `session_mode: stateful|stateless` toggle. Vision even retains an unused `Stateful bool` field (`internal/config/schema.go:85`) preserved from the migration for this future capability. For Vision's typical server set (kagi, context7, lgrep, svelte-mcp, gh_grep, firecrawl, sentry) — all read-heavy API/docs fronts with no sampling/elicitation/roots capabilities — shared mode is directly applicable and projected to reduce subprocess count from ~70 to ~7 (80%+ memory reduction) while eliminating per-session cold start.

Operator pain today: RAM cap reached with 10+ simultaneous OpenCode sessions across projects and tmux panes.

## What Changes

### Tier 1 — Config tuning (zero code change, ship first)

- Enable `shared_read_only_tools` + `shared_result_cache_ttl` for read-heavy servers in the operator's `~/.config/vision/servers.yaml`
- Tighten `session_timeout` defaults where idle reaping is safe
- Document per-server tuning profiles in `docs/CONFIGURATION.md`

### Tier 2 — Shared session mode (primary scope)

Add a new per-server config knob `session_mode: isolated | shared` (default `isolated` for back-compat). In shared mode:

1. **Config**: repurpose the currently-unused `Stateful bool` at `internal/config/schema.go:85` into an explicit `SessionMode` enum. Map legacy `stateful: true` to `session_mode: isolated` for backward compatibility.
2. **Shared session registry** (`internal/session/shared.go`, new file): per-server singleton subprocess keyed by server name, reference-counted by upstream subscribers, lazy spawn on first connect, idle teardown on refcount → 0.
3. **Capability-based eligibility check**: at first backend `initialize`, reject `session_mode: shared` with a clear operator error if the backend advertises `sampling`, `elicitation`, or `roots` capability. (These require per-request session association per MCP spec SEP-2260.)
4. **Notification fan-out / routing** in `internal/mcp/proxy.go`:
   - `tools/list_changed`: debounced re-fetch of `tools/list`, broadcast cached result to all subscriber sessions.
   - `resources/updated`: track subscriptions per (uri, upstream session), fan-out only to subscribed sessions.
   - `notifications/message` (logging): drop in shared mode (with debug log) to avoid cross-session info leakage. Keep current per-session relay in isolated mode.
   - `notifications/progress`: progress token correlation map — rewrite outbound `progressToken` to a backend-scoped token, reverse lookup on inbound notification to route only to the originating upstream session.
   - `notifications/cancelled`: upstream request-ID ↔ backend request-ID mapping so a cancellation from session A never aborts session B's concurrent call.
5. **Capability negotiation**: advertise the _intersection_ of (backend capabilities ∩ all connected upstream client capabilities) to each upstream; recompute on connect/disconnect.
6. **Preserved invariants**: each upstream session still gets its own `mcp.Server` and its own `initialize` handshake; only `ps.downstream` is shared. `downstreamMu`, `respawnMu`, `closeOnce`, and the `onSessionRemoved` callback path from `internal/mcp/proxy.go` are preserved (adapted to work against shared downstream).

### Tier 2b — Observability (bundled with Tier 2)

New `internal/metrics/` package exposing Prometheus metrics on the existing admin port (6275) at `/metrics`:

- `vision_subprocesses_active{server, mode}` — gauge; proves shared-mode reduction.
- `vision_upstream_sessions_active{server, mode}` — gauge.
- `vision_tool_call_duration_seconds{server, tool, outcome}` — histogram; detects contention.
- `vision_shared_cache_hits_total{server, tool}` — counter.
- `vision_admission_denied_total{server}` — counter.
- `vision_circuit_breaker_state{server}` — gauge (0 closed / 1 half / 2 open).
- `vision_subprocess_respawns_total{server, trigger}` — counter.

Overhead is negligible (atomic int64 per increment, lazy scrape) and the metrics are the operator's validation signal that shared mode actually delivered.

### Out of scope (explicitly)

- Tier 3 warm pool (`pool_min`/`pool_max` style preemptive subprocess pool). Not justified at current scale; Tier 2 projected to capture essentially all the benefit.
- Session persistence across daemon restarts beyond existing stale-session recovery.
- Cross-server session federation / gateway mode (single port fronting all servers).
- Migrating to the MCP 2026 stateless protocol draft (future-track).

## Success Criteria

Quantitative (measured via the new metrics):

- With 10 OpenCode sessions and the operator's current server set, `vision_subprocesses_active` totals ≤ N_servers × 2 (≈ 14 subprocesses), down from ≥ 50 today.
- Host RAM attributable to MCP subprocesses drops by ≥ 60% on a steady-state measurement.
- p95 tool-call latency in shared mode for read-heavy servers (kagi, context7, lgrep) is within 20% of isolated-mode baseline under equal concurrency.

Qualitative:

- No upstream-session cross-talk: logs, progress, cancellations, and resource updates arrive only at the correct session.
- A server advertising `sampling`, `elicitation`, or `roots` fails fast with an actionable error when configured with `session_mode: shared`.
- Existing isolated-mode behavior is unchanged — default remains `isolated`, and every current integration test continues to pass.
- `vision init` and `vision_*` admin tools reflect the per-server mode in status output.

## Affected Code

- `internal/config/schema.go` — new `SessionMode` enum field; migrate legacy `Stateful` interpretation.
- `internal/config/config_test.go` — schema back-compat tests.
- `internal/session/manager.go` — `SpawnSession()` branches on mode; admission/reaper adapt.
- `internal/session/shared.go` — **new**; `SharedSessionRegistry`, refcount, lifecycle.
- `internal/session/manager_test.go` — shared-mode unit tests.
- `internal/mcp/proxy.go` — multi-subscriber proxy, fan-out, correlation maps, capability intersection, compatibility check.
- `internal/mcp/proxy_test.go` — extend coverage for shared mode routing invariants.
- `internal/mcp/interop_test.go` — multi-session concurrency tests in shared mode.
- `internal/mcp/resilience.go` — verify circuit-breaker / retry semantics under shared downstream.
- `internal/admin/tools.go` — `vision_list` / `vision_status` expose session mode.
- `internal/admin/server.go` — mount `/metrics` endpoint.
- `internal/metrics/` — **new** package, prometheus registrations.
- `cmd/vision/main.go` — wire metrics registry into daemon startup.
- `docs/CONFIGURATION.md` — document `session_mode`, eligibility rules, tuning profiles.
- `docs/MCP_TRANSPORTS.md` — update architecture diagram for shared mode.
- `configs/servers.example.yaml` — example with `session_mode: shared` annotated.
- `~/.config/vision/servers.yaml` (operator-local, Tier 1) — not in repo, but documented in CONFIGURATION.md.

## Related Repositories

None — changes confined to `Vision-MCP-Manager`. The `opencode-plugin/` subtree is unaffected; OpenCode continues to open one MCP session per OpenCode session, which is the correct upstream behavior.

## Constraints

- **MUST** remain MCP-spec compliant per `modelcontextprotocol.io/specification/2025-11-25/` (current stable) and not regress against earlier supported versions.
- **MUST** default `session_mode: isolated` on existing configs so no operator sees behavior change without opting in.
- **MUST NOT** allow shared mode for servers advertising `sampling`, `elicitation`, or `roots` — SEP-2260 requires per-request session association.
- **MUST NOT** broadcast notifications to sessions that did not subscribe / did not originate the request.
- **MUST** preserve the existing proxy invariants around `downstreamMu`, `respawnMu`, and `closeOnce` so shared-mode teardown is race-free.
- **SHOULD** map the legacy unused `Stateful` field to the new enum rather than introducing a parallel config field.
- **SHOULD** ship Prometheus metrics in the same change so the memory-reduction win is measurable.
- **SHOULD** preserve existing `shared_read_only_tools` request-layer cache semantics; shared mode is a process-layer optimization and the two compose.
- **MUST NOT** change CLI command interface or break `.opencode.json` / `servers.yaml` backward compatibility.

## Impact

- **Operator RAM**: projected 60-80% reduction in MCP subprocess memory footprint for typical operator workflows.
- **Cold-start latency**: subprocess startup paid once per server per daemon lifetime instead of once per (session × server).
- **Contention**: stateless servers (kagi, context7, gh_grep) are I/O-bound and unaffected; CPU-bound servers (lgrep semantic search) may see contention under high concurrency, mitigated by the existing `max_in_flight_requests` knob.
- **Security**: shared mode is opt-in per-server; existing isolation remains the default, so no operator sees a posture change without explicit config.
- **Debuggability**: new metrics make the subprocess count, contention, and admission rejection rate observable for the first time.
- **Future-proofing**: aligns with the MCP 2026 stateless-protocol direction; positions Vision to adopt MRTR when it lands without architectural rework.

## Context

Primary research already completed (summarized in conversation history):

- **MCP spec analysis** — process sharing compliant per 2025-11-25 spec; capability negotiation mandatory per-session; SEP-2260 governs server→client request association; 2026 roadmap moves protocol stateless.
- **Ecosystem survey** — MCPJungle (`session_mode: stateful|stateless`), 1MCP (warm pool), Microsoft mcp-gateway (session affinity on K8s), IBM mcp-context-forge (stateless + Redis cache) all implement process sharing patterns; official SDKs do not ship a reference gateway.
- **OpenCode client behavior** — each OpenCode session intentionally opens a new MCP session; behavior is correct and unchangeable at the upstream layer. Optimization must live in Vision.
- **Vision codebase inventory** — 14 existing resilience mechanisms (idle reaper, cache, circuit breaker, retry, health probe, stale recovery, admission control, hardening, etc.); 11 identified gaps (most relevant: no subprocess sharing, no resource accounting, no metrics export, unused `Stateful` field).
- **Spec-edge case research** — concrete implementation patterns for `tools/list_changed`, logging fan-out, progress-token correlation, cancellation routing, capability intersection, resource-subscription tracking, sampling/elicitation/roots incompatibility.

Historical context: `changes/migrateVisionToGoMcpSdkWithStr/proposal.md` documents the deliberate move from the old shared-`StdioHTTPBridge` to per-session isolation. That move fixed MCP spec violations but eliminated process sharing. This proposal restores process sharing in a spec-correct way, opt-in per server.

## Discovery Agenda

Unresolved items carried forward to `/adv-discover`:

### Codebase

- Exact structural fit between `SharedSessionRegistry` and existing `session.Manager` — compose, extend, or parallel type.
- Which existing tests most cheaply extend to cover shared-mode notification routing.
- Whether `proxySession` evolves to hold a subscriber map, or a parallel `sharedProxySession` type is introduced.

### Ecosystem

- Final config surface: revive `Stateful bool` vs introduce `SessionMode` enum vs both with deprecation path. Pick the LBP shape for a long-lived operator config.
- Verify MCPJungle's current config schema and any lessons from its issue tracker since the survey snapshot.
- Check whether the official Go SDK has added shared-server helpers since the last review.

### Domain

- Which servers the operator wants enabled for shared mode in the first rollout (expected: kagi, context7, lgrep, svelte-mcp, gh_grep, firecrawl).
- Desired behavior for `notifications/message` in shared mode: drop silently, drop with debug log, or route via correlation when a progress token is present.
- Desired behavior when a shared subprocess crashes mid-operation for multiple sessions — synchronized respawn vs per-session error surface.

### Integration

- Whether any other active Vision change conflicts with this one (needs `adv_change_list` once ADV tools return).
- Whether admin-API response shapes (`vision_list`, `vision_status`) should break on mode introduction or remain additive.
- Whether the operator wants `/metrics` exposed on the admin port or a separate port for network ACL reasons.
- Whether CI should add a dedicated multi-session concurrency benchmark to lock in the projected numbers.

---

## Follow-up when ADV tools return

1. Run `adv_change_list` to detect overlap; reuse an existing change if one already matches.
2. `adv_change_create` with:
   - Title: "Add shared session mode with subprocess reuse for Vision MCP servers"
   - Why: the "Why" section above, verbatim.
3. `adv_change_update` to populate the rest of this proposal (all sections above).
4. Validate with proposal checklist once `docs/checklists/proposal-checklist.md` is present.
5. `adv_gate_complete gateId: proposal`.
6. Hand off to `/adv-discover` with the Discovery Agenda above as the explicit input.
