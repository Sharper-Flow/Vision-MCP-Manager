// Package mcp provides HTTP handlers for MCP (Model Context Protocol) servers.
// Each MCP server runs on its own dedicated port, proxying requests to the
// underlying stdio or HTTP transport.
package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/jrede/vision/internal/bridge"
)

// Handler handles MCP HTTP requests for a single server.
// It exposes POST /mcp for JSON-RPC requests and GET /mcp for server info.
type Handler struct {
	serverName string
	bridge     *bridge.StdioHTTPBridge
	logger     *slog.Logger
}

// NewHandler creates a new MCP HTTP handler for the given server.
func NewHandler(serverName string, b *bridge.StdioHTTPBridge, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		serverName: serverName,
		bridge:     b,
		logger:     logger,
	}
}

// ServeHTTP handles HTTP requests to the MCP endpoint.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.handleMCPRequest(w, r)
	case http.MethodGet:
		h.handleMCPInfo(w, r)
	case http.MethodOptions:
		// CORS preflight
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Mcp-Session-Id")
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleMCPRequest handles POST /mcp - JSON-RPC requests.
func (h *Handler) handleMCPRequest(w http.ResponseWriter, r *http.Request) {
	// Read request body
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024)) // 10MB limit
	if err != nil {
		h.writeJSONRPCError(w, nil, bridge.CodeParseError, "Failed to read request body")
		return
	}

	// Validate content type
	contentType := r.Header.Get("Content-Type")
	if contentType != "" && contentType != "application/json" {
		h.writeJSONRPCError(w, nil, bridge.CodeInvalidRequest, "Content-Type must be application/json")
		return
	}

	// Forward to bridge
	resp, err := h.bridge.ForwardRequest(r.Context(), body)
	if err != nil {
		h.logger.Error("failed to forward request",
			slog.String("server", h.serverName),
			slog.String("error", err.Error()),
		)
		h.writeJSONRPCError(w, nil, bridge.CodeInternalError, err.Error())
		return
	}

	// Handle notifications (no response)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Return response
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(resp)
}

// handleMCPInfo handles GET /mcp - server information.
func (h *Handler) handleMCPInfo(w http.ResponseWriter, r *http.Request) {
	info := MCPServerInfo{
		Server:   h.serverName,
		Protocol: "JSON-RPC 2.0",
		Version:  "1.0.0",
		Endpoints: []string{
			"POST /mcp - Send JSON-RPC request",
			"GET /mcp - Get server info",
		},
	}

	// Add bridge stats if available
	if h.bridge != nil {
		info.Stats = h.bridge.Stats()
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(info)
}

// writeJSONRPCError writes a JSON-RPC error response.
func (h *Handler) writeJSONRPCError(w http.ResponseWriter, id interface{}, code int, message string) {
	resp := bridge.NewErrorResponse(id, bridge.NewError(code, message))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // JSON-RPC errors still use 200
	json.NewEncoder(w).Encode(resp)
}

// MCPServerInfo contains information about an MCP server endpoint.
type MCPServerInfo struct {
	Server    string             `json:"server"`
	Protocol  string             `json:"protocol"`
	Version   string             `json:"version"`
	Endpoints []string           `json:"endpoints"`
	Stats     bridge.BridgeStats `json:"stats,omitempty"`
}

// --- Port Manager ---

// PortManager manages HTTP listeners for MCP servers.
// Each server gets its own dedicated port.
type PortManager struct {
	listeners map[string]*ServerListener
	mu        sync.RWMutex
	logger    *slog.Logger
}

// ServerListener holds the HTTP server for a single MCP server.
type ServerListener struct {
	Name    string
	Port    int
	Handler *Handler
	Server  *http.Server
}

// NewPortManager creates a new port manager.
func NewPortManager(logger *slog.Logger) *PortManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &PortManager{
		listeners: make(map[string]*ServerListener),
		logger:    logger,
	}
}

// Add registers and starts an HTTP listener for a server.
func (pm *PortManager) Add(name string, port int, b *bridge.StdioHTTPBridge) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if _, exists := pm.listeners[name]; exists {
		return ErrServerAlreadyRegistered
	}

	handler := NewHandler(name, b, pm.logger)

	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.Handle("/", handler) // Also handle root for convenience

	// Add health endpoint for each MCP port
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"server": name,
			"status": "ok",
			"stats":  b.Stats(),
		})
	})

	addr := formatAddr(port)
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	listener := &ServerListener{
		Name:    name,
		Port:    port,
		Handler: handler,
		Server:  srv,
	}
	pm.listeners[name] = listener

	// Start listener in background
	go func() {
		pm.logger.Info("starting MCP listener",
			slog.String("server", name),
			slog.String("addr", addr),
		)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			pm.logger.Error("MCP listener error",
				slog.String("server", name),
				slog.String("error", err.Error()),
			)
		}
	}()

	return nil
}

// Remove stops and removes the listener for a server.
func (pm *PortManager) Remove(name string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	listener, exists := pm.listeners[name]
	if !exists {
		return ErrServerNotRegistered
	}

	if err := listener.Server.Close(); err != nil {
		pm.logger.Warn("error closing server",
			slog.String("server", name),
			slog.String("error", err.Error()),
		)
	}

	delete(pm.listeners, name)
	pm.logger.Info("stopped MCP listener", slog.String("server", name))
	return nil
}

// Get returns the listener for a server.
func (pm *PortManager) Get(name string) *ServerListener {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.listeners[name]
}

// List returns all active listeners.
func (pm *PortManager) List() []*ServerListener {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	result := make([]*ServerListener, 0, len(pm.listeners))
	for _, l := range pm.listeners {
		result = append(result, l)
	}
	return result
}

// Close shuts down all listeners.
func (pm *PortManager) Close() error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for name, listener := range pm.listeners {
		if err := listener.Server.Close(); err != nil {
			pm.logger.Warn("error closing server",
				slog.String("server", name),
				slog.String("error", err.Error()),
			)
		}
	}
	pm.listeners = make(map[string]*ServerListener)
	return nil
}

// formatAddr formats a port number as an address string.
func formatAddr(port int) string {
	return fmt.Sprintf(":%d", port)
}

// Errors
var (
	ErrServerAlreadyRegistered = &PortManagerError{"server already registered"}
	ErrServerNotRegistered     = &PortManagerError{"server not registered"}
)

// PortManagerError is an error from the port manager.
type PortManagerError struct {
	Message string
}

func (e *PortManagerError) Error() string {
	return e.Message
}
