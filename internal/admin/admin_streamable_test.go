package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jrede/vision/internal/admin"
	"github.com/jrede/vision/internal/config"
)

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      int    `json:"id,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func postMCP(t *testing.T, url string, sessionID string, body rpcRequest) (*http.Response, rpcResponse) {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp error: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	return resp, rpcResp
}

func TestAdminServer_StreamableLifecycle(t *testing.T) {
	srv := admin.NewServer(admin.Config{Port: 16275})
	ctx := context.Background()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(ctx) })

	time.Sleep(50 * time.Millisecond)
	url := "http://127.0.0.1:16275/mcp"

	initResp, initRPC := postMCP(t, url, "", rpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "admin-test",
				"version": "1.0.0",
			},
		},
	})

	if initResp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200", initResp.StatusCode)
	}
	if initRPC.Error != nil {
		t.Fatalf("initialize error = %+v", *initRPC.Error)
	}

	var initResult struct {
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(initRPC.Result, &initResult); err != nil {
		t.Fatalf("unmarshal initialize result: %v", err)
	}
	if initResult.ServerInfo.Name != "vision-admin" {
		t.Fatalf("serverInfo.name = %q, want vision-admin", initResult.ServerInfo.Name)
	}

	sessionID := initResp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("missing Mcp-Session-Id header")
	}

	_, listRPC := postMCP(t, url, sessionID, rpcRequest{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/list",
		Params:  map[string]any{},
	})
	if listRPC.Error != nil {
		t.Fatalf("tools/list error = %+v", *listRPC.Error)
	}

	var listResult struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listRPC.Result, &listResult); err != nil {
		t.Fatalf("unmarshal tools/list result: %v", err)
	}
	if len(listResult.Tools) == 0 {
		t.Fatal("tools/list returned zero tools")
	}

	_, callRPC := postMCP(t, url, sessionID, rpcRequest{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      "vision_status",
			"arguments": map[string]any{},
		},
	})
	if callRPC.Error != nil {
		t.Fatalf("tools/call error = %+v", *callRPC.Error)
	}

	var callResult struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(callRPC.Result, &callResult); err != nil {
		t.Fatalf("unmarshal tools/call result: %v", err)
	}
	if len(callResult.Content) == 0 || callResult.Content[0].Text == "" {
		t.Fatal("tools/call returned empty content")
	}

	deleteReq, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("new delete request: %v", err)
	}
	deleteReq.Header.Set("Mcp-Session-Id", sessionID)
	deleteResp, err := http.DefaultClient.Do(deleteReq)
	if err != nil {
		t.Fatalf("DELETE /mcp error: %v", err)
	}
	defer deleteResp.Body.Close()
	if deleteResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /mcp status = %d, want 204", deleteResp.StatusCode)
	}

	badReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}`)))
	if err != nil {
		t.Fatalf("new post request: %v", err)
	}
	badReq.Header.Set("Content-Type", "application/json")
	badReq.Header.Set("Accept", "application/json, text/event-stream")
	badReq.Header.Set("Mcp-Session-Id", sessionID)
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatalf("POST /mcp after delete error: %v", err)
	}
	defer badResp.Body.Close()
	if badResp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST with deleted session status = %d, want 404", badResp.StatusCode)
	}
}

func TestAdminServer_HealthEndpoints(t *testing.T) {
	srv := admin.NewServer(admin.Config{Port: 16276})
	ctx := context.Background()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(ctx) })

	time.Sleep(50 * time.Millisecond)

	resp, err := http.Get("http://127.0.0.1:16276/health")
	if err != nil {
		t.Fatalf("GET /health error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", resp.StatusCode)
	}

	resp2, err := http.Get("http://127.0.0.1:16276/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error = %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp2.StatusCode)
	}
}

func TestAdminServer_SecurityMiddleware(t *testing.T) {
	srv := admin.NewServer(admin.Config{
		Port: 16277,
		DaemonConfig: &config.Config{
			Security: config.SecurityConfig{
				BearerToken:    "test-token",
				AllowedOrigins: []string{"http://allowed.local"},
			},
		},
	})
	ctx := context.Background()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(ctx) })

	time.Sleep(50 * time.Millisecond)
	url := "http://127.0.0.1:16277/mcp"

	// Missing auth should be rejected.
	reqBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"admin-test","version":"1.0.0"}}}`)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Origin", "http://allowed.local")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d, want 401", resp.StatusCode)
	}

	// Valid auth + allowed origin should pass initialize.
	req2, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Accept", "application/json, text/event-stream")
	req2.Header.Set("Origin", "http://allowed.local")
	req2.Header.Set("Authorization", "Bearer test-token")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("authorized POST /mcp error: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("authorized initialize status = %d, want 200", resp2.StatusCode)
	}

	// Disallowed origin should be rejected at preflight.
	preflight, err := http.NewRequest(http.MethodOptions, url, nil)
	if err != nil {
		t.Fatalf("new preflight request: %v", err)
	}
	preflight.Header.Set("Origin", "http://disallowed.local")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preResp, err := http.DefaultClient.Do(preflight)
	if err != nil {
		t.Fatalf("OPTIONS /mcp error: %v", err)
	}
	defer preResp.Body.Close()
	if preResp.StatusCode != http.StatusForbidden {
		t.Fatalf("disallowed origin preflight status = %d, want 403", preResp.StatusCode)
	}
}
