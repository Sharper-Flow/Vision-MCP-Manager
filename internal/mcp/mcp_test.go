package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jrede/vision/internal/bridge"
)

// mockBridge creates a simple mock stdio bridge for testing.
type mockPipes struct {
	stdin  *bytes.Buffer
	stdout *bytes.Buffer
}

func newMockBridge(t *testing.T) (*bridge.StdioHTTPBridge, *mockPipes) {
	t.Helper()

	pipes := &mockPipes{
		stdin:  &bytes.Buffer{},
		stdout: &bytes.Buffer{},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := bridge.NewStdioHTTPBridge(pipes.stdin, pipes.stdout, &bridge.BridgeOptions{
		Logger: logger,
	})

	return b, pipes
}

// --- Handler Tests ---

func TestHandler_GetInfo(t *testing.T) {
	b, _ := newMockBridge(t)
	defer b.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler("test-server", b, logger)

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var info MCPServerInfo
	if err := json.NewDecoder(w.Body).Decode(&info); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if info.Server != "test-server" {
		t.Errorf("expected server 'test-server', got %q", info.Server)
	}

	if info.Protocol != "JSON-RPC 2.0" {
		t.Errorf("expected protocol 'JSON-RPC 2.0', got %q", info.Protocol)
	}
}

func TestHandler_Options(t *testing.T) {
	b, _ := newMockBridge(t)
	defer b.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler("test-server", b, logger)

	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected status %d, got %d", http.StatusNoContent, w.Code)
	}

	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("expected CORS header")
	}
}

func TestHandler_UnsupportedMethod(t *testing.T) {
	b, _ := newMockBridge(t)
	defer b.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler("test-server", b, logger)

	req := httptest.NewRequest(http.MethodPut, "/mcp", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}
}

func TestHandler_PostInvalidContentType(t *testing.T) {
	b, _ := newMockBridge(t)
	defer b.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler("test-server", b, logger)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("test"))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK { // JSON-RPC errors still return 200
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp bridge.Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error == nil {
		t.Error("expected error response")
	}
}

// --- Port Manager Tests ---

func TestPortManager_AddAndGet(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := NewPortManager(logger)
	defer pm.Close()

	b, _ := newMockBridge(t)
	defer b.Close()

	if err := pm.Add("test-server", 16276, b); err != nil {
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

func TestPortManager_AddDuplicate(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := NewPortManager(logger)
	defer pm.Close()

	b1, _ := newMockBridge(t)
	defer b1.Close()
	b2, _ := newMockBridge(t)
	defer b2.Close()

	if err := pm.Add("test-server", 16276, b1); err != nil {
		t.Fatalf("failed to add server: %v", err)
	}

	err := pm.Add("test-server", 16277, b2)
	if err != ErrServerAlreadyRegistered {
		t.Errorf("expected ErrServerAlreadyRegistered, got %v", err)
	}
}

func TestPortManager_Remove(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := NewPortManager(logger)
	defer pm.Close()

	b, _ := newMockBridge(t)
	defer b.Close()

	if err := pm.Add("test-server", 16276, b); err != nil {
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

	b1, _ := newMockBridge(t)
	defer b1.Close()
	b2, _ := newMockBridge(t)
	defer b2.Close()

	pm.Add("server1", 16276, b1)
	pm.Add("server2", 16277, b2)

	listeners := pm.List()
	if len(listeners) != 2 {
		t.Errorf("expected 2 listeners, got %d", len(listeners))
	}
}

func TestPortManager_Close(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := NewPortManager(logger)

	b, _ := newMockBridge(t)
	defer b.Close()

	pm.Add("test-server", 16276, b)

	if err := pm.Close(); err != nil {
		t.Fatalf("failed to close port manager: %v", err)
	}

	if len(pm.List()) != 0 {
		t.Error("expected all listeners to be removed after close")
	}
}

// --- Format Addr Test ---

func TestFormatAddr(t *testing.T) {
	tests := []struct {
		port int
		want string
	}{
		{6276, ":6276"},
		{80, ":80"},
		{8080, ":8080"},
		{443, ":443"},
	}

	for _, tt := range tests {
		got := formatAddr(tt.port)
		if got != tt.want {
			t.Errorf("formatAddr(%d) = %q, want %q", tt.port, got, tt.want)
		}
	}
}
