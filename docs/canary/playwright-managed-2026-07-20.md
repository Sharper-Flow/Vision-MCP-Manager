# Playwright Managed HTTP Canary

## Window

- Status: running
- Started: `2026-07-20T01:11:20Z` (`2026-07-19T21:11:20-04:00`)
- Earliest completion: `2026-07-22T01:11:20Z`
- Environment: shared development host
- Change: `fixPlaywrightSessionIsolation`

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
