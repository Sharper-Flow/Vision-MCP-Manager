# Development Guidelines

## Configuration Safety

**IMPORTANT**: During development, we do NOT modify the machine's global configuration.

### Protected Locations (Do Not Touch Until Release)

- `~/.config/vision/` - Vision server registry
- `~/.opencode.json` - Global OpenCode config
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
│   │   └── opencode.json           # Test OpenCode config
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

### Pre-Release Checklist

Before modifying global config:

- [ ] All tests pass
- [ ] Manual testing complete with test configs
- [ ] User explicitly approves global config changes
- [ ] Backup existing configs if they exist
- [ ] Migration from Jarvis/MCPM tested (if applicable)

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
