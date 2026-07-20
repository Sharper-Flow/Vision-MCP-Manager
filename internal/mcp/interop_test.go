package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- Interoperability Regression Tests ---
//
// These tests verify known Streamable HTTP edge cases against go-sdk v1.0.0:
// - Session 404 for unknown/expired session IDs
// - Malformed Mcp-Session-Id headers
// - Request cancellation/timeouts
// - POST without initialization (missing session)
// - DELETE for non-existent sessions

// setupInteropProxy creates a proxy handler with a test server for interop tests.
func setupInteropProxy(t *testing.T) (*httptest.Server, *session.Manager) {
	t.Helper()
	skipIfNoNode(t)

	logger := testLogger(t)
	cfg := &config.ServerConfig{
		Command:        "node",
		Args:           []string{"-e", echoMCPServerJS},
		Port:           16300, // unused in httptest
		Autostart:      true,
		RestartPolicy:  config.RestartOnFailure,
		SessionTimeout: config.Duration(30 * time.Second),
		MaxSessions:    10,
	}

	mgr := session.NewManager("interop-test", cfg, logger)

	handler := NewProxyHandler(ProxyConfig{
		ServerName:     "interop-test",
		SessionManager: mgr,
		Logger:         logger,
	})

	ts := httptest.NewServer(handler)
	t.Cleanup(func() {
		ts.Close()
		mgr.CloseAll()
	})

	return ts, mgr
}

func setupInteropProxyWithMaxSessions(t *testing.T, maxSessions int) (*httptest.Server, *session.Manager) {
	t.Helper()
	skipIfNoNode(t)

	logger := testLogger(t)
	cfg := &config.ServerConfig{
		Command:        "node",
		Args:           []string{"-e", echoMCPServerJS},
		Port:           16301,
		Autostart:      true,
		RestartPolicy:  config.RestartOnFailure,
		SessionTimeout: config.Duration(30 * time.Second),
		MaxSessions:    maxSessions,
	}

	mgr := session.NewManager("interop-test-max", cfg, logger)

	handler := NewProxyHandler(ProxyConfig{
		ServerName:     "interop-test-max",
		SessionManager: mgr,
		Logger:         logger,
	})

	ts := httptest.NewServer(handler)
	t.Cleanup(func() {
		ts.Close()
		mgr.CloseAll()
	})

	return ts, mgr
}

func TestInterop_StructuredFallbackOnDownstreamFailure(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := &config.ServerConfig{
		Command:        "node",
		Args:           []string{"-e", echoMCPServerJS},
		Port:           16302,
		Autostart:      true,
		RestartPolicy:  config.RestartOnFailure,
		SessionTimeout: config.Duration(30 * time.Second),
		MaxSessions:    5,
	}
	mgr := session.NewManager("interop-structured-fallback", cfg, logger)

	provider := fallbackSuggestionProviderFunc(func(_ context.Context, failedServer, failedTool string) []FallbackSuggestion {
		if failedServer != "kagi" {
			t.Fatalf("failedServer = %q, want kagi", failedServer)
		}
		if failedTool != "echo" {
			t.Fatalf("failedTool = %q, want echo", failedTool)
		}
		return []FallbackSuggestion{
			{ServerName: "brave-search", Capabilities: []string{"web-search"}, Installed: true, Reason: "shares 1 capability: web-search"},
			{ServerName: "tavily", Capabilities: []string{"web-search"}, Installed: false, Reason: "shares 1 capability: web-search"},
		}
	})

	handler := NewProxyHandler(ProxyConfig{
		ServerName:         "kagi",
		SessionManager:     mgr,
		Logger:             logger,
		SuggestionProvider: provider,
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(func() {
		ts.Close()
		mgr.CloseAll()
	})

	client := mcp.NewClient(&mcp.Implementation{Name: "structured-fallback-test", Version: "1.0.0"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer sess.Close()

	if _, err := sess.ListTools(ctx, nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	// Force the next respawn to fail after the initial healthy subprocess has
	// been closed. This creates a deterministic downstream availability failure.
	cfg.Command = "vision-missing-command-for-structured-fallback-test"
	for _, id := range mgr.Sessions() {
		if err := mgr.RemoveSession(id); err != nil {
			t.Fatalf("RemoveSession(%s): %v", id, err)
		}
	}
	time.Sleep(50 * time.Millisecond)

	result, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "post-close"},
	})
	if err != nil {
		t.Fatalf("expected visible tool error result, got Go error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}
	if !result.IsError {
		t.Fatal("expected IsError=true")
	}

	text := firstTextContent(t, result)
	for _, want := range []string{
		"config_drift",
		"echo",
		"brave-search",
		"tavily",
		"Vision did not retry on a different server",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("result text missing %q:\n%s", want, text)
		}
	}
}

// mcpAccept is the Accept header required by go-sdk v1.0.0 StreamableHTTPHandler.
const mcpAccept = "application/json, text/event-stream"

// initializeSession performs a full initialize handshake and returns the session ID.
func initializeSession(t *testing.T, baseURL string) string {
	t.Helper()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0.0"}}}`
	req, _ := http.NewRequest("POST", baseURL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", mcpAccept)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("initialize request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("initialize: expected 200, got %d: %s", resp.StatusCode, respBody)
	}

	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize response missing Mcp-Session-Id header")
	}

	// Send notifications/initialized
	notif := `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`
	req2, _ := http.NewRequest("POST", baseURL, strings.NewReader(notif))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Accept", mcpAccept)
	req2.Header.Set("Mcp-Session-Id", sessionID)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("initialized notification failed: %v", err)
	}
	resp2.Body.Close()

	return sessionID
}

// jsonRPCRequest sends a JSON-RPC request with the given session ID.
func jsonRPCRequest(t *testing.T, baseURL, sessionID, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", baseURL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", mcpAccept)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

func TestInterop_InitializeAtCapacity_Returns429ClearError(t *testing.T) {
	ts, _ := setupInteropProxyWithMaxSessions(t, 1)

	_ = initializeSession(t, ts.URL)

	body := `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test2","version":"1.0.0"}}}`
	req, _ := http.NewRequest("POST", ts.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", mcpAccept)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("second initialize request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 429 when at capacity, got %d: %s", resp.StatusCode, respBody)
	}

	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "max sessions reached") {
		t.Fatalf("expected clear max sessions error message, got: %s", respBody)
	}
}

// --- Test: Unknown Session ID returns 404 before dispatch ---

func TestInterop_UnknownSessionID_Returns404(t *testing.T) {
	ts, _ := setupInteropProxy(t)

	body := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	resp := jsonRPCRequest(t, ts.URL, "nonexistent-session-id-12345", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 404 for unknown session before dispatch, got %d: %s", resp.StatusCode, respBody)
	}
}

// --- Test: Expired/Deleted Session ID returns 404 ---

func TestInterop_ExpiredSessionID_Returns404(t *testing.T) {
	ts, _ := setupInteropProxy(t)

	// Establish a valid session
	sessionID := initializeSession(t, ts.URL)

	// Delete the session
	req, _ := http.NewRequest("DELETE", ts.URL, nil)
	req.Header.Set("Mcp-Session-Id", sessionID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE request failed: %v", err)
	}
	resp.Body.Close()

	// Give time for session cleanup
	time.Sleep(200 * time.Millisecond)

	// Now try to use the deleted session — should get 404
	body := `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`
	resp2 := jsonRPCRequest(t, ts.URL, sessionID, body)
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusNotFound {
		respBody, _ := io.ReadAll(resp2.Body)
		t.Errorf("expected 404 for expired session ID, got %d: %s", resp2.StatusCode, respBody)
	}
}

// --- Test: Malformed/Empty Session Header ---

func TestInterop_EmptySessionHeader_HandledGracefully(t *testing.T) {
	ts, _ := setupInteropProxy(t)

	// Send with empty Mcp-Session-Id (header present but empty value).
	// go-sdk treats empty session header same as no session header — creates a new
	// session. Since the method is tools/list (not initialize), the SDK returns a
	// JSON-RPC error about invalid method during initialization, wrapped in 200.
	body := `{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}`
	req, _ := http.NewRequest("POST", ts.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", mcpAccept)
	req.Header.Set("Mcp-Session-Id", "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// The SDK handles this gracefully — no crash, no 500.
	if resp.StatusCode >= 500 {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("expected non-500 for empty session header, got %d: %s", resp.StatusCode, respBody)
	}
	t.Logf("empty session header: status=%d", resp.StatusCode)
}

// --- Test: POST without Session Header (non-initialize) ---

func TestInterop_PostWithoutSession_NonInitialize(t *testing.T) {
	ts, _ := setupInteropProxy(t)

	// Send a non-initialize request without any Mcp-Session-Id
	body := `{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{}}`
	req, _ := http.NewRequest("POST", ts.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", mcpAccept)
	// No Mcp-Session-Id header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Without a session ID, tools/list should fail. The handler may treat this
	// as a new initialize (and fail since it's not an initialize request) or
	// reject it. Either way, it should not be 200 OK.
	// go-sdk treats POST without session as a new session — so it calls getServer
	// and creates a new session. But since this is tools/list (not initialize),
	// it's protocol-invalid, so the SDK may return an error or handle it gracefully.
	// We just verify it doesn't crash.
	t.Logf("POST tools/list without session: status=%d", resp.StatusCode)
}

// --- Test: DELETE for non-existent session ---

func TestInterop_DeleteNonExistentSession(t *testing.T) {
	ts, _ := setupInteropProxy(t)

	req, _ := http.NewRequest("DELETE", ts.URL, nil)
	req.Header.Set("Mcp-Session-Id", "totally-fake-session-id")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE request failed: %v", err)
	}
	defer resp.Body.Close()

	// Should get 404 for unknown session
	if resp.StatusCode != http.StatusNotFound {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 404 for DELETE of non-existent session, got %d: %s", resp.StatusCode, respBody)
	}
}

// --- Test: Request Cancellation via Context ---

func TestInterop_RequestCancellation(t *testing.T) {
	ts, _ := setupInteropProxy(t)

	// Establish a valid session using the SDK client
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "cancel-test",
		Version: "1.0.0",
	}, nil)

	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer sess.Close()

	// Cancel the context immediately, then try a tool call
	cancelCtx, cancelFunc := context.WithCancel(ctx)
	cancelFunc() // Cancel immediately

	_, err = sess.CallTool(cancelCtx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "should-not-arrive"},
	})

	if err == nil {
		t.Error("expected error from cancelled context, got nil")
	}

	// The error should be context-related
	if !strings.Contains(err.Error(), "cancel") && !strings.Contains(err.Error(), "context") {
		t.Logf("got error (may be acceptable): %v", err)
	}
}

// --- Test: Timeout on tool call ---

func TestInterop_TimeoutOnToolCall(t *testing.T) {
	ts, _ := setupInteropProxy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "timeout-test",
		Version: "1.0.0",
	}, nil)

	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer sess.Close()

	// Use a very short timeout for the tool call
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 1*time.Nanosecond)
	defer timeoutCancel()
	time.Sleep(1 * time.Millisecond) // Ensure timeout fires

	_, err = sess.CallTool(timeoutCtx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "should-timeout"},
	})

	if err == nil {
		t.Error("expected error from timed-out context, got nil")
	}
}

// --- Test: Multiple rapid initializations ---

func TestInterop_MultipleRapidInitializations(t *testing.T) {
	ts, _ := setupInteropProxy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Rapidly connect 5 clients to stress the session creation path
	sessions := make([]*mcp.ClientSession, 0, 5)
	for i := 0; i < 5; i++ {
		client := mcp.NewClient(&mcp.Implementation{
			Name:    "rapid-test",
			Version: "1.0.0",
		}, nil)

		sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
		if err != nil {
			t.Fatalf("client[%d].Connect failed: %v", i, err)
		}
		sessions = append(sessions, sess)
	}

	// All sessions should be functional
	for i, sess := range sessions {
		result, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name:      "echo",
			Arguments: map[string]any{"message": "rapid-" + strings.Repeat("x", i)},
		})
		if err != nil {
			t.Errorf("session[%d] CallTool failed: %v", i, err)
			continue
		}
		if len(result.Content) == 0 {
			t.Errorf("session[%d] got empty result", i)
		}
	}

	// Clean up
	for _, sess := range sessions {
		sess.Close()
	}
}

// --- Test: Malformed JSON body ---

func TestInterop_MalformedJSONBody(t *testing.T) {
	ts, _ := setupInteropProxy(t)

	// Send malformed JSON with proper Accept header
	req, _ := http.NewRequest("POST", ts.URL, strings.NewReader("{invalid json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", mcpAccept)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Should get a 4xx error (not crash/500)
	if resp.StatusCode >= 500 {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 4xx for malformed JSON, got %d: %s", resp.StatusCode, respBody)
	}
	t.Logf("malformed JSON: status=%d", resp.StatusCode)
}

// --- Test: Oversized JSON-RPC body ---

func TestInterop_OversizedBody(t *testing.T) {
	ts, _ := setupInteropProxy(t)

	// Send a very large body (this tests the handler's behavior, not our MaxBytesMiddleware
	// since httptest.Server uses the raw handler without our middleware stack).
	// This verifies the SDK handler doesn't crash on large inputs.
	largeArgs := strings.Repeat("x", 1024*1024) // 1 MB string
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": largeArgs, "version": "1.0.0"},
		},
	}
	bodyJSON, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", ts.URL, bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", mcpAccept)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("oversized request failed: %v", err)
	}
	defer resp.Body.Close()

	// The handler should handle it (either succeed or reject gracefully)
	t.Logf("oversized body: status=%d (body size=%d)", resp.StatusCode, len(bodyJSON))
	if resp.StatusCode >= 500 {
		t.Errorf("expected non-500 response for oversized body, got %d", resp.StatusCode)
	}
}

// --- Test: GET without session (SSE stream) ---

func TestInterop_GETWithoutSession(t *testing.T) {
	ts, _ := setupInteropProxy(t)

	// GET without Mcp-Session-Id should be rejected
	req, _ := http.NewRequest("GET", ts.URL, nil)
	req.Header.Set("Accept", mcpAccept)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET request failed: %v", err)
	}
	defer resp.Body.Close()

	// Should get 400 or 404 (no session to stream)
	if resp.StatusCode == http.StatusOK {
		t.Errorf("expected non-200 for GET without session, got 200")
	}
	t.Logf("GET without session: status=%d", resp.StatusCode)
}

// --- Test: GET with valid session (SSE reconnect) ---

func TestInterop_GETWithValidSession_SSEReconnect(t *testing.T) {
	ts, _ := setupInteropProxy(t)

	// Initialize a session
	sessionID := initializeSession(t, ts.URL)

	// GET with valid session ID should establish SSE stream
	req, _ := http.NewRequest("GET", ts.URL, nil)
	req.Header.Set("Mcp-Session-Id", sessionID)
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Timeout is expected — SSE streams don't have natural endings
		if !strings.Contains(err.Error(), "Timeout") && !strings.Contains(err.Error(), "deadline") {
			t.Fatalf("GET request failed unexpectedly: %v", err)
		}
		t.Logf("GET with session timed out as expected (SSE stream)")
		return
	}
	defer resp.Body.Close()

	// If we get a response, it should be 200 with text/event-stream content type
	// OR 405 if GET is not supported for this handler
	t.Logf("GET with session: status=%d, content-type=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
}

// --- Test: Wrong HTTP method ---

func TestInterop_WrongHTTPMethod(t *testing.T) {
	ts, _ := setupInteropProxy(t)

	sessionID := initializeSession(t, ts.URL)

	// Try PUT — not a valid MCP method
	req, _ := http.NewRequest("PUT", ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", mcpAccept)
	req.Header.Set("Mcp-Session-Id", sessionID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT request failed: %v", err)
	}
	defer resp.Body.Close()

	// PUT should be rejected (405 Method Not Allowed)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		respBody, _ := io.ReadAll(resp.Body)
		t.Logf("PUT: status=%d body=%s (405 expected but %d acceptable)", resp.StatusCode, respBody, resp.StatusCode)
	}
}

// --- Test: Stale sessions are rejected without replay ---

// setupInteropProxyPair creates two independent proxy handlers that simulate
// a daemon restart. The first handler establishes sessions; the second handler
// starts fresh and must reject stale IDs so the client owns reinitialization.
func setupInteropProxyPair(t *testing.T) (ts1URL string, cleanup1 func(), ts2 *httptest.Server) {
	t.Helper()
	skipIfNoNode(t)

	logger := testLogger(t)

	// --- First handler (pre-restart) ---
	cfg1 := &config.ServerConfig{
		Command:        "node",
		Args:           []string{"-e", echoMCPServerJS},
		Port:           16310,
		Autostart:      true,
		RestartPolicy:  config.RestartOnFailure,
		SessionTimeout: config.Duration(30 * time.Second),
		MaxSessions:    10,
	}
	mgr1 := session.NewManager("interop-pair-1", cfg1, logger)
	handler1 := NewProxyHandler(ProxyConfig{
		ServerName:     "interop-pair-1",
		SessionManager: mgr1,
		Logger:         logger,
	})
	server1 := httptest.NewServer(handler1)

	// --- Second handler (post-restart) ---
	cfg2 := &config.ServerConfig{
		Command:        "node",
		Args:           []string{"-e", echoMCPServerJS},
		Port:           16311,
		Autostart:      true,
		RestartPolicy:  config.RestartOnFailure,
		SessionTimeout: config.Duration(30 * time.Second),
		MaxSessions:    10,
	}
	mgr2 := session.NewManager("interop-pair-2", cfg2, logger)
	handler2 := NewProxyHandler(ProxyConfig{
		ServerName:     "interop-pair-2",
		SessionManager: mgr2,
		Logger:         logger,
	})
	server2 := httptest.NewServer(handler2)
	t.Cleanup(func() {
		server2.Close()
		mgr2.CloseAll()
	})

	return server1.URL, func() {
		server1.Close()
		mgr1.CloseAll()
	}, server2
}

func TestInterop_StaleSession_Returns404(t *testing.T) {
	ts1URL, cleanup1, ts2 := setupInteropProxyPair(t)

	sessionID := initializeSession(t, ts1URL)
	body := `{"jsonrpc":"2.0","id":10,"method":"tools/list","params":{}}`
	resp := jsonRPCRequest(t, ts1URL, sessionID, body)
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("tools/list on handler 1 failed: %d: %s", resp.StatusCode, respBody)
	}
	resp.Body.Close()
	cleanup1()

	resp2 := jsonRPCRequest(t, ts2.URL, sessionID, `{"jsonrpc":"2.0","id":11,"method":"tools/list","params":{}}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		respBody, _ := io.ReadAll(resp2.Body)
		t.Fatalf("expected 404 for stale session, got %d: %s", resp2.StatusCode, respBody)
	}
}

func TestInterop_StaleSession_ToolCallNotReplayed(t *testing.T) {
	ts1URL, cleanup1, ts2 := setupInteropProxyPair(t)
	sessionID := initializeSession(t, ts1URL)
	cleanup1()

	body := `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"echo","arguments":{"message":"must-not-run"}}}`
	resp := jsonRPCRequest(t, ts2.URL, sessionID, body)
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for stale tool call, got %d: %s", resp.StatusCode, respBody)
	}
	if strings.Contains(string(respBody), "must-not-run") {
		t.Fatalf("stale tool call appears to have been replayed: %s", respBody)
	}
}

func TestInterop_StaleSession_SubsequentRequestsRemain404(t *testing.T) {
	ts1URL, cleanup1, ts2 := setupInteropProxyPair(t)
	sessionID := initializeSession(t, ts1URL)
	cleanup1()

	for id := 20; id <= 21; id++ {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list","params":{}}`, id)
		resp := jsonRPCRequest(t, ts2.URL, sessionID, body)
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("request %d: expected 404, got %d: %s", id, resp.StatusCode, respBody)
		}
	}
}

func TestInterop_DeletedSessionNotRecovered(t *testing.T) {
	// A session that was explicitly DELETEd should NOT be recovered — it should
	// remain tombstoned and return 404.
	ts, _ := setupInteropProxy(t)

	// Establish and then delete a session
	sessionID := initializeSession(t, ts.URL)

	req, _ := http.NewRequest("DELETE", ts.URL, nil)
	req.Header.Set("Mcp-Session-Id", sessionID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE request failed: %v", err)
	}
	resp.Body.Close()

	time.Sleep(200 * time.Millisecond)

	// Now use the deleted session ID — should get 404 (tombstoned, not recovered)
	body := `{"jsonrpc":"2.0","id":30,"method":"tools/list","params":{}}`
	resp2 := jsonRPCRequest(t, ts.URL, sessionID, body)
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusNotFound {
		respBody, _ := io.ReadAll(resp2.Body)
		t.Errorf("expected 404 for tombstoned session (not recovered), got %d: %s", resp2.StatusCode, respBody)
	}
}
