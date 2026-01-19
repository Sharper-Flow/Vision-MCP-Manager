package api

import (
	"log/slog"
	"net/http"

	"github.com/jrede/vision/internal/config"
	"github.com/jrede/vision/internal/server"
)

// Server is the management API HTTP server.
type Server struct {
	handlers *Handlers
	mux      *http.ServeMux
	logger   *slog.Logger
}

// ServerConfig configures the API server.
type ServerConfig struct {
	Registry       *server.Registry
	Config         *config.Config
	Logger         *slog.Logger
	AllowedOrigins []string // CORS allowed origins, use ["*"] for all
}

// NewServer creates a new management API server.
func NewServer(cfg ServerConfig) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if len(cfg.AllowedOrigins) == 0 {
		cfg.AllowedOrigins = []string{"*"} // Default: allow all for development
	}

	s := &Server{
		handlers: NewHandlers(cfg.Registry, cfg.Config),
		mux:      http.NewServeMux(),
		logger:   cfg.Logger,
	}

	s.registerRoutes(cfg.AllowedOrigins)
	return s
}

// registerRoutes sets up all API routes.
func (s *Server) registerRoutes(allowedOrigins []string) {
	// Health endpoints (no middleware for fast probes)
	s.mux.HandleFunc("GET /healthz", s.handlers.HealthzHandler)
	s.mux.HandleFunc("GET /ready", s.handlers.ReadyHandler)
	s.mux.HandleFunc("GET /health", s.handlers.HealthHandler)

	// API v1 routes - with full middleware stack
	apiMux := http.NewServeMux()

	// Server management routes
	apiMux.HandleFunc("GET /servers", s.handlers.ListServersHandler)
	apiMux.HandleFunc("POST /servers", s.handlers.AddServerHandler)

	// Server-specific routes with name parameter
	apiMux.HandleFunc("GET /servers/{name}", s.serverHandler(s.handlers.GetServerHandler))
	apiMux.HandleFunc("PUT /servers/{name}", s.serverHandler(s.handlers.UpdateServerHandler))
	apiMux.HandleFunc("DELETE /servers/{name}", s.serverHandler(s.handlers.DeleteServerHandler))

	// Server lifecycle routes
	apiMux.HandleFunc("POST /servers/{name}/start", s.serverHandler(s.handlers.StartServerHandler))
	apiMux.HandleFunc("POST /servers/{name}/stop", s.serverHandler(s.handlers.StopServerHandler))
	apiMux.HandleFunc("POST /servers/{name}/restart", s.serverHandler(s.handlers.RestartServerHandler))

	// Apply middleware and mount under /api/v1
	apiHandler := Chain(
		http.StripPrefix("/api/v1", apiMux),
		LoggingMiddleware(s.logger),
		CORSMiddleware(allowedOrigins),
		RecoveryMiddleware(s.logger),
	)

	s.mux.Handle("/api/v1/", apiHandler)
}

// serverHandler wraps handlers that need a server name parameter.
func (s *Server) serverHandler(fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			writeError(w, http.StatusBadRequest, "server name is required")
			return
		}
		// Validate server name (alphanumeric, hyphens, underscores)
		if !isValidServerName(name) {
			writeError(w, http.StatusBadRequest, "invalid server name")
			return
		}
		fn(w, r, name)
	}
}

// isValidServerName checks if a server name is valid.
func isValidServerName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	// Don't allow starting with hyphen or underscore
	if name[0] == '-' || name[0] == '_' {
		return false
	}
	return true
}

// Handler returns the HTTP handler for the server.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// ListenAndServe starts the HTTP server on the given address.
func (s *Server) ListenAndServe(addr string) error {
	s.logger.Info("starting management API server", slog.String("addr", addr))
	return http.ListenAndServe(addr, s.mux)
}
