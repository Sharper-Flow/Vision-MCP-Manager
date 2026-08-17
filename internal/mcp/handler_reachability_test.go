package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
)

func TestListenerProbeMiddlewareRespondsWithoutCallingNext(t *testing.T) {
	called := false
	handler := ListenerProbeMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	}))

	req := newListenerProbeRequest()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if called {
		t.Fatal("probe request reached wrapped handler")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected probe status 200, got %d", recorder.Code)
	}
	if got := recorder.Body.String(); got != ListenerProbeResponse {
		t.Fatalf("expected probe response %q, got %q", ListenerProbeResponse, got)
	}
}

func TestListenerProbeMiddlewareDoesNotConsumeRateLimitOrBypassNormalTraffic(t *testing.T) {
	called := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusAccepted)
	})
	handler := ListenerProbeMiddleware()(
		RateLimitMiddleware(1, time.Hour)(
			SecurityMiddleware(SecurityConfig{BearerToken: "secret"})(inner),
		),
	)

	for i := 0; i < 20; i++ {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, newListenerProbeRequest())
		if recorder.Code != http.StatusOK {
			t.Fatalf("probe %d: expected status 200, got %d", i, recorder.Code)
		}
	}

	normal := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0"}`))
	normal.Header.Set("Authorization", "Bearer secret")
	normalRecorder := httptest.NewRecorder()
	handler.ServeHTTP(normalRecorder, normal)

	if normalRecorder.Code != http.StatusAccepted {
		t.Fatalf("normal request was not allowed after probes, got %d", normalRecorder.Code)
	}
	if called != 1 {
		t.Fatalf("expected one normal request to reach wrapped handler, got %d", called)
	}
}

func TestListenerProbeMiddlewareDoesNotRequireBearerToken(t *testing.T) {
	called := false
	handler := ListenerProbeMiddleware()(
		SecurityMiddleware(SecurityConfig{BearerToken: "secret"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusAccepted)
		})),
	)

	probeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(probeRecorder, newListenerProbeRequest())
	if probeRecorder.Code != http.StatusOK {
		t.Fatalf("probe without token: expected status 200, got %d", probeRecorder.Code)
	}
	if called {
		t.Fatal("probe request reached wrapped handler")
	}

	normalRecorder := httptest.NewRecorder()
	handler.ServeHTTP(normalRecorder, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if normalRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("normal request without token: expected status 401, got %d", normalRecorder.Code)
	}
}

func TestListenerProbeMiddlewarePassesNormalMCPTrafficThrough(t *testing.T) {
	called := false
	handler := ListenerProbeMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "normal response")
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("normal request")))

	if !called {
		t.Fatal("normal MCP request did not reach wrapped handler")
	}
	if recorder.Code != http.StatusAccepted || recorder.Body.String() != "normal response" {
		t.Fatalf("normal MCP response changed: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestPortManagerBindFailureRecordsListenerProbe(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy test port: %v", err)
	}
	defer occupied.Close()

	records := make(chan slog.Record, 8)
	logger := slog.New(&recordingHandler{records: records})
	store := reachability.NewStore()
	pm := NewPortManager(logger)
	pm.SetReachabilityStore(store)
	defer pm.Close()

	if err := pm.AddStreamable("bind-failure", occupied.Addr().(*net.TCPAddr).Port, &noopHandler{}, nil); err != nil {
		t.Fatalf("add streamable: %v", err)
	}

	deadline := time.After(time.Second)
	for {
		select {
		case record := <-records:
			if record.Message != "streamable MCP listener error" {
				continue
			}
			value, ok := store.Get("bind-failure")
			if !ok {
				t.Fatal("bind failure did not create reachability evidence")
			}
			evidence, ok := value.Evidence[reachability.DepthListener]
			if !ok {
				t.Fatal("bind failure did not record listener-depth evidence")
			}
			if evidence.LastProbeOutcome != reachability.OutcomeFailure {
				t.Fatalf("expected failed listener probe, got %q", evidence.LastProbeOutcome)
			}
			if evidence.LastProbeError == "" {
				t.Fatal("expected bind failure error in listener probe evidence")
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for bind failure")
		}
	}
}

func TestPortManagerReachabilityStoreIsNilSafe(t *testing.T) {
	pm := NewPortManager(nil)
	pm.SetReachabilityStore(nil)
	if err := pm.AddStreamable("nil-store", 0, &noopHandler{}, nil); err != nil {
		t.Fatalf("add streamable with nil store: %v", err)
	}
	if err := pm.Remove("nil-store"); err != nil {
		t.Fatalf("remove streamable with nil store: %v", err)
	}
	if err := pm.Close(); err != nil {
		t.Fatalf("close port manager with nil store: %v", err)
	}
}

func TestPerServerHealthReportsReachableServerAsHealthy(t *testing.T) {
	store := reachability.NewStore()
	store.RecordProbe("reachable", reachability.ProbeResult{
		Depth:   reachability.DepthListener,
		Success: true,
	})

	pm := NewPortManager(nil)
	pm.SetReachabilityStore(store)
	defer pm.Close()
	if err := pm.AddStreamable("reachable", 0, &noopHandler{}, nil); err != nil {
		t.Fatalf("add streamable: %v", err)
	}

	status, response := servePerServerHealth(t, pm, "reachable")
	if status != http.StatusOK {
		t.Fatalf("healthy endpoint status = %d, want %d", status, http.StatusOK)
	}
	if response["server"] != "reachable" {
		t.Fatalf("server = %v, want reachable", response["server"])
	}
	if response["status"] != "ok" {
		t.Fatalf("status = %v, want ok", response["status"])
	}
}

func TestPerServerHealthReportsUnreachableServerWithReason(t *testing.T) {
	store := reachability.NewStore()
	for range reachability.FailureThreshold {
		store.RecordProbe("unreachable", reachability.ProbeResult{
			Depth: reachability.DepthListener,
			Error: "connection refused",
		})
	}

	pm := NewPortManager(nil)
	pm.SetReachabilityStore(store)
	defer pm.Close()
	if err := pm.AddStreamable("unreachable", 0, &noopHandler{}, nil); err != nil {
		t.Fatalf("add streamable: %v", err)
	}

	status, response := servePerServerHealth(t, pm, "unreachable")
	if status == http.StatusOK {
		t.Fatalf("unreachable endpoint returned HTTP 200: %#v", response)
	}
	if response["server"] != "unreachable" {
		t.Fatalf("server = %v, want unreachable", response["server"])
	}
	if response["status"] == "ok" {
		t.Fatalf("unreachable endpoint reported ok: %#v", response)
	}
	if reason, ok := response["reason"].(string); !ok || reason == "" {
		t.Fatalf("unreachable endpoint reason = %v, want non-empty string", response["reason"])
	}
}

func TestPerServerHealthDoesNotClaimHealthyWithoutProbeEvidence(t *testing.T) {
	pm := NewPortManager(nil)
	pm.SetReachabilityStore(reachability.NewStore())
	defer pm.Close()
	if err := pm.AddStreamable("unprobed", 0, &noopHandler{}, nil); err != nil {
		t.Fatalf("add streamable: %v", err)
	}

	status, response := servePerServerHealth(t, pm, "unprobed")
	if status == http.StatusOK {
		t.Fatalf("unprobed endpoint returned HTTP 200: %#v", response)
	}
	if response["server"] != "unprobed" {
		t.Fatalf("server = %v, want unprobed", response["server"])
	}
	if response["status"] == "ok" {
		t.Fatalf("unprobed endpoint reported ok: %#v", response)
	}
	if reason, ok := response["reason"].(string); !ok || reason == "" {
		t.Fatalf("unprobed endpoint reason = %v, want non-empty string", response["reason"])
	}
}

func TestPerServerHealthIsNilStoreSafe(t *testing.T) {
	pm := NewPortManager(nil)
	defer pm.Close()
	if err := pm.AddStreamable("nil-store-health", 0, &noopHandler{}, nil); err != nil {
		t.Fatalf("add streamable: %v", err)
	}

	status, response := servePerServerHealth(t, pm, "nil-store-health")
	if status == http.StatusOK {
		t.Fatalf("nil-store endpoint returned HTTP 200: %#v", response)
	}
	if response["server"] != "nil-store-health" {
		t.Fatalf("server = %v, want nil-store-health", response["server"])
	}
	if reason, ok := response["reason"].(string); !ok || reason == "" {
		t.Fatalf("nil-store endpoint reason = %v, want non-empty string", response["reason"])
	}
}

func servePerServerHealth(t *testing.T, pm *PortManager, name string) (int, map[string]interface{}) {
	t.Helper()
	listener := pm.Get(name)
	if listener == nil || listener.Server == nil || listener.Server.Handler == nil {
		t.Fatalf("missing listener handler for %q", name)
	}

	recorder := httptest.NewRecorder()
	listener.Server.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))

	var response map[string]interface{}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode health response: %v; body=%q", err, recorder.Body.String())
	}
	return recorder.Code, response
}

func newListenerProbeRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(ListenerProbeHeader, ListenerProbeHeaderValue)
	return req
}

type recordingHandler struct {
	records chan<- slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.records <- record
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h *recordingHandler) WithGroup(string) slog.Handler {
	return h
}
