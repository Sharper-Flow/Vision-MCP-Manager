// Package mcp provides HTTP handlers for MCP (Model Context Protocol) servers.
// Each MCP server runs on its own dedicated port, proxying requests to the
// underlying subprocess via the go-sdk StreamableHTTPHandler.
package mcp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
)

// --- Port Manager ---

// PortManager manages HTTP listeners for MCP servers.
// Each server gets its own dedicated port.
type PortManager struct {
	listeners         map[string]*ServerListener
	mu                sync.RWMutex
	wg                sync.WaitGroup // Tracks active listener goroutines for clean shutdown
	logger            *slog.Logger
	reachabilityStore *reachability.Store
}

// SessionCloser is implemented by types that manage per-session resources
// and need cleanup when a server listener is removed.
type SessionCloser interface {
	CloseAll()
}

// ServerListener holds the HTTP server for a single MCP server.
type ServerListener struct {
	Name           string
	Port           int
	MCPHandler     http.Handler  // Streamable HTTP handler
	SessionManager SessionCloser // Session manager for cleanup (nil if not applicable)
	Server         *http.Server
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

// AddStreamable registers and starts an HTTP listener backed by a StreamableHTTPHandler.
// The handler serves the MCP endpoint; sessionMgr (if non-nil) is closed when the listener is removed.
// If security is non-nil, the SecurityMiddleware is applied to the MCP handler.
// Default hardening (timeouts, body size cap, rate limiting) is always applied.
func (pm *PortManager) AddStreamable(name string, port int, handler http.Handler, sessionMgr SessionCloser, security ...SecurityConfig) error {
	return pm.addStreamableInternal(name, port, handler, sessionMgr, HardeningConfig{}, security...)
}

// AddStreamableWithHardening registers and starts a hardened HTTP listener with custom hardening config.
// This allows overriding default timeouts, body size limits, and rate limiting.
func (pm *PortManager) AddStreamableWithHardening(name string, port int, handler http.Handler, sessionMgr SessionCloser, hardening HardeningConfig, security ...SecurityConfig) error {
	return pm.addStreamableInternal(name, port, handler, sessionMgr, hardening, security...)
}

// addStreamableInternal is the shared implementation for AddStreamable and AddStreamableWithHardening.
func (pm *PortManager) addStreamableInternal(name string, port int, handler http.Handler, sessionMgr SessionCloser, hardening HardeningConfig, security ...SecurityConfig) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if _, exists := pm.listeners[name]; exists {
		return ErrServerAlreadyRegistered
	}

	// Apply security middleware if configured.
	// Inject the port manager's logger for security event observability.
	mcpHandler := handler
	if len(security) > 0 {
		sec := security[0]
		if sec.BearerToken != "" || len(sec.AllowedOrigins) > 0 {
			if sec.Logger == nil {
				sec.Logger = pm.logger.With(slog.String("server", name))
			}
			mcpHandler = SecurityMiddleware(sec)(handler)
		}
	}

	// Apply body size limiting middleware.
	mcpHandler = MaxBytesMiddleware(hardening.resolvedMaxBodyBytes())(mcpHandler)

	// Apply rate limiting middleware (unless disabled).
	// Pass the port manager's logger for rate limit denial observability.
	burst := hardening.resolvedRateBurst()
	if burst > 0 {
		serverLogger := pm.logger.With(slog.String("server", name))
		mcpHandler = RateLimitMiddleware(burst, hardening.resolvedRateInterval(), serverLogger)(mcpHandler)
	}

	// Compatibility shim for clients that probe streamable MCP endpoints with a
	// bare GET /mcp request before establishing a session. The MCP SDK rejects
	// this with 405/404, but some clients use a 200 SSE response as a cheap
	// liveness signal when listing servers.
	mcpHandler = ProbeCompatibilityMiddleware(name)(mcpHandler)

	// The internal listener probe must be outermost: it bypasses rate limiting
	// and bearer authentication and never reaches the MCP handler.
	mcpHandler = ListenerProbeMiddleware()(mcpHandler)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)

	// Health endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		status, reason, healthy := pm.perServerHealth(name)
		w.Header().Set("Content-Type", "application/json")
		if !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		response := map[string]interface{}{
			"server": name,
			"status": status,
		}
		if reason != "" {
			response["reason"] = reason
		}
		_ = json.NewEncoder(w).Encode(response)
	})

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Apply hardening timeouts to the HTTP server.
	hardening.ApplyServerTimeouts(srv)

	listener := &ServerListener{
		Name:           name,
		Port:           port,
		MCPHandler:     handler,
		SessionManager: sessionMgr,
		Server:         srv,
	}
	pm.listeners[name] = listener

	// Start listener in background
	pm.wg.Add(1)
	go func() {
		defer pm.wg.Done()
		pm.logger.Info("starting streamable MCP listener",
			slog.String("server", name),
			slog.String("addr", addr),
		)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			pm.logger.Error("streamable MCP listener error",
				slog.String("server", name),
				slog.String("error", err.Error()),
			)
			pm.recordListenerProbeFailure(name, err)
		}
	}()

	return nil
}

// SetReachabilityStore configures the optional store used for listener probe
// evidence. A nil store disables recording and is safe for existing callers.
func (pm *PortManager) SetReachabilityStore(store *reachability.Store) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.reachabilityStore = store
}

// perServerHealth reports listener health from probe-backed reachability.
// Without a store or completed probe evidence, health is unknown rather than
// healthy: absence of evidence is not evidence of health.
func (pm *PortManager) perServerHealth(name string) (status, reason string, healthy bool) {
	pm.mu.RLock()
	store := pm.reachabilityStore
	pm.mu.RUnlock()
	if store == nil {
		return "unhealthy", "reachability_unavailable", false
	}

	value, ok := store.Get(name)
	if !ok || len(value.Evidence) == 0 {
		return "unhealthy", "no_probe_evidence", false
	}

	switch value.State {
	case reachability.StateReachable:
		return "ok", "", true
	case reachability.StateUnreachable:
		return "unhealthy", "unreachable", false
	case reachability.StateProbing:
		return "unhealthy", "probe_in_progress", false
	case reachability.StateUnprobed:
		return "unhealthy", "probe_not_conclusive", false
	default:
		return "unhealthy", "unknown_reachability_state", false
	}
}

func (pm *PortManager) recordListenerProbeFailure(name string, err error) {
	pm.mu.RLock()
	store := pm.reachabilityStore
	pm.mu.RUnlock()
	if store == nil {
		return
	}

	store.RecordProbe(name, reachability.ProbeResult{
		Depth:       reachability.DepthListener,
		AttemptedAt: time.Now(),
		Error:       err.Error(),
	})
}

// ProbeCompatibilityMiddleware returns a short-lived legacy SSE handshake for
// GET /mcp requests that do not yet carry an Mcp-Session-Id.
//
// This preserves normal MCP session semantics for real clients while improving
// interoperability with older tooling that still probes MCP endpoints with the
// pre-streamable SSE transport.
func ProbeCompatibilityMiddleware(serverName string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.Header.Get("Mcp-Session-Id") == "" {
				if acceptsEventStream(r) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Cache-Control", "no-cache, no-transform")
					w.Header().Set("Connection", "keep-alive")
					w.Header().Set("X-Accel-Buffering", "no")
					w.WriteHeader(http.StatusOK)
					_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", r.URL.Path)
					_, _ = fmt.Fprintf(w, ": %s requires MCP initialize before SSE session\n\n", serverName)
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

// Remove stops and removes the listener for a server.
// If the listener has a SessionManager, all sessions are closed first.
func (pm *PortManager) Remove(name string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	listener, exists := pm.listeners[name]
	if !exists {
		return ErrServerNotRegistered
	}

	// Close session manager first (terminates subprocesses)
	if listener.SessionManager != nil {
		listener.SessionManager.CloseAll()
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

// Close shuts down all listeners, closes session managers, and waits for goroutines to exit.
func (pm *PortManager) Close() error {
	pm.mu.Lock()
	for name, listener := range pm.listeners {
		// Close session managers first (terminates subprocesses)
		if listener.SessionManager != nil {
			listener.SessionManager.CloseAll()
		}
		if err := listener.Server.Close(); err != nil {
			pm.logger.Warn("error closing server",
				slog.String("server", name),
				slog.String("error", err.Error()),
			)
		}
	}
	pm.listeners = make(map[string]*ServerListener)
	pm.mu.Unlock()

	// Wait for all listener goroutines to exit
	pm.wg.Wait()
	return nil
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
