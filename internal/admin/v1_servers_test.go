package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/supervisor"
)

// newTestRegistry constructs a bare registry for handler tests.
// The supervisor is nil because we do not exercise lifecycle paths.
func newTestRegistry() *server.Registry {
	return server.NewRegistry(nil, nil)
}

// TestHandleV1Servers_ReturnsListJSON verifies that GET /v1/servers returns
// a JSON object with a "servers" array containing the per-server status
// snapshots from the registry.
func TestHandleV1Servers_ReturnsListJSON(t *testing.T) {
	reg := newTestRegistry()
	s := &Server{registry: reg, running: true}

	req := httptest.NewRequest(http.MethodGet, "/v1/servers", nil)
	rr := httptest.NewRecorder()
	s.handleV1Servers(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rr.Body.String())
	}
	if body.Servers == nil {
		t.Fatalf("servers field missing or null; body=%s", rr.Body.String())
	}
}

// TestHandleV1ServerDetail_ReturnsSingleServer verifies that
// GET /v1/servers/{name} returns detailed status for one server.
func TestHandleV1ServerDetail_ReturnsSingleServer(t *testing.T) {
	reg := newTestRegistry()
	s := &Server{registry: reg, running: true}

	req := httptest.NewRequest(http.MethodGet, "/v1/servers/nonexistent", nil)
	req.SetPathValue("name", "nonexistent")
	rr := httptest.NewRecorder()
	s.handleV1ServerDetail(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown server should 404, got %d; body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandleV1Servers_NotRunningReturns503 verifies graceful response when
// the admin server is not running.
func TestHandleV1Servers_NotRunningReturns503(t *testing.T) {
	s := &Server{running: false}
	req := httptest.NewRequest(http.MethodGet, "/v1/servers", nil)
	rr := httptest.NewRecorder()
	s.handleV1Servers(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

// TestHandleV1Servers_EndpointWired confirms the mux registers /v1/servers
// when the admin server routes are built.
func TestHandleV1Servers_EndpointWired(t *testing.T) {
	reg := newTestRegistry()
	s := &Server{registry: reg, running: true}

	mux := http.NewServeMux()
	s.registerV1Routes(mux)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/servers")
	if err != nil {
		t.Fatalf("GET /v1/servers: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, b)
	}

	resp2, err := http.Get(ts.URL + "/v1/servers/missing")
	if err != nil {
		t.Fatalf("GET /v1/servers/missing: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("detail of missing server: status = %d, want 404", resp2.StatusCode)
	}
}

// testMetricsAccessor is a stub ServerMetricsAccessor for tests.
type testMetricsAccessor struct {
	snapshots map[string]*metrics.ServerMetricsSnapshot
}

func (a *testMetricsAccessor) ServerMetricsSnapshot(name string) *metrics.ServerMetricsSnapshot {
	return a.snapshots[name]
}

// TestHandleV1Servers_WithMetrics verifies that per-server session metrics
// are included in the /v1/servers response when ServerMetricsAccessor is set.
func TestHandleV1Servers_WithMetrics(t *testing.T) {
	reg := newTestRegistry()
	if err := reg.Add("test-server", &config.ServerConfig{Port: 18080}); err != nil {
		t.Fatalf("add test server: %v", err)
	}
	accessor := &testMetricsAccessor{
		snapshots: map[string]*metrics.ServerMetricsSnapshot{
			"test-server": {
				ActiveSessions:  3,
				AdmissionDenied: 1,
				ReapedByReason:  map[string]int64{"idle_timeout": 2},
			},
		},
	}
	s := &Server{registry: reg, running: true, serverMetricsAccessor: accessor}

	req := httptest.NewRequest(http.MethodGet, "/v1/servers", nil)
	rr := httptest.NewRecorder()
	s.handleV1Servers(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var body struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rr.Body.String())
	}

	if len(body.Servers) != 1 {
		t.Fatalf("servers length = %d, want 1; body=%s", len(body.Servers), rr.Body.String())
	}
	metricValue, ok := body.Servers[0]["session_metrics"].(map[string]any)
	if !ok {
		t.Fatalf("session_metrics missing or wrong type: %#v", body.Servers[0]["session_metrics"])
	}
	if got := metricValue["active_sessions"]; got != float64(3) {
		t.Fatalf("active_sessions = %v, want 3", got)
	}
	reaped, ok := metricValue["reaped_by_reason"].(map[string]any)
	if !ok {
		t.Fatalf("reaped_by_reason missing or wrong type: %#v", metricValue["reaped_by_reason"])
	}
	if got := reaped["idle_timeout"]; got != float64(2) {
		t.Fatalf("idle_timeout count = %v, want 2", got)
	}

	detailReq := httptest.NewRequest(http.MethodGet, "/v1/servers/test-server", nil)
	detailReq.SetPathValue("name", "test-server")
	detailRR := httptest.NewRecorder()
	s.handleV1ServerDetail(detailRR, detailReq)
	if detailRR.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200; body=%s", detailRR.Code, detailRR.Body.String())
	}
	var detail map[string]any
	if err := json.Unmarshal(detailRR.Body.Bytes(), &detail); err != nil {
		t.Fatalf("detail unmarshal: %v; body=%s", err, detailRR.Body.String())
	}
	if _, ok := detail["session_metrics"].(map[string]any); !ok {
		t.Fatalf("detail session_metrics missing or wrong type: %#v", detail["session_metrics"])
	}
}

// TestServerMetricsAccessor_NilAccessor verifies that nil accessor doesn't panic.
func TestServerMetricsAccessor_NilAccessor(t *testing.T) {
	reg := newTestRegistry()
	s := &Server{registry: reg, running: true, serverMetricsAccessor: nil}

	req := httptest.NewRequest(http.MethodGet, "/v1/servers", nil)
	rr := httptest.NewRecorder()

	// Should not panic
	s.handleV1Servers(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// TestToolList_WithMetrics verifies that vision_list includes session_metrics
// when ServerMetricsAccessor is wired.
func TestToolList_WithMetrics(t *testing.T) {
	reg := newTestRegistry()
	if err := reg.Add("test-srv", &config.ServerConfig{Port: 18081}); err != nil {
		t.Fatalf("add test server: %v", err)
	}
	accessor := &testMetricsAccessor{
		snapshots: map[string]*metrics.ServerMetricsSnapshot{
			"test-srv": {
				ActiveSessions:  5,
				AdmissionDenied: 0,
				ReapedByReason:  map[string]int64{"client_disconnected": 1},
			},
		},
	}
	s := &Server{registry: reg, running: true, serverMetricsAccessor: accessor}

	result, err := s.toolList(t.Context(), nil)
	if err != nil {
		t.Fatalf("toolList error: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatal("expected content in result")
	}

	// Parse the JSON
	var response ListResponse
	if err := json.Unmarshal([]byte(result.Content[0].Text), &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(response.Servers) != 1 {
		t.Fatalf("servers length = %d, want 1", len(response.Servers))
	}
	if response.Servers[0].SessionMetrics == nil {
		t.Fatal("session metrics should be populated")
	}
	if got := response.Servers[0].SessionMetrics.ActiveSessions; got != 5 {
		t.Fatalf("active sessions = %d, want 5", got)
	}
	if got := response.Servers[0].SessionMetrics.ReapedByReason["client_disconnected"]; got != 1 {
		t.Fatalf("client_disconnected reap count = %d, want 1", got)
	}
}

// TestHandleV1Servers_Timing_NilProcess verifies that a server with no managed
// Process (the stdio/lazy case) exposes registered_seconds but leaves
// uptime_seconds as JSON null.
func TestHandleV1Servers_Timing_NilProcess(t *testing.T) {
	reg := newTestRegistry()
	if err := reg.Add("stdio-server", &config.ServerConfig{Port: 18080, Command: "echo"}); err != nil {
		t.Fatalf("add test server: %v", err)
	}
	beforeAdd := time.Now()
	s := &Server{registry: reg, running: true}

	req := httptest.NewRequest(http.MethodGet, "/v1/servers", nil)
	rr := httptest.NewRecorder()
	s.handleV1Servers(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var list struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("list unmarshal: %v; body=%s", err, rr.Body.String())
	}
	if len(list.Servers) != 1 {
		t.Fatalf("servers length = %d, want 1; body=%s", len(list.Servers), rr.Body.String())
	}
	assertTiming(t, list.Servers[0], beforeAdd, true)

	detailReq := httptest.NewRequest(http.MethodGet, "/v1/servers/stdio-server", nil)
	detailReq.SetPathValue("name", "stdio-server")
	detailRR := httptest.NewRecorder()
	s.handleV1ServerDetail(detailRR, detailReq)
	if detailRR.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200; body=%s", detailRR.Code, detailRR.Body.String())
	}
	var detail map[string]any
	if err := json.Unmarshal(detailRR.Body.Bytes(), &detail); err != nil {
		t.Fatalf("detail unmarshal: %v; body=%s", err, detailRR.Body.String())
	}
	assertTiming(t, detail, beforeAdd, true)
}

// TestHandleV1Servers_Timing_ManagedProcess verifies that a server whose
// registry entry holds a running supervised Process exposes both
// registered_seconds and an integer uptime_seconds taken from the process
// itself, not from the server's registration age.
func TestHandleV1Servers_Timing_ManagedProcess(t *testing.T) {
	reg := newTestRegistry()
	if err := reg.Add("managed-server", &config.ServerConfig{
		Port:      6287,
		Transport: config.TransportManagedHTTP,
		Command:   "sh",
		Args:      []string{"-c", "sleep 30"},
		URL:       "http://127.0.0.1:16287/mcp",
	}); err != nil {
		t.Fatalf("add test server: %v", err)
	}
	beforeAdd := time.Now()

	proc := supervisor.NewManagedProcess("managed-server", reg.Get("managed-server").Config,
		config.SupervisionConfig{ShutdownTimeout: config.Duration(time.Second)},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- proc.Serve(ctx) }()

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		state, _ := proc.LifecycleSnapshot()
		if state == supervisor.StateRunning {
			break
		}
		select {
		case <-proc.LifecycleEvents():
		case <-deadline.C:
			t.Fatalf("managed process did not reach running state; state=%s", state)
		}
	}

	// Attach the running supervised process to the registry entry.
	reg.Get("managed-server").Process = proc

	// Let the process clock advance so uptime is non-zero.
	time.Sleep(1100 * time.Millisecond)

	s := &Server{registry: reg, running: true}

	req := httptest.NewRequest(http.MethodGet, "/v1/servers", nil)
	rr := httptest.NewRecorder()
	s.handleV1Servers(rr, req)

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("managed process did not exit after cancellation")
	}

	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var list struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("list unmarshal: %v; body=%s", err, rr.Body.String())
	}
	if len(list.Servers) != 1 {
		t.Fatalf("servers length = %d, want 1; body=%s", len(list.Servers), rr.Body.String())
	}
	assertTiming(t, list.Servers[0], beforeAdd, false)
	if uptime, ok := list.Servers[0]["uptime_seconds"].(float64); !ok || uptime <= 0 {
		t.Fatalf("uptime_seconds should be a positive integer, got %v", list.Servers[0]["uptime_seconds"])
	}

	detailReq := httptest.NewRequest(http.MethodGet, "/v1/servers/managed-server", nil)
	detailReq.SetPathValue("name", "managed-server")
	detailRR := httptest.NewRecorder()
	s.handleV1ServerDetail(detailRR, detailReq)
	if detailRR.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200; body=%s", detailRR.Code, detailRR.Body.String())
	}
	var detail map[string]any
	if err := json.Unmarshal(detailRR.Body.Bytes(), &detail); err != nil {
		t.Fatalf("detail unmarshal: %v; body=%s", err, detailRR.Body.String())
	}
	assertTiming(t, detail, beforeAdd, false)
}

// assertTiming checks that registered_seconds is present and non-negative and
// that uptime_seconds is null when nilProcess is true, or present otherwise.
func assertTiming(t *testing.T, m map[string]any, since time.Time, nilProcess bool) {
	t.Helper()
	registered, ok := m["registered_seconds"].(float64)
	if !ok {
		t.Fatalf("registered_seconds missing or wrong type: %#v", m["registered_seconds"])
	}
	elapsed := int64(time.Since(since).Seconds())
	if registered < 0 || int64(registered) > elapsed+1 {
		t.Fatalf("registered_seconds = %v; want 0..%d", registered, elapsed+1)
	}
	if nilProcess {
		if m["uptime_seconds"] != nil {
			t.Fatalf("uptime_seconds should be null for nil process, got %v", m["uptime_seconds"])
		}
		return
	}
	if _, ok := m["uptime_seconds"].(float64); !ok {
		t.Fatalf("uptime_seconds should be a number for managed process, got %v", m["uptime_seconds"])
	}
}
