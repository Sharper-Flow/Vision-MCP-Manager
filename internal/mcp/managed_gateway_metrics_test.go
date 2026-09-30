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

// TestManagedGatewayMissingSessionHeaderToolCallStillCounts proves a
// JSON-RPC tools/call without Mcp-Session-Id counts once even though the
// gateway rejects it before any session lookup. Call accounting is
// independent of the session-header branch.
func TestManagedGatewayMissingSessionHeaderToolCallStillCounts(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer backend.Close()

	dm := metrics.NewDaemonMetrics()
	gateway := newManagedGatewayWithDaemonMetrics(t, backend.URL+"/mcp", dm, backend.Client().Transport)

	req := httptest.NewRequest(http.MethodPost, "/mcp",
		bytes.NewBufferString(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}`))
	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("missing-session tools/call status = %d, want 404", resp.Code)
	}

	snap := dm.Snapshot()
	if snap.ToolCallsTotal != 1 {
		t.Errorf("tool_calls_total = %d, want 1 for a tools/call missing Mcp-Session-Id", snap.ToolCallsTotal)
	}
	if snap.ErrorsTotal != 0 {
		t.Errorf("errors_total = %d, want 0 (a pre-session rejection is not a forwarding failure)", snap.ErrorsTotal)
	}
}

// TestManagedGatewayMissingSessionHeaderBatchCountsEachMember proves a batch
// without Mcp-Session-Id counts every tools/call member once before the
// rejection.
func TestManagedGatewayMissingSessionHeaderBatchCountsEachMember(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer backend.Close()

	dm := metrics.NewDaemonMetrics()
	gateway := newManagedGatewayWithDaemonMetrics(t, backend.URL+"/mcp", dm, backend.Client().Transport)

	body := `[{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}},` +
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{}}},` +
		`{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}]`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("missing-session batch status = %d, want 400", resp.Code)
	}
	if got := dm.Snapshot().ToolCallsTotal; got != 2 {
		t.Errorf("tool_calls_total = %d, want 2 (each batch tools/call member counted once)", got)
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

// TestManagedGatewayToolsCallNotificationDoesNotCount proves a JSON-RPC
// notification (no id) whose method is tools/call is not counted as a tool
// call: MCP requests carry an id and notifications must omit it.
func TestManagedGatewayToolsCallNotificationDoesNotCount(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer backend.Close()

	dm := metrics.NewDaemonMetrics()
	gateway := newManagedGatewayWithDaemonMetrics(t, backend.URL+"/mcp", dm, backend.Client().Transport)

	req := httptest.NewRequest(http.MethodPost, "/mcp",
		bytes.NewBufferString(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"echo","arguments":{}}}`))
	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("missing-session tools/call notification status = %d, want 404", resp.Code)
	}

	snap := dm.Snapshot()
	if snap.ToolCallsTotal != 0 {
		t.Errorf("tool_calls_total = %d, want 0 for a tools/call notification (no id)", snap.ToolCallsTotal)
	}
}

// TestManagedGatewayBatchCountsOnlyGenuineRequests proves batch member
// classification: only members that are genuine JSON-RPC tools/call requests
// count, and a notification member without id does not.
func TestManagedGatewayBatchCountsOnlyGenuineRequests(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer backend.Close()

	dm := metrics.NewDaemonMetrics()
	gateway := newManagedGatewayWithDaemonMetrics(t, backend.URL+"/mcp", dm, backend.Client().Transport)

	body := `[{"jsonrpc":"2.0","method":"tools/call","params":{"name":"echo","arguments":{}}},` +
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{}}},` +
		`{"jsonrpc":"2.0","id":8,"method":"tools/list","params":{}}]`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("missing-session batch status = %d, want 400", resp.Code)
	}

	if got := dm.Snapshot().ToolCallsTotal; got != 1 {
		t.Errorf("tool_calls_total = %d, want 1 (only the id-bearing tools/call member counts)", got)
	}
}

// TestManagedGatewayForwardedNotificationDoesNotCount proves a tools/call
// notification forwarded on an established session is not counted: the
// counting decision classifies the envelope, not the session branch.
func TestManagedGatewayForwardedNotificationDoesNotCount(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodPost, "/mcp",
		bytes.NewBufferString(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"echo","arguments":{}}}`))
	req.Header.Set("Mcp-Session-Id", sessionID)
	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("forwarded tools/call notification status = %d, body = %s, want 200", resp.Code, resp.Body.String())
	}

	snap := dm.Snapshot()
	if snap.ToolCallsTotal != 0 {
		t.Errorf("tool_calls_total = %d, want 0 for a forwarded tools/call notification", snap.ToolCallsTotal)
	}
	if snap.ErrorsTotal != 0 {
		t.Errorf("errors_total = %d, want 0 for a forwarded tools/call notification", snap.ErrorsTotal)
	}
}

// TestManagedGatewayEnvelopeClassificationCountsGenuineRequestsOnly proves
// tool counting classifies the MCP JSON-RPC envelope per the 2025-11-25 basic
// Messages schema: only a member carrying jsonrpc "2.0", a string-or-integer
// id, and the tools/call method counts. Notifications, responses, and
// malformed members (null, boolean, object, array, and fractional ids;
// missing or wrong jsonrpc version; non-string method) count zero.
func TestManagedGatewayEnvelopeClassificationCountsGenuineRequestsOnly(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{name: "string id", body: `{"jsonrpc":"2.0","id":"req-1","method":"tools/call","params":{"name":"echo","arguments":{}}}`, want: 1},
		{name: "integer id", body: `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{}}}`, want: 1},
		{name: "negative integer id", body: `{"jsonrpc":"2.0","id":-3,"method":"tools/call","params":{"name":"echo","arguments":{}}}`, want: 1},
		{name: "empty string id", body: `{"jsonrpc":"2.0","id":"","method":"tools/call","params":{"name":"echo","arguments":{}}}`, want: 1},
		{name: "zero-fraction number id", body: `{"jsonrpc":"2.0","id":2.0,"method":"tools/call","params":{"name":"echo","arguments":{}}}`, want: 1},
		{name: "integer id beyond float64 precision", body: `{"jsonrpc":"2.0","id":9007199254740993,"method":"tools/call","params":{"name":"echo","arguments":{}}}`, want: 1},
		{name: "null id", body: `{"jsonrpc":"2.0","id":null,"method":"tools/call"}`, want: 0},
		{name: "boolean id", body: `{"jsonrpc":"2.0","id":true,"method":"tools/call"}`, want: 0},
		{name: "object id", body: `{"jsonrpc":"2.0","id":{},"method":"tools/call"}`, want: 0},
		{name: "array id", body: `{"jsonrpc":"2.0","id":[],"method":"tools/call"}`, want: 0},
		{name: "fractional id", body: `{"jsonrpc":"2.0","id":1.5,"method":"tools/call"}`, want: 0},
		{name: "missing jsonrpc", body: `{"id":1,"method":"tools/call"}`, want: 0},
		{name: "jsonrpc 1.0", body: `{"jsonrpc":"1.0","id":1,"method":"tools/call"}`, want: 0},
		{name: "tools/call notification", body: `{"jsonrpc":"2.0","method":"tools/call"}`, want: 0},
		{name: "result response", body: `{"jsonrpc":"2.0","id":9,"result":{}}`, want: 0},
		{name: "error response", body: `{"jsonrpc":"2.0","id":9,"error":{"code":-32603,"message":"boom"}}`, want: 0},
		{name: "null-id response", body: `{"jsonrpc":"2.0","id":null,"result":{}}`, want: 0},
		{name: "non-string method", body: `{"jsonrpc":"2.0","id":1,"method":5}`, want: 0},
		{name: "boolean params", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":true}`, want: 0},
		{name: "number params", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":42}`, want: 0},
		{name: "string params", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"not structured"}`, want: 0},
		{name: "array params", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":[]}`, want: 0},
		{name: "null params", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":null}`, want: 0},
		{name: "scalar body", body: `42`, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := countManagedToolsCalls([]byte(tc.body))
			if err != nil {
				t.Fatalf("countManagedToolsCalls(%s) returned error %v", tc.body, err)
			}
			if int64(got) != tc.want {
				t.Errorf("countManagedToolsCalls(%s) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
}

// TestManagedGatewayMalformedEnvelopeRejectedWithoutCounting proves the
// gateway rejects a session-less POST whose member is not a genuine MCP
// request without counting a tool call, while a genuine request rejected on
// the same path still counts exactly once. Call accounting classifies the
// envelope independently of the session-header branch.
func TestManagedGatewayMalformedEnvelopeRejectedWithoutCounting(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer backend.Close()

	cases := []struct {
		name string
		body string
		want int64
	}{
		{name: "genuine request still counts once", body: `{"jsonrpc":"2.0","id":"rejected","method":"tools/call","params":{"name":"echo","arguments":{}}}`, want: 1},
		{name: "null id member", body: `{"jsonrpc":"2.0","id":null,"method":"tools/call"}`, want: 0},
		{name: "fractional id member", body: `{"jsonrpc":"2.0","id":1.5,"method":"tools/call"}`, want: 0},
		{name: "missing jsonrpc member", body: `{"id":1,"method":"tools/call"}`, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := metrics.NewDaemonMetrics()
			gateway := newManagedGatewayWithDaemonMetrics(t, backend.URL+"/mcp", dm, backend.Client().Transport)
			req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(tc.body))
			resp := httptest.NewRecorder()
			gateway.ServeHTTP(resp, req)
			if resp.Code == http.StatusOK {
				t.Fatalf("session-less POST status = 200, want a rejection status")
			}
			if got := dm.Snapshot().ToolCallsTotal; got != tc.want {
				t.Errorf("tool_calls_total = %d after rejecting %s, want %d", got, tc.body, tc.want)
			}
			if got := dm.Snapshot().ErrorsTotal; got != 0 {
				t.Errorf("errors_total = %d after a pre-session rejection, want 0", got)
			}
		})
	}
}

// TestManagedGatewayMixedBatchCountsOnlyGenuineRequestsMembers proves batch
// member classification derives from the envelope owner: a batch mixing
// genuine requests with a notification, a response, and malformed members
// counts exactly the genuine tools/call requests, one each.
func TestManagedGatewayMixedBatchCountsOnlyGenuineRequestsMembers(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer backend.Close()

	dm := metrics.NewDaemonMetrics()
	gateway := newManagedGatewayWithDaemonMetrics(t, backend.URL+"/mcp", dm, backend.Client().Transport)

	body := `[` +
		`{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"echo","arguments":{}}},` +
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"echo","arguments":{}}},` +
		`{"jsonrpc":"2.0","id":9,"result":{}},` +
		`{"jsonrpc":"2.0","id":null,"method":"tools/call"},` +
		`{"jsonrpc":"2.0","id":1.5,"method":"tools/call"},` +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}` +
		`]`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("missing-session mixed batch status = %d, want 400", resp.Code)
	}

	if got := dm.Snapshot().ToolCallsTotal; got != 2 {
		t.Errorf("tool_calls_total = %d, want 2 (the string- and integer-id tools/call members only)", got)
	}
	if got := dm.Snapshot().ErrorsTotal; got != 0 {
		t.Errorf("errors_total = %d, want 0 for a pre-session batch rejection", got)
	}
}

// TestManagedGatewayBatchCountsGenuineMemberBesideInvalidSibling proves call
// counting is independent of whole-body admission: a batch holding one genuine
// tools/call request beside an invalid member is rejected with 400, and the
// genuine member still counts. Counting used to depend on the whole-envelope
// activity classification, which the invalid member failed, so the rejected
// batch reported tool_calls_total 0.
func TestManagedGatewayBatchCountsGenuineMemberBesideInvalidSibling(t *testing.T) {
	dm := metrics.NewDaemonMetrics()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer backend.Close()
	gateway := newManagedGatewayWithDaemonMetrics(t, backend.URL+"/mcp", dm, backend.Client().Transport)
	body := `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}},42]`
	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("mixed batch status = %d, want 400", w.Code)
	}
	if got := dm.Snapshot().ToolCallsTotal; got != 1 {
		t.Errorf("genuine batch member omitted due to invalid sibling: tool_calls_total = %d, want 1", got)
	}
	if got := dm.Snapshot().ErrorsTotal; got != 0 {
		t.Errorf("errors_total = %d, want 0 for a client-side admission rejection", got)
	}
}

// TestManagedGatewayFractionalRequestIDsDoNotCount proves request-ID
// integrality is decided from the exact JSON number, not a float64
// conversion: 1.0000000000000001 and 9007199254740992.5 round to integers in
// float64, and 1e-999 underflows to zero, so all three used to count as
// genuine requests.
func TestManagedGatewayFractionalRequestIDsDoNotCount(t *testing.T) {
	for _, id := range []string{"1.0000000000000001", "9007199254740992.5", "1e-999"} {
		body := `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call"}`
		count, err := countManagedToolsCalls([]byte(body))
		if err != nil {
			t.Fatalf("countManagedToolsCalls(%s): %v", id, err)
		}
		if count != 0 {
			t.Errorf("fractional id %s counted as a genuine request: got %d, want 0", id, count)
		}
	}
	// Integral spellings outside ParseInt's fast path still count.
	for _, id := range []string{"1.0", "1e3", "-2E2", "9007199254740993"} {
		body := `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call"}`
		count, err := countManagedToolsCalls([]byte(body))
		if err != nil {
			t.Fatalf("countManagedToolsCalls(%s): %v", id, err)
		}
		if count != 1 {
			t.Errorf("integral id %s not counted as a genuine request: got %d, want 1", id, count)
		}
	}
}

// TestManagedGatewayInvalidParamsDoNotCountAsRequestEnvelope proves the
// missing-session rejection path counts only genuine MCP envelopes: the
// basic Messages schema types params as an optional object, so a member
// carrying boolean, number, string, array, or null params is malformed and
// never counts, while the same envelope with object params counts exactly
// once on the same rejection path.
func TestManagedGatewayInvalidParamsDoNotCountAsRequestEnvelope(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer backend.Close()

	for _, params := range []string{`true`, `42`, `"not structured"`, `[]`, `null`} {
		t.Run(params, func(t *testing.T) {
			dm := metrics.NewDaemonMetrics()
			gateway := newManagedGatewayWithDaemonMetrics(t, backend.URL+"/mcp", dm, backend.Client().Transport)
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + params + `}`
			w := httptest.NewRecorder()
			gateway.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body)))
			if w.Code == http.StatusOK {
				t.Fatalf("malformed envelope params=%s answered 200, want a rejection status", params)
			}
			if got := dm.Snapshot().ToolCallsTotal; got != 0 {
				t.Errorf("malformed envelope params=%s counted: tool_calls_total=%d, status=%d; want 0", params, got, w.Code)
			}
			if got := dm.Snapshot().ErrorsTotal; got != 0 {
				t.Errorf("errors_total = %d after a pre-session rejection, want 0", got)
			}
		})
	}

	dm := metrics.NewDaemonMetrics()
	gateway := newManagedGatewayWithDaemonMetrics(t, backend.URL+"/mcp", dm, backend.Client().Transport)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`
	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body)))
	if w.Code == http.StatusOK {
		t.Fatalf("genuine request answered 200, want a rejection status")
	}
	if got := dm.Snapshot().ToolCallsTotal; got != 1 {
		t.Errorf("genuine request counted %d on the missing-session path, want 1", got)
	}
}
