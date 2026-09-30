package mcp

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/supervisor"
)

// failAfterInitTransport succeeds until armed, then fails every RoundTrip.
// It models a backend that accepted initialize but loses the next dispatch.
type failAfterInitTransport struct {
	inner http.RoundTripper
	fail  atomic.Bool
}

func (t *failAfterInitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.fail.Load() {
		return nil, errors.New("dispatch lost after accept")
	}
	return t.inner.RoundTrip(r)
}

func newManagedGatewayWithDaemonMetrics(t *testing.T, target string, dm *metrics.DaemonMetrics, transport http.RoundTripper) *ManagedHTTPGateway {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := NewManagedHTTPGateway(ManagedHTTPGatewayConfig{
		Target:        u,
		MaxSessions:   4,
		IdleTimeout:   30 * time.Minute,
		Transport:     transport,
		DaemonMetrics: dm,
	})
	if err != nil {
		t.Fatal(err)
	}
	return gateway
}

func newToolsCallRequest(sessionID string) *http.Request {
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	req.Header.Set("Mcp-Session-Id", sessionID)
	return req
}

func newToolsListRequest(sessionID string) *http.Request {
	body := `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	req.Header.Set("Mcp-Session-Id", sessionID)
	return req
}

// TestManagedGatewayToolCallIncrementsDaemonMetrics proves a forwarded
// tools/call counts once and a successful 2xx forwarding is not an error.
func TestManagedGatewayToolCallIncrementsDaemonMetrics(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Mcp-Session-Id") == "" {
			w.Header().Set("Mcp-Session-Id", "metrics-session")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"ok"}]}}`))
	}))
	defer backend.Close()

	dm := metrics.NewDaemonMetrics()
	gateway := newManagedGatewayWithDaemonMetrics(t, backend.URL+"/mcp", dm, backend.Client().Transport)
	sessionID := initializeManagedSession(t, gateway)

	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, newToolsCallRequest(sessionID))
	if resp.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d, body = %s", resp.Code, resp.Body.String())
	}

	snap := dm.Snapshot()
	if snap.ToolCallsTotal != 1 {
		t.Errorf("tool_calls_total = %d, want 1 after one forwarded tools/call", snap.ToolCallsTotal)
	}
	if snap.ErrorsTotal != 0 {
		t.Errorf("errors_total = %d, want 0 for a 2xx tools/call", snap.ErrorsTotal)
	}
}

// TestManagedGatewayToolErrorResultDoesNotCountError proves a 200 response
// carrying an application-level tool error (isError result) is not counted as
// a forwarding failure.
func TestManagedGatewayToolErrorResultDoesNotCountError(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Mcp-Session-Id") == "" {
			w.Header().Set("Mcp-Session-Id", "metrics-session")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"isError":true,"content":[{"type":"text","text":"tool failed"}]}}`))
	}))
	defer backend.Close()

	dm := metrics.NewDaemonMetrics()
	gateway := newManagedGatewayWithDaemonMetrics(t, backend.URL+"/mcp", dm, backend.Client().Transport)
	sessionID := initializeManagedSession(t, gateway)

	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, newToolsCallRequest(sessionID))
	if resp.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d, want 200", resp.Code)
	}

	snap := dm.Snapshot()
	if snap.ToolCallsTotal != 1 {
		t.Errorf("tool_calls_total = %d, want 1", snap.ToolCallsTotal)
	}
	if snap.ErrorsTotal != 0 {
		t.Errorf("errors_total = %d, want 0 for a 200 isError result", snap.ErrorsTotal)
	}
}

// TestManagedGatewayNon2xxToolCallCountsError proves a non-2xx tools/call
// response counts one forwarding failure.
func TestManagedGatewayNon2xxToolCallCountsError(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Mcp-Session-Id") == "" {
			w.Header().Set("Mcp-Session-Id", "metrics-session")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"overloaded"}}`))
	}))
	defer backend.Close()

	dm := metrics.NewDaemonMetrics()
	gateway := newManagedGatewayWithDaemonMetrics(t, backend.URL+"/mcp", dm, backend.Client().Transport)
	sessionID := initializeManagedSession(t, gateway)

	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, newToolsCallRequest(sessionID))
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("tools/call status = %d, want 503", resp.Code)
	}

	snap := dm.Snapshot()
	if snap.ToolCallsTotal != 1 {
		t.Errorf("tool_calls_total = %d, want 1", snap.ToolCallsTotal)
	}
	if snap.ErrorsTotal != 1 {
		t.Errorf("errors_total = %d, want 1 for one non-2xx tools/call response", snap.ErrorsTotal)
	}
}

// TestManagedGatewayProxyFailureCountsError proves a proxy-level dispatch
// failure on tools/call counts one forwarding failure.
func TestManagedGatewayProxyFailureCountsError(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Mcp-Session-Id", "metrics-session")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer backend.Close()

	transport := &failAfterInitTransport{inner: backend.Client().Transport}
	dm := metrics.NewDaemonMetrics()
	gateway := newManagedGatewayWithDaemonMetrics(t, backend.URL+"/mcp", dm, transport)
	sessionID := initializeManagedSession(t, gateway)
	transport.fail.Store(true)

	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, newToolsCallRequest(sessionID))
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("tools/call status = %d, want 502 on proxy failure", resp.Code)
	}

	snap := dm.Snapshot()
	if snap.ToolCallsTotal != 1 {
		t.Errorf("tool_calls_total = %d, want 1", snap.ToolCallsTotal)
	}
	if snap.ErrorsTotal != 1 {
		t.Errorf("errors_total = %d, want 1 for one proxy dispatch failure", snap.ErrorsTotal)
	}
}

// TestManagedBackendUnavailableCountsForwardingFailure proves a tools/call
// refused at backend admission counts one forwarding failure even though no
// reverse-proxy callback observed it.
func TestManagedBackendUnavailableCountsForwardingFailure(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "metrics-refused")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer backend.Close()
	dm := metrics.NewDaemonMetrics()
	gateway := newManagedGatewayWithDaemonMetrics(t, backend.URL+"/mcp", dm, backend.Client().Transport)
	coordinator := supervisor.NewBackendCoordinator()
	coordinator.MarkReady()
	gateway.backend = coordinator
	sessionID := initializeManagedSession(t, gateway)
	coordinator.MarkProcessLost()

	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, newToolsCallRequest(sessionID))
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("tools/call after process loss status = %d, want 503", resp.Code)
	}

	snap := dm.Snapshot()
	if snap.ToolCallsTotal != 1 {
		t.Errorf("tool_calls_total = %d, want 1 for the refused tools/call", snap.ToolCallsTotal)
	}
	if snap.ErrorsTotal != 1 {
		t.Errorf("errors_total = %d, want 1 for the backend admission refusal", snap.ErrorsTotal)
	}
}

// TestManagedGatewayNonToolMethodsDoNotCount proves only tools/call counts:
// initialize, tools/list, and DELETE leave tool_calls_total untouched.
func TestManagedGatewayNonToolMethodsDoNotCount(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Mcp-Session-Id") == "" {
			w.Header().Set("Mcp-Session-Id", "metrics-session")
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer backend.Close()

	dm := metrics.NewDaemonMetrics()
	gateway := newManagedGatewayWithDaemonMetrics(t, backend.URL+"/mcp", dm, backend.Client().Transport)
	sessionID := initializeManagedSession(t, gateway)

	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, newToolsListRequest(sessionID))
	if resp.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, want 200", resp.Code)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	deleteReq.Header.Set("Mcp-Session-Id", sessionID)
	delResp := httptest.NewRecorder()
	gateway.ServeHTTP(delResp, deleteReq)

	snap := dm.Snapshot()
	if snap.ToolCallsTotal != 0 {
		t.Errorf("tool_calls_total = %d, want 0 for initialize + tools/list + DELETE", snap.ToolCallsTotal)
	}
	if snap.ErrorsTotal != 0 {
		t.Errorf("errors_total = %d, want 0", snap.ErrorsTotal)
	}
}
