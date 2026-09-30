package mcp

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestCallDownstreamToolCountsClassifiedFailureOnly proves the stdio
// forwarding function counts only classified forwarding failures. Call volume
// is counted once at the tool handler entry, before cache/coalescing can
// serve a request without dispatching.
func TestCallDownstreamToolCountsClassifiedFailureOnly(t *testing.T) {
	dm := metrics.NewDaemonMetrics()
	ps := &proxySession{
		serverName:     "metrics-stdio",
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		shared:         true,
		requestTimeout: time.Second,
		retryConfig:    RetryConfig{MaxAttempts: 1},
		circuitBreaker: newCircuitBreaker(CircuitBreakerConfig{}, nil),
		daemonMetrics:  dm,
	}

	req := &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "echo"}}
	_, err := ps.callDownstreamTool(context.Background(), req, "echo")
	if err == nil {
		t.Fatal("expected a classified forwarding failure with no downstream session")
	}

	snap := dm.Snapshot()
	if snap.ToolCallsTotal != 0 {
		t.Errorf("tool_calls_total = %d, want 0 (the tool handler entry owns call counting)", snap.ToolCallsTotal)
	}
	if snap.ErrorsTotal != 1 {
		t.Errorf("errors_total = %d, want 1 (one classified forwarding failure)", snap.ErrorsTotal)
	}
}

// TestStdioProxyToolCallIncrementsDaemonMetrics proves the stdio proxy tool
// handler counts every upstream tools/call exactly once, even when forwarding
// fails before dispatch. An availability failure reaches the client as a tool
// result, but still counts one call and one forwarding error.
func TestStdioProxyToolCallIncrementsDaemonMetrics(t *testing.T) {
	dm := metrics.NewDaemonMetrics()
	ps := &proxySession{
		serverName:     "metrics-stdio",
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		shared:         true,
		requestTimeout: time.Second,
		retryConfig:    RetryConfig{MaxAttempts: 1},
		circuitBreaker: newCircuitBreaker(CircuitBreakerConfig{}, nil),
		daemonMetrics:  dm,
	}

	handler := makeProxyToolHandler(ps, "echo")
	req := &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "echo"}}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned a protocol error for an availability failure: %v", err)
	}
	if result == nil {
		t.Fatal("handler returned no availability result")
	}

	snap := dm.Snapshot()
	if snap.ToolCallsTotal != 1 {
		t.Errorf("tool_calls_total = %d, want 1 after one proxied tools/call", snap.ToolCallsTotal)
	}
	if snap.ErrorsTotal != 1 {
		t.Errorf("errors_total = %d, want 1 for the unavailable downstream forwarding failure", snap.ErrorsTotal)
	}
}

// TestCachedToolCallsCountEachRequest proves a cached shared read-only tool
// still counts one tools/call per upstream request. The increment sits at the
// tool handler entry, before the cache can serve a request without dispatch.
func TestCachedToolCallsCountEachRequest(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := testLogger(t)
	mgr := session.NewSharedSessionManager("metrics-cache", testServerConfig(), logger, time.Minute, nil)
	defer mgr.CloseAll()
	dm := metrics.NewDaemonMetrics()
	handler := NewProxyHandler(ProxyConfig{
		ServerName:            "metrics-cache",
		SharedManager:         mgr,
		Logger:                logger,
		SharedReadOnlyTools:   []string{"echo"},
		SharedResultCacheTTL:  time.Minute,
		SharedResultCacheSize: 8,
		DaemonMetrics:         dm,
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "metrics-client", Version: "1.0.0"}, nil)
	s, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("client.Connect failed: %v", err)
	}
	defer s.Close()
	for i := 0; i < 2; i++ {
		r, err := s.CallTool(ctx, &sdkmcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "same"}})
		if err != nil || r.IsError {
			t.Fatalf("call %d: result=%v err=%v", i, r, err)
		}
	}
	if got := dm.Snapshot().ToolCallsTotal; got != 2 {
		t.Fatalf("two upstream tools/call requests (second served from cache): tool_calls_total=%d, want 2", got)
	}
}

// TestProxyHandlerEndToEndCountsDaemonToolCalls proves a successful proxied
// tools/call increments tool_calls_total exactly once and leaves errors_total
// untouched.
func TestProxyHandlerEndToEndCountsDaemonToolCalls(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := testLogger(t)
	cfg := testServerConfig()

	mgr := session.NewManager("test-proxy-metrics", cfg, logger)
	defer mgr.CloseAll()

	dm := metrics.NewDaemonMetrics()
	handler := NewProxyHandler(ProxyConfig{
		ServerName:     "test-proxy-metrics",
		SessionManager: mgr,
		Logger:         logger,
		DaemonMetrics:  dm,
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("client.Connect failed: %v", err)
	}
	defer clientSession.Close()

	callResult, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "count-me"},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if callResult.IsError {
		t.Fatalf("echo tool returned IsError: %v", callResult.Content)
	}

	snap := dm.Snapshot()
	if snap.ToolCallsTotal != 1 {
		t.Errorf("tool_calls_total = %d, want 1 after one proxied tools/call", snap.ToolCallsTotal)
	}
	if snap.ErrorsTotal != 0 {
		t.Errorf("errors_total = %d, want 0 after a successful proxied tools/call", snap.ErrorsTotal)
	}
}
