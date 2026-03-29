# Changelog

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
