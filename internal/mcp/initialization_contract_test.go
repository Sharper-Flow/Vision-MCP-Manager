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

// The pinned SDK produces the reported text for a headerless tools/call, before
// Vision dispatches a tool. This control must not turn that protocol violation
// into successful uninitialized execution or an automatic mutation replay.
func TestSharedInitializationHeaderlessCallIsPredispatchRefusal(t *testing.T) {
	skipIfNoNode(t)
	logger := testLogger(t)
	sm := session.NewSharedSessionManager("headerless", testServerConfig(), logger, 0, nil)
	defer sm.CloseAll()
	counters := metrics.NewDaemonMetrics()
	ts := httptest.NewServer(NewProxyHandler(ProxyConfig{
		ServerName: "headerless", SharedManager: sm, Logger: logger, DaemonMetrics: counters,
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL, strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"message":"must-not-dispatch"}}}`,
	))
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
	if !strings.Contains(string(body), "invalid during session initialization") {
		t.Fatalf("missing initialization refusal: HTTP %d: %s", resp.StatusCode, body)
	}
	if got := counters.Snapshot().ToolCallsTotal; got != 0 {
		t.Errorf("headerless request dispatched %d tool calls", got)
	}
}
