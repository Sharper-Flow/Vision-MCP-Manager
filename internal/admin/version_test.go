package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleVersion_ReturnsCapabilityContract verifies GET /version
// returns {version, build, api: {v1_servers: true, ...}} — the contract
// OCA doctor uses for feature detection.
func TestHandleVersion_ReturnsCapabilityContract(t *testing.T) {
	s := &Server{running: true}
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rr := httptest.NewRecorder()
	s.handleVersion(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body struct {
		Version string          `json:"version"`
		Build   string          `json:"build"`
		API     map[string]bool `json:"api"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rr.Body.String())
	}

	if body.Version == "" {
		t.Error("version field should be populated (at least \"dev\")")
	}
	if !body.API["v1_servers"] {
		t.Error("api.v1_servers = false, want true for V1-capable Vision")
	}
	if !body.API["v1_servers_detail"] {
		t.Error("api.v1_servers_detail = false, want true")
	}
	if !body.API["version"] {
		t.Error("api.version = false, want true (self-reported)")
	}
}

// TestHandleVersion_IsWiredToMux verifies /version is registered in the
// admin server's mux alongside /health and /v1/servers.
func TestHandleVersion_IsWiredToMux(t *testing.T) {
	reg := newTestRegistry()
	s := &Server{registry: reg, running: true}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", s.handleVersion)
	s.registerV1Routes(mux)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/version")
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
