package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jrede/vision/internal/config"
	"github.com/jrede/vision/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// echoMCPServerJS is a minimal Node.js MCP server for testing.
// Responds to initialize, tools/list, and tools/call.
const echoMCPServerJS = `
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin, terminal: false });
rl.on('line', (line) => {
  try {
    const msg = JSON.parse(line);
    if (msg.method === 'initialize') {
      process.stdout.write(JSON.stringify({
        jsonrpc: '2.0', id: msg.id,
        result: {
          protocolVersion: '2025-03-26',
          capabilities: { tools: { listChanged: false } },
          serverInfo: { name: 'echo-test', version: '1.0.0' }
        }
      }) + '\n');
    } else if (msg.method === 'notifications/initialized') {
      // no response
    } else if (msg.method === 'tools/list') {
      process.stdout.write(JSON.stringify({
        jsonrpc: '2.0', id: msg.id,
        result: {
          tools: [{
            name: 'echo',
            description: 'Echoes input back',
            inputSchema: { type: 'object', properties: { message: { type: 'string' } } }
          }]
        }
      }) + '\n');
    } else if (msg.method === 'tools/call') {
      process.stdout.write(JSON.stringify({
        jsonrpc: '2.0', id: msg.id,
        result: {
          content: [{ type: 'text', text: 'echo: ' + (msg.params?.arguments?.message || '') + ' pid=' + process.pid }]
        }
      }) + '\n');
    } else if (msg.id) {
      process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: msg.id, result: {} }) + '\n');
    }
  } catch (e) {
    process.stderr.write('Error: ' + e.message + '\n');
  }
});
`

const slowEchoMCPServerJS = `
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin, terminal: false });
rl.on('line', (line) => {
  try {
    const msg = JSON.parse(line);
    if (msg.method === 'initialize') {
      process.stdout.write(JSON.stringify({
        jsonrpc: '2.0', id: msg.id,
        result: {
          protocolVersion: '2025-03-26',
          capabilities: { tools: { listChanged: false } },
          serverInfo: { name: 'slow-echo-test', version: '1.0.0' }
        }
      }) + '\n');
    } else if (msg.method === 'notifications/initialized') {
      // no response
    } else if (msg.method === 'tools/list') {
      process.stdout.write(JSON.stringify({
        jsonrpc: '2.0', id: msg.id,
        result: {
          tools: [{
            name: 'echo',
            description: 'Echoes input back slowly',
            inputSchema: { type: 'object', properties: { message: { type: 'string' } } }
          }]
        }
      }) + '\n');
    } else if (msg.method === 'tools/call') {
      setTimeout(() => {
        process.stdout.write(JSON.stringify({
          jsonrpc: '2.0', id: msg.id,
          result: {
            content: [{ type: 'text', text: 'echo: ' + (msg.params?.arguments?.message || '') + ' pid=' + process.pid }]
          }
        }) + '\n');
      }, 200);
    } else if (msg.id) {
      process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: msg.id, result: {} }) + '\n');
    }
  } catch (e) {
    process.stderr.write('Error: ' + e.message + '\n');
  }
});
`

func skipIfNoNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func testServerConfig() *config.ServerConfig {
	return testServerConfigWithScript(echoMCPServerJS)
}

func testServerConfigWithScript(script string) *config.ServerConfig {
	return &config.ServerConfig{
		Command:        "node",
		Args:           []string{"-e", script},
		Port:           16290,
		Autostart:      true,
		RestartPolicy:  config.RestartOnFailure,
		SessionTimeout: config.Duration(30 * time.Second),
		MaxSessions:    10,
	}
}

func extractTextContent(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected non-empty tool result content")
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	return tc.Text
}

func TestProxyHandler_CoalescesConcurrentReadOnlyCalls(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testServerConfigWithScript(slowEchoMCPServerJS)
	mgr := session.NewManager("test-proxy-coalesce", cfg, logger)
	defer mgr.CloseAll()

	handler := NewProxyHandler(ProxyConfig{
		ServerName:            "test-proxy-coalesce",
		SessionManager:        mgr,
		Logger:                logger,
		SharedReadOnlyTools:   []string{"echo"},
		SharedResultCacheTTL:  time.Second,
		SharedResultCacheSize: 8,
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	clientA := mcp.NewClient(&mcp.Implementation{Name: "client-a", Version: "1.0.0"}, nil)
	clientB := mcp.NewClient(&mcp.Implementation{Name: "client-b", Version: "1.0.0"}, nil)
	sessA, err := clientA.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("clientA.Connect failed: %v", err)
	}
	defer sessA.Close()
	sessB, err := clientB.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("clientB.Connect failed: %v", err)
	}
	defer sessB.Close()

	results := make([]string, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		callResult, err := sessA.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "shared"}})
		if err != nil {
			t.Errorf("sessA.CallTool failed: %v", err)
			return
		}
		results[0] = extractTextContent(t, callResult)
	}()
	go func() {
		defer wg.Done()
		callResult, err := sessB.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "shared"}})
		if err != nil {
			t.Errorf("sessB.CallTool failed: %v", err)
			return
		}
		results[1] = extractTextContent(t, callResult)
	}()
	wg.Wait()

	if results[0] == "" || results[1] == "" {
		t.Fatalf("expected both results non-empty, got %q and %q", results[0], results[1])
	}
	if results[0] != results[1] {
		t.Fatalf("expected coalesced results to match, got %q and %q", results[0], results[1])
	}
}

func TestProxyHandler_CachesReadOnlyCallsAcrossSessions(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testServerConfig()
	mgr := session.NewManager("test-proxy-cache", cfg, logger)
	defer mgr.CloseAll()

	handler := NewProxyHandler(ProxyConfig{
		ServerName:            "test-proxy-cache",
		SessionManager:        mgr,
		Logger:                logger,
		SharedReadOnlyTools:   []string{"echo"},
		SharedResultCacheTTL:  2 * time.Second,
		SharedResultCacheSize: 8,
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	clientA := mcp.NewClient(&mcp.Implementation{Name: "client-a", Version: "1.0.0"}, nil)
	clientB := mcp.NewClient(&mcp.Implementation{Name: "client-b", Version: "1.0.0"}, nil)
	sessA, err := clientA.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("clientA.Connect failed: %v", err)
	}
	defer sessA.Close()
	sessB, err := clientB.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("clientB.Connect failed: %v", err)
	}
	defer sessB.Close()

	first, err := sessA.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "cache-me"}})
	if err != nil {
		t.Fatalf("sessA.CallTool failed: %v", err)
	}
	second, err := sessB.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "cache-me"}})
	if err != nil {
		t.Fatalf("sessB.CallTool failed: %v", err)
	}

	textA := extractTextContent(t, first)
	textB := extractTextContent(t, second)
	if textA != textB {
		t.Fatalf("expected cached result reuse across sessions, got %q and %q", textA, textB)
	}
}

func TestProxyHandler_EnforcesInFlightConcurrencyLimit(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testServerConfigWithScript(slowEchoMCPServerJS)
	mgr := session.NewManager("test-proxy-limit", cfg, logger)
	defer mgr.CloseAll()

	handler := NewProxyHandler(ProxyConfig{
		ServerName:          "test-proxy-limit",
		SessionManager:      mgr,
		Logger:              logger,
		MaxInFlightRequests: 1,
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	clientA := mcp.NewClient(&mcp.Implementation{Name: "client-a", Version: "1.0.0"}, nil)
	clientB := mcp.NewClient(&mcp.Implementation{Name: "client-b", Version: "1.0.0"}, nil)
	sessA, err := clientA.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("clientA.Connect failed: %v", err)
	}
	defer sessA.Close()
	sessB, err := clientB.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("clientB.Connect failed: %v", err)
	}
	defer sessB.Close()

	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := sessA.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "first"}})
		if err != nil {
			t.Errorf("sessA.CallTool failed: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		_, err := sessB.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "second"}})
		if err != nil {
			t.Errorf("sessB.CallTool failed: %v", err)
		}
	}()
	wg.Wait()

	if elapsed := time.Since(start); elapsed < 350*time.Millisecond {
		t.Fatalf("expected serialized execution due to in-flight limit, elapsed = %v", elapsed)
	}
}

// TestProxyHandler_EndToEnd verifies that the proxy handler:
// 1. Accepts an MCP client connection via StreamableHTTP
// 2. Spawns a downstream subprocess per session
// 3. Discovers and proxies tools from the downstream
// 4. Forwards tool calls and returns results
func TestProxyHandler_EndToEnd(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testServerConfig()

	// Create session manager
	mgr := session.NewManager("test-proxy", cfg, logger)
	defer mgr.CloseAll()

	// Create proxy handler
	handler := NewProxyHandler(ProxyConfig{
		ServerName:     "test-proxy",
		SessionManager: mgr,
		Logger:         logger,
	})

	// Start httptest server
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Connect as an MCP client via Streamable HTTP
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}, nil)

	clientSession, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("client.Connect failed: %v", err)
	}
	defer clientSession.Close()

	// Verify tools are discovered through the proxy
	toolsResult, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	if len(toolsResult.Tools) == 0 {
		t.Fatal("expected at least one tool from proxy, got none")
	}

	foundEcho := false
	for _, tool := range toolsResult.Tools {
		if tool.Name == "echo" {
			foundEcho = true
			break
		}
	}
	if !foundEcho {
		t.Errorf("expected 'echo' tool in proxy tools, got: %v", toolsResult.Tools)
	}

	// Verify tool call is proxied to downstream
	callResult, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "hello-proxy"},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}

	if len(callResult.Content) == 0 {
		t.Fatal("expected content in CallTool result, got none")
	}

	tc, ok := callResult.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", callResult.Content[0])
	}

	if tc.Text == "" {
		t.Error("expected non-empty text in CallTool result")
	}

	t.Logf("proxy result: %s", tc.Text)

	// Verify it contains our message and a PID (proving subprocess was spawned)
	if !strings.Contains(tc.Text, "hello-proxy") {
		t.Errorf("expected result to contain 'hello-proxy', got %q", tc.Text)
	}
	if !strings.Contains(tc.Text, "pid=") {
		t.Errorf("expected result to contain 'pid=', got %q", tc.Text)
	}
}

// TestProxyHandler_TwoSessionsGetIsolatedSubprocesses verifies that two
// concurrent client sessions each get their own downstream subprocess.
func TestProxyHandler_TwoSessionsGetIsolatedSubprocesses(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testServerConfig()

	mgr := session.NewManager("test-proxy-iso", cfg, logger)
	defer mgr.CloseAll()

	handler := NewProxyHandler(ProxyConfig{
		ServerName:     "test-proxy-iso",
		SessionManager: mgr,
		Logger:         logger,
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Connect two clients
	results := make([]string, 2)
	for i := 0; i < 2; i++ {
		client := mcp.NewClient(&mcp.Implementation{
			Name:    "test-client",
			Version: "1.0.0",
		}, nil)

		sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
		if err != nil {
			t.Fatalf("client[%d].Connect failed: %v", i, err)
		}
		defer sess.Close()

		callResult, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name:      "echo",
			Arguments: map[string]any{"message": "test"},
		})
		if err != nil {
			t.Fatalf("client[%d].CallTool failed: %v", i, err)
		}

		if len(callResult.Content) > 0 {
			if tc, ok := callResult.Content[0].(*mcp.TextContent); ok {
				results[i] = tc.Text
			}
		}
	}

	// Both should have results
	if results[0] == "" || results[1] == "" {
		t.Fatalf("expected both results non-empty, got %q and %q", results[0], results[1])
	}

	// Results should differ (different PIDs = different subprocesses)
	if results[0] == results[1] {
		t.Errorf("expected different subprocesses (different PIDs), but got identical results: %q", results[0])
	}

	t.Logf("session 0: %s", results[0])
	t.Logf("session 1: %s", results[1])
}

// TestProxyHandler_SessionTeardownIsolation verifies that closing one client
// session does not affect another session's downstream subprocess.
// Specifically: connect two clients, close client A, verify client B still works.
func TestProxyHandler_SessionTeardownIsolation(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testServerConfig()

	mgr := session.NewManager("test-teardown", cfg, logger)
	defer mgr.CloseAll()

	handler := NewProxyHandler(ProxyConfig{
		ServerName:     "test-teardown",
		SessionManager: mgr,
		Logger:         logger,
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Connect client A
	clientA := mcp.NewClient(&mcp.Implementation{
		Name:    "client-A",
		Version: "1.0.0",
	}, nil)
	sessA, err := clientA.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("client A Connect failed: %v", err)
	}

	// Connect client B
	clientB := mcp.NewClient(&mcp.Implementation{
		Name:    "client-B",
		Version: "1.0.0",
	}, nil)
	sessB, err := clientB.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("client B Connect failed: %v", err)
	}

	// Verify both sessions have 2 sessions tracked
	if count := mgr.SessionCount(); count != 2 {
		t.Fatalf("expected 2 sessions, got %d", count)
	}

	// Verify both clients can call tools
	resultA, err := sessA.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "from-A"},
	})
	if err != nil {
		t.Fatalf("client A CallTool failed: %v", err)
	}
	textA := ""
	if len(resultA.Content) > 0 {
		if tc, ok := resultA.Content[0].(*mcp.TextContent); ok {
			textA = tc.Text
		}
	}

	resultB, err := sessB.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "from-B"},
	})
	if err != nil {
		t.Fatalf("client B CallTool failed: %v", err)
	}
	textB := ""
	if len(resultB.Content) > 0 {
		if tc, ok := resultB.Content[0].(*mcp.TextContent); ok {
			textB = tc.Text
		}
	}

	t.Logf("before teardown — A: %s, B: %s", textA, textB)

	// Close client A
	if err := sessA.Close(); err != nil {
		t.Logf("sessA.Close error (expected in some cases): %v", err)
	}

	// Wait for deterministic teardown: only client B should remain.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.SessionCount() == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if count := mgr.SessionCount(); count != 1 {
		t.Fatalf("expected 1 session after closing A, got %d", count)
	}

	// Client B should still be fully functional
	resultB2, err := sessB.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "still-alive"},
	})
	if err != nil {
		t.Fatalf("client B CallTool after A teardown failed: %v", err)
	}

	if len(resultB2.Content) == 0 {
		t.Fatal("expected non-empty result from client B after A teardown")
	}

	tc, ok := resultB2.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", resultB2.Content[0])
	}

	if !strings.Contains(tc.Text, "still-alive") {
		t.Errorf("expected result to contain 'still-alive', got %q", tc.Text)
	}

	t.Logf("after A teardown — B result: %s", tc.Text)

	// Clean up client B
	sessB.Close()
}

// TestProxyHandler_PortManagerAddStreamable verifies AddStreamable via proxy handler.
func TestProxyHandler_PortManagerAddStreamable(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pm := NewPortManager(logger)
	defer pm.Close()

	// Use a simple handler (no real MCP - just testing the port manager wiring)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	if err := pm.AddStreamable("test-streamable", 16291, handler, nil); err != nil {
		t.Fatalf("AddStreamable failed: %v", err)
	}

	listener := pm.Get("test-streamable")
	if listener == nil {
		t.Fatal("expected listener to be found")
	}
	if listener.Name != "test-streamable" {
		t.Errorf("expected name 'test-streamable', got %q", listener.Name)
	}
	if listener.Port != 16291 {
		t.Errorf("expected port 16291, got %d", listener.Port)
	}
	if listener.MCPHandler == nil {
		t.Error("expected MCPHandler to be set")
	}
}

// notifyingMCPServerJS is a Node.js MCP server that sends notifications.
// When "trigger_notify" tool is called, it sends a tools/list_changed notification
// and adds a new tool to its list. When "trigger_remove" is called, it removes
// the dynamic tool and sends tools/list_changed. When "send_log" is called, it sends a
// logging/message notification.
const notifyingMCPServerJS = `
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin, terminal: false });
let extraTool = false;
rl.on('line', (line) => {
  try {
    const msg = JSON.parse(line);
    if (msg.method === 'initialize') {
      process.stdout.write(JSON.stringify({
        jsonrpc: '2.0', id: msg.id,
        result: {
          protocolVersion: '2025-03-26',
          capabilities: { tools: { listChanged: true }, logging: {} },
          serverInfo: { name: 'notifying-test', version: '1.0.0' }
        }
      }) + '\n');
    } else if (msg.method === 'notifications/initialized') {
      // no response
    } else if (msg.method === 'tools/list') {
      const tools = [{
        name: 'trigger_notify',
        description: 'Triggers a tools/list_changed notification',
        inputSchema: { type: 'object', properties: {} }
      }, {
        name: 'trigger_remove',
        description: 'Removes a dynamically added tool and notifies',
        inputSchema: { type: 'object', properties: {} }
      }, {
        name: 'send_log',
        description: 'Sends a logging notification',
        inputSchema: { type: 'object', properties: { message: { type: 'string' } } }
      }];
      if (extraTool) {
        tools.push({
          name: 'new_tool',
          description: 'A dynamically added tool',
          inputSchema: { type: 'object', properties: { value: { type: 'string' } } }
        });
      }
      process.stdout.write(JSON.stringify({
        jsonrpc: '2.0', id: msg.id,
        result: { tools: tools }
      }) + '\n');
    } else if (msg.method === 'tools/call') {
      if (msg.params?.name === 'trigger_notify') {
        extraTool = true;
        // Send tools/list_changed notification (no id = notification)
        process.stdout.write(JSON.stringify({
          jsonrpc: '2.0',
          method: 'notifications/tools/list_changed',
          params: {}
        }) + '\n');
        // Respond to the tool call
        process.stdout.write(JSON.stringify({
          jsonrpc: '2.0', id: msg.id,
          result: { content: [{ type: 'text', text: 'notification sent' }] }
        }) + '\n');
      } else if (msg.params?.name === 'trigger_remove') {
        extraTool = false;
        process.stdout.write(JSON.stringify({
          jsonrpc: '2.0',
          method: 'notifications/tools/list_changed',
          params: {}
        }) + '\n');
        process.stdout.write(JSON.stringify({
          jsonrpc: '2.0', id: msg.id,
          result: { content: [{ type: 'text', text: 'removal notification sent' }] }
        }) + '\n');
      } else if (msg.params?.name === 'send_log') {
        // Send logging notification
        process.stdout.write(JSON.stringify({
          jsonrpc: '2.0',
          method: 'notifications/message',
          params: { level: 'info', data: msg.params?.arguments?.message || 'test log', logger: 'test' }
        }) + '\n');
        // Respond to the tool call
        process.stdout.write(JSON.stringify({
          jsonrpc: '2.0', id: msg.id,
          result: { content: [{ type: 'text', text: 'log sent' }] }
        }) + '\n');
      } else {
        process.stdout.write(JSON.stringify({
          jsonrpc: '2.0', id: msg.id,
          result: { content: [{ type: 'text', text: 'unknown tool' }] }
        }) + '\n');
      }
    } else if (msg.id) {
      process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: msg.id, result: {} }) + '\n');
    }
  } catch (e) {
    process.stderr.write('Error: ' + e.message + '\n');
  }
});
`

// TestProxyHandler_NotificationRelay verifies that notifications from the
// downstream server are relayed to the upstream client.
// Specifically tests tools/list_changed relay: when downstream adds a tool
// and sends the notification, the proxy re-discovers tools and the upstream
// client sees the new tool.
func TestProxyHandler_NotificationRelay(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := &config.ServerConfig{
		Command:        "node",
		Args:           []string{"-e", notifyingMCPServerJS},
		Port:           16295,
		Autostart:      true,
		RestartPolicy:  config.RestartOnFailure,
		SessionTimeout: config.Duration(30 * time.Second),
		MaxSessions:    10,
	}

	mgr := session.NewManager("test-notify", cfg, logger)
	defer mgr.CloseAll()

	handler := NewProxyHandler(ProxyConfig{
		ServerName:     "test-notify",
		SessionManager: mgr,
		Logger:         logger,
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Track tools/list_changed notifications received by the upstream client.
	toolsChanged := make(chan struct{}, 10)
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "test-client-notify",
		Version: "1.0.0",
	}, &mcp.ClientOptions{
		ToolListChangedHandler: func(_ context.Context, _ *mcp.ToolListChangedRequest) {
			toolsChanged <- struct{}{}
		},
	})

	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer sess.Close()

	// Verify initial tool list has 3 tools.
	initialTools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("initial ListTools failed: %v", err)
	}
	if len(initialTools.Tools) != 3 {
		t.Fatalf("expected 3 initial tools, got %d", len(initialTools.Tools))
	}

	// Trigger the downstream to send tools/list_changed and add a new tool.
	callResult, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "trigger_notify",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool trigger_notify failed: %v", err)
	}
	if len(callResult.Content) > 0 {
		if tc, ok := callResult.Content[0].(*mcp.TextContent); ok {
			t.Logf("trigger result: %s", tc.Text)
		}
	}

	// Wait for the tools/list_changed notification to be relayed to us.
	select {
	case <-toolsChanged:
		t.Log("received tools/list_changed notification")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for tools/list_changed notification")
	}

	// After re-discovery, the proxy should now expose 4 tools.
	// Give a brief moment for the proxy to re-register tools.
	time.Sleep(100 * time.Millisecond)

	updatedTools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("updated ListTools failed: %v", err)
	}
	if len(updatedTools.Tools) != 4 {
		names := make([]string, len(updatedTools.Tools))
		for i, t := range updatedTools.Tools {
			names[i] = t.Name
		}
		t.Fatalf("expected 4 tools after notification, got %d: %v", len(updatedTools.Tools), names)
	}

	// Verify the new tool is present.
	foundNew := false
	for _, tool := range updatedTools.Tools {
		if tool.Name == "new_tool" {
			foundNew = true
			break
		}
	}
	if !foundNew {
		t.Error("expected 'new_tool' in updated tool list")
	}

	// Trigger downstream to remove the dynamic tool and notify again.
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "trigger_remove",
		Arguments: map[string]any{},
	}); err != nil {
		t.Fatalf("CallTool trigger_remove failed: %v", err)
	}

	select {
	case <-toolsChanged:
		t.Log("received tools/list_changed notification for removal")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for tools/list_changed removal notification")
	}

	time.Sleep(100 * time.Millisecond)

	postRemovalTools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("post-removal ListTools failed: %v", err)
	}
	if len(postRemovalTools.Tools) != 3 {
		names := make([]string, len(postRemovalTools.Tools))
		for i, t := range postRemovalTools.Tools {
			names[i] = t.Name
		}
		t.Fatalf("expected 3 tools after removal, got %d: %v", len(postRemovalTools.Tools), names)
	}

	for _, tool := range postRemovalTools.Tools {
		if tool.Name == "new_tool" {
			t.Fatal("expected 'new_tool' to be removed from upstream tool list")
		}
	}

	t.Logf("notification relay test passed: %d -> %d -> %d tools", len(initialTools.Tools), len(updatedTools.Tools), len(postRemovalTools.Tools))
}

// TestProxyHandler_ConcurrentInitCallDelete hammers the proxy with multiple
// goroutines each performing initialize → call → close in parallel.
// This test is specifically designed to surface data races under -race.
func TestProxyHandler_ConcurrentInitCallDelete(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := &config.ServerConfig{
		Command:        "node",
		Args:           []string{"-e", echoMCPServerJS},
		Port:           16296,
		Autostart:      true,
		RestartPolicy:  config.RestartOnFailure,
		SessionTimeout: config.Duration(30 * time.Second),
		MaxSessions:    20,
	}

	mgr := session.NewManager("test-race", cfg, logger)
	defer mgr.CloseAll()

	handler := NewProxyHandler(ProxyConfig{
		ServerName:     "test-race",
		SessionManager: mgr,
		Logger:         logger,
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	const numClients = 5
	errs := make(chan error, numClients)

	for i := 0; i < numClients; i++ {
		go func(id int) {
			client := mcp.NewClient(&mcp.Implementation{
				Name:    "race-client",
				Version: "1.0.0",
			}, nil)

			sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
			if err != nil {
				errs <- err
				return
			}

			// List tools
			if _, err := sess.ListTools(ctx, nil); err != nil {
				errs <- err
				sess.Close()
				return
			}

			// Call tool
			if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
				Name:      "echo",
				Arguments: map[string]any{"message": "race-test"},
			}); err != nil {
				errs <- err
				sess.Close()
				return
			}

			// Close session (triggers DELETE)
			sess.Close()
			errs <- nil
		}(i)
	}

	// Wait for all goroutines
	var failures int
	for i := 0; i < numClients; i++ {
		if err := <-errs; err != nil {
			t.Errorf("client goroutine failed: %v", err)
			failures++
		}
	}

	if failures > 0 {
		t.Fatalf("%d/%d clients failed", failures, numClients)
	}

	// After all clients close, sessions should drain deterministically.
	deadline := time.After(5 * time.Second)
	for {
		if mgr.SessionCount() == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("expected 0 sessions after all clients closed, got %d", mgr.SessionCount())
		case <-time.After(100 * time.Millisecond):
		}
	}

	t.Logf("all %d concurrent clients completed init/call/delete without races", numClients)
}

// TestProxySession_CallToolVsClose verifies that when the downstream session is
// closed while tool calls are in-flight, Vision normalizes the error to
// ErrDownstreamUnavailable rather than leaking raw SDK internals ("client is
// closing") back through the MCP response. This is a regression test for the
// bug where agents received opaque SDK errors instead of a stable Vision error.
func TestProxySession_CallToolVsClose(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := &config.ServerConfig{
		Command:        "node",
		Args:           []string{"-e", echoMCPServerJS},
		Port:           16297,
		Autostart:      true,
		RestartPolicy:  config.RestartOnFailure,
		SessionTimeout: config.Duration(30 * time.Second),
		MaxSessions:    5,
	}

	mgr := session.NewManager("test-race-close", cfg, logger)
	defer mgr.CloseAll()

	handler := NewProxyHandler(ProxyConfig{
		ServerName:     "test-race-close",
		SessionManager: mgr,
		Logger:         logger,
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Establish a single session.
	client := mcp.NewClient(&mcp.Implementation{Name: "race-close-client", Version: "1.0.0"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer sess.Close()

	if _, err := sess.ListTools(ctx, nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	// Verify a normal call works before the close.
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "pre-close"},
	}); err != nil {
		t.Fatalf("pre-close CallTool failed: %v", err)
	}

	// Force-close the downstream subprocess directly (simulates reaper/idle
	// timeout). This happens while the upstream HTTP session remains open.
	// The next tool call should get a clean Vision error, not raw SDK internals.
	sessionIDs := mgr.Sessions()
	if len(sessionIDs) == 0 {
		t.Fatal("expected at least one active session")
	}
	for _, id := range sessionIDs {
		if err := mgr.RemoveSession(id); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
			t.Fatalf("RemoveSession: %v", err)
		}
	}

	// Give closeDownstream time to propagate (it runs synchronously via DELETE
	// handler, but RemoveSession is immediate here).
	time.Sleep(50 * time.Millisecond)

	// Now hammer calls in parallel; all should get ErrDownstreamUnavailable
	// (or a transport error) — NOT raw "client is closing".
	const callers = 5
	var wg sync.WaitGroup
	var rawSdkErrors sync.Map

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3 {
				_, err := sess.CallTool(ctx, &mcp.CallToolParams{
					Name:      "echo",
					Arguments: map[string]any{"message": "post-close"},
				})
				if err != nil {
					// Errors from the upstream transport layer (connection closed,
					// context errors) are expected and OK.
					// What is NOT OK: Vision forwarding a raw "client is closing"
					// as the tool result error.
					msg := err.Error()
					if strings.Contains(msg, "client is closing") &&
						!strings.Contains(msg, "downstream session unavailable") {
						rawSdkErrors.Store(msg, true)
					}
				}
			}
		}()
	}
	wg.Wait()

	// No raw SDK close errors should have leaked as proxy errors.
	rawSdkErrors.Range(func(k, _ any) bool {
		t.Errorf("raw SDK error leaked through Vision proxy: %v", k)
		return true
	})
}

// TestProxyHandler_RespawnAfterReap verifies that when a downstream session is
// reaped (idle timeout, crash), the next tool call transparently respawns a new
// downstream subprocess instead of returning ErrDownstreamUnavailable.
// This is the fix for the "downstream session unavailable" error that agents
// (e.g., Context7 callers) were seeing intermittently.
func TestProxyHandler_RespawnAfterReap(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := &config.ServerConfig{
		Command:        "node",
		Args:           []string{"-e", echoMCPServerJS},
		Port:           16298,
		Autostart:      true,
		RestartPolicy:  config.RestartOnFailure,
		SessionTimeout: config.Duration(30 * time.Second),
		MaxSessions:    10,
	}

	mgr := session.NewManager("test-respawn", cfg, logger)
	defer mgr.CloseAll()

	handler := NewProxyHandler(ProxyConfig{
		ServerName:     "test-respawn",
		SessionManager: mgr,
		Logger:         logger,
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Connect as an MCP client.
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "respawn-client",
		Version: "1.0.0",
	}, nil)

	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer sess.Close()

	// Verify initial tool call works.
	result1, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "before-reap"},
	})
	if err != nil {
		t.Fatalf("pre-reap CallTool failed: %v", err)
	}
	text1 := ""
	if len(result1.Content) > 0 {
		if tc, ok := result1.Content[0].(*mcp.TextContent); ok {
			text1 = tc.Text
		}
	}
	if !strings.Contains(text1, "before-reap") {
		t.Fatalf("expected 'before-reap' in result, got %q", text1)
	}
	t.Logf("pre-reap result: %s", text1)

	// Simulate reaper: force-remove all downstream sessions.
	sessionIDs := mgr.Sessions()
	if len(sessionIDs) == 0 {
		t.Fatal("expected at least one active session before reap")
	}
	for _, id := range sessionIDs {
		if err := mgr.RemoveSession(id); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
			t.Fatalf("RemoveSession: %v", err)
		}
	}

	// Give closeDownstream time to propagate.
	time.Sleep(100 * time.Millisecond)

	// The next tool call should trigger a respawn and succeed.
	result2, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "after-respawn"},
	})
	if err != nil {
		t.Fatalf("post-reap CallTool failed (respawn should have succeeded): %v", err)
	}

	text2 := ""
	if len(result2.Content) > 0 {
		if tc, ok := result2.Content[0].(*mcp.TextContent); ok {
			text2 = tc.Text
		}
	}
	if !strings.Contains(text2, "after-respawn") {
		t.Fatalf("expected 'after-respawn' in result, got %q", text2)
	}

	// The PID should differ (new subprocess was spawned).
	if text1 == text2 {
		t.Errorf("expected different PIDs after respawn, but got identical results: %q", text1)
	}

	t.Logf("post-respawn result: %s (different PID confirms new subprocess)", text2)
}

// TestProxyHandler_ConcurrentRespawn verifies that when multiple tool calls
// hit a closed downstream simultaneously, only one respawn occurs and all
// callers get valid results.
func TestProxyHandler_ConcurrentRespawn(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := &config.ServerConfig{
		Command:        "node",
		Args:           []string{"-e", echoMCPServerJS},
		Port:           16299,
		Autostart:      true,
		RestartPolicy:  config.RestartOnFailure,
		SessionTimeout: config.Duration(30 * time.Second),
		MaxSessions:    10,
	}

	mgr := session.NewManager("test-concurrent-respawn", cfg, logger)
	defer mgr.CloseAll()

	handler := NewProxyHandler(ProxyConfig{
		ServerName:     "test-concurrent-respawn",
		SessionManager: mgr,
		Logger:         logger,
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "concurrent-respawn-client",
		Version: "1.0.0",
	}, nil)

	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer sess.Close()

	// Verify initial call works.
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "init"},
	}); err != nil {
		t.Fatalf("initial CallTool failed: %v", err)
	}

	// Kill downstream.
	for _, id := range mgr.Sessions() {
		_ = mgr.RemoveSession(id)
	}
	time.Sleep(100 * time.Millisecond)

	// Fire multiple concurrent tool calls — all should succeed via respawn.
	const callers = 5
	var wg sync.WaitGroup
	results := make([]string, callers)
	errs := make([]error, callers)

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r, err := sess.CallTool(ctx, &mcp.CallToolParams{
				Name:      "echo",
				Arguments: map[string]any{"message": fmt.Sprintf("concurrent-%d", idx)},
			})
			errs[idx] = err
			if err == nil && len(r.Content) > 0 {
				if tc, ok := r.Content[0].(*mcp.TextContent); ok {
					results[idx] = tc.Text
				}
			}
		}(i)
	}
	wg.Wait()

	// All callers should have succeeded.
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Errorf("caller %d failed: %v", i, errs[i])
		}
		if results[i] == "" {
			t.Errorf("caller %d got empty result", i)
		}
	}

	// All results should have the same PID (single respawn).
	pids := make(map[string]bool)
	for _, r := range results {
		if r != "" {
			// Extract pid= portion
			for _, part := range strings.Fields(r) {
				if strings.Contains(part, "pid=") {
					pids[part] = true
				}
			}
		}
	}
	if len(pids) > 1 {
		t.Errorf("expected single respawn (1 PID), got %d different PIDs: %v", len(pids), pids)
	}

	t.Logf("concurrent respawn test passed: %d callers, %d unique PIDs", callers, len(pids))
}

// TestProxyHandler_IndexCleanupAfterRespawn verifies that after a downstream
// respawn, the old session ID is removed from the session manager and the new
// session ID is present. This prevents admission counter leaks and ensures
// touch/close operate on the correct session.
//
// The critical assertion is that a second reap+respawn cycle works correctly,
// which proves the session index (idx.byDownstream) was updated after the first
// respawn. If the index still has the old key, the second reap's onSessionRemoved
// callback won't find the proxy session, breaking the cleanup chain.
func TestProxyHandler_IndexCleanupAfterRespawn(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := &config.ServerConfig{
		Command:        "node",
		Args:           []string{"-e", echoMCPServerJS},
		Port:           16300,
		Autostart:      true,
		RestartPolicy:  config.RestartOnFailure,
		SessionTimeout: config.Duration(30 * time.Second),
		MaxSessions:    2, // Low limit to catch admission leaks
	}

	mgr := session.NewManager("test-index-cleanup", cfg, logger)
	defer mgr.CloseAll()

	handler := NewProxyHandler(ProxyConfig{
		ServerName:     "test-index-cleanup",
		SessionManager: mgr,
		Logger:         logger,
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Connect as an MCP client.
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "index-cleanup-client",
		Version: "1.0.0",
	}, nil)

	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer sess.Close()

	// Verify initial tool call works.
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "initial"},
	}); err != nil {
		t.Fatalf("initial CallTool failed: %v", err)
	}

	// --- First reap+respawn cycle ---

	oldIDs1 := mgr.Sessions()
	if len(oldIDs1) != 1 {
		t.Fatalf("expected 1 session before first reap, got %d", len(oldIDs1))
	}

	for _, id := range oldIDs1 {
		if err := mgr.RemoveSession(id); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
			t.Fatalf("first RemoveSession: %v", err)
		}
	}
	time.Sleep(100 * time.Millisecond)

	// Tool call triggers first respawn.
	result1, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "after-respawn-1"},
	})
	if err != nil {
		t.Fatalf("first respawn CallTool failed: %v", err)
	}
	text1 := ""
	if len(result1.Content) > 0 {
		if tc, ok := result1.Content[0].(*mcp.TextContent); ok {
			text1 = tc.Text
		}
	}
	if !strings.Contains(text1, "after-respawn-1") {
		t.Fatalf("expected 'after-respawn-1', got %q", text1)
	}

	// Verify old session is gone and new session exists.
	for _, oldID := range oldIDs1 {
		if tracked := mgr.GetSession(oldID); tracked != nil {
			t.Errorf("old session %q still in manager after first respawn", oldID)
		}
	}
	if count := mgr.SessionCount(); count != 1 {
		t.Fatalf("expected 1 session after first respawn, got %d (admission leak)", count)
	}

	// --- Second reap+respawn cycle (proves index was updated) ---

	oldIDs2 := mgr.Sessions()
	if len(oldIDs2) != 1 {
		t.Fatalf("expected 1 session before second reap, got %d", len(oldIDs2))
	}

	// Verify the new session ID differs from the first.
	if oldIDs2[0] == oldIDs1[0] {
		t.Fatalf("session ID unchanged after first respawn: %q", oldIDs2[0])
	}

	for _, id := range oldIDs2 {
		if err := mgr.RemoveSession(id); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
			t.Fatalf("second RemoveSession: %v", err)
		}
	}
	time.Sleep(100 * time.Millisecond)

	// Tool call triggers second respawn. If the index wasn't updated after the
	// first respawn, the onSessionRemoved callback won't find the proxy session
	// for the second reap, and closeDownstream won't be called — causing the
	// respawn to fail or the session count to leak.
	result2, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "after-respawn-2"},
	})
	if err != nil {
		t.Fatalf("second respawn CallTool failed: %v", err)
	}
	text2 := ""
	if len(result2.Content) > 0 {
		if tc, ok := result2.Content[0].(*mcp.TextContent); ok {
			text2 = tc.Text
		}
	}
	if !strings.Contains(text2, "after-respawn-2") {
		t.Fatalf("expected 'after-respawn-2', got %q", text2)
	}

	// Final assertions: no admission leak, old sessions gone.
	for _, oldID := range oldIDs2 {
		if tracked := mgr.GetSession(oldID); tracked != nil {
			t.Errorf("old session %q still in manager after second respawn", oldID)
		}
	}
	if count := mgr.SessionCount(); count != 1 {
		t.Errorf("expected 1 session after second respawn, got %d (admission leak)", count)
	}

	t.Logf("index cleanup test passed: two reap+respawn cycles, session count=%d", mgr.SessionCount())
}

// TestProxyHandler_ProactiveHealthCheck verifies that the per-session health
// probe detects a dead downstream subprocess and triggers closeDownstream
// before any tool call hits the failure path. The next tool call then
// transparently respawns a new downstream.
func TestProxyHandler_ProactiveHealthCheck(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := &config.ServerConfig{
		Command:             "node",
		Args:                []string{"-e", echoMCPServerJS},
		Port:                16301,
		Autostart:           true,
		RestartPolicy:       config.RestartOnFailure,
		SessionTimeout:      config.Duration(30 * time.Second),
		MaxSessions:         10,
		HealthCheckInterval: config.Duration(100 * time.Millisecond), // Very fast for testing
	}

	mgr := session.NewManager("test-health-probe", cfg, logger)
	defer mgr.CloseAll()

	handler := NewProxyHandler(ProxyConfig{
		ServerName:          "test-health-probe",
		SessionManager:      mgr,
		Logger:              logger,
		HealthCheckInterval: 100 * time.Millisecond,
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Connect as an MCP client.
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "health-probe-client",
		Version: "1.0.0",
	}, nil)

	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer sess.Close()

	// Verify initial tool call works.
	result1, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "before-kill"},
	})
	if err != nil {
		t.Fatalf("pre-kill CallTool failed: %v", err)
	}
	text1 := ""
	if len(result1.Content) > 0 {
		if tc, ok := result1.Content[0].(*mcp.TextContent); ok {
			text1 = tc.Text
		}
	}
	t.Logf("pre-kill result: %s", text1)

	// Simulate an external downstream crash by closing the tracked downstream
	// client session directly while leaving the manager entry in place. This
	// forces the background probe to detect the dead connection; it does NOT go
	// through RemoveSession/onSessionRemoved.
	sessionIDs := mgr.Sessions()
	if len(sessionIDs) != 1 {
		t.Fatalf("expected exactly one active session, got %d", len(sessionIDs))
	}
	tracked := mgr.GetSession(sessionIDs[0])
	if tracked == nil || tracked.Downstream == nil {
		t.Fatal("expected tracked downstream session before simulated crash")
	}
	if err := tracked.Downstream.Close(); err != nil {
		t.Fatalf("failed to close downstream client session: %v", err)
	}

	// Wait for the health probe to observe 3 consecutive failures and call
	// closeDownstream. 100ms interval + threshold 3 + scheduling slack.
	time.Sleep(700 * time.Millisecond)

	// The manager entry should still exist until respawn cleanup runs; the key
	// signal is that the next tool call succeeds via respawn rather than leaking
	// ErrDownstreamUnavailable.

	// The next tool call should trigger respawn and succeed.
	result2, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "after-health-respawn"},
	})
	if err != nil {
		t.Fatalf("post-health-check CallTool failed (respawn should have succeeded): %v", err)
	}

	text2 := ""
	if len(result2.Content) > 0 {
		if tc, ok := result2.Content[0].(*mcp.TextContent); ok {
			text2 = tc.Text
		}
	}
	if !strings.Contains(text2, "after-health-respawn") {
		t.Fatalf("expected 'after-health-respawn' in result, got %q", text2)
	}

	// Verify session count is 1 (no leak).
	if count := mgr.SessionCount(); count != 1 {
		t.Errorf("expected 1 session after health respawn, got %d", count)
	}

	t.Logf("proactive health check test passed: probe-triggered respawn succeeded, count=%d", mgr.SessionCount())
}
