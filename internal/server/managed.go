package server

import (
	"time"

	"github.com/jrede/vision/internal/config"
	"github.com/jrede/vision/internal/supervisor"
)

// State represents the lifecycle state of a managed server.
type State string

const (
	// StateStopped indicates the server is not running.
	StateStopped State = "stopped"
	// StateStarting indicates the server is starting up.
	StateStarting State = "starting"
	// StateRunning indicates the server is running normally.
	StateRunning State = "running"
	// StateStopping indicates the server is shutting down.
	StateStopping State = "stopping"
	// StateCrashed indicates the server exited unexpectedly.
	StateCrashed State = "crashed"
	// StateFailed indicates the server failed to start or exceeded restart limits.
	StateFailed State = "failed"
)

// ManagedServer represents an MCP server registered in the registry.
// It tracks configuration, state, and runtime metadata.
type ManagedServer struct {
	// Name is the unique identifier for this server.
	Name string

	// Config is the server configuration from servers.yaml.
	Config *config.ServerConfig

	// State is the current lifecycle state.
	State State

	// Process is the underlying supervised process (nil when stopped).
	Process *supervisor.ManagedProcess

	// Metadata
	CreatedAt    time.Time
	StartedAt    time.Time
	StoppedAt    time.Time
	RestartCount int
	LastError    error
}

// IsRunning returns true if the server is in a running state.
func (s *ManagedServer) IsRunning() bool {
	return s.State == StateRunning || s.State == StateStarting
}

// IsStopped returns true if the server is in a stopped state.
func (s *ManagedServer) IsStopped() bool {
	return s.State == StateStopped || s.State == StateStopping
}

// Uptime returns how long the server has been running, or 0 if not running.
func (s *ManagedServer) Uptime() time.Duration {
	if !s.IsRunning() || s.StartedAt.IsZero() {
		return 0
	}
	return time.Since(s.StartedAt)
}

// PID returns the process ID if running, or 0 if not.
func (s *ManagedServer) PID() int {
	if s.Process == nil {
		return 0
	}
	return s.Process.PID()
}

// Port returns the HTTP port this server is exposed on.
func (s *ManagedServer) Port() int {
	if s.Config == nil {
		return 0
	}
	return s.Config.Port
}

// Transport returns the transport type (stdio, http, sse).
func (s *ManagedServer) Transport() config.TransportType {
	if s.Config == nil {
		return ""
	}
	return s.Config.InferTransport()
}

// Status returns a snapshot of the server's current state.
func (s *ManagedServer) Status() ServerStatus {
	var lastErr string
	if s.LastError != nil {
		lastErr = s.LastError.Error()
	}

	var restartCount int
	if s.Process != nil {
		restartCount = s.Process.RestartCount()
	}

	return ServerStatus{
		Name:         s.Name,
		State:        s.State,
		Port:         s.Port(),
		Transport:    s.Transport(),
		PID:          s.PID(),
		Uptime:       s.Uptime(),
		RestartCount: restartCount,
		LastError:    lastErr,
		Autostart:    s.Config != nil && s.Config.Autostart,
	}
}

// ServerStatus is a JSON-serializable snapshot of server state.
type ServerStatus struct {
	Name         string               `json:"name"`
	State        State                `json:"state"`
	Port         int                  `json:"port"`
	Transport    config.TransportType `json:"transport"`
	PID          int                  `json:"pid,omitempty"`
	Uptime       time.Duration        `json:"uptime,omitempty"`
	RestartCount int                  `json:"restart_count"`
	LastError    string               `json:"last_error,omitempty"`
	Autostart    bool                 `json:"autostart"`
}
