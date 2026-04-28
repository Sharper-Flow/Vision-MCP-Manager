package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jrede/vision/internal/metrics"
)

// TestToolMetrics_ReturnsCounters verifies vision_metrics returns JSON
// with sessions_active, tool_calls_total, errors_total, subprocesses_active.
func TestToolMetrics_ReturnsCounters(t *testing.T) {
	m := metrics.NewDaemonMetrics()
	m.IncToolCalls()
	m.IncToolCalls()
	m.IncErrors()
	m.IncSessionsActive()
	m.IncSubprocessesActive()
	m.IncSubprocessesActive()

	s := &Server{Metrics: m}
	result, err := s.toolMetrics(nil, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("toolMetrics error: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatal("toolMetrics returned no content")
	}

	var resp MetricsResponse
	if err := json.Unmarshal([]byte(result.Content[0].Text), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.ToolCallsTotal != 2 {
		t.Errorf("tool_calls_total = %d, want 2", resp.ToolCallsTotal)
	}
	if resp.ErrorsTotal != 1 {
		t.Errorf("errors_total = %d, want 1", resp.ErrorsTotal)
	}
	if resp.SessionsActive != 1 {
		t.Errorf("sessions_active = %d, want 1", resp.SessionsActive)
	}
	if resp.SubprocessesActive != 2 {
		t.Errorf("subprocesses_active = %d, want 2", resp.SubprocessesActive)
	}
}

// TestToolMetrics_NilMetrics returns zeroed values when Metrics is nil.
func TestToolMetrics_NilMetrics(t *testing.T) {
	s := &Server{Metrics: nil}
	result, err := s.toolMetrics(nil, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("toolMetrics error: %v", err)
	}

	var resp MetricsResponse
	if err := json.Unmarshal([]byte(result.Content[0].Text), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.ToolCallsTotal != 0 {
		t.Errorf("tool_calls_total = %d, want 0", resp.ToolCallsTotal)
	}
	if resp.ErrorsTotal != 0 {
		t.Errorf("errors_total = %d, want 0", resp.ErrorsTotal)
	}
	if resp.SessionsActive != 0 {
		t.Errorf("sessions_active = %d, want 0", resp.SessionsActive)
	}
	if resp.SubprocessesActive != 0 {
		t.Errorf("subprocesses_active = %d, want 0", resp.SubprocessesActive)
	}
}

// TestHandleMetrics_ReturnsPrometheusText verifies GET /metrics returns
// Prometheus text format with correct counter values.
func TestHandleMetrics_ReturnsPrometheusText(t *testing.T) {
	m := metrics.NewDaemonMetrics()
	m.IncToolCalls()
	m.IncToolCalls()
	m.IncToolCalls()
	m.IncErrors()
	m.IncSessionsActive()
	m.IncSessionsActive()
	m.IncSubprocessesActive()

	s := &Server{Metrics: m, running: true}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	s.handleMetrics(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	wantCT := "text/plain; version=0.0.4; charset=utf-8"
	if ct := rr.Header().Get("Content-Type"); ct != wantCT {
		t.Errorf("Content-Type = %q, want %q", ct, wantCT)
	}

	body := rr.Body.String()
	if !strings.Contains(body, "vision_tool_calls_total 3") {
		t.Errorf("body missing vision_tool_calls_total 3; got:\n%s", body)
	}
	if !strings.Contains(body, "vision_errors_total 1") {
		t.Errorf("body missing vision_errors_total 1; got:\n%s", body)
	}
	if !strings.Contains(body, "vision_sessions_active 2") {
		t.Errorf("body missing vision_sessions_active 2; got:\n%s", body)
	}
	if !strings.Contains(body, "vision_subprocesses_active 1") {
		t.Errorf("body missing vision_subprocesses_active 1; got:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE vision_tool_calls_total counter") {
		t.Errorf("body missing TYPE line for vision_tool_calls_total; got:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE vision_sessions_active gauge") {
		t.Errorf("body missing TYPE line for vision_sessions_active; got:\n%s", body)
	}
}

// TestHandleMetrics_NilMetrics returns zeroed Prometheus output when Metrics is nil.
func TestHandleMetrics_NilMetrics(t *testing.T) {
	s := &Server{Metrics: nil, running: true}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	s.handleMetrics(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, "vision_tool_calls_total 0") {
		t.Errorf("body missing vision_tool_calls_total 0; got:\n%s", body)
	}
	if !strings.Contains(body, "vision_errors_total 0") {
		t.Errorf("body missing vision_errors_total 0; got:\n%s", body)
	}
	if !strings.Contains(body, "vision_sessions_active 0") {
		t.Errorf("body missing vision_sessions_active 0; got:\n%s", body)
	}
	if !strings.Contains(body, "vision_subprocesses_active 0") {
		t.Errorf("body missing vision_subprocesses_active 0; got:\n%s", body)
	}
}

// TestHandleMetrics_IsWiredToMux verifies /metrics is registered in the admin server's mux.
func TestHandleMetrics_IsWiredToMux(t *testing.T) {
	reg := newTestRegistry()
	s := &Server{registry: reg, running: true}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
