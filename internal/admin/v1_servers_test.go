package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
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
