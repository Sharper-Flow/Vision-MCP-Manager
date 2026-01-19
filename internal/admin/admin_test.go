package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jrede/vision/internal/admin"
	"github.com/jrede/vision/internal/bridge"
)

func TestServer_StartStop(t *testing.T) {
	srv := admin.NewServer(admin.Config{
		Port: 16275, // Use high port to avoid conflicts
	})

	ctx := context.Background()

	// Start server
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if !srv.IsRunning() {
		t.Error("IsRunning() = false, want true")
	}

	// Give server time to start
	time.Sleep(50 * time.Millisecond)

	// Test health endpoint
	resp, err := http.Get("http://localhost:16275/health")
	if err != nil {
		t.Fatalf("GET /health error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /health status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Stop server
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	if srv.IsRunning() {
		t.Error("IsRunning() = true, want false after Stop()")
	}
}

func TestServer_MCPInfo(t *testing.T) {
	srv := admin.NewServer(admin.Config{
		Port: 16276,
	})

	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)

	time.Sleep(50 * time.Millisecond)

	resp, err := http.Get("http://localhost:16276/mcp")
	if err != nil {
		t.Fatalf("GET /mcp error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /mcp status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var info map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("Decode error = %v", err)
	}

	if info["name"] != "vision-admin" {
		t.Errorf("info[name] = %v, want vision-admin", info["name"])
	}
}

func TestServer_Initialize(t *testing.T) {
	srv := admin.NewServer(admin.Config{
		Port: 16277,
	})

	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)

	time.Sleep(50 * time.Millisecond)

	// Send initialize request
	req := bridge.Request{
		JSONRPC: "2.0",
		Method:  "initialize",
		ID:      1,
	}
	reqBody, _ := json.Marshal(req)

	resp, err := http.Post("http://localhost:16277/mcp", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /mcp error = %v", err)
	}
	defer resp.Body.Close()

	var rpcResp bridge.Response
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("Decode error = %v", err)
	}

	if rpcResp.Error != nil {
		t.Errorf("initialize returned error: %v", rpcResp.Error)
	}

	var result admin.InitializeResult
	if err := json.Unmarshal(rpcResp.Result, &result); err != nil {
		t.Fatalf("Unmarshal result error = %v", err)
	}

	if result.ServerInfo.Name != "vision-admin" {
		t.Errorf("ServerInfo.Name = %q, want %q", result.ServerInfo.Name, "vision-admin")
	}
}

func TestServer_ToolsList(t *testing.T) {
	srv := admin.NewServer(admin.Config{
		Port: 16278,
	})

	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)

	time.Sleep(50 * time.Millisecond)

	// Send tools/list request
	req := bridge.Request{
		JSONRPC: "2.0",
		Method:  "tools/list",
		ID:      1,
	}
	reqBody, _ := json.Marshal(req)

	resp, err := http.Post("http://localhost:16278/mcp", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /mcp error = %v", err)
	}
	defer resp.Body.Close()

	var rpcResp bridge.Response
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("Decode error = %v", err)
	}

	if rpcResp.Error != nil {
		t.Errorf("tools/list returned error: %v", rpcResp.Error)
	}

	var result admin.ToolsListResult
	if err := json.Unmarshal(rpcResp.Result, &result); err != nil {
		t.Fatalf("Unmarshal result error = %v", err)
	}

	// Should have all 6 tools
	expectedTools := []string{
		"vision_list",
		"vision_add",
		"vision_remove",
		"vision_search",
		"vision_init",
		"vision_status",
		"vision_guidance",
	}

	if len(result.Tools) != len(expectedTools) {
		t.Errorf("tools count = %d, want %d", len(result.Tools), len(expectedTools))
	}

	toolNames := make(map[string]bool)
	for _, tool := range result.Tools {
		toolNames[tool.Name] = true
	}

	for _, expected := range expectedTools {
		if !toolNames[expected] {
			t.Errorf("missing tool: %s", expected)
		}
	}
}

func TestServer_ToolsCallStatus(t *testing.T) {
	srv := admin.NewServer(admin.Config{
		Port: 16279,
	})

	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)

	time.Sleep(50 * time.Millisecond)

	// Send tools/call request for vision_status
	callParams := admin.ToolCallParams{
		Name:      "vision_status",
		Arguments: json.RawMessage(`{}`),
	}
	paramsBytes, _ := json.Marshal(callParams)

	req := bridge.Request{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  paramsBytes,
		ID:      1,
	}
	reqBody, _ := json.Marshal(req)

	resp, err := http.Post("http://localhost:16279/mcp", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /mcp error = %v", err)
	}
	defer resp.Body.Close()

	var rpcResp bridge.Response
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("Decode error = %v", err)
	}

	if rpcResp.Error != nil {
		t.Errorf("tools/call returned error: %v", rpcResp.Error)
	}

	var result admin.ToolCallResult
	if err := json.Unmarshal(rpcResp.Result, &result); err != nil {
		t.Fatalf("Unmarshal result error = %v", err)
	}

	if len(result.Content) == 0 {
		t.Error("result.Content is empty")
	}

	if result.Content[0].Type != "text" {
		t.Errorf("content type = %q, want %q", result.Content[0].Type, "text")
	}

	// Status should contain "Vision Daemon Status"
	if result.Content[0].Text == "" {
		t.Error("status text is empty")
	}
}

func TestServer_MethodNotFound(t *testing.T) {
	srv := admin.NewServer(admin.Config{
		Port: 16280,
	})

	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)

	time.Sleep(50 * time.Millisecond)

	// Send unknown method
	req := bridge.Request{
		JSONRPC: "2.0",
		Method:  "unknown/method",
		ID:      1,
	}
	reqBody, _ := json.Marshal(req)

	resp, err := http.Post("http://localhost:16280/mcp", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /mcp error = %v", err)
	}
	defer resp.Body.Close()

	var rpcResp bridge.Response
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("Decode error = %v", err)
	}

	if rpcResp.Error == nil {
		t.Error("expected error for unknown method")
	}

	if rpcResp.Error.Code != bridge.CodeMethodNotFound {
		t.Errorf("error code = %d, want %d", rpcResp.Error.Code, bridge.CodeMethodNotFound)
	}
}

func TestServer_UnknownTool(t *testing.T) {
	srv := admin.NewServer(admin.Config{
		Port: 16281,
	})

	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)

	time.Sleep(50 * time.Millisecond)

	// Send tools/call request for unknown tool
	callParams := admin.ToolCallParams{
		Name:      "unknown_tool",
		Arguments: json.RawMessage(`{}`),
	}
	paramsBytes, _ := json.Marshal(callParams)

	req := bridge.Request{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  paramsBytes,
		ID:      1,
	}
	reqBody, _ := json.Marshal(req)

	resp, err := http.Post("http://localhost:16281/mcp", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /mcp error = %v", err)
	}
	defer resp.Body.Close()

	var rpcResp bridge.Response
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("Decode error = %v", err)
	}

	// Unknown tool should return JSON-RPC error -32601 (method not found) per MCP spec
	if rpcResp.Error == nil {
		t.Fatal("expected JSON-RPC error for unknown tool")
	}

	if rpcResp.Error.Code != bridge.CodeMethodNotFound {
		t.Errorf("expected error code %d, got %d", bridge.CodeMethodNotFound, rpcResp.Error.Code)
	}
}
