package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/supervisor"
)

// testSetup creates a test environment with registry and handlers.
func testSetup(t *testing.T) (*server.Registry, *Server) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Create supervisor (won't actually run processes in tests)
	sup := supervisor.New(config.SupervisionConfig{}, logger)

	// Create registry
	reg := server.NewRegistry(sup, logger)

	// Create API server
	apiServer := NewServer(ServerConfig{
		Registry:       reg,
		Config:         &config.Config{},
		Logger:         logger,
		AllowedOrigins: []string{"*"},
	})

	return reg, apiServer
}

// --- Health Endpoint Tests ---

func TestHealthzHandler(t *testing.T) {
	_, apiServer := testSetup(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	apiServer.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", resp["status"])
	}
}

func TestReadyHandler(t *testing.T) {
	_, apiServer := testSetup(t)

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	w := httptest.NewRecorder()

	apiServer.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp["status"] != "ready" {
		t.Errorf("expected status 'ready', got %q", resp["status"])
	}
}

func TestHealthHandler(t *testing.T) {
	_, apiServer := testSetup(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	apiServer.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp HealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "healthy" {
		t.Errorf("expected status 'healthy', got %q", resp.Status)
	}

	if resp.Uptime == "" {
		t.Error("expected uptime to be set")
	}
}

// --- Server Management API Tests ---

func TestListServersHandler_Empty(t *testing.T) {
	_, apiServer := testSetup(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/servers", nil)
	w := httptest.NewRecorder()

	apiServer.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp APIResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp.Success {
		t.Error("expected success to be true")
	}

	servers, ok := resp.Data.([]interface{})
	if !ok {
		t.Fatalf("expected data to be array, got %T", resp.Data)
	}
	if len(servers) != 0 {
		t.Errorf("expected 0 servers, got %d", len(servers))
	}
}

func TestAddServerHandler(t *testing.T) {
	_, apiServer := testSetup(t)

	body := `{
		"name": "test-server",
		"config": {
			"port": 6276,
			"command": "/usr/bin/echo",
			"args": ["hello"]
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/servers", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	apiServer.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	var resp APIResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp.Success {
		t.Error("expected success to be true")
	}
}

func TestAddServerHandler_DuplicateName(t *testing.T) {
	reg, apiServer := testSetup(t)

	// Add a server directly
	cfg := &config.ServerConfig{
		Port:    6276,
		Command: "/usr/bin/echo",
	}
	if err := reg.Add("test-server", cfg); err != nil {
		t.Fatalf("failed to add server: %v", err)
	}

	// Try to add via API
	body := `{
		"name": "test-server",
		"config": {
			"port": 6277,
			"command": "/usr/bin/echo"
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/servers", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	apiServer.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected status %d, got %d", http.StatusConflict, w.Code)
	}
}

func TestAddServerHandler_InvalidJSON(t *testing.T) {
	_, apiServer := testSetup(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/servers", strings.NewReader("{invalid}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	apiServer.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestAddServerHandler_MissingName(t *testing.T) {
	_, apiServer := testSetup(t)

	body := `{
		"config": {
			"port": 6276,
			"command": "/usr/bin/echo"
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/servers", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	apiServer.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestGetServerHandler(t *testing.T) {
	reg, apiServer := testSetup(t)

	// Add a server
	cfg := &config.ServerConfig{
		Port:    6276,
		Command: "/usr/bin/echo",
	}
	if err := reg.Add("test-server", cfg); err != nil {
		t.Fatalf("failed to add server: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/servers/test-server", nil)
	w := httptest.NewRecorder()

	apiServer.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp APIResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp.Success {
		t.Error("expected success to be true")
	}
}

func TestGetServerHandler_NotFound(t *testing.T) {
	_, apiServer := testSetup(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/servers/nonexistent", nil)
	w := httptest.NewRecorder()

	apiServer.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, w.Code)
	}
}

func TestDeleteServerHandler(t *testing.T) {
	reg, apiServer := testSetup(t)

	// Add a server
	cfg := &config.ServerConfig{
		Port:    6276,
		Command: "/usr/bin/echo",
	}
	if err := reg.Add("test-server", cfg); err != nil {
		t.Fatalf("failed to add server: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/servers/test-server", nil)
	w := httptest.NewRecorder()

	apiServer.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	// Verify it's gone
	if reg.Get("test-server") != nil {
		t.Error("expected server to be removed")
	}
}

func TestDeleteServerHandler_NotFound(t *testing.T) {
	_, apiServer := testSetup(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/servers/nonexistent", nil)
	w := httptest.NewRecorder()

	apiServer.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, w.Code)
	}
}

// --- Middleware Tests ---

func TestLoggingMiddleware(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware := LoggingMiddleware(logger)
	wrapped := middleware(handler)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	wrapped.ServeHTTP(w, req)

	output := buf.String()
	if !strings.Contains(output, "GET") {
		t.Error("expected log to contain method")
	}
	if !strings.Contains(output, "/test") {
		t.Error("expected log to contain path")
	}
	if !strings.Contains(output, "200") {
		t.Error("expected log to contain status code")
	}
}

func TestCORSMiddleware(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware := CORSMiddleware([]string{"*"})
	wrapped := middleware(handler)

	// Test preflight
	req := httptest.NewRequest(http.MethodOptions, "/test", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()

	wrapped.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected status %d for preflight, got %d", http.StatusNoContent, w.Code)
	}

	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("expected CORS Allow-Origin header")
	}
	if w.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("expected CORS Allow-Methods header")
	}
}

func TestCORSMiddleware_SpecificOrigin(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware := CORSMiddleware([]string{"http://allowed.com"})
	wrapped := middleware(handler)

	// Test allowed origin
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "http://allowed.com")
	w := httptest.NewRecorder()

	wrapped.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "http://allowed.com" {
		t.Error("expected CORS Allow-Origin header for allowed origin")
	}

	// Test disallowed origin
	req2 := httptest.NewRequest(http.MethodGet, "/test", nil)
	req2.Header.Set("Origin", "http://notallowed.com")
	w2 := httptest.NewRecorder()

	wrapped.ServeHTTP(w2, req2)

	if w2.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("expected no CORS header for disallowed origin")
	}
}

func TestRecoveryMiddleware(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	middleware := RecoveryMiddleware(logger)
	wrapped := middleware(handler)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	// Should not panic
	wrapped.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d after panic, got %d", http.StatusInternalServerError, w.Code)
	}
}

// --- Server Name Validation Tests ---

func TestIsValidServerName(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"valid-name", true},
		{"valid_name", true},
		{"valid123", true},
		{"ValidName", true},
		{"a", true},
		{"a-b-c", true},
		{"-invalid", false},              // starts with hyphen
		{"_invalid", false},              // starts with underscore
		{"", false},                      // empty
		{"has space", false},             // contains space
		{"has.dot", false},               // contains dot
		{"has/slash", false},             // contains slash
		{strings.Repeat("a", 65), false}, // too long
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isValidServerName(tt.name)
			if result != tt.expected {
				t.Errorf("isValidServerName(%q) = %v, want %v", tt.name, result, tt.expected)
			}
		})
	}
}

// --- Integration Tests ---

func TestListServersAfterAdd(t *testing.T) {
	_, apiServer := testSetup(t)

	// Add a server
	addBody := `{
		"name": "my-server",
		"config": {
			"port": 6276,
			"command": "/usr/bin/echo"
		}
	}`

	addReq := httptest.NewRequest(http.MethodPost, "/api/v1/servers", strings.NewReader(addBody))
	addReq.Header.Set("Content-Type", "application/json")
	addW := httptest.NewRecorder()
	apiServer.Handler().ServeHTTP(addW, addReq)

	if addW.Code != http.StatusCreated {
		t.Fatalf("failed to add server: %s", addW.Body.String())
	}

	// List servers
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/servers", nil)
	listW := httptest.NewRecorder()
	apiServer.Handler().ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, listW.Code)
	}

	var resp APIResponse
	if err := json.NewDecoder(listW.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	servers, ok := resp.Data.([]interface{})
	if !ok {
		t.Fatalf("expected data to be array, got %T", resp.Data)
	}
	if len(servers) != 1 {
		t.Errorf("expected 1 server, got %d", len(servers))
	}
}
