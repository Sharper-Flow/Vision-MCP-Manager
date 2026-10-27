package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const sharedCatalogServerJS = `
const fs = require('fs');
const readline = require('readline');
let tools = JSON.parse(fs.readFileSync(process.env.CATALOG_PATH, 'utf8'));
const send = (msg) => process.stdout.write(JSON.stringify(msg) + '\n');
readline.createInterface({ input: process.stdin }).on('line', (line) => {
  const msg = JSON.parse(line);
  if (msg.method === 'initialize') {
    send({ jsonrpc: '2.0', id: msg.id, result: {
      protocolVersion: '2025-03-26', capabilities: { tools: { listChanged: true } },
      serverInfo: { name: 'shared-catalog-fixture', version: '1.0.0' }
    }});
  } else if (msg.method === 'tools/list') {
    send({ jsonrpc: '2.0', id: msg.id, result: { tools } });
  } else if (msg.method === 'tools/call') {
    if (msg.params.name === 'change_catalog') {
      tools = JSON.parse(fs.readFileSync(process.env.CATALOG_PATH, 'utf8'));
      send({ jsonrpc: '2.0', method: 'notifications/tools/list_changed' });
    }
    send({ jsonrpc: '2.0', id: msg.id, result: {
      content: [{ type: 'text', text: 'ok' }]
    }});
  } else if (msg.id !== undefined) {
    send({ jsonrpc: '2.0', id: msg.id, result: {} });
  }
});
`

func sharedCatalogTools(field, fieldType, exclusive string) []*mcp.Tool {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{field: map[string]any{"type": fieldType}},
		"required":   []string{field},
	}
	return []*mcp.Tool{
		{Name: "change_catalog", InputSchema: map[string]any{"type": "object"}},
		{Name: "stable", InputSchema: schema, OutputSchema: schema},
		{Name: exclusive, InputSchema: map[string]any{"type": "object"}},
	}
}

func writeSharedCatalog(t *testing.T, path string, tools []*mcp.Tool) {
	t.Helper()
	data, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func sharedCatalogJSON(t *testing.T, tools []*mcp.Tool) string {
	t.Helper()
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	data, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestProxyHandler_SharedCatalogRefresh(t *testing.T) {
	for _, trigger := range []string{"respawn", "list_changed"} {
		t.Run(trigger, func(t *testing.T) {
			skipIfNoNode(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			catalogPath := filepath.Join(t.TempDir(), "catalog.json")
			initial := sharedCatalogTools("old_value", "string", "old_only")
			updated := sharedCatalogTools("new_value", "integer", "new_only")
			writeSharedCatalog(t, catalogPath, initial)
			cfg := testServerConfig()
			cfg.Args = []string{"-e", sharedCatalogServerJS}
			cfg.Env = map[string]string{"CATALOG_PATH": catalogPath}
			logger := testLogger(t)
			sm := session.NewSharedSessionManager("shared-catalog", cfg, logger, 0, nil)
			defer sm.CloseAll()
			if _, err := sm.GetOrCreateSession(ctx, "fixture"); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(NewProxyHandler(ProxyConfig{
				ServerName: "shared-catalog", SharedManager: sm, Logger: logger,
			}))
			defer server.Close()
			clients := make([]*mcp.ClientSession, 2)
			changes := make([]chan struct{}, len(clients))
			for i := range clients {
				changed := make(chan struct{}, 32)
				changes[i] = changed
				client := mcp.NewClient(&mcp.Implementation{Name: "catalog-client", Version: "1.0.0"}, &mcp.ClientOptions{
					ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
						select {
						case changed <- struct{}{}:
						default:
						}
					},
				})
				var err error
				clients[i], err = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL}, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer clients[i].Close()
				listed, err := clients[i].ListTools(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := sharedCatalogJSON(t, listed.Tools), sharedCatalogJSON(t, initial); got != want {
					t.Fatalf("initial catalog = %s, want %s", got, want)
				}
			}
			writeSharedCatalog(t, catalogPath, updated)
			if trigger == "respawn" {
				if err := sm.Downstream().Close(); err != nil {
					t.Fatal(err)
				}
				// Closure reaches the manager through its asynchronous Wait observer.
				for sm.HasDownstream() {
					if ctx.Err() != nil {
						t.Fatal("manager did not observe downstream closure")
					}
					time.Sleep(time.Millisecond)
				}
				if _, err := sm.GetOrCreateSession(ctx, "fixture"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := clients[0].CallTool(ctx, &mcp.CallToolParams{Name: "change_catalog"}); err != nil {
					t.Fatal(err)
				}
			}
			for i, client := range clients {
				awaitSharedCatalog(t, ctx, client, changes[i], updated, i)
			}
		})
	}
}

func TestSharedCatalogSnapshotAfterCloseDoesNotReacquireLease(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	logger := testLogger(t)
	sm := session.NewSharedSessionManager("closed-catalog", testServerConfig(), logger, 0, nil)
	defer sm.CloseAll()
	var ps *proxySession
	_, err := newSharedModeServer(ctx, "closed-catalog", sm, logger,
		nil, nil, nil, time.Second, RetryConfig{}, CircuitBreakerConfig{}, nil, nil, nil,
		func(p *proxySession) { ps = p }, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sm.RefCount() != 1 {
		t.Fatalf("initial shared leases = %d, want 1", sm.RefCount())
	}

	// A callback copied before close may still invoke refresh after lease retirement.
	ps.closeDownstream("test close")
	if sm.RefCount() != 0 {
		t.Fatalf("shared leases after close = %d, want 0", sm.RefCount())
	}
	ps.handleToolListChanged(ctx)
	if sm.RefCount() != 0 || sm.SessionCount() != 0 {
		t.Errorf("stale notification reacquired a retired lease: refs=%d sessions=%d", sm.RefCount(), sm.SessionCount())
	}
	ps.downstreamMu.RLock()
	defer ps.downstreamMu.RUnlock()
	if !ps.downstreamClosed || ps.downstream != nil {
		t.Error("stale notification revived the closed proxy's downstream")
	}
}

func TestSharedRespawnDoesNotWaitOnProxyRefresh(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	logger := testLogger(t)
	sm := session.NewSharedSessionManager("respawn-lock-order", testServerConfig(), logger, 0, nil)
	defer sm.CloseAll()
	var ps *proxySession
	_, err := newSharedModeServer(ctx, "respawn-lock-order", sm, logger,
		nil, nil, nil, time.Second, RetryConfig{}, CircuitBreakerConfig{}, nil, nil, nil,
		func(p *proxySession) { ps = p }, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ps.closeDownstream("test close")

	// A respawn under the manager lock must not wait on a proxy's refresh lock.
	ps.respawnMu.Lock()
	if err := sm.Downstream().Close(); err != nil {
		ps.respawnMu.Unlock()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := sm.GetOrCreateSession(ctx, "respawner")
		done <- err
	}()
	select {
	case err = <-done:
		ps.respawnMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		ps.respawnMu.Unlock()
		<-done
		t.Fatal("shared respawn waited on a proxy refresh while holding the manager lock")
	}
}

func TestSharedFailedDiscoveryClosesAfterRefreshUnlock(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	logger := testLogger(t)
	sm := session.NewSharedSessionManager("discovery-lock-order", testServerConfigWithScript(discoveryFailureMCPServerJS), logger, 0, nil)
	defer sm.CloseAll()
	var ps *proxySession
	refreshUnlocked := false
	_, err := newSharedModeServer(ctx, "discovery-lock-order", sm, logger,
		nil, nil, nil, time.Second, RetryConfig{}, CircuitBreakerConfig{}, nil, nil, nil,
		func(p *proxySession) { ps = p }, nil, nil,
		func(string) {
			// A manager-lock holder can be waiting on respawnMu while this close releases the lease.
			if refreshUnlocked = ps.respawnMu.TryLock(); refreshUnlocked {
				ps.respawnMu.Unlock()
			}
		})
	if err == nil || !strings.Contains(err.Error(), "failed to list downstream tools") {
		t.Fatalf("shared registration error = %v, want failed initial tools/list", err)
	}
	if !refreshUnlocked {
		t.Fatal("failed shared discovery closed its lease while holding respawnMu")
	}
}

func awaitSharedCatalog(t *testing.T, ctx context.Context, client *mcp.ClientSession, changes <-chan struct{}, want []*mcp.Tool, index int) {
	t.Helper()
	wantJSON := sharedCatalogJSON(t, want)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-changes:
			listed, err := client.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if sharedCatalogJSON(t, listed.Tools) == wantJSON {
				return
			}
		case <-deadline.C:
			listed, err := client.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Errorf("client %d did not receive list_changed with the refreshed catalog: got %s, want %s", index, sharedCatalogJSON(t, listed.Tools), wantJSON)
			return
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
