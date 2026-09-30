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

// TestCallDownstreamToolCountsToolCallAndClassifiedFailure proves the stdio
// proxy path increments the daemon-wide tool_calls_total once per tools/call
// and errors_total once per classified forwarding failure. A shared-mode
// session with no downstream models the production forwarding failure.
func TestCallDownstreamToolCountsToolCallAndClassifiedFailure(t *testing.T) {
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
	if snap.ToolCallsTotal != 1 {
		t.Errorf("tool_calls_total = %d, want 1 (one tools/call forwarded attempt)", snap.ToolCallsTotal)
	}
	if snap.ErrorsTotal != 1 {
		t.Errorf("errors_total = %d, want 1 (one classified forwarding failure)", snap.ErrorsTotal)
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
