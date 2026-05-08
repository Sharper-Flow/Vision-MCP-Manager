package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const acceptanceEchoMCPServerJS = `
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin, terminal: false });
rl.on('line', (line) => {
  const msg = JSON.parse(line);
  if (msg.method === 'initialize') {
    process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: msg.id, result: { protocolVersion: '2025-03-26', capabilities: { tools: { listChanged: false } }, serverInfo: { name: 'echo-test', version: '1.0.0' } } }) + '\n');
  } else if (msg.method === 'notifications/initialized') {
  } else if (msg.method === 'tools/list') {
    process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: msg.id, result: { tools: [{ name: 'echo', description: 'Echoes input back', inputSchema: { type: 'object', properties: { message: { type: 'string' } } } }] } }) + '\n');
  } else if (msg.method === 'tools/call') {
    process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: msg.id, result: { content: [{ type: 'text', text: 'echo: ' + (msg.params?.arguments?.message || '') + ' pid=' + process.pid }] } }) + '\n');
  }
});
`

func skipIfNoNodeDaemon(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
}

func waitForTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to accept connections", addr)
}

func TestSlotGroupAcceptance_VirtualRoutingDirectPortAndVersion(t *testing.T) {
	skipIfNoNodeDaemon(t)
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "servers.yaml")

	data := []byte(fmt.Sprintf("slot_groups:\n  playwright:\n    template: playwright-slot\n    base_port: 6283\n    count: 2\n    group_port: 6282\n    defaults:\n      command: node\n      args: ['-e', %q]\n      autostart: true\n      session_timeout: 30s\n      max_sessions: 10\n      shared_read_only_tools: ['echo']\n      shared_result_cache_ttl: 1s\n", acceptanceEchoMCPServerJS))
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile(config): %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d, err := New(Config{ConfigPath: configPath, Logger: logger, ManagementPort: 6274})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if err := d.Start(); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	defer d.Stop(5 * time.Second)
	waitForTCP(t, "127.0.0.1:6274")
	waitForTCP(t, "127.0.0.1:6282")
	waitForTCP(t, "127.0.0.1:6283")
	waitForTCP(t, "127.0.0.1:6284")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	transport := &mcp.StreamableClientTransport{Endpoint: "http://127.0.0.1:6282/mcp"}
	client1 := mcp.NewClient(&mcp.Implementation{Name: "client-1", Version: "1.0.0"}, nil)
	client2 := mcp.NewClient(&mcp.Implementation{Name: "client-2", Version: "1.0.0"}, nil)
	sess1, err := client1.Connect(ctx, transport, nil)
	if err != nil { t.Fatalf("client1.Connect: %v", err) }
	defer sess1.Close()
	sess2, err := client2.Connect(ctx, transport, nil)
	if err != nil { t.Fatalf("client2.Connect: %v", err) }
	defer sess2.Close()

	listener1 := d.portManager.Get("playwright-slot-1")
	listener2 := d.portManager.Get("playwright-slot-2")
	if listener1 == nil || listener2 == nil {
		t.Fatal("slot listeners missing")
	}
	mgr1 := listener1.SessionManager.(*session.Manager)
	mgr2 := listener2.SessionManager.(*session.Manager)
	if mgr1.SessionCount() != 1 || mgr2.SessionCount() != 1 {
		t.Fatalf("expected virtual routing to distribute sessions, got slot1=%d slot2=%d", mgr1.SessionCount(), mgr2.SessionCount())
	}

	results := make([]string, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		res, err := sess1.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "shared"}})
		if err != nil {
			t.Errorf("sess1.CallTool failed: %v", err)
			return
		}
		results[0] = res.Content[0].(*mcp.TextContent).Text
	}()
	go func() {
		defer wg.Done()
		res, err := sess2.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "shared"}})
		if err != nil {
			t.Errorf("sess2.CallTool failed: %v", err)
			return
		}
		results[1] = res.Content[0].(*mcp.TextContent).Text
	}()
	wg.Wait()
	if results[0] == "" || results[1] == "" {
		t.Fatalf("expected both shared-tool results, got %q and %q", results[0], results[1])
	}
	if results[0] != results[1] {
		t.Fatalf("expected group proxy to preserve shared_read_only_tools behavior, got %q and %q", results[0], results[1])
	}

	directClient := mcp.NewClient(&mcp.Implementation{Name: "direct-client", Version: "1.0.0"}, nil)
	directSess, err := directClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://127.0.0.1:6283/mcp"}, nil)
	if err != nil { t.Fatalf("direct Connect: %v", err) }
	defer directSess.Close()
	if _, err := directSess.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "hi"}}); err != nil {
		t.Fatalf("direct CallTool: %v", err)
	}

	resp, err := http.Get("http://127.0.0.1:6274/version")
	if err != nil { t.Fatalf("GET /version: %v", err) }
	defer resp.Body.Close()
	var versionResp struct{ API map[string]bool `json:"api"` }
	if err := json.NewDecoder(resp.Body).Decode(&versionResp); err != nil {
		t.Fatalf("decode /version: %v", err)
	}
	if !versionResp.API["v1_slots"] || !versionResp.API["v1_slot_groups"] {
		t.Fatalf("missing slot capabilities: %#v", versionResp.API)
	}
}
