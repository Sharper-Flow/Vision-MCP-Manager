# Playwright Managed HTTP Canary

## Window

- Status: closed — disposition recorded 2026-07-20 (accept-accumulated-evidence)
- Started: `2026-07-20T01:11:20Z` (`2026-07-19T21:11:20-04:00`)
- Original earliest completion: `2026-07-22T01:11:20Z`
- Environment: shared development host
- Change: `fixPlaywrightSessionIsolation`

## Disposition (2026-07-20)

The original 48-hour window was interrupted before completion when the operator
deployed the reconciled bridge binary and restarted `vision.service` (violating
success signal #3 and changing the baseline binary). The window is therefore
not clean and cannot be read at its original completion time.

Decision (user-approved): accept the canary as satisfied by accumulated
equivalent-code evidence rather than restart a fresh 48-hour soak, because:

- The reconciled bridge binary `v1.3.1-18-g6c55628` contains the identical
  managed-native-HTTP implementation as the prior canary baseline
  `v1.3.1-7-g2f8c0c4-dirty` (`2f8c0c4`); the only added code is the disjoint
  `alignVisionCodeMode` work, which does not touch the managed-http/Playwright
  path (verified: `git diff 3ecd6fd..alignVisionCodeMode -- opencode-plugin/`
  and admin/catalog/config changes do not intersect the gateway/lease/supervisor
  files).
- Correctness is independently proven by the pinned real Playwright integration
  test `tr_mrsifq9i_9e216350` (AC1-AC9: two-client BrowserContext isolation over
  10 alternating rounds, six-session capacity/no-eviction, 30-minute
  application-idle expiry, in-flight boundary, forced process kill + readiness-
  gated restart within 30s, stale-ID pre-dispatch 404, bounded secret-safe
  lifecycle diagnostics), plus race repeat `tr_mrsighk4_8b104675`.
- Live smoke on the bridge binary confirms `state=running`,
  `transport=managed-http` semantics via `session_lifecycle` present in
  `vision_list`, backend ready, external endpoint unchanged at
  `http://127.0.0.1:6287/mcp`.

Rollback path remains available and tested (`tr_mrsirgqc_c50f2893`, navigable
browser in 1.921s). No cross-session leakage, capacity false-denial, or restart
loop was observed during the ~20h of pre-interruption soak under equivalent
code.

## Deployment Baseline

- Installed Vision: `v1.3.1-7-g2f8c0c4-dirty`, commit `2f8c0c4`
- External endpoint unchanged: `http://127.0.0.1:6287/mcp`
- Internal backend: `http://127.0.0.1:16287/mcp`, loopback-only
- Playwright MCP: local pinned installation `0.0.77`
- Browser: `--browser chromium --headless --isolated`
- Capacity: 6
- Application-idle expiry: 30 minutes
- Live config backup: `~/.config/vision/servers.yaml.pre-fixPlaywrightSessionIsolation.bak`
- Environment backup: `~/.config/vision/environment.pre-fixPlaywrightSessionIsolation.bak`

Only the Playwright server entry changed. The obsolete `PLAYWRIGHT_HOST_PLATFORM_OVERRIDE=ubuntu24.04-x64` was removed after the pinned real test passed on Ubuntu 26.04 without it (`tr_mrsiwyfq_4b8d14f8`).

## Start Evidence

- `vision config validate`: pass
- `vision.service`: active after explicit restart
- Admin projection: `state=running`, `transport=managed-http`, `backend_state=ready`, `restart_count=0`
- Listener ownership: Vision on `127.0.0.1:6287`; Playwright child on `127.0.0.1:16287`
- Live browser smoke through port 6287: pass (`browser_navigate` + `browser_snapshot`)
- Post-smoke lifecycle: `capacity_used=0`, one closed row, reason `client_delete`
- Warning journal after deployment: empty
- OpenCode source/config/dependencies: unchanged
- Obsolete Ubuntu 24.04 host override removed from Vision environment and `~/.zshrc`
- Global Playwright skill now documents managed native HTTP, pinned Chromium headless mode, per-session BrowserContext isolation, and native Ubuntu 26.04 support
- Non-secret system backup and environment docs synchronized in isolated toolbox branch `config/playwright-managed-default`

## Success Signals

At or after earliest completion, all must hold:

1. Zero reported cross-session page, cookie, navigation, or element-reference leaks.
2. Zero admission denials while fewer than six sessions had application activity in the preceding 30 minutes.
3. No operator-triggered Vision restart, profile deletion, lock deletion, or manual session cleanup after canary start.
4. Port `6287` remains the client endpoint with no OpenCode changes.
5. Initialization, expiry, cleanup, restart, and denial events remain explainable through `session_lifecycle`, metrics, and journal evidence.

## Completion Read

Run one bounded read after `2026-07-22T01:11:20Z`:

```bash
vision --json daemon status
curl -fsS http://127.0.0.1:6275/v1/servers/playwright | jq '{
  state, transport, restart_count, session_metrics, session_lifecycle
}'
journalctl --user -u vision.service --since '2026-07-20 01:11:20 UTC' --no-pager
```

Record counts and any user-reported isolation incidents in this file. Do not poll continuously.

## Rollback Triggers

Rollback immediately for cross-session leakage, repeated false capacity denial, backend restart loops, failure to return `ready` within 30 seconds, or inability to explain lifecycle state.

Restore the two backups, run `vision config validate`, and restart `vision.service`. The tested stateful-stdio rollback path reached a navigable real browser in 1.921 seconds (`tr_mrsirgqc_c50f2893`); target remains under 10 minutes.
