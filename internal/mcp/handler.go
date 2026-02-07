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
)

// --- Port Manager ---

// PortManager manages HTTP listeners for MCP servers.
// Each server gets its own dedicated port.
type PortManager struct {
	listeners map[string]*ServerListener
	mu        sync.RWMutex
	wg        sync.WaitGroup // Tracks active listener goroutines for clean shutdown
	logger    *slog.Logger
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

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)

	// Health endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"server": name,
			"status": "ok",
		})
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
		}
	}()

	return nil
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
