package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestProxyPreservesLoggingCapability proves the proxy still advertises the
// SDK's default logging capability. A non-nil ServerCapabilities value is
// cloned without SDK defaults, so the explicit Tools capability must carry
// Logging explicitly.
func TestProxyPreservesLoggingCapability(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := testLogger(t)
	mgr := session.NewManager("metrics-logging", testServerConfig(), logger)
	defer mgr.CloseAll()
	ts := httptest.NewServer(NewProxyHandler(ProxyConfig{
		ServerName:     "metrics-logging",
		SessionManager: mgr,
		Logger:         logger,
	}))
	defer ts.Close()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "metrics-client", Version: "1.0.0"}, nil)
	s, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("client.Connect failed: %v", err)
	}
	defer s.Close()
	if s.InitializeResult().Capabilities.Logging == nil {
		t.Fatal("proxy initialize omitted the logging capability that HasTools-only options advertised before the explicit Capabilities value")
	}
}

// TestSDKDefaultCapabilitiesPreserveLogging is the option-level control for
// the proxy capability test: with no explicit Capabilities the SDK advertises
// Logging by default. The proxy's explicit Capabilities value must preserve
// that contract.
func TestSDKDefaultCapabilitiesPreserveLogging(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "metrics-baseline", Version: "1.0.0"}, &sdkmcp.ServerOptions{})
	server.AddTool(&sdkmcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{}, nil
	})
	ts := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil))
	defer ts.Close()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "metrics-client", Version: "1.0.0"}, nil)
	s, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("client.Connect failed: %v", err)
	}
	defer s.Close()
	if s.InitializeResult().Capabilities.Logging == nil {
		t.Fatal("SDK default capabilities lost logging; the SDK default changed")
	}
}
