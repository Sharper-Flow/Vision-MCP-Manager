package mcp

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
	return &config.ServerConfig{
		Command:        "node",
		Args:           []string{"-e", echoMCPServerJS},
		Port:           16290,
		Autostart:      true,
		RestartPolicy:  config.RestartOnFailure,
		SessionTimeout: config.Duration(30 * time.Second),
		MaxSessions:    10,
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
	if !containsSubstring(tc.Text, "hello-proxy") {
		t.Errorf("expected result to contain 'hello-proxy', got %q", tc.Text)
	}
	if !containsSubstring(tc.Text, "pid=") {
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

	if !containsSubstring(tc.Text, "still-alive") {
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

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
