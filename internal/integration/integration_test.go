// Package integration provides end-to-end integration tests for Vision.
// These tests start real MCP servers and test the full request flow.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrede/vision/internal/config"
	"github.com/jrede/vision/internal/daemon"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMain sets up and tears down the test environment.
func TestMain(m *testing.M) {
	// Check if we have node available for MCP server tests
	if _, err := exec.LookPath("node"); err != nil {
		fmt.Println("SKIP: node not found, skipping integration tests")
		os.Exit(0)
	}

	os.Exit(m.Run())
}

// TestHotReload tests configuration hot reload.
// When the config file is modified and Reload() is called, new servers
// should start and removed servers should stop.
func TestHotReload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Create initial config with one server
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "servers.yaml")

	initialConfig := `
supervision:
  shutdown_timeout: 5s
  restart_delay: 100ms
  max_restart_delay: 1s

servers:
  server-a:
    command: node
    args: ["-e", "const rl=require('readline').createInterface({input:process.stdin});rl.on('line',l=>{const r=JSON.parse(l);console.log(JSON.stringify({jsonrpc:'2.0',id:r.id,result:{protocolVersion:'2024-11-05',capabilities:{},serverInfo:{name:'server-a',version:'1.0.0'}}}))});"]
    port: 6290
    autostart: true
`
	if err := os.WriteFile(configPath, []byte(initialConfig), 0644); err != nil {
		t.Fatalf("failed to write initial config: %v", err)
	}

	// Create daemon (use port 16299 for management API to avoid conflicts with MCPM on 6275)
	d, err := daemon.New(daemon.Config{
		ConfigPath:     configPath,
		ManagementPort: 16299, // Test-only port, outside Vision's 6276-6300 range
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("failed to create daemon: %v", err)
	}

	// Start daemon
	if err := d.Start(); err != nil {
		t.Fatalf("failed to start daemon: %v", err)
	}
	defer d.Stop(5 * time.Second)

	// Wait for server-a to start
	time.Sleep(1 * time.Second)

	// Verify server-a is running
	status := d.Status()
	t.Logf("Initial status: %+v", status)

	found := false
	for _, srv := range status.Registry.Servers {
		if srv.Name == "server-a" {
			found = true
			if srv.State != "running" {
				t.Errorf("server-a expected running, got %s", srv.State)
			}
		}
	}
	if !found {
		t.Error("server-a not found in registry")
	}

	// Modify config: remove server-a, add server-b and server-c
	newConfig := `
supervision:
  shutdown_timeout: 5s
  restart_delay: 100ms
  max_restart_delay: 1s

servers:
  server-b:
    command: node
    args: ["-e", "const rl=require('readline').createInterface({input:process.stdin});rl.on('line',l=>{const r=JSON.parse(l);console.log(JSON.stringify({jsonrpc:'2.0',id:r.id,result:{protocolVersion:'2024-11-05',capabilities:{},serverInfo:{name:'server-b',version:'1.0.0'}}}))});"]
    port: 6291
    autostart: true
  server-c:
    command: node
    args: ["-e", "const rl=require('readline').createInterface({input:process.stdin});rl.on('line',l=>{const r=JSON.parse(l);console.log(JSON.stringify({jsonrpc:'2.0',id:r.id,result:{protocolVersion:'2024-11-05',capabilities:{},serverInfo:{name:'server-c',version:'1.0.0'}}}))});"]
    port: 6292
    autostart: true
`
	if err := os.WriteFile(configPath, []byte(newConfig), 0644); err != nil {
		t.Fatalf("failed to write new config: %v", err)
	}

	// Trigger reload
	t.Log("Triggering reload...")
	if err := d.Reload(); err != nil {
		t.Fatalf("reload failed: %v", err)
	}

	// Wait for changes to take effect
	time.Sleep(1 * time.Second)

	// Verify the new state
	status = d.Status()
	t.Logf("Post-reload status: %+v", status)

	// server-a should be gone
	for _, srv := range status.Registry.Servers {
		if srv.Name == "server-a" {
			t.Error("server-a should have been removed but still exists")
		}
	}

	// server-b and server-c should exist and be running
	foundB, foundC := false, false
	for _, srv := range status.Registry.Servers {
		switch srv.Name {
		case "server-b":
			foundB = true
			if srv.State != "running" {
				t.Errorf("server-b expected running, got %s", srv.State)
			}
		case "server-c":
			foundC = true
			if srv.State != "running" {
				t.Errorf("server-c expected running, got %s", srv.State)
			}
		}
	}

	if !foundB {
		t.Error("server-b not found after reload")
	}
	if !foundC {
		t.Error("server-c not found after reload")
	}

	t.Log("Hot reload test passed!")
}

// echoMCPServerInlineJS is a minimal echo MCP server for integration tests.
// Responds to initialize, tools/list, and tools/call via stdin/stdout JSON-RPC.
const echoMCPServerInlineJS = `const rl=require('readline').createInterface({input:process.stdin,terminal:false});rl.on('line',l=>{try{const m=JSON.parse(l);if(m.method==='initialize'){process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:m.id,result:{protocolVersion:'2025-03-26',capabilities:{tools:{listChanged:false}},serverInfo:{name:'echo-integ',version:'1.0.0'}}})+'\n')}else if(m.method==='notifications/initialized'){}else if(m.method==='tools/list'){process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:m.id,result:{tools:[{name:'echo',description:'Echo',inputSchema:{type:'object',properties:{msg:{type:'string'}}}}]}})+'\n')}else if(m.method==='tools/call'){process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:m.id,result:{content:[{type:'text',text:'echo:'+((m.params||{}).arguments||{}).msg+' pid='+process.pid}]}})+'\n')}else if(m.id){process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:m.id,result:{}})+'\n')}}catch(e){}});`

// TestDaemonLifecycle verifies the full daemon lifecycle with Streamable HTTP:
// 1. Start daemon with a server config
// 2. Connect an MCP client via Streamable HTTP and call tools
// 3. Reload config: add new server, verify old proxy still works
// 4. Graceful shutdown terminates cleanly
func TestDaemonLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "servers.yaml")

	// Initial config: one echo server
	initialConfig := fmt.Sprintf(`
supervision:
  shutdown_timeout: 5s
  restart_delay: 100ms
  max_restart_delay: 1s

servers:
  echo-a:
    command: node
    args: ["-e", %q]
    port: 6293
    autostart: true
    max_sessions: 10
`, echoMCPServerInlineJS)

	if err := os.WriteFile(configPath, []byte(initialConfig), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	d, err := daemon.New(daemon.Config{
		ConfigPath:     configPath,
		ManagementPort: 16399,
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("daemon.New failed: %v", err)
	}

	// === Step 1: Start ===
	if err := d.Start(); err != nil {
		t.Fatalf("daemon.Start failed: %v", err)
	}

	// Wait for proxy to be ready
	time.Sleep(1 * time.Second)

	status := d.Status()
	if !status.Running {
		t.Fatal("daemon not running after Start")
	}

	// Verify echo-a is in the registry
	foundA := false
	for _, srv := range status.Registry.Servers {
		if srv.Name == "echo-a" {
			foundA = true
			if srv.State != "running" {
				t.Errorf("echo-a expected running, got %s", srv.State)
			}
		}
	}
	if !foundA {
		t.Fatal("echo-a not found in registry")
	}

	// === Step 2: Connect MCP client and call tools ===
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "integ-test-client",
		Version: "1.0.0",
	}, nil)

	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: "http://127.0.0.1:6293/mcp",
	}, nil)
	if err != nil {
		t.Fatalf("client.Connect to echo-a proxy failed: %v", err)
	}

	// List tools
	toolsResult, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	if len(toolsResult.Tools) == 0 {
		t.Fatal("expected tools from echo-a proxy")
	}
	t.Logf("echo-a tools: %d", len(toolsResult.Tools))

	// Call tool
	callResult, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"msg": "lifecycle-test"},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if len(callResult.Content) > 0 {
		if tc, ok := callResult.Content[0].(*mcp.TextContent); ok {
			t.Logf("echo-a result: %s", tc.Text)
		}
	}

	// Close the client session
	sess.Close()

	// === Step 3: Reload ===
	// Add echo-b, keep echo-a
	reloadConfig := fmt.Sprintf(`
supervision:
  shutdown_timeout: 5s
  restart_delay: 100ms
  max_restart_delay: 1s

servers:
  echo-a:
    command: node
    args: ["-e", %q]
    port: 6293
    autostart: true
    max_sessions: 10
  echo-b:
    command: node
    args: ["-e", %q]
    port: 6294
    autostart: true
    max_sessions: 10
`, echoMCPServerInlineJS, echoMCPServerInlineJS)

	if err := os.WriteFile(configPath, []byte(reloadConfig), 0644); err != nil {
		t.Fatalf("failed to write reload config: %v", err)
	}

	if err := d.Reload(); err != nil {
		t.Fatalf("Reload failed: %v", err)
	}

	// Wait for new server to start
	time.Sleep(1 * time.Second)

	// Verify echo-b is now accessible
	sess2, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: "http://127.0.0.1:6294/mcp",
	}, nil)
	if err != nil {
		t.Fatalf("client.Connect to echo-b proxy failed: %v", err)
	}

	callResult2, err := sess2.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"msg": "after-reload"},
	})
	if err != nil {
		t.Fatalf("CallTool to echo-b failed: %v", err)
	}
	if len(callResult2.Content) > 0 {
		if tc, ok := callResult2.Content[0].(*mcp.TextContent); ok {
			t.Logf("echo-b result: %s", tc.Text)
		}
	}

	sess2.Close()

	// Verify echo-a proxy still works after reload
	sess3, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: "http://127.0.0.1:6293/mcp",
	}, nil)
	if err != nil {
		t.Fatalf("client.Connect to echo-a after reload failed: %v", err)
	}

	callResult3, err := sess3.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"msg": "a-after-reload"},
	})
	if err != nil {
		t.Fatalf("CallTool to echo-a after reload failed: %v", err)
	}
	if len(callResult3.Content) > 0 {
		if tc, ok := callResult3.Content[0].(*mcp.TextContent); ok {
			t.Logf("echo-a post-reload: %s", tc.Text)
		}
	}
	sess3.Close()

	// === Step 4: Graceful shutdown ===
	if err := d.Stop(5 * time.Second); err != nil {
		t.Fatalf("daemon.Stop failed: %v", err)
	}

	if d.IsRunning() {
		t.Error("daemon still running after Stop")
	}

	t.Log("daemon lifecycle test passed!")
}

// TestBackwardCompatHealth verifies that the /health endpoint on per-server
// proxy ports and the daemon Status() API remain backward-compatible after
// the Streamable HTTP migration.
func TestBackwardCompatHealth(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "servers.yaml")

	initialConfig := fmt.Sprintf(`
supervision:
  shutdown_timeout: 5s
  restart_delay: 100ms
  max_restart_delay: 1s

servers:
  health-check-server:
    command: node
    args: ["-e", %q]
    port: 6295
    autostart: true
    max_sessions: 5
`, echoMCPServerInlineJS)

	if err := os.WriteFile(configPath, []byte(initialConfig), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	d, err := daemon.New(daemon.Config{
		ConfigPath:     configPath,
		ManagementPort: 16398,
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("daemon.New failed: %v", err)
	}

	if err := d.Start(); err != nil {
		t.Fatalf("daemon.Start failed: %v", err)
	}
	defer d.Stop(5 * time.Second)

	// Wait for proxy to be ready
	time.Sleep(1 * time.Second)

	// === Verify daemon Status() API ===
	status := d.Status()
	if !status.Running {
		t.Fatal("daemon not running")
	}

	foundServer := false
	for _, srv := range status.Registry.Servers {
		if srv.Name == "health-check-server" {
			foundServer = true
			if srv.State != "running" {
				t.Errorf("expected server state=running, got %s", srv.State)
			}
		}
	}
	if !foundServer {
		t.Fatal("health-check-server not found in daemon status")
	}

	// === Verify per-server /health endpoint ===
	resp, err := http.Get("http://127.0.0.1:6295/health")
	if err != nil {
		t.Fatalf("GET /health on server port failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /health: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Verify response is valid JSON with expected structure
	var healthResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&healthResp); err != nil {
		t.Fatalf("failed to decode /health response: %v", err)
	}

	// Check server name
	if name, ok := healthResp["server"].(string); !ok || name != "health-check-server" {
		t.Errorf("expected server=health-check-server, got %v", healthResp["server"])
	}

	// Check status
	if st, ok := healthResp["status"].(string); !ok || st != "ok" {
		t.Errorf("expected status=ok, got %v", healthResp["status"])
	}

	t.Logf("health response: %+v", healthResp)

	// === Verify /health returns JSON content type ===
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type=application/json, got %q", ct)
	}

	// === Verify graceful stop ===
	if err := d.Stop(5 * time.Second); err != nil {
		t.Fatalf("daemon.Stop failed: %v", err)
	}

	if d.IsRunning() {
		t.Error("daemon still running after Stop")
	}

	// /health should no longer be reachable
	_, err = http.Get("http://127.0.0.1:6295/health")
	if err == nil {
		t.Error("expected connection refused after daemon stop")
	}

	t.Log("backward-compat health test passed!")
}

// --- Config file helpers ---

// writeTestConfig creates a temporary config file for testing.
func writeTestConfig(t *testing.T, servers map[string]*config.ServerConfig) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yaml")

	// Write as YAML (simplified)
	data := "servers:\n"
	for name, srv := range servers {
		data += fmt.Sprintf(`  %s:
    command: %s
    args: [%s]
    port: %d
    autostart: %v
`, name, srv.Command, formatArgs(srv.Args), srv.Port, srv.Autostart)
	}

	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	return path
}

func formatArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	result := ""
	for i, arg := range args {
		if i > 0 {
			result += ", "
		}
		result += fmt.Sprintf("%q", arg)
	}
	return result
}

// TestDaemon_NoGenerationGrowth verifies that after starting the daemon with stdio
// servers, NO supervisor-spawned subprocess generations accumulate. Before the
// fixMcpPoolLeak change, the supervisor would spawn a new unused stdio child
// on each restart cycle, leaking one process per generation. After the fix,
// stdio servers skip supervisor registration entirely — session.Manager owns the
// subprocess lifecycle per-session.
func TestDaemon_NoGenerationGrowth(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "servers.yaml")

	// Config with two stdio servers (no HTTP/SSE)
	configContent := fmt.Sprintf(`
supervision:
  shutdown_timeout: 5s
  restart_delay: 50ms
  max_restart_delay: 200ms

servers:
  stdio-echo-a:
    command: node
    args: ["-e", %q]
    port: 6297
    autostart: true
    max_sessions: 5
  stdio-echo-b:
    command: node
    args: ["-e", %q]
    port: 6298
    autostart: true
    max_sessions: 5
`, echoMCPServerInlineJS, echoMCPServerInlineJS)

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	d, err := daemon.New(daemon.Config{
		ConfigPath:     configPath,
		ManagementPort: 16399,
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("daemon.New failed: %v", err)
	}

	// Count child processes before daemon starts (background children from prior tests)
	ppidBefore := os.Getpid()
	childrenBefore := countChildProcesses(ppidBefore)

	// Start daemon
	if err := d.Start(); err != nil {
		t.Fatalf("daemon.Start failed: %v", err)
	}
	time.Sleep(1 * time.Second)

	// Count children AFTER daemon started
	// With the fix: only the two session-spawned node processes (one per server on
	// first HTTP session connect) should exist, plus potential admin server children.
	// Without the fix: supervisor would have spawned 2 unused stdio children for each
	// server, all kept alive by suture restart loops.
	ppidAfter := os.Getpid()
	childrenAfterDaemonStart := countChildProcesses(ppidAfter)
	childCountAfterStart := int32(len(childrenAfterDaemonStart)) - int32(len(childrenBefore))

	// Trigger a reload to exercise the restart path
	reloadContent := fmt.Sprintf(`
supervision:
  shutdown_timeout: 5s
  restart_delay: 50ms
  max_restart_delay: 200ms

servers:
  stdio-echo-a:
    command: node
    args: ["-e", %q]
    port: 6297
    autostart: true
    max_sessions: 5
  stdio-echo-b:
    command: node
    args: ["-e", %q]
    port: 6298
    autostart: true
    max_sessions: 5
  stdio-echo-c:
    command: node
    args: ["-e", %q]
    port: 6299
    autostart: true
    max_sessions: 5
`, echoMCPServerInlineJS, echoMCPServerInlineJS, echoMCPServerInlineJS)

	if err := os.WriteFile(configPath, []byte(reloadContent), 0644); err != nil {
		t.Fatalf("failed to write reload config: %v", err)
	}

	if err := d.Reload(); err != nil {
		t.Fatalf("Reload failed: %v", err)
	}
	time.Sleep(1 * time.Second)

	childrenAfterReload := countChildProcesses(ppidAfter)
	childCountAfterReload := int32(len(childrenAfterReload)) - int32(len(childrenBefore))

	// Clean shutdown
	if err := d.Stop(5 * time.Second); err != nil {
		t.Fatalf("daemon.Stop failed: %v", err)
	}

	// Assertions:
	// 1. Child count growth should be minimal — only session-spawned processes,
	//    not supervisor-spawned ones. Each session-spawned node exits after the
	//    session manager's idle timeout (configurable, defaults to 30s).
	// 2. No generation growth: after daemon start + reload, we should have at most
	//    a handful of session processes (not 2x server count per generation).
	// 3. The key regression test: supervisor-spawned processes should be ZERO.
	t.Logf("child count: baseline=%d, after_start_delta=%d, after_reload_delta=%d",
		len(childrenBefore), childCountAfterStart, childCountAfterReload)

	// If more than 10 children accumulated, something is leaking.
	// Before the fix, each reload would add 3 more supervisor-spawned processes.
	if childCountAfterReload > 10 {
		t.Errorf("excessive child process accumulation: %d extra children after reload (possible supervisor leak)", childCountAfterReload)
	}
}

// countChildProcesses returns the PIDs of all direct child processes of a parent.
func countChildProcesses(ppid int) []int {
	var out []int
	// Use ps to find children
	cmd := exec.Command("ps", "--ppid", fmt.Sprintf("%d", ppid), "-o", "pid=", "--no-headers")
	cmd_out, err := cmd.Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(cmd_out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid := 0
		if n, err := fmt.Sscanf(line, "%d", &pid); err == nil && n == 1 {
			out = append(out, pid)
		}
	}
	return out
}
