// Package admin provides the Admin MCP server for Vision management.
// It exposes MCP tools for server lifecycle management on port 6275.
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jrede/vision/internal/bridge"
	"github.com/jrede/vision/internal/catalog"
	"github.com/jrede/vision/internal/server"
)

// DefaultPort is the default port for the Admin MCP server.
const DefaultPort = 6275

// Server is the Admin MCP server that provides management tools.
type Server struct {
	registry  *server.Registry
	catalog   *catalog.Catalog
	port      int
	logger    *slog.Logger
	httpSrv   *http.Server
	startedAt time.Time

	mu      sync.RWMutex
	running bool
}

// Config configures the Admin MCP server.
type Config struct {
	Registry *server.Registry
	Catalog  *catalog.Catalog
	Port     int
	Logger   *slog.Logger
}

// NewServer creates a new Admin MCP server.
func NewServer(cfg Config) *Server {
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	// Use default catalog if none provided
	cat := cfg.Catalog
	if cat == nil {
		cat = catalog.Default()
	}

	return &Server{
		registry: cfg.Registry,
		catalog:  cat,
		port:     cfg.Port,
		logger:   cfg.Logger,
	}
}

// Start begins serving the Admin MCP server.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("admin server already running")
	}

	mux := http.NewServeMux()

	// MCP endpoint - handles JSON-RPC
	mux.HandleFunc("POST /mcp", s.handleMCP)
	mux.HandleFunc("GET /mcp", s.handleMCPInfo)
	mux.HandleFunc("OPTIONS /mcp", s.handleCORS)

	// Health endpoint
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	s.httpSrv = &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	s.startedAt = time.Now()
	s.running = true
	s.mu.Unlock()

	s.logger.Info("starting admin MCP server",
		slog.String("addr", addr),
		slog.Int("port", s.port),
	)

	// Start server in background
	go func() {
		if err := s.httpSrv.ListenAndServe(); err != http.ErrServerClosed {
			s.logger.Error("admin server error", slog.String("error", err.Error()))
		}
	}()

	return nil
}

// Stop gracefully shuts down the Admin MCP server.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	s.running = false
	srv := s.httpSrv
	s.mu.Unlock()

	s.logger.Info("stopping admin MCP server")

	if srv != nil {
		return srv.Shutdown(ctx)
	}
	return nil
}

// IsRunning returns whether the server is currently running.
func (s *Server) IsRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

// Port returns the port the server is configured to run on.
func (s *Server) Port() int {
	return s.port
}

// --- HTTP Handlers ---

// handleMCP handles POST /mcp - MCP JSON-RPC requests.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	// Read request body
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024)) // 1MB limit
	if err != nil {
		s.writeError(w, nil, bridge.CodeParseError, "Failed to read request body")
		return
	}

	// Parse JSON-RPC request
	var req bridge.Request
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, nil, bridge.CodeParseError, "Invalid JSON: "+err.Error())
		return
	}

	// Validate JSON-RPC version
	if req.JSONRPC != "2.0" {
		s.writeError(w, req.ID, bridge.CodeInvalidRequest, "Invalid JSON-RPC version")
		return
	}

	// Route to handler
	resp := s.handleMethod(r.Context(), &req)

	// Write response
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(resp)
}

// handleMethod dispatches to the appropriate method handler.
func (s *Server) handleMethod(ctx context.Context, req *bridge.Request) *bridge.Response {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	default:
		return bridge.NewErrorResponse(req.ID, bridge.NewError(
			bridge.CodeMethodNotFound,
			fmt.Sprintf("Method not found: %s", req.Method),
		))
	}
}

// handleMCPInfo handles GET /mcp - returns server info.
func (s *Server) handleMCPInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]interface{}{
		"name":     "vision-admin",
		"version":  "1.0.0",
		"protocol": "MCP/JSON-RPC 2.0",
		"tools":    len(s.getTools()),
		"uptime":   time.Since(s.startedAt).String(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(info)
}

// handleCORS handles OPTIONS /mcp - CORS preflight.
func (s *Server) handleCORS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.WriteHeader(http.StatusNoContent)
}

// handleHealth handles GET /health - detailed health status.
// Returns {"status": "ok"} when healthy, {"status": "degraded", "errors": [...]} when servers have errors.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	running := s.running
	s.mu.RUnlock()

	if !running {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "unhealthy"})
		return
	}

	// Check registry for failed servers
	var errors []string
	if s.registry != nil {
		regStatus := s.registry.Status()
		if regStatus.FailedServers > 0 {
			// Get error details from failed servers
			for _, srv := range s.registry.List() {
				status := srv.Status()
				if status.State == "failed" || status.State == "crashed" {
					if status.LastError != "" {
						errors = append(errors, fmt.Sprintf("%s: %s", status.Name, status.LastError))
					} else {
						errors = append(errors, fmt.Sprintf("%s: failed", status.Name))
					}
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if len(errors) > 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "degraded",
			"errors": errors,
		})
	} else {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// handleHealthz handles GET /healthz - liveness probe.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// writeError writes a JSON-RPC error response.
func (s *Server) writeError(w http.ResponseWriter, id interface{}, code int, message string) {
	resp := bridge.NewErrorResponse(id, bridge.NewError(code, message))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // JSON-RPC errors use 200
	json.NewEncoder(w).Encode(resp)
}
