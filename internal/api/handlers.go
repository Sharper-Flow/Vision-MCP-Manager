package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jrede/vision/internal/config"
	"github.com/jrede/vision/internal/server"
)

// Handlers provides HTTP handlers for the management API.
type Handlers struct {
	registry  *server.Registry
	config    *config.Config
	startedAt time.Time
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(registry *server.Registry, cfg *config.Config) *Handlers {
	return &Handlers{
		registry:  registry,
		config:    cfg,
		startedAt: time.Now(),
	}
}

// --- Health Endpoints ---

// HealthzHandler returns 200 OK if the service is alive (liveness probe).
// GET /healthz
func (h *Handlers) HealthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ReadyHandler returns 200 OK if the service is ready to accept traffic.
// GET /ready
func (h *Handlers) ReadyHandler(w http.ResponseWriter, r *http.Request) {
	// For now, we're ready if the registry exists
	// In the future, we could check if critical servers are running
	w.Header().Set("Content-Type", "application/json")
	if h.registry == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "not ready", "reason": "registry not initialized"})
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}

// HealthResponse is the detailed health status response.
type HealthResponse struct {
	Status   string                `json:"status"`
	Uptime   string                `json:"uptime"`
	Registry server.RegistryStatus `json:"registry"`
}

// HealthHandler returns detailed health status.
// GET /health
func (h *Handlers) HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	status := "healthy"
	if h.registry == nil {
		status = "degraded"
	}

	resp := HealthResponse{
		Status: status,
		Uptime: time.Since(h.startedAt).Round(time.Second).String(),
	}

	if h.registry != nil {
		resp.Registry = h.registry.Status()
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// --- Server Management API ---

// APIResponse wraps API responses with consistent structure.
type APIResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(APIResponse{Success: status < 400, Data: data})
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: message})
}

// ListServersHandler returns all registered servers.
// GET /api/v1/servers
func (h *Handlers) ListServersHandler(w http.ResponseWriter, r *http.Request) {
	if h.registry == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not initialized")
		return
	}
	servers := h.registry.List()
	statuses := make([]server.ServerStatus, 0, len(servers))
	for _, srv := range servers {
		statuses = append(statuses, srv.Status())
	}
	writeJSON(w, http.StatusOK, statuses)
}

// GetServerHandler returns a specific server by name.
// GET /api/v1/servers/{name}
func (h *Handlers) GetServerHandler(w http.ResponseWriter, r *http.Request, name string) {
	if h.registry == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not initialized")
		return
	}
	srv := h.registry.Get(name)
	if srv == nil {
		writeError(w, http.StatusNotFound, "server not found: "+name)
		return
	}
	writeJSON(w, http.StatusOK, srv.Status())
}

// AddServerRequest is the request body for adding a server.
type AddServerRequest struct {
	Name   string              `json:"name"`
	Config config.ServerConfig `json:"config"`
}

// AddServerHandler adds a new server to the registry.
// POST /api/v1/servers
func (h *Handlers) AddServerHandler(w http.ResponseWriter, r *http.Request) {
	if h.registry == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not initialized")
		return
	}

	var req AddServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	// Validate the config
	if err := req.Config.Validate(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid config: "+err.Error())
		return
	}

	if err := h.registry.Add(req.Name, &req.Config); err != nil {
		if errors.Is(err, server.ErrServerExists) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	srv := h.registry.Get(req.Name)
	writeJSON(w, http.StatusCreated, srv.Status())
}

// UpdateServerRequest is the request body for updating a server.
type UpdateServerRequest struct {
	Config config.ServerConfig `json:"config"`
}

// UpdateServerHandler updates an existing server's configuration.
// PUT /api/v1/servers/{name}
func (h *Handlers) UpdateServerHandler(w http.ResponseWriter, r *http.Request, name string) {
	if h.registry == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not initialized")
		return
	}

	srv := h.registry.Get(name)
	if srv == nil {
		writeError(w, http.StatusNotFound, "server not found: "+name)
		return
	}

	// Server must be stopped to update
	if srv.IsRunning() {
		writeError(w, http.StatusConflict, "server must be stopped to update")
		return
	}

	var req UpdateServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if err := req.Config.Validate(name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid config: "+err.Error())
		return
	}

	// Remove and re-add with new config
	if err := h.registry.Remove(name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.registry.Add(name, &req.Config); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	srv = h.registry.Get(name)
	writeJSON(w, http.StatusOK, srv.Status())
}

// DeleteServerHandler removes a server from the registry.
// DELETE /api/v1/servers/{name}
func (h *Handlers) DeleteServerHandler(w http.ResponseWriter, r *http.Request, name string) {
	if h.registry == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not initialized")
		return
	}

	srv := h.registry.Get(name)
	if srv == nil {
		writeError(w, http.StatusNotFound, "server not found: "+name)
		return
	}

	// Stop the server first if running
	if srv.IsRunning() {
		if err := h.registry.Stop(name); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to stop server: "+err.Error())
			return
		}
	}

	if err := h.registry.Remove(name); err != nil {
		if errors.Is(err, server.ErrServerNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "server removed"})
}

// --- Server Lifecycle Operations ---

// StartServerHandler starts a server.
// POST /api/v1/servers/{name}/start
func (h *Handlers) StartServerHandler(w http.ResponseWriter, r *http.Request, name string) {
	if h.registry == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not initialized")
		return
	}

	srv := h.registry.Get(name)
	if srv == nil {
		writeError(w, http.StatusNotFound, "server not found: "+name)
		return
	}

	if err := h.registry.Start(name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	srv = h.registry.Get(name)
	writeJSON(w, http.StatusOK, srv.Status())
}

// StopServerHandler stops a server.
// POST /api/v1/servers/{name}/stop
func (h *Handlers) StopServerHandler(w http.ResponseWriter, r *http.Request, name string) {
	if h.registry == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not initialized")
		return
	}

	srv := h.registry.Get(name)
	if srv == nil {
		writeError(w, http.StatusNotFound, "server not found: "+name)
		return
	}

	if err := h.registry.Stop(name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	srv = h.registry.Get(name)
	writeJSON(w, http.StatusOK, srv.Status())
}

// RestartServerHandler restarts a server.
// POST /api/v1/servers/{name}/restart
func (h *Handlers) RestartServerHandler(w http.ResponseWriter, r *http.Request, name string) {
	if h.registry == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not initialized")
		return
	}

	srv := h.registry.Get(name)
	if srv == nil {
		writeError(w, http.StatusNotFound, "server not found: "+name)
		return
	}

	if err := h.registry.Restart(name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	srv = h.registry.Get(name)
	writeJSON(w, http.StatusOK, srv.Status())
}
