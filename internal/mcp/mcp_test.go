package mcp

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- Port Manager Tests ---

func TestPortManager_AddStreamableAndGet(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := NewPortManager(logger)
	defer pm.Close()

	handler := &noopHandler{}

	if err := pm.AddStreamable("test-server", 16276, handler, nil); err != nil {
		t.Fatalf("failed to add server: %v", err)
	}

	listener := pm.Get("test-server")
	if listener == nil {
		t.Fatal("expected listener to be found")
	}

	if listener.Name != "test-server" {
		t.Errorf("expected name 'test-server', got %q", listener.Name)
	}

	if listener.Port != 16276 {
		t.Errorf("expected port 16276, got %d", listener.Port)
	}
}

func TestPortManager_AddStreamableDuplicate(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := NewPortManager(logger)
	defer pm.Close()

	handler := &noopHandler{}

	if err := pm.AddStreamable("test-server", 16276, handler, nil); err != nil {
		t.Fatalf("failed to add server: %v", err)
	}

	err := pm.AddStreamable("test-server", 16277, handler, nil)
	if err != ErrServerAlreadyRegistered {
		t.Errorf("expected ErrServerAlreadyRegistered, got %v", err)
	}
}

func TestPortManager_Remove(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := NewPortManager(logger)
	defer pm.Close()

	handler := &noopHandler{}

	if err := pm.AddStreamable("test-server", 16276, handler, nil); err != nil {
		t.Fatalf("failed to add server: %v", err)
	}

	if err := pm.Remove("test-server"); err != nil {
		t.Fatalf("failed to remove server: %v", err)
	}

	if pm.Get("test-server") != nil {
		t.Error("expected server to be removed")
	}
}

func TestPortManager_RemoveNotFound(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := NewPortManager(logger)
	defer pm.Close()

	err := pm.Remove("nonexistent")
	if err != ErrServerNotRegistered {
		t.Errorf("expected ErrServerNotRegistered, got %v", err)
	}
}

func TestPortManager_List(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := NewPortManager(logger)
	defer pm.Close()

	handler := &noopHandler{}

	pm.AddStreamable("server1", 16276, handler, nil)
	pm.AddStreamable("server2", 16277, handler, nil)

	listeners := pm.List()
	if len(listeners) != 2 {
		t.Errorf("expected 2 listeners, got %d", len(listeners))
	}
}

func TestPortManager_Close(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := NewPortManager(logger)

	handler := &noopHandler{}

	pm.AddStreamable("test-server", 16276, handler, nil)

	if err := pm.Close(); err != nil {
		t.Fatalf("failed to close port manager: %v", err)
	}

	if len(pm.List()) != 0 {
		t.Error("expected all listeners to be removed after close")
	}
}

func TestPortManager_RemoveWithSessionManager(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := NewPortManager(logger)
	defer pm.Close()

	handler := &noopHandler{}
	sm := &mockSessionCloser{}

	if err := pm.AddStreamable("test-server", 16276, handler, sm); err != nil {
		t.Fatalf("failed to add server: %v", err)
	}

	if err := pm.Remove("test-server"); err != nil {
		t.Fatalf("failed to remove server: %v", err)
	}

	if !sm.closed {
		t.Error("expected SessionManager.CloseAll() to be called on Remove")
	}
}

func TestProbeCompatibilityMiddleware_GETWithoutSessionReturnsSSE(t *testing.T) {
	handler := ProbeCompatibilityMiddleware("test-server")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for compatibility probe")
	}))

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "application/json, text/event-stream")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("expected text/event-stream content type, got %q", got)
	}
	body := rr.Body.String()
	if body == "" {
		t.Fatal("expected SSE probe body")
	}
	if body != "event: endpoint\ndata: /mcp\n\n: test-server requires MCP initialize before SSE session\n\n" {
		t.Fatalf("unexpected SSE probe body: %q", body)
	}
}

func TestProbeCompatibilityMiddleware_GETWithSessionFallsThrough(t *testing.T) {
	called := false
	handler := ProbeCompatibilityMiddleware("test-server")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	}))

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", "abc123")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if !called {
		t.Fatal("expected next handler to be called when session id is present")
	}
	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected downstream status, got %d", rr.Code)
	}
}

// --- Test Helpers ---

// noopHandler is an http.Handler that does nothing (for port manager tests).
type noopHandler struct{}

func (h *noopHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// mockSessionCloser tracks whether CloseAll was called.
type mockSessionCloser struct {
	closed bool
}

func (m *mockSessionCloser) CloseAll() {
	m.closed = true
}
