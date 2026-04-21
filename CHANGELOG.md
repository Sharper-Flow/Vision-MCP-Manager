# Changelog

## 2026-04-21

### Slot groups: transparent load-balanced MCP server pools

#### What changed
- Added `slot_groups` configuration section to `servers.yaml`. Each group declares a template name, base port, count, group port, and shared defaults. Vision expands groups into flat server entries at load time and creates a virtual listener that transparently routes sessions to the least-loaded healthy slot.
- New multiplexer (`internal/slots`) with health-aware least-loaded selection, pending-assignment accounting, sticky session routing, and 2-second quarantine for slots that fail initialization.
- New admin MCP tool `vision_slot_status` and HTTP endpoints `GET /v1/slots` / `GET /v1/slots/{group}` for per-slot session observability.
- Config save round-trips preserve `slot_groups` while omitting synthesized slot entries.

#### Why
- Servers like Playwright MCP require process-per-session isolation but need concurrent throughput that a single process can't provide. Slot groups give each agent its own browser context while presenting a single endpoint.
- Transparent routing means zero agent-side changes — agents connect to one port, Vision handles the rest.

#### Migration
- No breaking changes. `slot_groups` is opt-in; existing `servers.yaml` files work unchanged.
- To adopt: add a `slot_groups:` section to your `servers.yaml` with `template`, `base_port`, `count`, `group_port`, and `defaults`. Remove any manually duplicated server entries for the same tool.
- Reload with `vision daemon reload`. Verify with `vision_slot_status` or `curl http://localhost:6275/v1/slots`.

## 2026-03-29

### Vision availability hardening

#### What changed
- Added a `networked` availability profile for upstream-backed MCP servers with stronger defaults for request timeout, retry, circuit-breaker behavior, shared result caching, and max in-flight requests.
- Upgraded `vision_init` to reconcile existing OpenCode MCP config instead of blindly overwriting it, including repairing stale Vision-managed endpoint mappings like the Kagi port drift issue.
- Added safe cross-session sharing for explicitly whitelisted read-only tools through in-flight request coalescing and bounded short-lived caching.
- Added explicit availability failure categories for config drift, provider timeout, retry exhaustion, and circuit-open conditions.
- Updated Vision docs and examples to explain the hardened model and recommended configuration.

#### Why
- A recent Kagi timeout incident showed that Vision needed stronger defaults for network-backed MCP servers and a safer recovery path for stale client endpoint mappings.
- Multiple concurrent OpenCode agents should be able to share safe read-only work without breaking MCP session isolation.
- Operators need clearer failure signals and more predictable availability behavior under retries, rate limits, and transient upstream failures.
