// Package supervisor provides Erlang-style process supervision using suture.
// It manages MCP server processes with automatic restart and graceful termination.
package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/ownership"
	"github.com/thejerf/suture/v4"
)

// Supervisor wraps suture.Supervisor with Vision-specific functionality.
type Supervisor struct {
	*suture.Supervisor
	services       map[string]*ManagedProcess
	mu             sync.RWMutex
	config         config.SupervisionConfig
	logger         *slog.Logger
	leaseStore     ownership.LeaseStore
	daemonID       string
	processOptions []ProcessOption
}

// Option configures optional managed-backend ownership wiring.
type Option func(*Supervisor)

func WithLeaseStore(store ownership.LeaseStore, daemonID string) Option {
	return func(s *Supervisor) { s.leaseStore, s.daemonID = store, daemonID }
}

func WithProcessOptions(options ...ProcessOption) Option {
	return func(s *Supervisor) { s.processOptions = append(s.processOptions, options...) }
}

// New creates a new Supervisor with the given configuration.
func New(cfg config.SupervisionConfig, logger *slog.Logger) *Supervisor {
	return NewWithOptions(cfg, logger)
}

func NewWithOptions(cfg config.SupervisionConfig, logger *slog.Logger, options ...Option) *Supervisor {
	if logger == nil {
		logger = slog.Default()
	}

	spec := suture.Spec{
		EventHook:                eventHook(logger),
		Timeout:                  cfg.ShutdownTimeout.Duration(),
		PassThroughPanics:        false, // Catch panics and restart
		DontPropagateTermination: false,
		// Explicit failure thresholds prevent one crashing service from
		// causing global supervisor backoff that delays restart of others.
		// FailureDecay: failures decay over 60 seconds (float64, seconds).
		// FailureThreshold: enter backoff after 5 failures within decay window.
		// FailureBackoff: wait 30 seconds in backoff before resuming restarts.
		FailureDecay:     60,
		FailureThreshold: 5,
		FailureBackoff:   30 * time.Second,
	}

	s := &Supervisor{
		Supervisor: suture.New("vision", spec),
		services:   make(map[string]*ManagedProcess),
		config:     cfg,
		logger:     logger,
	}
	for _, option := range options {
		option(s)
	}
	return s
}

// AddServer registers a server to be supervised.
// The server won't start until Start() is called on the supervisor.
func (s *Supervisor) AddServer(name string, serverCfg *config.ServerConfig) (*ManagedProcess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.services[name]; exists {
		return nil, fmt.Errorf("server %q already registered", name)
	}

	proc := NewManagedProcessWithOwnership(name, serverCfg, s.config, s.logger, s.leaseStore, s.daemonID, s.processOptions...)
	s.services[name] = proc

	// Keep the exact registration token so dynamic stop removes the supervised
	// child rather than leaving an orphaned restart loop.
	proc.token = s.Add(proc)

	s.logger.Info("registered server",
		slog.String("name", name),
		slog.Int("port", serverCfg.Port),
		slog.String("transport", string(serverCfg.InferTransport())),
	)

	return proc, nil
}

// RemoveServer unregisters and stops a server.
func (s *Supervisor) RemoveServer(name string) error {
	return s.RemoveServerAndWait(context.Background(), name)
}

// RemoveServerAndWait unregisters a service and waits for its Serve method to
// finish. This keeps registry shutdown truthful: callers must not clear a
// process reference or its ownership lease before process-group cleanup ends.
func (s *Supervisor) RemoveServerAndWait(ctx context.Context, name string) error {
	s.mu.Lock()
	proc, exists := s.services[name]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("server %q not found", name)
	}
	timeout := s.config.ShutdownTimeout.Duration()
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			s.mu.Unlock()
			return ctx.Err()
		}
		if timeout <= 0 || remaining < timeout {
			timeout = remaining
		}
	}
	s.mu.Unlock()

	// A live Serve invocation needs RemoveAndWait: Suture Remove alone is
	// asynchronous, so the registry could otherwise report cleanup before the
	// process group and lease are gone. Terminal services have already returned
	// from Serve and completed their exit cleanup, so ordinary removal avoids
	// asking an unstarted supervisor to wait.
	if state := proc.State(); state == StateRunning || state == StateStarting {
		if err := s.RemoveAndWait(proc.token, timeout); err != nil {
			return fmt.Errorf("stop server %q: %w", name, err)
		}
	} else {
		_ = s.Remove(proc.token)
	}

	s.mu.Lock()
	if s.services[name] == proc {
		delete(s.services, name)
	}
	s.mu.Unlock()

	s.logger.Info("removed server", slog.String("name", name))
	return nil
}

// GetServer returns the managed process for a server, or nil if not found.
func (s *Supervisor) GetServer(name string) *ManagedProcess {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.services[name]
}

// Servers returns all registered server names.
func (s *Supervisor) Servers() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.services))
	for name := range s.services {
		names = append(names, name)
	}
	return names
}

// eventHook creates a suture EventHook that logs lifecycle events.
func eventHook(logger *slog.Logger) suture.EventHook {
	var prevTerminate suture.EventServiceTerminate

	return func(ei suture.Event) {
		m := ei.Map()
		l := logger.With(
			slog.String("supervisor", fmt.Sprintf("%v", m["supervisor_name"])),
			slog.String("service", fmt.Sprintf("%v", m["service_name"])),
		)

		switch e := ei.(type) {
		case suture.EventStopTimeout:
			l.Warn("service failed to terminate in time")

		case suture.EventServicePanic:
			l.Error("service panic caught",
				slog.String("panic", e.PanicMsg),
			)

		case suture.EventServiceTerminate:
			// Suppress duplicate consecutive failures
			errStr := fmt.Sprintf("%v", e.Err)
			prevErrStr := fmt.Sprintf("%v", prevTerminate.Err)
			if e.ServiceName == prevTerminate.ServiceName && e.Err != nil && prevTerminate.Err != nil &&
				errStr == prevErrStr {
				l.Debug("service failed repeatedly",
					slog.String("error", errStr),
				)
			} else {
				if e.Err != nil {
					l.Warn("service failed",
						slog.String("error", errStr),
					)
				} else {
					l.Info("service terminated normally")
				}
			}
			prevTerminate = e

		case suture.EventBackoff:
			l.Debug("exiting backoff state")

		case suture.EventResume:
			l.Info("entering backoff state due to repeated failures")

		default:
			l.Debug("supervisor event",
				slog.Int("type", int(e.Type())),
				slog.String("event", e.String()),
			)
		}
	}
}

// ServiceState represents the current state of a managed service.
type ServiceState string

const (
	StateStopped  ServiceState = "stopped"
	StateStarting ServiceState = "starting"
	StateRunning  ServiceState = "running"
	StateCrashed  ServiceState = "crashed"
	StateFailed   ServiceState = "failed" // Max restarts exceeded
)

// ServiceStatus contains runtime information about a managed service.
type ServiceStatus struct {
	Name         string
	State        ServiceState
	PID          int
	Port         int
	Uptime       time.Duration
	RestartCount int
	LastError    error
	StartedAt    time.Time
}
