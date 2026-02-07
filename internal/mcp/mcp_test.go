package mcp

import (
	"io"
	"log/slog"
	"net/http"
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
