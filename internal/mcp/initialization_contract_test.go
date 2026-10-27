package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A received initialize response is the protocol boundary, not tools/list or
// the first successful call. Exercise the first application calls concurrently
// without warming the client with another RPC or retrying a refusal.
func TestSharedInitializationConcurrentFirstCalls(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	logger := testLogger(t)
	sm := session.NewSharedSessionManager("initialization", testServerConfig(), logger, 0, nil)
	defer sm.CloseAll()
	counters := metrics.NewDaemonMetrics()
	ts := httptest.NewServer(NewProxyHandler(ProxyConfig{
		ServerName: "initialization", SharedManager: sm, Logger: logger, DaemonMetrics: counters,
	}))
	defer ts.Close()
	client := sdk.NewClient(&sdk.Implementation{Name: "first-calls", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	const calls = 16
	start := make(chan struct{})
	errs := make(chan error, calls)
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := cs.CallTool(ctx, &sdk.CallToolParams{
				Name: "echo", Arguments: map[string]any{"message": fmt.Sprintf("first-%d", i)},
			})
			if err == nil && (result == nil || result.IsError || len(result.Content) == 0) {
				err = fmt.Errorf("call %d: missing successful content: %+v", i, result)
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if got := counters.Snapshot().ToolCallsTotal; got != calls {
		t.Errorf("dispatched calls = %d, want %d", got, calls)
	}
}

// Session-based Streamable HTTP must refuse headerless messages other than
// initialize before allocating a session, not merely before dispatching a tool.
func TestProxyInitializationRequiresSessionID(t *testing.T) {
	skipIfNoNode(t)
	for _, mode := range []string{"shared", "per-session", "selector"} {
		t.Run(mode, func(t *testing.T) {
			logger := testLogger(t)
			counters := metrics.NewDaemonMetrics()
			serverCounters := metrics.NewServerMetrics()
			sm := session.NewSharedSessionManager("headerless", testServerConfig(), logger, 0, serverCounters)
			defer sm.CloseAll()
			mgr := session.NewManager("headerless", testServerConfig(), logger)
			defer mgr.CloseAll()
			selector := &testSelector{mgr: mgr}
			cfg := ProxyConfig{ServerName: "headerless", Logger: logger, DaemonMetrics: counters, Metrics: serverCounters}
			switch mode {
			case "shared":
				cfg.SharedManager = sm
			case "per-session":
				cfg.SessionManager = mgr
			case "selector":
				cfg.Selector = selector
			}
			ts := httptest.NewServer(NewProxyHandler(cfg))
			defer ts.Close()
			for _, tc := range []struct{ name, body string }{
				{"tool-call", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"message":"must-not-dispatch"}}}`},
				{"tool-list", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`},
				{"initialized", `{"jsonrpc":"2.0","method":"notifications/initialized"}`},
				{"cancelled", `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`},
				{"ping", `{"jsonrpc":"2.0","id":3,"method":"ping"}`},
				{"response", `{"jsonrpc":"2.0","id":4,"result":{}}`},
				{"malformed", `{`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL, strings.NewReader(tc.body))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Accept", mcpAccept)
					resp, err := ts.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					body, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Fatal(err)
					}
					if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "Mcp-Session-Id") || !strings.Contains(string(body), "initialize") {
						t.Errorf("want HTTP 400 with session initialization guidance, got HTTP %d: %s", resp.StatusCode, body)
					}
					if got := resp.Header.Get("Mcp-Session-Id"); got != "" {
						t.Errorf("refusal assigned session ID %q", got)
					}
				})
			}
			if got := counters.Snapshot().ToolCallsTotal; got != 0 {
				t.Errorf("headerless messages dispatched %d tool calls", got)
			}
			if got := sm.SessionCount(); got != 0 || sm.RefCount() != 0 || sm.HasDownstream() {
				t.Errorf("headerless messages allocated shared state: sessions=%d refs=%d downstream=%v", got, sm.RefCount(), sm.HasDownstream())
			}
			for reason, count := range serverCounters.Snapshot().SpawnedByReason {
				if count != 0 {
					t.Errorf("headerless messages spawned %d processes for %s", count, reason)
				}
			}
			if got := mgr.SessionCount(); got != 0 {
				t.Errorf("headerless messages allocated %d per-session processes", got)
			}
			selector.mu.Lock()
			defer selector.mu.Unlock()
			if len(selector.selected) != 0 {
				t.Errorf("headerless messages selected %d slots", len(selector.selected))
			}
		})
	}
}

func TestSharedInitializationAfterExpiredSession(t *testing.T) {
	skipIfNoNode(t)
	logger := testLogger(t)
	sm := session.NewSharedSessionManager("expired", testServerConfig(), logger, 0, nil)
	defer sm.CloseAll()
	counters := metrics.NewDaemonMetrics()
	ts := httptest.NewServer(NewProxyHandler(ProxyConfig{
		ServerName: "expired", SharedManager: sm, Logger: logger, DaemonMetrics: counters,
	}))
	defer ts.Close()
	httpClient := ts.Client()
	httpClient.Timeout = 10 * time.Second
	sid := initializeRawSession(t, httpClient, ts.URL)
	sendSessionPost(t, httpClient, ts.URL, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	deleteReq, err := http.NewRequest(http.MethodDelete, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	deleteReq.Header.Set("Mcp-Session-Id", sid)
	resp, err := httpClient.Do(deleteReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", resp.StatusCode)
	}
	for _, tc := range []struct {
		name, sessionID string
		wantStatus      int
	}{
		{"expired", sid, http.StatusNotFound},
		{"missing", "", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(
				`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"message":"must-not-dispatch"}}}`,
			))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", mcpAccept)
			req.Header.Set("Mcp-Session-Id", tc.sessionID)
			resp, err := httpClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
	if got := sm.SessionCount(); got != 0 {
		t.Fatalf("refusals allocated %d shared leases", got)
	}
	if got := counters.Snapshot().ToolCallsTotal; got != 0 {
		t.Fatalf("refusals dispatched %d tool calls", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := sdk.NewClient(&sdk.Implementation{Name: "reinitialize", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	result, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "reinitialized"}})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || !strings.HasPrefix(extractTextContent(t, result), "echo: reinitialized pid=") {
		t.Fatalf("reinitialized call failed: %+v", result)
	}
	if got := counters.Snapshot().ToolCallsTotal; got != 1 {
		t.Errorf("dispatched calls = %d, want 1", got)
	}
}
