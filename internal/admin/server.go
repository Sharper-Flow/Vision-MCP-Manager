// Package admin provides the Admin MCP server for Vision management.
// It exposes MCP tools for server lifecycle management on port 6275.
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/catalog"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	visionmcp "github.com/Sharper-Flow/Vision-MCP-Manager/internal/mcp"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultPort is the default port for the Admin MCP server.
const DefaultPort = 6275

// Server is the Admin MCP server that provides management tools.
type Server struct {
	registry                 *server.Registry
	catalog                  *catalog.Catalog
	instructions             *config.Instructions
	daemonConfig             *config.Config // Mutable reference for persisting changes
	configPath               string         // Path to servers.yaml
	port                     int
	logger                   *slog.Logger
	httpSrv                  *http.Server
	mcpServer                *mcp.Server
	startedAt                time.Time
	slotSessionAccessor      SlotSessionAccessor      // Optional: provides live session counts
	serverMetricsAccessor    ServerMetricsAccessor    // Optional: provides per-server session metrics
	sessionLifecycleAccessor SessionLifecycleAccessor // Optional managed-HTTP lifecycle projection
	reachabilityStore        *reachability.Store
	reachabilityGrace        time.Duration
	Metrics                  *metrics.DaemonMetrics
	listenerExposure         visionmcp.ListenerExposure

	mu      sync.RWMutex
	running bool
}

// Config configures the Admin MCP server.
type Config struct {
	Registry                 *server.Registry
	Catalog                  *catalog.Catalog
	Instructions             *config.Instructions
	DaemonConfig             *config.Config // Mutable reference for persisting changes
	ConfigPath               string         // Path to servers.yaml for config persistence
	Port                     int
	Logger                   *slog.Logger
	SlotSessionAccessor      SlotSessionAccessor      // Optional: provides live session counts
	ServerMetricsAccessor    ServerMetricsAccessor    // Optional: provides per-server session metrics
	SessionLifecycleAccessor SessionLifecycleAccessor // Optional managed-HTTP lifecycle projection
	ReachabilityStore        *reachability.Store
	ReachabilityGrace        time.Duration
	Metrics                  *metrics.DaemonMetrics
}

// NewServer creates a new Admin MCP server.
func NewServer(cfg Config) *Server {
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ReachabilityGrace <= 0 {
		cfg.ReachabilityGrace = DefaultReachabilityGrace
	}
	// Use default catalog if none provided
	cat := cfg.Catalog
	if cat == nil {
		cat = catalog.Default()
	}
	// Use empty instructions if none provided
	inst := cfg.Instructions
	if inst == nil {
		inst = &config.Instructions{
			Servers: make(map[string]*config.ServerInstructions),
			Tools:   make(map[string]*config.ToolInstructions),
		}
	}

	return &Server{
		registry:                 cfg.Registry,
		catalog:                  cat,
		instructions:             inst,
		daemonConfig:             cfg.DaemonConfig,
		configPath:               cfg.ConfigPath,
		port:                     cfg.Port,
		logger:                   cfg.Logger,
		slotSessionAccessor:      cfg.SlotSessionAccessor,
		serverMetricsAccessor:    cfg.ServerMetricsAccessor,
		sessionLifecycleAccessor: cfg.SessionLifecycleAccessor,
		reachabilityStore:        cfg.ReachabilityStore,
		reachabilityGrace:        cfg.ReachabilityGrace,
		Metrics:                  cfg.Metrics,
		listenerExposure:         visionmcp.ListenerLoopback,
	}
}

// Start begins serving the Admin MCP server.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("admin server already running")
	}

	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "vision-admin",
		Version: "1.0.0",
	}, nil)
	s.registerTools(mcpServer)

	mux := http.NewServeMux()
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpServer
	}, &mcp.StreamableHTTPOptions{JSONResponse: true})

	secCfg := visionmcp.SecurityConfig{Logger: s.logger.With(slog.String("component", "admin-security")), ListenerExposure: s.listenerExposure}
	if s.daemonConfig != nil {
		secCfg.BearerToken = s.daemonConfig.Security.BearerToken
		secCfg.AllowedOrigins = s.daemonConfig.Security.AllowedOrigins
	}
	mux.Handle("/mcp", visionmcp.SecurityMiddleware(secCfg)(streamable))

	// Health endpoints
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Version + capability contract (OCA integration — V5)
	mux.HandleFunc("GET /version", s.handleVersion)

	// Prometheus metrics endpoint
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	// V1 admin endpoints (OCA integration): /v1/servers, /v1/servers/{name}
	s.registerV1Routes(mux)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	s.httpSrv = &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	s.mcpServer = mcpServer
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

// handleHealth handles GET /health - detailed health status.
// Returns {"status": "ok"} when healthy, {"status": "degraded", "errors": [...]} when servers have errors.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	running := s.running
	s.mu.RUnlock()

	if !running {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "unhealthy"})
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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "degraded",
			"errors": errors,
		})
	} else {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// handleHealthz handles GET /healthz - liveness probe.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleMetrics handles GET /metrics - Prometheus text format.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var snap metrics.Snapshot
	if s.Metrics != nil {
		snap = s.Metrics.Snapshot()
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	_, _ = fmt.Fprintf(w, "# HELP vision_tool_calls_total Total number of tool calls processed\n")
	_, _ = fmt.Fprintf(w, "# TYPE vision_tool_calls_total counter\n")
	_, _ = fmt.Fprintf(w, "vision_tool_calls_total %d\n", snap.ToolCallsTotal)
	_, _ = fmt.Fprintf(w, "# HELP vision_errors_total Total number of errors\n")
	_, _ = fmt.Fprintf(w, "# TYPE vision_errors_total counter\n")
	_, _ = fmt.Fprintf(w, "vision_errors_total %d\n", snap.ErrorsTotal)
	_, _ = fmt.Fprintf(w, "# HELP vision_sessions_active Number of active sessions\n")
	_, _ = fmt.Fprintf(w, "# TYPE vision_sessions_active gauge\n")
	_, _ = fmt.Fprintf(w, "vision_sessions_active %d\n", snap.SessionsActive)
	_, _ = fmt.Fprintf(w, "# HELP vision_subprocesses_active Number of active subprocesses\n")
	_, _ = fmt.Fprintf(w, "# TYPE vision_subprocesses_active gauge\n")
	_, _ = fmt.Fprintf(w, "vision_subprocesses_active %d\n", snap.SubprocessesActive)
}

// SetServerMetricsAccessor wires the per-server metrics accessor after construction.
func (s *Server) SetServerMetricsAccessor(a ServerMetricsAccessor) {
	s.serverMetricsAccessor = a
}

func (s *Server) SetSessionLifecycleAccessor(a SessionLifecycleAccessor) {
	s.sessionLifecycleAccessor = a
}

// SetReachabilityStore wires the daemon-owned probe evidence store after the
// admin server has been constructed.
func (s *Server) SetReachabilityStore(store *reachability.Store) {
	s.reachabilityStore = store
}
