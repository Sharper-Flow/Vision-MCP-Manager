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
