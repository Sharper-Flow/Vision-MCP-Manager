# Vision — stdio subprocess leak in per-session MCP proxy

**Status:** Fix landed in working tree on 2026-04-14. Daemon restart required to deploy. Test added: `TestProxySession_DownstreamNotificationsDoNotTouchSession`.

## Fix summary

Root cause: `proxySession.touch()` was called from three downstream-initiated notification handlers (`handleToolListChanged`, `handleLoggingMessage`, `handleProgress` in `internal/mcp/proxy.go`). Any chatty downstream server kept refreshing `LastActivity`, defeating the idle reaper even when the upstream client process was long dead.

The fix removes those three `ps.touch()` calls. Touch is now only called on:
- Upstream HTTP requests carrying a session header (proxy.go ~line 406)
- Tool-call dispatch from upstream client (`makeProxyToolHandler`, proxy.go ~line 859)

These are the only legitimate signals that a live client is still using the session. Once they stop arriving, the existing reaper (`internal/session/manager.go reapExpiredSessions`) idle-outs the session within `SessionTimeout` (default 5m) and tears down the downstream subprocess.

For lgrep specifically, `~/.config/vision/servers.yaml` was tightened to `session_timeout: 5m` (down from 30m) given each lgrep child holds 1.5–2 GB RSS. Other servers can keep their own values.

Build + tests: `cd ~/dev/vision && go test ./... -timeout 120s` → all packages pass. The new regression test fails on the un-patched code (verified by reverting locally) and passes after the fix.

## Original severity: HIGH — `lgrep` is the worst case

The leak affects every stdio server proxied through Vision, but **`lgrep` is dramatically worse than `svelte-mcp`** because each lgrep subprocess holds a full semantic index (Lance DB + embeddings) resident and runs 80–140 threads. Measured on Tue Apr 14 ~19:55 local:

```
11 lgrep processes under one Vision daemon
Total RSS:  21.3 GB    (avg ~1.9 GB each, peak 2.5 GB)
Total threads: ~1250
inotify instances: 33   (auto-watch on project trees)
Oldest process: 1h49m — outlived its opencode client long ago
```

At the same time:
- `svelte-mcp`: 14 children × ~195 MB = 2.6 GB
- Host memory: 30 GB / 41 GB used, 3.7 GB swap in use, 1.4 GB free

Combined, Vision-managed stdio leaks accounted for **~24 GB resident**, roughly 60% of host RAM, forcing OpenCode sessions into swap thrash.

### Why this manifests as "pokeedge-web freezes the TUI"

Not every project trips it with the same intensity. Concrete failure mode observed:

1. `LGREP_WARM_PATHS` in `~/.config/vision/servers.yaml` names three pre-warmed projects. Any other project is cold on first query.
2. `~/dev/pokeedge-web` is the largest *cold* project the user regularly opens (790 source files, 192 MB embedding index). First semantic query embeds all chunks via Voyage API, which can exceed lgrep's internal `TOOL_TIMEOUT_S=45s`.
3. Each new opencode session there spawns a **fresh** lgrep subprocess via `session.Manager.SpawnSession`. When that opencode later exits without a clean MCP DELETE, the subprocess is orphaned (still holding its 1.5–2.5 GB index + Voyage client state).
4. After 6–10 such cycles, 15–20 GB of RAM is held by zombie lgreps. Each new pokeedge-web session's cold index build now happens under swap pressure. The lgrep MCP call stalls past the 55 s Vision `request_timeout`. The OpenCode TUI event loop, already waiting on the MCP response, appears frozen — animation spinning, input locked, only `/exit` or Ctrl-C recovers.
5. Smaller projects (scratch dirs, already-warm backends) don't trigger it because the first query returns in <1 s.

### Why `vision_restart` is not a full fix

`vision_restart lgrep` restarts the Vision supervisor/proxy entry but **does not reap per-session children** — they detach to init (ppid=1 equivalent under Vision daemon) and keep running. Empirical sequence:

```
vision_restart lgrep        → 11 orphans → 10 → 8 over 8s (natural exits)
kill -TERM remaining 5 PIDs → 0 orphans, 19 GB RAM recovered
```

Operators must run `pgrep -f '^lgrep$' | xargs kill -TERM` after `vision_restart` to actually reclaim memory.

**Observed:** Tue Apr 14 19:40 local. 14 orphaned `svelte-mcp` stdio subprocesses alive under a single Vision daemon PID, consuming ~2.6 GB RSS combined. Host-wide memory pressure followed (30/41 GB used, 3.7 GB swap), which caused OpenCode TUI sessions in large workspaces (`~/dev/pokeedge-web`) to freeze — animation running, input locked, only `/exit` or `Ctrl-C` recovered the session.

## Reproduction signals

```
# All leaked children reparented to one Vision daemon:
$ ps -eo pid,ppid,rss,cmd | grep svelte-mcp
3142356  3273888  31MB   node .../bin/svelte-mcp
3245842  3273888  31MB   node .../bin/svelte-mcp
3538373  3273888  195MB  node .../bin/svelte-mcp
...  (14 total, all ppid=3273888)

# Meanwhile only ~10 live opencode processes were using svelte-mcp.
# Some opencodes had exited without triggering cleanup.
```

After `vision_restart svelte-mcp` all children were reaped, RAM freed 2.6 GB, TUI freezes stopped.

## Root cause analysis

Vision creates one stdio subprocess **per upstream MCP client session** via `session.Manager.SpawnSession` (`internal/mcp/proxy.go` near line 707). This is by design for isolation, but cleanup depends on:

1. Explicit client `DELETE /mcp` request → `RemoveSession` → `SetOnSessionRemoved` → `closeDownstream`.
2. Idle reaper (`internal/session/manager.go:261 reapExpiredSessions`) firing `SessionTimeout` (default 5m).

**Observed failure mode: neither path fires for silently-dying clients.**

### Path 1: missing DELETE from client

OpenCode MCP clients that crash, get SIGKILL'd, or have their TUI force-closed never send the HTTP DELETE. The proxy HTTP handler only records a tombstone and removes the session when it sees `r.Method == http.MethodDelete && sessionHeader != ""` (proxy.go around line 412). No such detection on raw socket/SSE disconnect.

Note: `mcp.NewStreamableHTTPHandler` (Go MCP SDK) will emit a server-sent-events stream. When the underlying TCP peer goes away, the write should fail on next send — but without a write there is no signal, and an idle session can hold the TCP keepalive open for minutes before the OS closes it.

### Path 2: idle reaper never triggers

`proxySession.touch()` is called at **five** call sites (proxy.go lines 406, 749, 819, 838, 859), including:

- line 406: `ps.touch()` on **every upstream HTTP request**, including session initialization, tool list requests, and implicit polling.
- line 819/838/859: `touch()` on **every downstream notification** — `tools/list_changed`, logging messages, progress updates — so a chatty downstream can keep its own session fresh with zero upstream activity.

Combined effect: `LastActivity` is continually refreshed as long as any MCP traffic touches the proxy, even if the upstream client process is dead. The 5m idle reaper essentially never fires for stdio children whose downstream is still alive-but-chatty, or whose upstream HTTP connection hasn't fully FIN'd yet.

### Path 3: supervisor does not reap stdio children

`internal/server/registry.go:207` deliberately skips supervisor registration for stdio transports ("session.Manager is the sole lifecycle owner"). This is correct in isolation but means the supervisor can't be a safety net when session.Manager misses a cleanup.

## Suggested fix directions

Rank by impact × cost:

1. **Detect broken upstream HTTP stream → immediate RemoveSession.** The Go MCP SDK's `StreamableHTTPHandler` exposes request context cancellation. Wire a `context.AfterFunc` (or `<-r.Context().Done()`) in the per-session HTTP path so that when the peer closes the TCP connection, the associated `byUpstream[sessionHeader]` entry is torn down via `mgr.RemoveSession(ps.sessionID)`. This is the cheapest correct fix — most clients disconnect cleanly even if the process dies.
2. **Scope `touch()` to client-originated activity only.** Remove `touch()` from downstream-notification handlers (lines 819, 838, 859). Idle means "no client traffic", not "no traffic at all". This lets the reaper actually expire abandoned sessions.
3. **Add a hard session cap per server** (`MaxSessions`) and default it to `3 × max_parallel_clients` or similar. When capacity is hit, evict the oldest idle session. Already scaffolded in `config/schema.go:96` but currently defaults to 0 (unlimited).
4. **Default `SessionTTL` to something like 2h** for stdio servers so even chatty sessions eventually recycle. Currently 0 (disabled).
5. **Add a Vision-daemon watchdog** that every ~60s counts orphaned stdio PIDs whose registered session ID no longer exists in `session.Manager` and reaps them. Useful as a belt-and-suspenders backstop; not strictly required if (1) + (2) are correct.

## Candidate code touchpoints

- `internal/mcp/proxy.go:230` — `NewStreamableHTTPHandler` callback; add peer-disconnect detection here
- `internal/mcp/proxy.go:819,838,859` — drop `touch()` from downstream notification relays
- `internal/session/manager.go:261 reapExpiredSessions` — already correct; just needs `LastActivity` to actually reflect client activity
- `internal/config/schema.go:100` — consider lowering `SessionTTL` default from `0s` for stdio transports

## Test plan sketch

- Integration test: spin up a fake stdio server, connect 5 upstream clients, kill all 5 client processes with SIGKILL, assert all 5 downstream PIDs are reaped within 10s (not 5 minutes).
- Unit test: verify `touch()` is NOT called from `handleToolListChanged`, `handleLoggingMessage`, `handleProgress`.
- Regression: ensure a healthy long-running session (tool calls every 1m) is NOT reaped across a 30m window.

## Related / out of scope

- ADV plugin was also failing to load on sessions where `project.json` had stale `features.slop_scan.defensive_guard_threshold: 0.25` (float). This is tracked separately and patched on 2026-04-14 in `oc-plugins/advance/plugin/src/storage/json.ts` (graceful fallback to defaults on ZodError). Not a Vision bug.
