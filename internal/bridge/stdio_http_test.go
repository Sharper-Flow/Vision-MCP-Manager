package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testPipes creates paired pipes for testing the bridge.
// The bridge writes to toServer and reads from fromServer.
// The mock server reads from toServer and writes to fromServer.
type testPipes struct {
	// Bridge side
	toServer   *io.PipeWriter // bridge writes here (server's stdin)
	fromServer *io.PipeReader // bridge reads here (server's stdout)

	// Server side
	serverStdin  *io.PipeReader // server reads here
	serverStdout *io.PipeWriter // server writes here
}

func newTestPipes() *testPipes {
	serverStdinR, serverStdinW := io.Pipe()
	serverStdoutR, serverStdoutW := io.Pipe()

	return &testPipes{
		toServer:     serverStdinW,
		fromServer:   serverStdoutR,
		serverStdin:  serverStdinR,
		serverStdout: serverStdoutW,
	}
}

func (p *testPipes) Close() {
	p.toServer.Close()
	p.fromServer.Close()
	p.serverStdin.Close()
	p.serverStdout.Close()
}

// mockMCPServer simulates an MCP server over stdio.
type mockMCPServer struct {
	stdin   *io.PipeReader
	stdout  *io.PipeWriter
	handler func(req *Request) *Response
	done    chan struct{}
	stopped atomic.Bool
}

func newMockMCPServer(stdin *io.PipeReader, stdout *io.PipeWriter, handler func(*Request) *Response) *mockMCPServer {
	return &mockMCPServer{
		stdin:   stdin,
		stdout:  stdout,
		handler: handler,
		done:    make(chan struct{}),
	}
}

func (m *mockMCPServer) Start() {
	go func() {
		scanner := bufio.NewScanner(m.stdin)
		for scanner.Scan() {
			if m.stopped.Load() {
				return
			}

			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			var req Request
			if err := json.Unmarshal(line, &req); err != nil {
				continue
			}

			// Skip notifications (no response needed)
			if req.ID == nil {
				continue
			}

			resp := m.handler(&req)
			respData, _ := json.Marshal(resp)
			m.stdout.Write(append(respData, '\n'))
		}
		close(m.done)
	}()
}

func (m *mockMCPServer) Stop() {
	m.stopped.Store(true)
	m.stdin.Close()  // This will unblock the scanner
	m.stdout.Close() // Close stdout to unblock bridge's readLoop
	<-m.done         // Wait for goroutine to exit
}

// --- End-to-End HTTP Bridge Tests ---

func TestStdioHTTPBridge_EndToEnd_SimpleRequest(t *testing.T) {
	pipes := newTestPipes()

	// Create mock MCP server
	server := newMockMCPServer(pipes.serverStdin, pipes.serverStdout, func(req *Request) *Response {
		return &Response{
			JSONRPC: "2.0",
			Result:  json.RawMessage(`{"message":"hello from server"}`),
			ID:      req.ID,
		}
	})
	server.Start()

	// Create bridge
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := NewStdioHTTPBridge(pipes.toServer, pipes.fromServer, &BridgeOptions{
		RequestTimeout: 5 * time.Second,
		Logger:         logger,
	})
	bridge.Start()

	// Send request through ForwardRequest (simulates HTTP handler)
	reqData := []byte(`{"jsonrpc":"2.0","method":"test/echo","params":{"text":"hello"},"id":1}`)
	ctx := context.Background()
	respData, err := bridge.ForwardRequest(ctx, reqData)

	// Cleanup (order matters: server.Stop closes stdout, which unblocks bridge's readLoop)
	server.Stop()
	bridge.Close()

	if err != nil {
		t.Fatalf("ForwardRequest error: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(respData, &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if resp.Error != nil {
		t.Errorf("Unexpected error: %v", resp.Error)
	}

	var result map[string]string
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if result["message"] != "hello from server" {
		t.Errorf("Expected message 'hello from server', got %q", result["message"])
	}
}

func TestStdioHTTPBridge_EndToEnd_ErrorResponse(t *testing.T) {
	pipes := newTestPipes()

	// Create mock MCP server that returns errors
	server := newMockMCPServer(pipes.serverStdin, pipes.serverStdout, func(req *Request) *Response {
		return &Response{
			JSONRPC: "2.0",
			Error:   NewError(CodeMethodNotFound, "Method not found"),
			ID:      req.ID,
		}
	})
	server.Start()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := NewStdioHTTPBridge(pipes.toServer, pipes.fromServer, &BridgeOptions{
		RequestTimeout: 5 * time.Second,
		Logger:         logger,
	})
	bridge.Start()

	reqData := []byte(`{"jsonrpc":"2.0","method":"nonexistent","params":{},"id":1}`)
	ctx := context.Background()
	respData, err := bridge.ForwardRequest(ctx, reqData)

	server.Stop()
	bridge.Close()

	if err != nil {
		t.Fatalf("ForwardRequest error: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(respData, &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if resp.Error == nil {
		t.Fatal("Expected error response")
	}

	if resp.Error.Code != CodeMethodNotFound {
		t.Errorf("Expected error code %d, got %d", CodeMethodNotFound, resp.Error.Code)
	}
}

// --- Concurrent Request Tests ---

func TestStdioHTTPBridge_ConcurrentRequests(t *testing.T) {
	pipes := newTestPipes()

	// Track request count
	var requestCount atomic.Int32

	// Mock server that echoes back
	server := newMockMCPServer(pipes.serverStdin, pipes.serverStdout, func(req *Request) *Response {
		requestCount.Add(1)
		// Small delay to simulate processing
		time.Sleep(5 * time.Millisecond)
		return &Response{
			JSONRPC: "2.0",
			Result:  json.RawMessage(`{"received":true}`),
			ID:      req.ID,
		}
	})
	server.Start()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := NewStdioHTTPBridge(pipes.toServer, pipes.fromServer, &BridgeOptions{
		RequestTimeout: 10 * time.Second,
		MaxQueueSize:   100,
		Logger:         logger,
	})
	bridge.Start()

	// Send concurrent requests
	const numRequests = 20
	var wg sync.WaitGroup
	errors := make(chan error, numRequests)
	results := make(chan int, numRequests)

	for i := 1; i <= numRequests; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			req := Request{
				JSONRPC: "2.0",
				Method:  "test",
				ID:      id,
			}
			reqData, _ := json.Marshal(req)

			ctx := context.Background()
			respData, err := bridge.ForwardRequest(ctx, reqData)
			if err != nil {
				errors <- err
				return
			}

			var resp Response
			if err := json.Unmarshal(respData, &resp); err != nil {
				errors <- err
				return
			}

			if resp.Error != nil {
				errors <- resp.Error
				return
			}

			results <- id
		}(i)
	}

	wg.Wait()
	close(errors)
	close(results)

	server.Stop()
	bridge.Close()

	// Check for errors
	for err := range errors {
		t.Errorf("Request failed: %v", err)
	}

	// Count successful responses
	successCount := 0
	for range results {
		successCount++
	}

	if successCount != numRequests {
		t.Errorf("Expected %d successful requests, got %d", numRequests, successCount)
	}

	// Verify server received all requests
	if requestCount.Load() != numRequests {
		t.Errorf("Server received %d requests, expected %d", requestCount.Load(), numRequests)
	}
}

func TestStdioHTTPBridge_ConcurrentRequestsWithVaryingLatency(t *testing.T) {
	pipes := newTestPipes()

	// Mock server with varying response times
	server := newMockMCPServer(pipes.serverStdin, pipes.serverStdout, func(req *Request) *Response {
		// Extract ID and use it to determine delay
		idNum := 0
		switch v := req.ID.(type) {
		case float64:
			idNum = int(v)
		case int:
			idNum = v
		}
		// Odd IDs respond faster than even IDs
		if idNum%2 == 1 {
			time.Sleep(2 * time.Millisecond)
		} else {
			time.Sleep(20 * time.Millisecond)
		}

		return &Response{
			JSONRPC: "2.0",
			Result:  json.RawMessage(`{"id_received":` + ExtractID(req.ID) + `}`),
			ID:      req.ID,
		}
	})
	server.Start()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := NewStdioHTTPBridge(pipes.toServer, pipes.fromServer, &BridgeOptions{
		RequestTimeout: 10 * time.Second,
		MaxQueueSize:   50,
		Logger:         logger,
	})
	bridge.Start()

	// Send requests concurrently and verify each gets correct response
	const numRequests = 10
	var wg sync.WaitGroup
	responseIDs := make(chan int, numRequests)
	errCh := make(chan error, numRequests)

	for i := 1; i <= numRequests; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			req := Request{
				JSONRPC: "2.0",
				Method:  "test",
				ID:      id,
			}
			reqData, _ := json.Marshal(req)

			ctx := context.Background()
			respData, err := bridge.ForwardRequest(ctx, reqData)
			if err != nil {
				errCh <- err
				return
			}

			var resp Response
			if err := json.Unmarshal(respData, &resp); err != nil {
				errCh <- err
				return
			}

			// Verify response ID matches request ID
			respID := ExtractID(resp.ID)
			expectedID := ExtractID(id)
			if respID != expectedID {
				errCh <- &Error{Code: -1, Message: "Response ID mismatch"}
				return
			}

			responseIDs <- id
		}(i)
	}

	wg.Wait()
	close(responseIDs)
	close(errCh)

	server.Stop()
	bridge.Close()

	// Check for errors
	for err := range errCh {
		t.Errorf("Request failed: %v", err)
	}

	// Count responses
	count := 0
	for range responseIDs {
		count++
	}

	if count != numRequests {
		t.Errorf("Expected %d responses, got %d", numRequests, count)
	}
}

// --- MCP Protocol Compliance Tests ---

func TestStdioHTTPBridge_MCPInitialize(t *testing.T) {
	pipes := newTestPipes()

	// Mock MCP server that handles initialize
	server := newMockMCPServer(pipes.serverStdin, pipes.serverStdout, func(req *Request) *Response {
		if req.Method == "initialize" {
			return &Response{
				JSONRPC: "2.0",
				Result: json.RawMessage(`{
					"protocolVersion": "2024-11-05",
					"serverInfo": {
						"name": "test-server",
						"version": "1.0.0"
					},
					"capabilities": {
						"tools": {}
					}
				}`),
				ID: req.ID,
			}
		}
		return &Response{
			JSONRPC: "2.0",
			Error:   NewError(CodeMethodNotFound, "Unknown method"),
			ID:      req.ID,
		}
	})
	server.Start()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := NewStdioHTTPBridge(pipes.toServer, pipes.fromServer, &BridgeOptions{
		RequestTimeout: 5 * time.Second,
		Logger:         logger,
	})
	bridge.Start()

	// Send initialize request
	initReq := `{
		"jsonrpc": "2.0",
		"method": "initialize",
		"params": {
			"protocolVersion": "2024-11-05",
			"capabilities": {},
			"clientInfo": {
				"name": "test-client",
				"version": "1.0.0"
			}
		},
		"id": 1
	}`

	ctx := context.Background()
	respData, err := bridge.ForwardRequest(ctx, []byte(initReq))

	server.Stop()
	bridge.Close()

	if err != nil {
		t.Fatalf("Initialize request failed: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(respData, &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if resp.Error != nil {
		t.Fatalf("Initialize returned error: %v", resp.Error)
	}

	// Verify response structure
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Capabilities struct {
			Tools map[string]any `json:"tools"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if result.ProtocolVersion != "2024-11-05" {
		t.Errorf("Expected protocol version '2024-11-05', got %q", result.ProtocolVersion)
	}

	if result.ServerInfo.Name != "test-server" {
		t.Errorf("Expected server name 'test-server', got %q", result.ServerInfo.Name)
	}
}

func TestStdioHTTPBridge_MCPToolsList(t *testing.T) {
	pipes := newTestPipes()

	// Mock MCP server with tools
	server := newMockMCPServer(pipes.serverStdin, pipes.serverStdout, func(req *Request) *Response {
		if req.Method == "tools/list" {
			return &Response{
				JSONRPC: "2.0",
				Result: json.RawMessage(`{
					"tools": [
						{
							"name": "get_time",
							"description": "Get current time",
							"inputSchema": {
								"type": "object",
								"properties": {}
							}
						},
						{
							"name": "search",
							"description": "Search for information",
							"inputSchema": {
								"type": "object",
								"properties": {
									"query": {"type": "string"}
								},
								"required": ["query"]
							}
						}
					]
				}`),
				ID: req.ID,
			}
		}
		return &Response{
			JSONRPC: "2.0",
			Error:   NewError(CodeMethodNotFound, "Unknown method"),
			ID:      req.ID,
		}
	})
	server.Start()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := NewStdioHTTPBridge(pipes.toServer, pipes.fromServer, &BridgeOptions{
		RequestTimeout: 5 * time.Second,
		Logger:         logger,
	})
	bridge.Start()

	// Send tools/list request
	toolsReq := `{"jsonrpc":"2.0","method":"tools/list","params":{},"id":1}`

	ctx := context.Background()
	respData, err := bridge.ForwardRequest(ctx, []byte(toolsReq))

	server.Stop()
	bridge.Close()

	if err != nil {
		t.Fatalf("tools/list request failed: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(respData, &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if resp.Error != nil {
		t.Fatalf("tools/list returned error: %v", resp.Error)
	}

	// Verify tools
	var result struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if len(result.Tools) != 2 {
		t.Errorf("Expected 2 tools, got %d", len(result.Tools))
	}

	toolNames := make(map[string]bool)
	for _, tool := range result.Tools {
		toolNames[tool.Name] = true
	}

	if !toolNames["get_time"] {
		t.Error("Expected 'get_time' tool")
	}
	if !toolNames["search"] {
		t.Error("Expected 'search' tool")
	}
}

func TestStdioHTTPBridge_MCPNotification(t *testing.T) {
	pipes := newTestPipes()

	// Track received notifications
	var notificationsReceived atomic.Int32
	readerDone := make(chan struct{})

	// Custom notification handler that counts notifications
	go func() {
		defer close(readerDone)
		scanner := bufio.NewScanner(pipes.serverStdin)
		for scanner.Scan() {
			line := scanner.Bytes()
			var req Request
			if err := json.Unmarshal(line, &req); err != nil {
				continue
			}
			if req.ID == nil && req.Method == "notifications/initialized" {
				notificationsReceived.Add(1)
			}
		}
	}()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := NewStdioHTTPBridge(pipes.toServer, pipes.fromServer, &BridgeOptions{
		Logger: logger,
	})
	bridge.Start()

	// Send notification (no ID = notification)
	notifReq := `{"jsonrpc":"2.0","method":"notifications/initialized"}`

	ctx := context.Background()
	respData, err := bridge.ForwardRequest(ctx, []byte(notifReq))

	// Give time for notification to be processed
	time.Sleep(50 * time.Millisecond)

	// Close pipes first to unblock readers (order matters!)
	pipes.serverStdout.Close() // Unblocks bridge's readLoop
	pipes.toServer.Close()     // Unblocks notification reader
	bridge.Close()
	<-readerDone // Wait for reader goroutine to finish

	if err != nil {
		t.Fatalf("Notification failed: %v", err)
	}

	// Notifications should return nil (no response expected)
	if respData != nil {
		t.Errorf("Expected nil response for notification, got: %s", string(respData))
	}

	if notificationsReceived.Load() != 1 {
		t.Errorf("Expected 1 notification, received %d", notificationsReceived.Load())
	}
}

// --- Error Handling Tests ---

func TestStdioHTTPBridge_ClosedBridge(t *testing.T) {
	pipes := newTestPipes()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := NewStdioHTTPBridge(pipes.toServer, pipes.fromServer, &BridgeOptions{
		Logger: logger,
	})
	bridge.Start()

	// Close pipes first to unblock bridge's readLoop, then close bridge
	pipes.serverStdout.Close()
	bridge.Close()

	// Try to send request after close
	req, _ := NewRequest(1, "test", nil)
	ctx := context.Background()
	_, err := bridge.SendRequest(ctx, req)

	if err != ErrBridgeClosed {
		t.Errorf("Expected ErrBridgeClosed, got: %v", err)
	}
}

func TestStdioHTTPBridge_MalformedRequest(t *testing.T) {
	pipes := newTestPipes()

	// Drain server stdin to prevent blocking
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := pipes.serverStdin.Read(buf); err != nil {
				return
			}
		}
	}()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := NewStdioHTTPBridge(pipes.toServer, pipes.fromServer, &BridgeOptions{
		Logger: logger,
	})
	bridge.Start()

	// Malformed JSON - returns error immediately without going to server
	ctx := context.Background()
	respData, err := bridge.ForwardRequest(ctx, []byte(`{invalid json`))

	// Close pipes first, then bridge
	pipes.serverStdout.Close()
	bridge.Close()

	if err != nil {
		t.Fatalf("ForwardRequest error: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(respData, &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if resp.Error == nil {
		t.Error("Expected error for malformed JSON")
	}

	if resp.Error.Code != CodeParseError {
		t.Errorf("Expected parse error code %d, got %d", CodeParseError, resp.Error.Code)
	}
}

func TestStdioHTTPBridge_InvalidJSONRPCVersion(t *testing.T) {
	pipes := newTestPipes()

	// Drain server stdin
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := pipes.serverStdin.Read(buf); err != nil {
				return
			}
		}
	}()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := NewStdioHTTPBridge(pipes.toServer, pipes.fromServer, &BridgeOptions{
		Logger: logger,
	})
	bridge.Start()

	// Wrong JSON-RPC version - returns error immediately without going to server
	ctx := context.Background()
	respData, err := bridge.ForwardRequest(ctx, []byte(`{"jsonrpc":"1.0","method":"test","id":1}`))

	// Close pipes first, then bridge
	pipes.serverStdout.Close()
	bridge.Close()

	if err != nil {
		t.Fatalf("ForwardRequest error: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(respData, &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if resp.Error == nil {
		t.Error("Expected error for invalid JSON-RPC version")
	}

	if resp.Error.Code != CodeInvalidRequest {
		t.Errorf("Expected invalid request error code %d, got %d", CodeInvalidRequest, resp.Error.Code)
	}
}

func TestStdioHTTPBridge_ContextCancellation(t *testing.T) {
	pipes := newTestPipes()

	// Drain server stdin to prevent blocking
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := pipes.serverStdin.Read(buf); err != nil {
				return
			}
		}
	}()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := NewStdioHTTPBridge(pipes.toServer, pipes.fromServer, &BridgeOptions{
		RequestTimeout: 10 * time.Second,
		Logger:         logger,
	})
	bridge.Start()

	// Create a context that we'll cancel
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after a short delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	req, _ := NewRequest(1, "test", nil)
	_, err := bridge.SendRequest(ctx, req)

	// Close pipes first, then bridge
	pipes.serverStdout.Close()
	bridge.Close()

	if err != context.Canceled {
		t.Errorf("Expected context.Canceled, got: %v", err)
	}
}

func TestStdioHTTPBridge_RequestWithoutID(t *testing.T) {
	pipes := newTestPipes()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := NewStdioHTTPBridge(pipes.toServer, pipes.fromServer, &BridgeOptions{
		Logger: logger,
	})
	bridge.Start()

	// Request without ID should fail (doesn't go to server)
	ctx := context.Background()
	req := &Request{
		JSONRPC: "2.0",
		Method:  "test",
		ID:      nil,
	}

	_, err := bridge.SendRequest(ctx, req)

	// Close pipes first, then bridge
	pipes.serverStdout.Close()
	bridge.Close()

	if err == nil {
		t.Error("Expected error for request without ID")
	}
}

// --- HTTP Handler Integration Tests ---

func TestHTTPHandler_EndToEnd(t *testing.T) {
	pipes := newTestPipes()

	// Mock MCP server
	server := newMockMCPServer(pipes.serverStdin, pipes.serverStdout, func(req *Request) *Response {
		return &Response{
			JSONRPC: "2.0",
			Result:  json.RawMessage(`{"processed":true}`),
			ID:      req.ID,
		}
	})
	server.Start()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := NewStdioHTTPBridge(pipes.toServer, pipes.fromServer, &BridgeOptions{
		RequestTimeout: 5 * time.Second,
		Logger:         logger,
	})
	bridge.Start()

	// Create HTTP handler using the bridge
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}

		resp, err := bridge.ForwardRequest(r.Context(), body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(resp)
	})

	// Create test server
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Make HTTP request
	reqBody := `{"jsonrpc":"2.0","method":"test","params":{},"id":"http-test-1"}`
	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("HTTP request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	var jsonResp Response
	if err := json.NewDecoder(resp.Body).Decode(&jsonResp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}
	resp.Body.Close()

	// Close in correct order: server first (unblocks bridge), then bridge
	server.Stop()
	bridge.Close()

	if jsonResp.Error != nil {
		t.Errorf("Unexpected error: %v", jsonResp.Error)
	}

	// Verify ID matches
	if ExtractID(jsonResp.ID) != "http-test-1" {
		t.Errorf("Response ID mismatch: expected 'http-test-1', got %v", jsonResp.ID)
	}
}

func TestHTTPHandler_ConcurrentHTTPRequests(t *testing.T) {
	pipes := newTestPipes()

	// Mock MCP server
	server := newMockMCPServer(pipes.serverStdin, pipes.serverStdout, func(req *Request) *Response {
		time.Sleep(5 * time.Millisecond) // Simulate processing
		return &Response{
			JSONRPC: "2.0",
			Result:  json.RawMessage(`{"ok":true}`),
			ID:      req.ID,
		}
	})
	server.Start()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := NewStdioHTTPBridge(pipes.toServer, pipes.fromServer, &BridgeOptions{
		RequestTimeout: 10 * time.Second,
		MaxQueueSize:   100,
		Logger:         logger,
	})
	bridge.Start()

	// Create HTTP handler
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		resp, err := bridge.ForwardRequest(r.Context(), body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(resp)
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Send concurrent HTTP requests
	const numRequests = 15
	var wg sync.WaitGroup
	successCount := atomic.Int32{}

	for i := 1; i <= numRequests; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			reqBody := `{"jsonrpc":"2.0","method":"test","params":{},"id":` + ExtractID(id) + `}`
			resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(reqBody))
			if err != nil {
				t.Errorf("HTTP request %d failed: %v", id, err)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				var jsonResp Response
				if err := json.NewDecoder(resp.Body).Decode(&jsonResp); err == nil && jsonResp.Error == nil {
					successCount.Add(1)
				}
			}
		}(i)
	}

	wg.Wait()

	server.Stop()
	bridge.Close()

	if successCount.Load() != numRequests {
		t.Errorf("Expected %d successful requests, got %d", numRequests, successCount.Load())
	}
}
