package server

import (
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/supervisor"
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
	state := s.State
	pid := s.PID()
	uptime := s.Uptime()
	restartCount := s.RestartCount
	lastErr := s.LastError
	if s.Process != nil {
		// Process.Status takes the supervisor lock and is the authoritative
		// snapshot for supervised servers. Do not combine it with separately
		// read process fields: that would produce an internally inconsistent
		// response during a terminal transition.
		processStatus := s.Process.Status()
		state = mapProcessState(processStatus.State)
		pid = processStatus.PID
		uptime = processStatus.Uptime
		restartCount = processStatus.RestartCount
		if processStatus.LastError != nil {
			lastErr = processStatus.LastError
		}
	}

	lastErrorText := ""
	if lastErr != nil {
		lastErrorText = lastErr.Error()
	}

	required := s.Config != nil && s.Config.Required

	return ServerStatus{
		Name:         s.Name,
		State:        state,
		Port:         s.Port(),
		Transport:    s.Transport(),
		PID:          pid,
		Uptime:       uptime,
		RestartCount: restartCount,
		LastError:    lastErrorText,
		Autostart:    s.Config != nil && s.Config.Autostart,
		Required:     required,
	}
}

func mapProcessState(state supervisor.ServiceState) State {
	switch state {
	case supervisor.StateStopped:
		return StateStopped
	case supervisor.StateStarting:
		return StateStarting
	case supervisor.StateRunning:
		return StateRunning
	case supervisor.StateCrashed:
		return StateCrashed
	case supervisor.StateFailed:
		return StateFailed
	default:
		return StateFailed
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
	// Required reflects the config.ServerConfig.Required flag. External
	// tools such as OCA doctor use this combined with State to surface
	// required-but-not-running servers as errors (rather than warnings).
	Required bool `json:"required"`
}
