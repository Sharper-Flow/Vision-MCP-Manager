# Legacy MCPM → Vision Transition Audit

**Date:** 2026-01-19  
**Purpose:** Ensure complete and correct migration from the legacy MCPM stack to Vision

---

## 1. Architecture Comparison

### Legacy MCPM Architecture
```
Claude Code Config (~/.claude.json)
    │
    └── HTTP Profiles ──────────────► MCPM Docker Container (mcp-daemon)
        ├── essentials:6276             ├── time
        │                               └── fetch-mcp
        ├── memory:6277                 ├── basic-memory
        │                               └── mem0-mcp
        ├── dev-core:6278               ├── context7
        │                               ├── sonarqube
        │                               └── svelte-mcp
        └── research:6281               ├── kagimcp
                                        ├── firecrawl
                                        └── arxiv-mcp
```

### Vision Architecture
```
Claude Code Config (~/.claude.json)
    │
    └── HTTP Servers ───────────────► Vision Daemon (systemd)
        ├── context7:6276               Admin MCP on 6275
        ├── sonarqube:6277              └── vision_* tools
        ├── svelte-mcp:6278
        ├── kagimcp:6279
        ├── arxiv-mcp:6280
        └── firecrawl:6281
```

**Key Difference:** Vision doesn't use "profile" aggregation. Each server gets its own port.

---

## 2. Port Mapping Comparison

### Current (Legacy MCPM)
| Profile | Port | Servers |
|---------|------|---------|
| essentials | 6276 | time, fetch-mcp |
| memory | 6277 | basic-memory, mem0-mcp |
| dev-core | 6278 | context7, sonarqube, svelte-mcp |
| data | 6279 | mcp-server-qdrant |
| frontend | 6280 | svelte-mcp, figma-mcp, playwright |
| research | 6281 | kagimcp, firecrawl, arxiv-mcp |

### Vision (Configured)
| Server | Port | Notes |
|--------|------|-------|
| context7 | 6276 | ✅ Configured |
| sonarqube | 6277 | ✅ Configured |
| svelte-mcp | 6278 | ✅ Configured |
| kagimcp | 6279 | ✅ Configured |
| arxiv-mcp | 6280 | ✅ Configured |
| firecrawl | 6281 | ✅ Configured |
| time | 6282 | ✅ Configured |
| basic-memory | 6284 | ✅ Configured |
| lgrep | 6285 | ✅ Configured |
| sentry | 6289 | ✅ Configured |
| pokeedge-data-ops | 6290 | ✅ Configured |
| pokeedge-sync-ops | 6291 | ✅ Configured |

### ⚠️ GAPS IDENTIFIED

| Server | In Legacy MCPM | In Vision | Action Needed |
|--------|-----------|-----------|---------------|
| time | ✅ (essentials) | ✅ Configured | None |
| fetch-mcp | ✅ (essentials) | ❌ Missing | Optional / add only if needed |
| basic-memory | ✅ (memory) | ✅ Configured | None |
| mem0-mcp | ✅ (memory) | ❌ Missing | Optional |
| mcp-server-qdrant | ✅ (data) | ❌ Missing | Add to Vision |
| figma-mcp | ✅ (frontend) | ❌ Missing | Optional |
| playwright | ✅ (frontend) | ❌ Missing | Optional |
| gh_grep | ✅ | ✅ Direct remote MCP | Configure directly in OpenCode |

---

## 3. Claude Code Config Update Required

### Legacy Config (`~/.claude.json`)
```json
{
  "mcpServers": {
    "essentials": {
      "url": "http://localhost:6276/mcp",
      "type": "http"
    },
    "memory": {
      "url": "http://localhost:6277/mcp",
      "type": "http"
    },
    "dev-core": {
      "url": "http://localhost:6278/mcp",
      "type": "http"
    },
    "research": {
      "url": "http://localhost:6281/mcp",
      "type": "http"
    }
  }
}
```

### Required Config for Vision
```json
{
  "mcpServers": {
    "vision-admin": {
      "url": "http://localhost:6275/mcp",
      "type": "http"
    },
    "context7": {
      "url": "http://localhost:6276/mcp",
      "type": "http"
    },
    "sonarqube": {
      "url": "http://localhost:6277/mcp",
      "type": "http"
    },
    "svelte-mcp": {
      "url": "http://localhost:6278/mcp",
      "type": "http"
    },
    "kagimcp": {
      "url": "http://localhost:6279/mcp",
      "type": "http"
    },
    "arxiv-mcp": {
      "url": "http://localhost:6280/mcp",
      "type": "http"
    },
    "firecrawl": {
      "url": "http://localhost:6281/mcp",
      "type": "http"
    },
    "time": {
      "url": "http://localhost:6282/mcp",
      "type": "http"
    },
    "basic-memory": {
      "url": "http://localhost:6284/mcp",
      "type": "http"
    },
    "gh_grep": {
      "url": "https://mcp.grep.app",
      "type": "remote"
    }
  }
}
```

---

## 4. Missing Servers to Add to Vision

### Priority 1: Essentials (Currently in Use)
```yaml
# Add to ~/.config/vision/servers.yaml

  time:
    port: 6282
    command: uvx
    args:
      - "mcp-server-time"
      - "--local-timezone=America/New_York"
    autostart: true

  basic-memory:
    port: 6284
    command: uvx
    args:
      - "basic-memory"
      - "mcp"
    autostart: true

```

### Priority 2: Data (If Needed)
```yaml
  mcp-server-qdrant:
    port: 6286
    command: uvx
    args:
      - "mcp-server-qdrant"
    env:
      QDRANT_URL: "${QDRANT_URL}"
      QDRANT_API_KEY: "${QDRANT_API_KEY}"
      COLLECTION_NAME: "pokeedge-codebase"
    autostart: false  # Start on demand
```

---

## 5. Environment Variables Audit

### Required (Currently Set)
| Variable | Status | Used By |
|----------|--------|---------|
| CONTEXT7_API_KEY | ✅ | context7 |
| SONARQUBE_TOKEN | ✅ | sonarqube |
| SONARQUBE_ORG | ✅ | sonarqube |
| SONARQUBE_URL | ✅ | sonarqube |
| KAGI_API_KEY | ✅ | kagimcp |
| FIRECRAWL_API_KEY | ✅ | firecrawl |
| QDRANT_API_KEY | ✅ | mcp-server-qdrant (if added) |
| OPENAI_API_KEY | ✅ | mem0-mcp (if added) |

### Missing (Need to Add)
| Variable | Status | Used By |
|----------|--------|---------|
| QDRANT_URL | ❌ | mcp-server-qdrant |

---

## 6. Transition Checklist

### Before Go-Live
- [ ] Add any still-needed optional servers to Vision config (for example fetch-mcp, qdrant, mem0-mcp)
- [ ] Add QDRANT_URL to env file
- [ ] Update Claude Code config with Vision servers
- [ ] Test Vision daemon starts without errors

### Go-Live
- [ ] Stop MCPM: `docker stop mcp-daemon`
- [ ] Start Vision: `systemctl --user start vision`
- [ ] Verify health: `curl localhost:6275/health`
- [ ] Test tools in Claude Code session

### Post Go-Live
- [ ] Monitor logs: `journalctl --user -u vision -f`
- [ ] Verify all tools respond
- [ ] Remove legacy MCPM endpoints from Claude Code config (optional, for cleanup)

---

## 7. Rollback Plan

```bash
# If Vision fails:
systemctl --user stop vision
docker start mcp-daemon

# Revert Claude Code config if changed
# (Keep backup of current config before editing)
```

---

## 8. Transition Completed

### Servers Added to Vision
- ✅ time (port 6282)
- ✅ basic-memory (port 6284)

### Remote MCPs Configured Directly In OpenCode
- ✅ gh_grep (`https://mcp.grep.app`)

### Files Prepared
- ✅ `~/.config/vision/servers.yaml` - 10 servers configured
- ✅ `~/.claude.json.backup-legacy` - Backup of current config
- ✅ `~/.claude.json.vision` - New config ready to apply

### Go-Live Commands
```bash
# 1. Stop MCPM
docker stop mcp-daemon

# 2. Start Vision
systemctl --user start vision

# 3. Wait for servers to start (10-15 seconds)
sleep 15

# 4. Verify health
curl http://localhost:6275/health

# 5. Restart OpenCode to pick up new config
# (Config already updated in ~/.config/opencode/opencode.json)
```

### Rollback Commands
```bash
# Stop Vision
systemctl --user stop vision

# Restore previous OpenCode config
cp ~/.config/opencode/opencode.json.backup-legacy ~/.config/opencode/opencode.json

# Restart MCPM
docker start mcp-daemon

# Restart OpenCode
```

---

## 9. Final Server Mapping

| Server | Port | Transport | alwaysAllow |
|--------|------|-----------|-------------|
| vision-admin | 6275 | http | - |
| context7 | 6276 | stdio→http | - |
| sonarqube | 6277 | stdio→http | - |
| svelte-mcp | 6278 | stdio→http | - |
| kagimcp | 6279 | stdio→http | ✅ |
| arxiv-mcp | 6280 | stdio→http | ✅ |
| firecrawl | 6281 | stdio→http | ✅ |
| time | 6282 | stdio→http | ✅ |
| basic-memory | 6284 | stdio→http | - |
