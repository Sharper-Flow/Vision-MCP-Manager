## 2026-08-12 (v1.4.1)

### Fixed

- register tools via Hooks.tool record so they actually load (#11)

## 2026-08-12 (v1.4.0)

### Added

- add opencode MCP connect/disconnect tools (#10)

## 2026-07-23 (v1.3.5)

### Fixed

- move tool-name constants out of plugin entry module (#9)

## 2026-07-21 (v1.3.4)

### Changed

- adopt canonical Bun+TS baseline for opencode-plugin (#8) (chore: dev-tooling)

## 2026-07-20 (v1.3.3)

### Changed

- checkpoint tk-ef3c415e946e (chore: adv)
- recover uncommitted execution artifacts (doc command fixes + canary log) (chore: adv)
- checkpoint tk-9a9f7a3c97f7 (chore: adv)
- checkpoint tk-bdee41e3a1dc (chore: adv)
- checkpoint tk-843acba2614a (chore: adv)
- checkpoint tk-aa51a2ac11e1 (chore: adv)
- checkpoint tk-1b69bde98992 (chore: adv)
- checkpoint tk-bdb9cacc8c32 (chore: adv)
- checkpoint tk-88088081a3ba (chore: adv)

## 2026-07-20 (v1.3.2)

### Changed

- checkpoint tk-c4e59766c5a2 (chore: adv)
- checkpoint tk-c4e59766c5a2 (chore: adv)
- checkpoint tk-9ef35bcb8036 (chore: adv)
- checkpoint tk-39678588b5b7 (chore: adv)
- checkpoint tk-b0f4268b0d10 (chore: adv)
- checkpoint tk-eea953cc3675 (chore: adv)
- checkpoint tk-7f2fd01ce108 (chore: adv)
- checkpoint tk-dc7f0581f8d4 (chore: adv)
- checkpoint tk-50e858ed2361 (chore: adv)

## 2026-05-25 (v1.3.1)

### Changed

- enable Vision Admin MCP for project (chore)

## 2026-05-21 (v1.3.0)

### Added

- add deploy-local.sh for dev-loop binary deploys

## 2026-05-09 (v1.2.5)


## 2026-05-09 (v1.2.4)


## 2026-05-09 (v1.2.3)


## 2026-05-08 (v1.2.2)

### Fixed

- address review suggestions — defense-in-depth logging, Source sanitization, DRY isInstalled
### Changed

- update changelog for v1.2.2 — structured fallback suggestions (docs)
- clarify classification/conversion function attribution in MCP_TRANSPORTS.md (docs)

## 2026-05-08 (v1.2.2)

### Added

- structured fallback suggestions on upstream tool failure — AvailabilityError now returns CallToolResult{IsError:true} with failure category, server/tool name, explicit no-auto-routing disclaimer, and up to 3 alternative tool suggestions ranked by catalog capability overlap
- FallbackSuggestionProvider interface (internal/mcp) — consumer-defined, zero-coupling with catalog
- catalogSuggestionProvider (internal/daemon) — ranks alternatives by capability overlap, top-3 limit
- sanitizeSource() — strips control characters from suggestion Source URLs
- defense-in-depth logging in finishToolCall for contract violations

### Changed

- daemon now owns catalog.Default() construction, wires to admin + proxy provider
- finishToolCall helper separates error classification from handler routing
- docs: clarify classification/conversion function attribution in MCP_TRANSPORTS.md

## 2026-05-08 (v1.2.1)

### Fixed

- build binaries in auto-release workflow

## 2026-05-08 (v1.2.0)

### Added

- add tool naming guidance and proxy config tests
### Fixed

- resolve flaky TestProxyHandler_SelectorLifecycle
- fix process substitution in auto-release changelog step
- close stderr data race in collectStderr
- resolve all lint issues, add pre-push hook
- rename module from github.com/jrede/vision to github.com/Sharper-Flow/Vision-MCP-Manager
- remove duplicate tests, add golangci-lint v2 config
- replace nil context with context.TODO in admin tests
- replace nil context with context.TODO in metrics_test.go
- refcount leak on spawn failure + subscriber panic recovery
### Changed

- add conventional commits auto-release workflow (ci)
- cleanup project root, fix CI deprecation warnings (chore)
- decouple from OpenCode-specific framing (docs: readme)
- session snapshot (chore: worktree)

# Changelog

## 2026-04-21 (v1.1.1)

### Port range expansion for slot groups

#### What changed
- `MaxPort` expanded from `6300` to `6325` (range doubled from 25 to 50 ports).
- Existing configs using ports in the original 6276–6300 range continue to validate unchanged.

#### Why
- v1.1.0 slot groups consume `count + 1` ports per group (N slots plus one virtual port). Realistic deployments with 3–4 stateful pools hit the original 25-port ceiling before the feature could be adopted.

#### Migration
- No action required for existing configs. Ports 6301–6325 are now available for new servers or slot-group expansion.

## 2026-04-21 (v1.1.0)

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
