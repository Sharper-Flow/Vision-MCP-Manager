package mcp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
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

// discoveryFailureMCPServerJS answers initialize but fails every tools/list,
// so proxy session creation dies during initial tool discovery.
const discoveryFailureMCPServerJS = `
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin, terminal: false });
rl.on('line', (line) => {
  try {
    const msg = JSON.parse(line);
    if (!('id' in msg)) return;
    if (msg.method === 'initialize') {
      process.stdout.write(JSON.stringify({
        jsonrpc: '2.0', id: msg.id,
        result: {
          protocolVersion: '2025-03-26',
          capabilities: { tools: {} },
          serverInfo: { name: 'discovery-failure', version: '1.0.0' }
        }
      }) + '\n');
    } else {
      process.stdout.write(JSON.stringify({
        jsonrpc: '2.0', id: msg.id,
        error: { code: -32603, message: 'discovery failed' }
      }) + '\n');
    }
  } catch (e) {
    process.stderr.write('Error: ' + e.message + '\n');
  }
});
`

// TestFailedInitialToolsListLeavesSessionsActiveAtZero proves a failed initial
// tools/list in stateful and shared mode closes the half-built proxy session
// without releasing an active-session credit it never acquired. The per-server
// counter — and the derived sessions_active gauge — must stay at zero.
func TestFailedInitialToolsListLeavesSessionsActiveAtZero(t *testing.T) {
	skipIfNoNode(t)
	cfg := testServerConfigWithScript(discoveryFailureMCPServerJS)
	cfg.ApplyDefaults()
	logger := testLogger(t)

	for _, shared := range []bool{false, true} {
		name := "stateful"
		if shared {
			name = "shared"
		}
		t.Run(name, func(t *testing.T) {
			owner := metrics.NewServerMetrics()
			dm := metrics.NewDaemonMetrics()
			dm.SetGaugeProviders(func() int64 { return owner.Snapshot().ActiveSessions }, nil)
			pc := ProxyConfig{ServerName: name, Logger: logger, Metrics: owner, DaemonMetrics: dm}
			if shared {
				mgr := session.NewSharedSessionManager(name, cfg, logger, time.Minute, owner)
				defer mgr.CloseAll()
				pc.SharedManager = mgr
			} else {
				mgr := session.NewManager(name, cfg, logger)
				defer mgr.CloseAll()
				pc.SessionManager = mgr
			}
			h := NewProxyHandler(pc)
			r := httptest.NewRequest(http.MethodPost, "/mcp",
				strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"discovery-failure","version":"1"}}}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Accept", "application/json, text/event-stream")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code < 400 {
				t.Fatalf("expected failed initialize, got status %d body %s", w.Code, w.Body.String())
			}
			if got := dm.Snapshot().SessionsActive; got != 0 {
				t.Errorf("failed initialize: sessions_active = %d, want 0 (no credit was ever acquired)", got)
			}
		})
	}
}

// TestStatefulReapRespawnDeleteBalancesSessionGauge proves the stateful owner
// lifecycle stays balanced across a reap, a respawn that continues the same
// initialized upstream session, and the client DELETE that finally ends it:
// 1 after respawn, then 0 after delete, never negative.
func TestStatefulReapRespawnDeleteBalancesSessionGauge(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := testLogger(t)
	mgr := session.NewManager("metrics-reap-respawn", testServerConfig(), logger)
	defer mgr.CloseAll()
	owner := metrics.NewServerMetrics()
	dm := metrics.NewDaemonMetrics()
	dm.SetGaugeProviders(func() int64 { return owner.Snapshot().ActiveSessions }, nil)
	ts := httptest.NewServer(NewProxyHandler(ProxyConfig{
		ServerName:     "metrics-reap-respawn",
		SessionManager: mgr,
		Logger:         logger,
		Metrics:        owner,
		DaemonMetrics:  dm,
	}))
	defer ts.Close()

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "metrics-client", Version: "1.0.0"}, nil)
	s, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("client.Connect failed: %v", err)
	}
	defer s.Close()

	if _, err := s.CallTool(ctx, &sdkmcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "before"}}); err != nil {
		t.Fatalf("initial CallTool failed: %v", err)
	}
	if got := dm.Snapshot().SessionsActive; got != 1 {
		t.Fatalf("after initialize: sessions_active = %d, want 1", got)
	}

	// Reap the downstream behind the live upstream session.
	ids := mgr.Sessions()
	if len(ids) != 1 {
		t.Fatalf("manager sessions = %v, want exactly one", ids)
	}
	if err := mgr.RemoveSession(ids[0]); err != nil {
		t.Fatalf("RemoveSession (reap): %v", err)
	}

	// The next tool call respawns the downstream for the same upstream session.
	result, err := s.CallTool(ctx, &sdkmcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "respawn"}})
	if err != nil || result.IsError {
		t.Fatalf("respawn CallTool failed: result=%v err=%v", result, err)
	}
	if mgr.SessionCount() != 1 {
		t.Fatalf("respawned manager session count = %d, want 1", mgr.SessionCount())
	}
	if got := dm.Snapshot().SessionsActive; got != 1 {
		t.Errorf("after respawn: sessions_active = %d, want 1 (the upstream session is still live)", got)
	}

	// The client DELETE ends the upstream session and releases the credit.
	if err := s.Close(); err != nil {
		t.Fatalf("client close (DELETE): %v", err)
	}
	if got := dm.Snapshot().SessionsActive; got != 0 {
		t.Errorf("after delete: sessions_active = %d, want 0 (balanced, never negative)", got)
	}
}

// TestRemovalDuringInitializationPublishStaysBalanced proves the
// initialization transition is coherent: a manager removal that lands inside
// the publish callback releases the credit the transition acquired, and a
// later close cannot strand it. Publishing before acquiring let a removal in
// that interval consume closeOnce against a creditless session and leave
// sessions_active stranded at 1.
func TestRemovalDuringInitializationPublishStaysBalanced(t *testing.T) {
	for _, shared := range []bool{false, true} {
		name := "stateful"
		if shared {
			name = "shared"
		}
		t.Run(name, func(t *testing.T) {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			cfg := &config.ServerConfig{Command: "true"}
			owner := metrics.NewServerMetrics()
			dm := metrics.NewDaemonMetrics()
			dm.SetGaugeProviders(func() int64 { return owner.Snapshot().ActiveSessions }, nil)

			ps := &proxySession{
				serverName:     "metrics-init-race",
				sessionID:      "proxy-metrics-init-race-1",
				logger:         logger,
				metrics:        owner,
				requestTimeout: time.Second,
				retryConfig:    RetryConfig{MaxAttempts: 1},
				circuitBreaker: newCircuitBreaker(CircuitBreakerConfig{}, nil),
				daemonMetrics:  dm,
			}
			var mgr *session.Manager
			if shared {
				sm := session.NewSharedSessionManager("metrics-init-race", cfg, logger, time.Minute, owner)
				defer sm.CloseAll()
				ps.shared = true
				ps.sharedMgr = sm
			} else {
				mgr = session.NewManager("metrics-init-race", cfg, logger)
				defer mgr.CloseAll()
				ps.mgr = mgr
				mgr.SetOnSessionRemoved(func(sessionID string) {
					ps.closeDownstream("session removed by manager")
				})
			}

			ps.mu.Lock()
			ps.upstreamSessionID = "upstream-init-race"
			ps.mu.Unlock()
			ps.publishInitialized(func(_ string, ps *proxySession) {
				// A removal fires while the publish callback runs, before
				// control returns to the initialization path. This is the
				// body the manager's removal callback runs for stateful
				// mode, and the expiry wiring's call for shared mode.
				ps.closeDownstream("session removed by manager")
			})
			// The stateful removal wiring stays production-shaped: the
			// manager's removal callback runs closeDownstream, a no-op
			// against the already-consumed closeOnce.
			if mgr != nil {
				_ = mgr.RemoveSession("downstream-init-race")
			}
			// A later close (client DELETE) must be a no-op against the
			// already-balanced credit.
			ps.closeDownstream("upstream delete")

			if got := dm.Snapshot().SessionsActive; got != 0 {
				t.Fatalf("sessions_active = %d after removal during publish and a later close, want 0", got)
			}
		})
	}
}

// isErrorEchoMCPServerJS answers tools/call with a successful isError result:
// a transport-level success carrying an application-level tool error.
const isErrorEchoMCPServerJS = `
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin, terminal: false });
rl.on('line', (line) => {
  try {
    const msg = JSON.parse(line);
    if (!('id' in msg)) return;
    let result = {};
    if (msg.method === 'initialize') {
      result = { protocolVersion: '2025-03-26', capabilities: { tools: {} }, serverInfo: { name: 'iserror-echo', version: '1.0.0' } };
    } else if (msg.method === 'tools/list') {
      result = { tools: [{ name: 'echo', inputSchema: { type: 'object' } }] };
    } else if (msg.method === 'tools/call') {
      result = { isError: true, content: [{ type: 'text', text: 'application error' }] };
    }
    process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: msg.id, result }) + '\n');
  } catch (e) {
    process.stderr.write('Error: ' + e.message + '\n');
  }
});
`

// TestStdioIsErrorResultDoesNotCountForwardingFailure proves a successful
// isError tool result forwards without counting a forwarding error: the
// downstream answered, so errors_total stays at zero.
func TestStdioIsErrorResultDoesNotCountForwardingFailure(t *testing.T) {
	skipIfNoNode(t)
	cfg := testServerConfigWithScript(isErrorEchoMCPServerJS)
	cfg.ApplyDefaults()
	logger := testLogger(t)
	mgr := session.NewManager("metrics-iserror", cfg, logger)
	defer mgr.CloseAll()
	dm := metrics.NewDaemonMetrics()
	ts := httptest.NewServer(NewProxyHandler(ProxyConfig{
		ServerName:     "metrics-iserror",
		SessionManager: mgr,
		Logger:         logger,
		DaemonMetrics:  dm,
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "metrics-client", Version: "1.0.0"}, nil)
	s, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("client.Connect failed: %v", err)
	}
	defer s.Close()

	r, err := s.CallTool(ctx, &sdkmcp.CallToolParams{Name: "echo", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("isError result must forward as a successful response, got transport error: %v", err)
	}
	if !r.IsError {
		t.Fatalf("expected the application tool error to surface as isError, got: %+v", r)
	}
	snap := dm.Snapshot()
	if snap.ToolCallsTotal != 1 || snap.ErrorsTotal != 0 {
		t.Fatalf("snapshot = %+v, want tool_calls_total=1 errors_total=0", snap)
	}
}

// pidDiscoveryFailureMCPServerJS returns the discovery-failure fixture with a
// startup hook that records the subprocess PID, so a test can verify the
// manager removal path actually terminated the half-built subprocess.
func pidDiscoveryFailureMCPServerJS(pidFile string) string {
	pidHook := "const fs = require('fs');\nfs.writeFileSync(" + strconv.Quote(pidFile) + ", String(process.pid));\n"
	return pidHook + discoveryFailureMCPServerJS
}

// TestFailedStatefulDiscoveryReleasesAdmissionAndSubprocess proves a failed
// initial tools/list in stateful mode returns the half-built session to its
// owner: the manager removal path closes the SDK session and subprocess and
// frees the admission slot. The next initialization must reach discovery
// instead of admission denial, and the session gauges must never move.
func TestFailedStatefulDiscoveryReleasesAdmissionAndSubprocess(t *testing.T) {
	skipIfNoNode(t)
	logger := testLogger(t)
	pidFile := filepath.Join(t.TempDir(), "subprocess.pid")
	cfg := testServerConfigWithScript(pidDiscoveryFailureMCPServerJS(pidFile))
	cfg.ApplyDefaults()
	cfg.MaxSessions = 1
	mgr := session.NewManager("metrics-discovery-release", cfg, logger)
	defer mgr.CloseAll()
	owner := metrics.NewServerMetrics()
	dm := metrics.NewDaemonMetrics()
	dm.SetGaugeProviders(func() int64 { return owner.Snapshot().ActiveSessions }, nil)
	handler := NewProxyHandler(ProxyConfig{
		ServerName:     "metrics-discovery-release",
		SessionManager: mgr,
		Logger:         logger,
		Metrics:        owner,
		DaemonMetrics:  dm,
	})
	initialize := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"discovery-release","version":"1"}}}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	assertSubprocessGone := func(pid int) {
		t.Helper()
		proc, err := os.FindProcess(pid)
		if err != nil {
			t.Fatalf("FindProcess(%d): %v", pid, err)
		}
		// Manager removal closes the SDK session synchronously, and the SDK
		// transport waits for the command; the bounded retry only absorbs the
		// kernel's reap of an already-exited child.
		deadline := time.Now().Add(2 * time.Second)
		for {
			if err := proc.Signal(syscall.Signal(0)); err != nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("subprocess %d is still alive after the failed initialization returned", pid)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}

	first := initialize()
	if first.Code < 400 {
		t.Fatalf("fixture did not fail discovery: status %d body %s", first.Code, first.Body.String())
	}
	if n := mgr.SessionCount(); n != 0 {
		t.Fatalf("manager kept %d sessions after failed discovery, want 0: a rejected initialization must not consume admission", n)
	}
	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("fixture did not record a subprocess PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatalf("bad PID record %q: %v", pidData, err)
	}
	assertSubprocessGone(pid)

	// The next initialization reaches discovery again instead of admission
	// denial, and the second failure balances the same way.
	second := initialize()
	if second.Code == http.StatusTooManyRequests {
		t.Fatalf("second initialize hit admission denial (429): the first failure retained its manager slot")
	}
	if second.Code != first.Code {
		t.Fatalf("second initialize status %d, want %d: discovery must run again", second.Code, first.Code)
	}
	if n := mgr.SessionCount(); n != 0 {
		t.Fatalf("manager kept %d sessions after the second failed discovery, want 0", n)
	}
	if got := dm.Snapshot().SessionsActive; got != 0 {
		t.Errorf("sessions_active = %d, want 0 across both failures", got)
	}
	if got := owner.Snapshot().ActiveSessions; got != 0 {
		t.Errorf("per-server active sessions = %d, want 0 across both failures", got)
	}
	if got := owner.Snapshot().ReapedByReason[metrics.ReapReasonToolsListFailed]; got != 2 {
		t.Errorf("reaped[tools_list_failed] = %d, want 2 (one per failed discovery)", got)
	}
}

// TestStatefulRemovalDuringPublicationReleasesCredit proves the whole
// initialization-to-manager-removal ordering end to end: a real manager
// removal that lands inside the publish callback — before the initialized
// handler finishes — closes the session and releases its credit through the
// removal path, and the client DELETE that follows cannot strand it.
func TestStatefulRemovalDuringPublicationReleasesCredit(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	logger := testLogger(t)
	owner := metrics.NewServerMetrics()
	mgr := session.NewManager("metrics-init-race", testServerConfig(), logger)
	defer mgr.CloseAll()

	var published atomic.Pointer[proxySession]
	mgr.SetOnSessionRemoved(func(id string) {
		if ps := published.Load(); ps != nil && ps.downstreamID() == id {
			ps.closeDownstream("session removed by manager")
		}
	})
	srv, err := newPerSessionServer(ctx, "metrics-init-race", mgr, logger,
		nil, nil, nil, 0, time.Second, RetryConfig{}, CircuitBreakerConfig{},
		owner, nil, nil,
		nil,
		func(_ string, ps *proxySession) {
			published.Store(ps)
			// A real manager removal fires while publication runs, before
			// control returns to the initialization path.
			if err := mgr.RemoveSession(ps.downstreamID()); err != nil {
				t.Errorf("removal during publication: %v", err)
			}
		}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)
	initialized := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
		if r.Header.Get("Vision-Test-Initialized") == "yes" {
			close(initialized)
		}
	}))
	defer ts.Close()

	post := func(body string, headers map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp
	}

	init := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"init-race","version":"1"}}}`, nil)
	if init.StatusCode >= 400 {
		t.Fatalf("initialize failed: status %d", init.StatusCode)
	}
	sid := init.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize returned no session id")
	}
	// The notification triggers the publish callback; the barrier request
	// below forces the SDK to finish processing it before the assertions.
	post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		map[string]string{"Mcp-Session-Id": sid, "Vision-Test-Initialized": "yes"})
	select {
	case <-initialized:
	case <-time.After(10 * time.Second):
		t.Fatal("initialized notification was not processed")
	}
	post(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, map[string]string{"Mcp-Session-Id": sid})

	ps := published.Load()
	if ps == nil {
		t.Fatal("initialization callback did not run")
	}
	if n := mgr.SessionCount(); n != 0 {
		t.Fatalf("manager holds %d sessions after the removal during publication, want 0", n)
	}
	// The client DELETE that follows must be a no-op against the balanced credit.
	ps.closeDownstream("upstream delete")
	if got := owner.Snapshot().ActiveSessions; got != 0 {
		t.Fatalf("active-session credit stranded at %d after removal during publication, want 0", got)
	}
}

// TestRemovalBeforeInitializationPublishDoesNotStrandSession proves the
// stateful lifecycle stays coherent when a manager removal lands between
// initialize and notifications/initialized: the removal reaches the proxy
// before upstream publication, late initialization does not credit the
// removed downstream generation, and the next tools/call respawns a live
// manager session — gauge 0 with no manager session before the call, 1 with
// one respawned manager session after the successful call, and 0 after the
// client DELETE.
func TestRemovalBeforeInitializationPublishDoesNotStrandSession(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := testLogger(t)
	mgr := session.NewManager("metrics-removal-before-init", testServerConfig(), logger)
	defer mgr.CloseAll()
	owner := metrics.NewServerMetrics()
	dm := metrics.NewDaemonMetrics()
	dm.SetGaugeProviders(func() int64 { return owner.Snapshot().ActiveSessions }, nil)
	ts := httptest.NewServer(NewProxyHandler(ProxyConfig{
		ServerName:     "metrics-removal-before-init",
		SessionManager: mgr,
		Logger:         logger,
		Metrics:        owner,
		DaemonMetrics:  dm,
	}))
	defer ts.Close()

	post := func(body, sessionID string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sessionID != "" {
			req.Header.Set("Mcp-Session-Id", sessionID)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp, string(payload)
	}

	initResp, initBody := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"removal-before-init","version":"1"}}}`, "")
	if initResp.StatusCode >= 400 {
		t.Fatalf("initialize failed: status %d body %s", initResp.StatusCode, initBody)
	}
	sid := initResp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize returned no Mcp-Session-Id")
	}

	// The manager removal lands after the downstream spawn but before the
	// upstream client finishes initialization.
	ids := mgr.Sessions()
	if len(ids) != 1 {
		t.Fatalf("manager sessions = %v after initialize, want exactly one", ids)
	}
	if err := mgr.RemoveSession(ids[0]); err != nil {
		t.Fatalf("RemoveSession before initialization publish: %v", err)
	}
	if n := mgr.SessionCount(); n != 0 {
		t.Fatalf("manager holds %d sessions after removal, want 0", n)
	}

	// Late initialization must not credit the removed downstream generation.
	if initNotifResp, _ := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, sid); initNotifResp.StatusCode >= 400 {
		t.Fatalf("notifications/initialized failed: status %d", initNotifResp.StatusCode)
	}
	if got := dm.Snapshot().SessionsActive; got != 0 {
		t.Fatalf("sessions_active = %d after a removal that landed before initialization, want 0 (no manager session exists)", got)
	}

	// tools/list still answers from the upstream-registered tools.
	listResp, body := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, sid)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list status = %d body %s, want 200", listResp.StatusCode, body)
	}

	// The next tools/call respawns a live manager session for the still
	// initialized upstream session and succeeds.
	callResp, callBody := post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"message":"after-removal"}}}`, sid)
	if callResp.StatusCode != http.StatusOK {
		t.Fatalf("tools/call status = %d body %s, want 200", callResp.StatusCode, callBody)
	}
	if strings.Contains(callBody, `"isError":true`) || strings.Contains(callBody, `"error"`) {
		t.Fatalf("tools/call failed instead of respawning the removed downstream: %s", callBody)
	}
	if n := mgr.SessionCount(); n != 1 {
		t.Fatalf("manager holds %d sessions after the respawned tools/call, want 1", n)
	}
	if got := dm.Snapshot().SessionsActive; got != 1 {
		t.Fatalf("sessions_active = %d after the respawned tools/call, want 1 (one manager session owns the initialized upstream session)", got)
	}

	// The client DELETE ends the upstream session and releases the credit.
	delReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	delReq.Header.Set("Mcp-Session-Id", sid)
	delResp, err := ts.Client().Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, delResp.Body)
	_ = delResp.Body.Close()
	if got := dm.Snapshot().SessionsActive; got != 0 {
		t.Errorf("sessions_active = %d after DELETE, want 0 (balanced, never negative)", got)
	}
	if n := mgr.SessionCount(); n != 0 {
		t.Errorf("manager holds %d sessions after DELETE, want 0", n)
	}
}

// TestPublishInitializedSkipsCreditForClosedDownstream proves late
// initialization does not acquire the active-session credit for a downstream
// generation a manager removal already closed. The credit belongs to a live
// downstream generation with an initialized upstream; the next tool call
// respawns a live generation and reacquires it there.
func TestPublishInitializedSkipsCreditForClosedDownstream(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	owner := metrics.NewServerMetrics()
	dm := metrics.NewDaemonMetrics()
	dm.SetGaugeProviders(func() int64 { return owner.Snapshot().ActiveSessions }, nil)
	mgr := session.NewManager("metrics-late-init", &config.ServerConfig{Command: "true"}, logger)
	defer mgr.CloseAll()
	ps := &proxySession{
		serverName:    "metrics-late-init",
		sessionID:     "proxy-metrics-late-init-1",
		logger:        logger,
		mgr:           mgr,
		metrics:       owner,
		daemonMetrics: dm,
	}
	ps.downstreamMu.Lock()
	ps.downstreamClosed = true
	ps.downstreamMu.Unlock()
	ps.mu.Lock()
	ps.upstreamSessionID = "upstream-late-init"
	ps.mu.Unlock()

	ps.publishInitialized(nil)

	if got := dm.Snapshot().SessionsActive; got != 0 {
		t.Fatalf("late initialization credited a removed downstream generation: sessions_active = %d, want 0", got)
	}
}

// respawnCreditHookReporter wires a deterministic hook into the session-credit
// acquisition so a test can make a manager removal land exactly inside the
// respawn's credit window, on a goroutine production actually uses for
// removals (the reaper and admin removals never run on the respawning
// goroutine).
type respawnCreditHookReporter struct {
	*metrics.ServerMetrics
	onAcquire func()
}

func (r *respawnCreditHookReporter) IncActiveSessions() {
	r.ServerMetrics.IncActiveSessions()
	if r.onAcquire != nil {
		r.onAcquire()
	}
}

// TestRespawnedGenerationRemovedDuringCreditWindowStaysBalanced proves the
// respawn owns its generation coherently: the removal-owner index carries the
// respawned session before the respawn can take credit for it, so a manager
// removal inside the credit window closes the new generation and releases the
// credit. Registering the owner only after the credit left a window where the
// removal missed the proxy and stranded sessions_active at 1 with a manager
// population of 0.
func TestRespawnedGenerationRemovedDuringCreditWindowStaysBalanced(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := testLogger(t)
	mgr := session.NewManager("metrics-credit-window", testServerConfig(), logger)
	defer mgr.CloseAll()
	owner := &respawnCreditHookReporter{ServerMetrics: metrics.NewServerMetrics()}
	dm := metrics.NewDaemonMetrics()
	dm.SetGaugeProviders(func() int64 { return owner.Snapshot().ActiveSessions }, nil)
	// The initial acquisition has no respawned generation, so the hook is a
	// no-op there. Inside the respawn's credit window it removes the
	// respawned session from a second goroutine — the goroutine production
	// actually uses for removals — and waits until the manager has dropped
	// it. The manager deletes the session from its map before its removal
	// callback runs, so the wait never waits on the callback, which blocks
	// on the credit window's own closeMu.
	owner.onAcquire = func() {
		var respawned []string
		for _, id := range mgr.Sessions() {
			if strings.Contains(id, "-respawn-") {
				respawned = append(respawned, id)
			}
		}
		if len(respawned) == 0 {
			return
		}
		go func() {
			for _, id := range respawned {
				if err := mgr.RemoveSession(id); err != nil {
					t.Errorf("RemoveSession(respawned): %v", err)
				}
			}
		}()
		deadline := time.Now().Add(10 * time.Second)
		for {
			gone := true
			for _, id := range mgr.Sessions() {
				if strings.Contains(id, "-respawn-") {
					gone = false
				}
			}
			if gone {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("manager never dropped the respawned session")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	ts := httptest.NewServer(NewProxyHandler(ProxyConfig{
		ServerName:     "metrics-credit-window",
		SessionManager: mgr,
		Logger:         logger,
		Metrics:        owner,
		DaemonMetrics:  dm,
	}))
	defer ts.Close()

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "metrics-client", Version: "1.0.0"}, nil)
	s, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("client.Connect failed: %v", err)
	}
	defer s.Close()
	if _, err := s.CallTool(ctx, &sdkmcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "initial"}}); err != nil {
		t.Fatalf("initial CallTool failed: %v", err)
	}
	ids := mgr.Sessions()
	if len(ids) != 1 {
		t.Fatalf("initial manager population = %v, want one session", ids)
	}
	if err := mgr.RemoveSession(ids[0]); err != nil {
		t.Fatalf("RemoveSession(initial): %v", err)
	}

	// The removal hook fires inside the respawn's credit window. The call
	// itself may succeed or fail; the invariant is the settled state.
	_, _ = s.CallTool(ctx, &sdkmcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "respawn"}})

	deadline := time.Now().Add(10 * time.Second)
	for dm.Snapshot().SessionsActive != 0 || mgr.SessionCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("settled unbalanced: sessions_active = %d, manager sessions = %d; want 0/0",
				dm.Snapshot().SessionsActive, mgr.SessionCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestFailedSharedDiscoveryReleasesRemovalOwner proves a shared-mode proxy
// session whose initial tools/list fails removes its spawn-time removal-owner
// registration. The index cleanup used to run only through the upstream-keyed
// onClosed callback, which never fires before upstream initialization, and
// SharedSessionManager.RemoveSession does not invoke the expiry callback, so
// repeated rejected initializations accumulated unreachable owners.
func TestFailedSharedDiscoveryReleasesRemovalOwner(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := testServerConfigWithScript(discoveryFailureMCPServerJS)
	cfg.ApplyDefaults()
	logger := testLogger(t)
	owner := metrics.NewServerMetrics()
	sm := session.NewSharedSessionManager("shared-owner-release", cfg, logger, time.Minute, owner)
	defer sm.CloseAll()
	byDownstream := make(map[string]*proxySession)
	byUpstream := make(map[string]*proxySession)
	_, err := newSharedModeServer(ctx, "shared-owner-release", sm, logger,
		nil, nil, nil, time.Second, RetryConfig{}, CircuitBreakerConfig{}, owner, nil, nil,
		func(ps *proxySession) { byDownstream[ps.downstreamID()] = ps },
		func(id string, ps *proxySession) { byUpstream[id] = ps; byDownstream[ps.downstreamID()] = ps },
		func(id string) {
			ps := byUpstream[id]
			delete(byUpstream, id)
			if ps != nil {
				delete(byDownstream, ps.downstreamID())
			}
		},
		func(sessionID string) { delete(byDownstream, sessionID) },
	)
	if err == nil {
		t.Fatal("fixture did not fail tools/list")
	}
	if got := len(byDownstream); got != 0 {
		t.Errorf("failed shared initialize retained %d removal owner(s), want 0", got)
	}
	if got := len(byUpstream); got != 0 {
		t.Errorf("failed shared initialize retained %d upstream registration(s), want 0", got)
	}
}
