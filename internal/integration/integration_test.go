// Package integration provides end-to-end integration tests for Vision.
// These tests start real MCP servers and test the full request flow.
package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jrede/vision/internal/bridge"
	"github.com/jrede/vision/internal/config"
	"github.com/jrede/vision/internal/mcp"
	"github.com/jrede/vision/internal/supervisor"
)

// TestMain sets up and tears down the test environment.
func TestMain(m *testing.M) {
	// Check if we have npx available for MCP server tests
	if _, err := exec.LookPath("npx"); err != nil {
		fmt.Println("SKIP: npx not found, skipping integration tests")
		os.Exit(0)
	}

	os.Exit(m.Run())
}

// TestBridgeWithPipes tests the bridge with simple in-memory pipes.
// This validates that the bridge works correctly before testing full integration.
func TestBridgeWithPipes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Create pipes to simulate subprocess
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	// Create bridge
	br := bridge.NewStdioHTTPBridge(stdinW, stdoutR, &bridge.BridgeOptions{
		RequestTimeout: 5 * time.Second,
		Logger:         logger,
	})
	br.Start()

	// Cleanup function - must close pipes to unblock readers
	cleanup := func() {
		stdinW.Close()
		stdinR.Close()
		stdoutW.Close()
		stdoutR.Close()
		br.Close()
	}
	defer cleanup()

	// Mock MCP server in a goroutine
	go func() {
		scanner := bufio.NewScanner(stdinR)
		for scanner.Scan() {
			line := scanner.Text()
			t.Logf("Mock server received: %s", line)

			var req map[string]any
			if err := json.Unmarshal([]byte(line), &req); err != nil {
				t.Logf("Mock server parse error: %v", err)
				continue
			}

			method := req["method"].(string)
			id := req["id"]

			var resp map[string]any
			if method == "initialize" {
				resp = map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result": map[string]any{
						"protocolVersion": "2024-11-05",
						"capabilities":    map[string]any{},
						"serverInfo": map[string]any{
							"name":    "mock-server",
							"version": "1.0.0",
						},
					},
				}
			} else {
				resp = map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"error": map[string]any{
						"code":    -32601,
						"message": "Method not found",
					},
				}
			}

			respBytes, _ := json.Marshal(resp)
			fmt.Fprintf(stdoutW, "%s\n", respBytes)
			t.Logf("Mock server sent: %s", respBytes)
		}
	}()

	// Test sending a request through the bridge
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reqData := []byte(`{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}},"id":1}`)
	respData, err := br.ForwardRequest(ctx, reqData)
	if err != nil {
		t.Fatalf("ForwardRequest error: %v", err)
	}

	t.Logf("Response: %s", respData)

	var resp map[string]any
	if err := json.Unmarshal(respData, &resp); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if resp["error"] != nil {
		t.Errorf("Response has error: %v", resp["error"])
	}
	if resp["result"] == nil {
		t.Error("Response has no result")
	}
}

// TestFullRequestFlow tests the complete flow: HTTP → Bridge → Server → Response.
// This is the critical integration test for Phase 10.
func TestFullRequestFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Create a simple echo MCP server config
	// We'll use a custom test server that echoes back requests
	serverCfg := &config.ServerConfig{
		Command:   "node",
		Args:      []string{"-e", echoMCPServerJS},
		Port:      16276, // Use a high port to avoid conflicts
		Autostart: true,
	}

	// Create supervisor
	supCfg := config.SupervisionConfig{
		ShutdownTimeout: config.Duration(5 * time.Second),
	}
	sup := supervisor.New(supCfg, logger)

	// Start supervisor in background
	ctx, cancel := context.WithCancel(context.Background())
	sup.ServeBackground(ctx)

	// Add and start the server
	proc, err := sup.AddServer("test-echo", serverCfg)
	if err != nil {
		t.Fatalf("failed to add server: %v", err)
	}

	// Wait for process to start
	time.Sleep(500 * time.Millisecond)

	// Verify process is running
	if proc.State() != supervisor.StateRunning {
		t.Fatalf("server not running, state: %v", proc.State())
	}

	t.Logf("Server started with PID %d", proc.PID())

	// Create bridge to the subprocess
	br := bridge.NewStdioHTTPBridge(proc.Stdin(), proc.Stdout(), &bridge.BridgeOptions{
		RequestTimeout: 10 * time.Second,
		Logger:         logger,
	})
	br.Start()

	// Create port manager and add the server
	pm := mcp.NewPortManager(logger)
	if err := pm.Add("test-echo", serverCfg.Port, br); err != nil {
		t.Fatalf("failed to add to port manager: %v", err)
	}

	// Cleanup in correct order:
	// 1. Close port manager (stops HTTP listener)
	// 2. Cancel context (stops supervisor, which terminates subprocess)
	// 3. Wait for subprocess to exit (this closes the pipes)
	// 4. Close bridge (readLoop will exit because pipes are closed)
	defer func() {
		pm.Close()
		cancel()
		time.Sleep(500 * time.Millisecond) // Give subprocess time to terminate
		br.Close()
	}()

	// Wait for HTTP server to be ready
	time.Sleep(200 * time.Millisecond)

	// Test 1: Send initialize request
	t.Run("Initialize", func(t *testing.T) {
		t.Log("Sending initialize request...")
		req := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "initialize",
			"params": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"clientInfo": map[string]any{
					"name":    "vision-test",
					"version": "1.0.0",
				},
			},
		}

		resp := sendMCPRequest(t, serverCfg.Port, req)
		t.Logf("Got response: %v", resp)
		if resp["error"] != nil {
			t.Errorf("initialize returned error: %v", resp["error"])
		}
		if resp["result"] == nil {
			t.Error("initialize returned no result")
		}
	})

	// Test 2: Send tools/list request
	t.Run("ToolsList", func(t *testing.T) {
		t.Log("Sending tools/list request...")
		req := map[string]any{
			"jsonrpc": "2.0",
			"id":      2,
			"method":  "tools/list",
		}

		resp := sendMCPRequest(t, serverCfg.Port, req)
		t.Logf("Got tools/list response: %v", resp)
		if resp["error"] != nil {
			t.Errorf("tools/list returned error: %v", resp["error"])
		}
	})
}

// TestConcurrentClients tests multiple clients accessing the same server.
func TestConcurrentClients(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	serverCfg := &config.ServerConfig{
		Command:   "node",
		Args:      []string{"-e", echoMCPServerJS},
		Port:      16277,
		Autostart: true,
	}

	supCfg := config.SupervisionConfig{
		ShutdownTimeout: config.Duration(5 * time.Second),
	}
	sup := supervisor.New(supCfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	sup.ServeBackground(ctx)

	proc, err := sup.AddServer("test-concurrent", serverCfg)
	if err != nil {
		t.Fatalf("failed to add server: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	br := bridge.NewStdioHTTPBridge(proc.Stdin(), proc.Stdout(), &bridge.BridgeOptions{
		RequestTimeout: 10 * time.Second,
		Logger:         logger,
	})
	br.Start()

	pm := mcp.NewPortManager(logger)
	if err := pm.Add("test-concurrent", serverCfg.Port, br); err != nil {
		t.Fatalf("failed to add to port manager: %v", err)
	}

	// Cleanup in correct order
	defer func() {
		pm.Close()
		cancel()
		time.Sleep(500 * time.Millisecond)
		br.Close()
	}()

	time.Sleep(200 * time.Millisecond)

	// Send concurrent requests
	const numClients = 10
	var wg sync.WaitGroup
	errors := make(chan error, numClients)

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()

			req := map[string]any{
				"jsonrpc": "2.0",
				"id":      clientID,
				"method":  "initialize",
				"params": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{},
					"clientInfo": map[string]any{
						"name":    fmt.Sprintf("client-%d", clientID),
						"version": "1.0.0",
					},
				},
			}

			resp := sendMCPRequestQuiet(serverCfg.Port, req)
			if resp == nil {
				errors <- fmt.Errorf("client %d: nil response", clientID)
				return
			}
			if resp["error"] != nil {
				errors <- fmt.Errorf("client %d: error %v", clientID, resp["error"])
				return
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	var errs []error
	for err := range errors {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		t.Errorf("concurrent requests had %d errors:", len(errs))
		for _, err := range errs {
			t.Errorf("  - %v", err)
		}
	}
}

// TestCrashRecovery tests that crashed servers are restarted.
func TestCrashRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Use a server that exits after one request
	serverCfg := &config.ServerConfig{
		Command:   "node",
		Args:      []string{"-e", crashingMCPServerJS},
		Port:      16278,
		Autostart: true,
	}

	supCfg := config.SupervisionConfig{
		ShutdownTimeout:     config.Duration(5 * time.Second),
		RestartDelay:        config.Duration(100 * time.Millisecond),
		MaxRestartDelay:     config.Duration(1 * time.Second),
		HealthCheckInterval: config.Duration(100 * time.Millisecond),
	}
	sup := supervisor.New(supCfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	sup.ServeBackground(ctx)
	defer cancel()

	proc, err := sup.AddServer("test-crash", serverCfg)
	if err != nil {
		t.Fatalf("failed to add server: %v", err)
	}

	// Wait for initial start
	time.Sleep(500 * time.Millisecond)

	initialPID := proc.PID()
	if initialPID == 0 {
		t.Fatal("server did not start (PID is 0)")
	}

	t.Logf("Initial PID: %d", initialPID)

	// Create bridge and send a request to trigger the crash
	br := bridge.NewStdioHTTPBridge(proc.Stdin(), proc.Stdout(), &bridge.BridgeOptions{
		RequestTimeout: 5 * time.Second,
		Logger:         logger,
	})
	br.Start()

	// Send initialize request to trigger the crash timer
	ctx, reqCancel := context.WithTimeout(context.Background(), 2*time.Second)
	initReq, _ := bridge.NewRequest(1, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
	})
	_, _ = br.SendRequest(ctx, initReq) // May or may not get response before crash
	reqCancel()

	// Wait for the server to crash (500ms timer) and for supervisor to restart it
	time.Sleep(2 * time.Second)

	// Check that it restarted (restart count > 0)
	restarts := proc.RestartCount()
	newPID := proc.PID()

	t.Logf("After crash: PID=%d, restarts=%d", newPID, restarts)

	// The server should have restarted at least once
	if restarts == 0 {
		t.Error("server did not restart after crash (restart count is 0)")
	}

	// Cleanup
	br.Close()
}

// TestHealthEndpoint tests the per-server health endpoint.
func TestHealthEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	serverCfg := &config.ServerConfig{
		Command:   "node",
		Args:      []string{"-e", echoMCPServerJS},
		Port:      16279,
		Autostart: true,
	}

	supCfg := config.SupervisionConfig{
		ShutdownTimeout: config.Duration(5 * time.Second),
	}
	sup := supervisor.New(supCfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	sup.ServeBackground(ctx)

	proc, err := sup.AddServer("test-health", serverCfg)
	if err != nil {
		t.Fatalf("failed to add server: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	br := bridge.NewStdioHTTPBridge(proc.Stdin(), proc.Stdout(), &bridge.BridgeOptions{
		RequestTimeout: 10 * time.Second,
		Logger:         logger,
	})
	br.Start()

	pm := mcp.NewPortManager(logger)
	if err := pm.Add("test-health", serverCfg.Port, br); err != nil {
		t.Fatalf("failed to add to port manager: %v", err)
	}

	// Cleanup in correct order
	defer func() {
		pm.Close()
		cancel()
		time.Sleep(500 * time.Millisecond)
		br.Close()
	}()

	time.Sleep(200 * time.Millisecond)

	// Test health endpoint
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health", serverCfg.Port))
	if err != nil {
		t.Fatalf("failed to get health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("health returned status %d", resp.StatusCode)
	}

	var health map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("failed to decode health response: %v", err)
	}

	if health["status"] != "ok" {
		t.Errorf("health status is not ok: %v", health["status"])
	}
}

// --- Test helpers ---

func sendMCPRequest(t *testing.T, port int, req map[string]any) map[string]any {
	t.Helper()
	resp := sendMCPRequestQuiet(port, req)
	if resp == nil {
		t.Fatal("nil response from MCP request")
	}
	return resp
}

func sendMCPRequestQuiet(port int, req map[string]any) map[string]any {
	body, err := json.Marshal(req)
	if err != nil {
		return nil
	}

	url := fmt.Sprintf("http://localhost:%d/mcp", port)

	// Use a client with timeout
	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	httpResp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	defer httpResp.Body.Close()

	var resp map[string]any
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil
	}

	return resp
}

// --- Test MCP servers implemented in Node.js ---

// echoMCPServerJS is a minimal MCP server that responds to all requests.
const echoMCPServerJS = `
const readline = require('readline');

const rl = readline.createInterface({
  input: process.stdin,
  output: process.stdout,
  terminal: false
});

rl.on('line', (line) => {
  try {
    const req = JSON.parse(line);
    let response;
    
    if (req.method === 'initialize') {
      response = {
        jsonrpc: '2.0',
        id: req.id,
        result: {
          protocolVersion: '2024-11-05',
          capabilities: {
            tools: {}
          },
          serverInfo: {
            name: 'test-echo-server',
            version: '1.0.0'
          }
        }
      };
    } else if (req.method === 'tools/list') {
      response = {
        jsonrpc: '2.0',
        id: req.id,
        result: {
          tools: [
            {
              name: 'echo',
              description: 'Echoes back the input',
              inputSchema: {
                type: 'object',
                properties: {
                  message: { type: 'string' }
                }
              }
            }
          ]
        }
      };
    } else if (req.method === 'tools/call') {
      const args = req.params?.arguments || {};
      response = {
        jsonrpc: '2.0',
        id: req.id,
        result: {
          content: [
            {
              type: 'text',
              text: 'Echo: ' + (args.message || 'no message')
            }
          ]
        }
      };
    } else if (req.method === 'notifications/initialized') {
      // Notification - no response needed
      return;
    } else {
      response = {
        jsonrpc: '2.0',
        id: req.id,
        error: {
          code: -32601,
          message: 'Method not found: ' + req.method
        }
      };
    }
    
    console.log(JSON.stringify(response));
  } catch (e) {
    const errorResponse = {
      jsonrpc: '2.0',
      id: null,
      error: {
        code: -32700,
        message: 'Parse error: ' + e.message
      }
    };
    console.log(JSON.stringify(errorResponse));
  }
});
`

// crashingMCPServerJS is a server that crashes after a short delay.
const crashingMCPServerJS = `
const readline = require('readline');

const rl = readline.createInterface({
  input: process.stdin,
  output: process.stdout,
  terminal: false
});

let requestCount = 0;

rl.on('line', (line) => {
  try {
    const req = JSON.parse(line);
    requestCount++;
    
    // Crash after 500ms
    setTimeout(() => {
      process.exit(1);
    }, 500);
    
    if (req.method === 'initialize') {
      const response = {
        jsonrpc: '2.0',
        id: req.id,
        result: {
          protocolVersion: '2024-11-05',
          capabilities: {},
          serverInfo: {
            name: 'crashing-server',
            version: '1.0.0'
          }
        }
      };
      console.log(JSON.stringify(response));
    }
  } catch (e) {
    // Ignore parse errors
  }
});
`

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
