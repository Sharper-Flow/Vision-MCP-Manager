package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jrede/vision/internal/admin"
	"github.com/jrede/vision/internal/bridge"
)

// Helper to call an MCP tool - returns result or nil if RPC error
func callTool(t *testing.T, port int, toolName string, args interface{}) (*admin.ToolCallResult, *bridge.Error) {
	t.Helper()

	argsBytes, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	callParams := admin.ToolCallParams{
		Name:      toolName,
		Arguments: argsBytes,
	}
	paramsBytes, _ := json.Marshal(callParams)

	req := bridge.Request{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  paramsBytes,
		ID:      1,
	}
	reqBody, _ := json.Marshal(req)

	resp, err := http.Post(
		"http://localhost:"+itoa(port)+"/mcp",
		"application/json",
		bytes.NewReader(reqBody),
	)
	if err != nil {
		t.Fatalf("POST /mcp error = %v", err)
	}
	defer resp.Body.Close()

	var rpcResp bridge.Response
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("Decode error = %v", err)
	}

	if rpcResp.Error != nil {
		return nil, rpcResp.Error
	}

	var result admin.ToolCallResult
	if err := json.Unmarshal(rpcResp.Result, &result); err != nil {
		t.Fatalf("Unmarshal result error = %v", err)
	}

	return &result, nil
}

func itoa(n int) string {
	return string([]byte{
		byte('0' + n/10000%10),
		byte('0' + n/1000%10),
		byte('0' + n/100%10),
		byte('0' + n/10%10),
		byte('0' + n%10),
	})
}

// TestTools_VisionList tests the vision_list tool
// Note: Without a registry, this returns an error (expected behavior)
func TestTools_VisionList(t *testing.T) {
	srv := admin.NewServer(admin.Config{Port: 16290})
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)
	time.Sleep(50 * time.Millisecond)

	result, rpcErr := callTool(t, 16290, "vision_list", map[string]interface{}{})

	if rpcErr != nil {
		// Without a registry configured, vision_list returns an internal error
		t.Log("vision_list returned RPC error (registry not configured):", rpcErr.Message)
		return
	}

	// Should return some text (either error or server list)
	if len(result.Content) == 0 || result.Content[0].Text == "" {
		t.Error("vision_list returned empty content")
	}
}

// TestTools_VisionSearch tests the vision_search tool with various queries
func TestTools_VisionSearch(t *testing.T) {
	srv := admin.NewServer(admin.Config{Port: 16291})
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)
	time.Sleep(50 * time.Millisecond)

	tests := []struct {
		name           string
		query          string
		capability     string
		wantMatch      string
		expectRPCError bool
	}{
		{
			name:      "search by name",
			query:     "context7",
			wantMatch: "context7",
		},
		{
			name:       "search by capability",
			capability: "documentation",
			wantMatch:  "documentation",
		},
		{
			name:      "search by description",
			query:     "web scraping",
			wantMatch: "firecrawl",
		},
		{
			name:           "empty query returns error",
			query:          "",
			expectRPCError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := map[string]interface{}{}
			if tt.query != "" {
				args["query"] = tt.query
			}
			if tt.capability != "" {
				args["capability"] = tt.capability
			}

			result, rpcErr := callTool(t, 16291, "vision_search", args)

			if tt.expectRPCError {
				if rpcErr == nil {
					t.Error("expected RPC error for empty query")
				}
				return
			}

			if rpcErr != nil {
				t.Errorf("vision_search returned RPC error: %v", rpcErr)
				return
			}

			if result.IsError {
				t.Errorf("vision_search returned tool error: %s", result.Content[0].Text)
				return
			}

			text := result.Content[0].Text
			if !strings.Contains(strings.ToLower(text), strings.ToLower(tt.wantMatch)) {
				t.Errorf("vision_search result doesn't contain %q:\n%s", tt.wantMatch, text)
			}
		})
	}
}

// TestTools_VisionSearchNoResults tests search with no matches
func TestTools_VisionSearchNoResults(t *testing.T) {
	srv := admin.NewServer(admin.Config{Port: 16292})
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)
	time.Sleep(50 * time.Millisecond)

	result, rpcErr := callTool(t, 16292, "vision_search", map[string]interface{}{
		"query": "nonexistent_server_xyz123",
	})

	if rpcErr != nil {
		t.Errorf("vision_search returned RPC error: %v", rpcErr)
		return
	}

	if result.IsError {
		t.Errorf("vision_search returned tool error for no results")
	}

	// Parse JSON response
	text := result.Content[0].Text
	var searchResp struct {
		Success bool          `json:"success"`
		Results []interface{} `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &searchResp); err != nil {
		t.Fatalf("failed to parse search response: %v", err)
	}

	if !searchResp.Success {
		t.Error("expected success=true for no results")
	}
	if len(searchResp.Results) != 0 {
		t.Errorf("expected empty results, got %d", len(searchResp.Results))
	}
}

// TestTools_VisionAdd_NotConfigured tests adding a server not in registry
func TestTools_VisionAdd_NotConfigured(t *testing.T) {
	srv := admin.NewServer(admin.Config{Port: 16293})
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)
	time.Sleep(50 * time.Millisecond)

	// Try to add a server from catalog (not yet configured)
	result, rpcErr := callTool(t, 16293, "vision_add", map[string]interface{}{
		"name": "context7",
	})

	if rpcErr != nil {
		t.Errorf("vision_add returned RPC error: %v", rpcErr)
		return
	}

	// Parse JSON response
	text := result.Content[0].Text
	var addResp struct {
		Success bool   `json:"success"`
		Name    string `json:"name"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &addResp); err != nil {
		t.Fatalf("failed to parse add response: %v", err)
	}

	// Should return success=false with instructions (not yet configured)
	if addResp.Success {
		t.Error("expected success=false for unconfigured server")
	}
	if !strings.Contains(addResp.Error, "not configured") && !strings.Contains(addResp.Error, "servers.yaml") {
		t.Errorf("expected configuration instructions, got: %s", addResp.Error)
	}
}

// TestTools_VisionAdd_NotFound tests adding a server not in catalog
func TestTools_VisionAdd_NotFound(t *testing.T) {
	srv := admin.NewServer(admin.Config{Port: 16294})
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)
	time.Sleep(50 * time.Millisecond)

	result, rpcErr := callTool(t, 16294, "vision_add", map[string]interface{}{
		"name": "nonexistent_server_xyz",
	})

	if rpcErr != nil {
		t.Errorf("vision_add returned RPC error: %v", rpcErr)
		return
	}

	// Parse JSON response
	text := result.Content[0].Text
	var addResp struct {
		Success bool   `json:"success"`
		Name    string `json:"name"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &addResp); err != nil {
		t.Fatalf("failed to parse add response: %v", err)
	}

	// Should return success=false with not found error
	if addResp.Success {
		t.Error("expected success=false for unknown server")
	}
	if !strings.Contains(addResp.Error, "not found") {
		t.Errorf("expected 'not found' message, got: %s", addResp.Error)
	}
}

// TestTools_VisionRemove_NotFound tests removing non-existent server
func TestTools_VisionRemove_NotFound(t *testing.T) {
	srv := admin.NewServer(admin.Config{Port: 16295})
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)
	time.Sleep(50 * time.Millisecond)

	result, rpcErr := callTool(t, 16295, "vision_remove", map[string]interface{}{
		"name": "nonexistent",
	})

	if rpcErr != nil {
		t.Errorf("vision_remove returned RPC error: %v", rpcErr)
		return
	}

	// Parse JSON response
	text := result.Content[0].Text
	var removeResp struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &removeResp); err != nil {
		t.Fatalf("failed to parse remove response: %v", err)
	}

	// Should return success=false
	if removeResp.Success {
		t.Error("expected success=false for removing non-existent server")
	}
}

// TestTools_VisionInit tests the vision_init tool
// Note: Without a registry, this returns an error (expected behavior)
func TestTools_VisionInit(t *testing.T) {
	srv := admin.NewServer(admin.Config{Port: 16296})
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)
	time.Sleep(50 * time.Millisecond)

	result, rpcErr := callTool(t, 16296, "vision_init", map[string]interface{}{
		"path": "/tmp/test-opencode.json",
	})

	if rpcErr != nil {
		// Without a registry configured, vision_init returns an internal error
		t.Log("vision_init returned RPC error (registry not configured):", rpcErr.Message)
		return
	}

	// Should return some text (either error or config content)
	if len(result.Content) == 0 || result.Content[0].Text == "" {
		t.Error("vision_init returned empty content")
	}
}

// TestTools_VisionStatus tests the vision_status tool
func TestTools_VisionStatus(t *testing.T) {
	srv := admin.NewServer(admin.Config{Port: 16297})
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)
	time.Sleep(50 * time.Millisecond)

	result, rpcErr := callTool(t, 16297, "vision_status", map[string]interface{}{})

	if rpcErr != nil {
		t.Errorf("vision_status returned RPC error: %v", rpcErr)
		return
	}

	if result.IsError {
		t.Errorf("vision_status returned error: %s", result.Content[0].Text)
		return
	}

	// Parse JSON response
	text := result.Content[0].Text
	var statusResp struct {
		Healthy  bool    `json:"healthy"`
		Uptime   string  `json:"uptime"`
		MemoryMB float64 `json:"memory_mb"`
		Servers  struct {
			Running int `json:"running"`
			Stopped int `json:"stopped"`
			Error   int `json:"error"`
		} `json:"servers"`
	}
	if err := json.Unmarshal([]byte(text), &statusResp); err != nil {
		t.Fatalf("failed to parse status response: %v", err)
	}

	// Should have valid uptime
	if statusResp.Uptime == "" {
		t.Error("expected uptime in status")
	}
	// Should have memory info
	if statusResp.MemoryMB <= 0 {
		t.Error("expected positive memory_mb in status")
	}
}

// TestTools_ConcurrentCalls tests concurrent tool calls
func TestTools_ConcurrentCalls(t *testing.T) {
	srv := admin.NewServer(admin.Config{Port: 16298})
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)
	time.Sleep(50 * time.Millisecond)

	const numCalls = 10
	var wg sync.WaitGroup
	errors := make(chan error, numCalls)

	// Make concurrent calls to different tools
	tools := []string{"vision_list", "vision_status", "vision_search"}

	for i := 0; i < numCalls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			toolName := tools[i%len(tools)]
			args := map[string]interface{}{}
			if toolName == "vision_search" {
				args["query"] = "time"
			}

			argsBytes, _ := json.Marshal(args)
			callParams := admin.ToolCallParams{
				Name:      toolName,
				Arguments: argsBytes,
			}
			paramsBytes, _ := json.Marshal(callParams)

			req := bridge.Request{
				JSONRPC: "2.0",
				Method:  "tools/call",
				Params:  paramsBytes,
				ID:      i,
			}
			reqBody, _ := json.Marshal(req)

			resp, err := http.Post(
				"http://localhost:16298/mcp",
				"application/json",
				bytes.NewReader(reqBody),
			)
			if err != nil {
				errors <- err
				return
			}
			defer resp.Body.Close()

			var rpcResp bridge.Response
			if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
				errors <- err
				return
			}

			// RPC errors are OK for tools that need registry (vision_list)
			// We just want to ensure no HTTP/connection errors
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("concurrent call error: %v", err)
	}
}

// TestTools_InvalidArguments tests error handling for invalid arguments
func TestTools_InvalidArguments(t *testing.T) {
	srv := admin.NewServer(admin.Config{Port: 16300})
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Stop(ctx)
	time.Sleep(50 * time.Millisecond)

	tests := []struct {
		name     string
		toolName string
		args     interface{}
	}{
		{
			name:     "vision_add missing name",
			toolName: "vision_add",
			args:     map[string]interface{}{},
		},
		{
			name:     "vision_remove missing name",
			toolName: "vision_remove",
			args:     map[string]interface{}{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, rpcErr := callTool(t, 16300, tt.toolName, tt.args)

			// Should return JSON-RPC error -32602 for invalid params
			if rpcErr == nil {
				t.Error("expected RPC error for invalid arguments")
			} else if rpcErr.Code != -32602 {
				t.Errorf("expected error code -32602, got %d", rpcErr.Code)
			}
		})
	}
}
