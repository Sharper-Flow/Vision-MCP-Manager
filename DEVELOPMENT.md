# Development Guidelines

## Configuration Safety

**IMPORTANT**: During development, we do NOT modify the machine's global configuration.

### Protected Locations (Do Not Touch Until Release)

- `~/.config/vision/` - Vision server registry
- `~/.config/opencode/opencode.jsonc` - Global OpenCode config
- `~/.config/opencode/` - OpenCode config directory

### Development Configuration

All development and testing uses local fixtures:

```
~/dev/vision/
├── testdata/
│   ├── configs/
│   │   ├── valid-servers.yaml      # Valid config for happy path tests
│   │   ├── invalid-servers.yaml    # Malformed config for error tests
│   │   ├── empty-servers.yaml      # Empty config edge case
│   └── fixtures/
│       └── ...                     # Other test fixtures
```

### Testing Strategy

1. **Unit tests**: Use in-memory configs or `testdata/` fixtures
2. **Integration tests**: Use temp directories (`t.TempDir()`)
3. **Manual testing**: Use `--config` flag to specify test config path

```bash
# Run with test config (not global)
./bin/vision --config ./testdata/configs/valid-servers.yaml daemon start

# Or use environment variable
VISION_CONFIG=./testdata/configs/valid-servers.yaml ./bin/vision daemon start
```

### Local Dev Deploy

Vision installs as a real binary at `~/.local/bin/vision`, supervised by the
user systemd unit `vision.service`. During development you should NOT run the
daemon out of `./bin/vision` against your live `~/.config/vision/servers.yaml`
— do all out-of-tree work with `--config testdata/...` (see above), and use
the deploy script to flip the installed binary when you want to test against
the real machine config.

```bash
# Build + copy to ~/.local/bin/vision (daemon keeps running OLD binary)
./scripts/deploy-local.sh
# or: make deploy-local

# Build + copy + restart vision.service (daemon flips to NEW binary)
./scripts/deploy-local.sh --restart
# or: make deploy-local-restart

# Drift check (exit 1 if installed differs from a fresh build)
./scripts/deploy-local.sh --check
# or: make deploy-local-check

# Preview without writing
./scripts/deploy-local.sh --dry-run
```

Properties:

- **Real file copy, never a symlink.** Stale symlinks across the dev/install
  boundary fail under `realpath`, backup tools, and dev-path moves.
- **Idempotent.** Re-running with no code changes is a no-op (`cmp -s`).
- **Scoped writes.** Only `~/.local/bin/vision` is touched. Protected
  locations (`~/.config/vision/`, `~/.config/opencode/`)
  are never modified by this script — those remain `scripts/install.sh`'s
  responsibility for first-time setup.
- **Explicit restart.** Service restart is opt-in via `--restart` so you can
  stage a new binary without disrupting a running daemon.

This is the deploy-local pattern used across the toolbox (see
`~/dev/advance/scripts/deploy-local.sh` and `~/dev/omp/Makefile install`).
Do not edit `~/.local/bin/vision` directly — always redeploy from source.

### Pre-Release Checklist

Before modifying global config:

- [ ] All tests pass
- [ ] Manual testing complete with test configs
- [ ] User explicitly approves global config changes
- [ ] Backup existing configs if they exist
- [ ] Migration from legacy MCPM tested (if applicable)

### Config Flag Implementation

The CLI must support:

```go
rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", 
    "config file (default: ~/.config/vision/servers.yaml)")
```

This allows all development to use project-local configs.

## Integration Testing

### Running Integration Tests

```bash
# Run all integration tests
go test ./internal/integration/... -v

# Run a specific test
go test ./internal/integration/... -v -run TestFullRequestFlow

# Skip integration tests (short mode)
go test ./internal/integration/... -short
```

### Test Architecture

Integration tests use embedded Node.js MCP servers defined as JavaScript strings in the test file. This ensures:
- **Reproducibility**: No external dependencies needed
- **Speed**: Servers start immediately
- **Control**: Tests can define custom server behavior (echo, crash, etc.)

### Key Findings from Integration Testing

1. **Cleanup Order Matters**: When tearing down tests, resources must be closed in this order:
   - Port manager (stops HTTP listener)
   - Context cancel (triggers supervisor to terminate subprocess)
   - Wait for subprocess termination (pipes close naturally)
   - Bridge close (readLoop exits because pipes are closed)

2. **Bridge ReadLoop Blocking**: The bridge's `readLoop` blocks on `scanner.Scan()` until the underlying pipe is closed. Cancelling the context alone is not sufficient - the subprocess must terminate to close the stdout pipe.

3. **Concurrent Access**: The bridge correctly handles concurrent requests through:
   - Mutex-protected stdin writes
   - Request ID correlation for response matching
   - Pending request channels for async response routing

### Test Coverage

| Test | What it validates |
|------|-------------------|
| `TestBridgeWithPipes` | Bridge works with mock in-memory pipes |
| `TestFullRequestFlow` | HTTP → Bridge → Subprocess → Response |
| `TestConcurrentClients` | 10 simultaneous clients work correctly |
| `TestCrashRecovery` | Supervisor restarts crashed processes |
| `TestHealthEndpoint` | Per-server /health endpoint works |
| `TestHotReload` | Config file changes trigger server add/remove |

### Race Detection

All tests pass with the Go race detector:

```bash
go test -race ./... -count=1
```

Key concurrency fixes:
1. **ManagedProcess I/O**: `Stdin()` and `Stdout()` are protected by a separate `ioMu` mutex to prevent races between spawn() and accessors.
2. **HealthChecker mockBridge**: Test mock uses thread-safe setters for concurrent access during recovery tests.
3. **Daemon management port**: Now configurable via `ManagementPort` in daemon.Config.

## Proxy Downstream Respawn

When a downstream MCP subprocess becomes unavailable (reaped by idle timeout,
crashed, or otherwise closed), the proxy layer transparently respawns a new
subprocess on the next tool call instead of returning `ErrDownstreamUnavailable`.

### How It Works

1. `makeProxyToolHandler` detects `downstreamClosed == true` or `downstream == nil`
2. Calls `proxySession.respawnDownstream()` which:
   - Acquires `respawnMu` to serialize concurrent respawn attempts
   - Double-checks the downstream state (another goroutine may have already respawned)
   - Spawns a new subprocess via `session.Manager.SpawnSession()`
   - Re-discovers tools from the new downstream
   - Atomically swaps the downstream pointer and resets the closed flag
3. If respawn succeeds, the tool call proceeds normally against the new downstream
4. If respawn fails, `ErrDownstreamUnavailable` is returned as before

### Concurrency Safety

- `respawnMu` ensures only one goroutine spawns a new subprocess
- Other concurrent callers block on `respawnMu`, then see the already-respawned
  downstream via the double-check pattern
- `closeOnce` is reset after respawn so the new downstream can be closed cleanly

### Test Coverage

| Test | What it validates |
|------|-------------------|
| `TestProxyHandler_RespawnAfterReap` | Tool call succeeds after downstream is reaped (different PID confirms new subprocess) |
| `TestProxyHandler_ConcurrentRespawn` | 5 concurrent callers all succeed with only 1 respawn (single PID) |
