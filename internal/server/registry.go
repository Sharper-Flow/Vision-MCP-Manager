// Package server provides the server registry for managing MCP servers.
// It acts as the coordination layer between configuration and supervision.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/supervisor"
)

// Errors returned by Registry operations.
var (
	ErrServerExists     = errors.New("server already exists")
	ErrServerNotFound   = errors.New("server not found")
	ErrServerRunning    = errors.New("server is running")
	ErrServerNotRunning = errors.New("server is not running")
)

// ServerEventType indicates the type of server lifecycle event.
type ServerEventType int

const (
	// EventServerStarted fires when a server has successfully started.
	EventServerStarted ServerEventType = iota
	// EventServerStopped fires when a server has been stopped.
	EventServerStopped
)

// ServerEvent contains information about a server lifecycle change.
// Events are fired asynchronously after the corresponding operation completes,
// and are delivered to the handler in the order the operations occurred.
type ServerEvent struct {
	Type   ServerEventType
	Name   string
	Server *ManagedServer
}

// ServerEventHandler is called when server lifecycle events occur.
//
// Handlers must be non-blocking and must not call back into the registry.
// Events are delivered sequentially by a single goroutine, so a slow handler
// delays every subsequent event; delivery is bounded by handlerTimeout, after
// which the handler is abandoned and ordering is no longer guaranteed for it.
type ServerEventHandler func(event ServerEvent)

const (
	// eventQueueCapacity bounds the lifecycle event queue. Events are produced
	// at server start/stop cadence, so this is far above steady-state depth; it
	// exists to bound memory if a handler wedges, not to absorb normal load.
	eventQueueCapacity = 256

	// eventHandlerTimeout bounds how long a single handler invocation may delay
	// delivery of the next event. Serializing delivery restores ordering but
	// introduces head-of-line blocking; this is what keeps that blocking finite,
	// so one wedged handler cannot stall daemon lifecycle handling forever.
	eventHandlerTimeout = 30 * time.Second
)

// Registry manages MCP server configurations and their lifecycle.
// It provides a thread-safe interface for adding, removing, starting,
// and stopping servers.
type Registry struct {
	supervisor    *supervisor.Supervisor
	servers       map[string]*ManagedServer
	mu            sync.RWMutex
	logger        *slog.Logger
	eventHandler  ServerEventHandler
	startupBlocks map[string]error
	startupGuard  func(string) error

	// Lifecycle events are delivered through a single queue drained by one
	// goroutine, so handlers observe events in the order they occurred.
	eventQueue chan ServerEvent
	stopCh     chan struct{}
	drainDone  chan struct{}
	closeOnce  sync.Once

	// handlerTimeout defaults to eventHandlerTimeout; overridable in tests so
	// timeout behavior can be exercised without a 30s wait.
	handlerTimeout time.Duration
}

func (r *Registry) SetStartupGuard(guard func(string) error) {
	r.mu.Lock()
	r.startupGuard = guard
	r.mu.Unlock()
}

// NewRegistry creates a new server registry with the given supervisor.
func NewRegistry(sup *supervisor.Supervisor, logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	r := &Registry{
		supervisor:     sup,
		servers:        make(map[string]*ManagedServer),
		logger:         logger,
		startupBlocks:  make(map[string]error),
		eventQueue:     make(chan ServerEvent, eventQueueCapacity),
		stopCh:         make(chan struct{}),
		drainDone:      make(chan struct{}),
		handlerTimeout: eventHandlerTimeout,
	}
	go r.drainEvents()
	return r
}

// drainEvents delivers queued lifecycle events one at a time, in arrival order.
// A single drain goroutine is what makes ordering observable to handlers.
func (r *Registry) drainEvents() {
	defer close(r.drainDone)

	for {
		select {
		case event := <-r.eventQueue:
			r.deliverEvent(event)
		case <-r.stopCh:
			// Deliver whatever is already queued before shutting down, so a
			// pending Stop event is never lost at teardown.
			for {
				select {
				case event := <-r.eventQueue:
					r.deliverEvent(event)
				default:
					return
				}
			}
		}
	}
}

// deliverEvent invokes the registered handler for a single event, bounded by
// eventHandlerTimeout. On timeout the handler is abandoned (Go cannot cancel a
// running function) and delivery continues, trading strict serialization for
// liveness — a wedged handler degrades ordering rather than halting the daemon.
func (r *Registry) deliverEvent(event ServerEvent) {
	r.mu.RLock()
	handler := r.eventHandler
	r.mu.RUnlock()

	if handler == nil {
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler(event)
	}()

	timeout := r.handlerTimeout
	if timeout <= 0 {
		timeout = eventHandlerTimeout
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		r.logger.Error("server lifecycle event handler timed out; continuing delivery",
			slog.String("name", event.Name),
			slog.Duration("timeout", timeout),
		)
	}
}

// Close stops event delivery and waits for queued events to drain.
// It is safe to call multiple times.
func (r *Registry) Close() {
	r.closeOnce.Do(func() {
		close(r.stopCh)
	})
	<-r.drainDone
}

// BlockStartup records a reconciliation conflict without changing config.
func (r *Registry) BlockStartup(name string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startupBlocks[name] = err
	if srv := r.servers[name]; srv != nil {
		srv.State = StateFailed
		srv.LastError = err
	}
}

// SetEventHandler registers a callback for server lifecycle events.
// Only one handler can be registered at a time; subsequent calls replace
// the previous handler. Pass nil to unregister the current handler.
// The handler is called asynchronously after state changes complete, and
// receives events in the order those state changes occurred.
func (r *Registry) SetEventHandler(handler ServerEventHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eventHandler = handler
}

// fireEvent enqueues an event for ordered delivery to the registered handler.
// This is called outside of locks to avoid deadlocks.
//
// Events MUST be delivered in the order they occurred. A previous implementation
// dispatched each event with a bare `go handler(event)`, which provides no
// ordering guarantee across goroutines. Because Restart is Stop-then-Start, a
// Start handler could run before the Stop handler that preceded it — the Start
// path then saw a stale listener and returned success, and the late Stop tore
// that listener down. The server reported Running with nothing bound. This was
// the shared root cause of two confirmed production outages.
//
// The send blocks when the queue is full, applying backpressure rather than
// dropping events: a lost Stop event would resurrect exactly the defect above.
// Progress is guaranteed because delivery is bounded by handlerTimeout.
func (r *Registry) fireEvent(event ServerEvent) {
	select {
	case r.eventQueue <- event:
	case <-r.stopCh:
		// Registry is shutting down; drop rather than block a caller forever.
		r.logger.Debug("dropping lifecycle event after registry close",
			slog.String("name", event.Name),
		)
	}
}

// Add registers a new server configuration.
// The server is not started automatically; call Start() to start it.
func (r *Registry) Add(name string, cfg *config.ServerConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.servers[name]; exists {
		return fmt.Errorf("%w: %s", ErrServerExists, name)
	}

	// Create managed server entry
	srv := &ManagedServer{
		Name:      name,
		Config:    cfg,
		State:     StateStopped,
		CreatedAt: time.Now(),
	}
	r.servers[name] = srv

	r.logger.Info("server added to registry",
		slog.String("name", name),
		slog.Int("port", cfg.Port),
	)

	return nil
}

// Remove unregisters a server. The server must be stopped first.
func (r *Registry) Remove(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	srv, exists := r.servers[name]
	if !exists {
		return fmt.Errorf("%w: %s", ErrServerNotFound, name)
	}

	if srv.State == StateRunning || srv.State == StateStarting || srv.State == StateStopping {
		return fmt.Errorf("%w: %s (state: %s)", ErrServerRunning, name, srv.State)
	}

	delete(r.servers, name)

	r.logger.Info("server removed from registry",
		slog.String("name", name),
	)

	return nil
}

// Get returns a server by name, or nil if not found.
func (r *Registry) Get(name string) *ManagedServer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.servers[name]
}

// List returns all registered servers sorted by name.
func (r *Registry) List() []*ManagedServer {
	r.mu.RLock()
	defer r.mu.RUnlock()

	servers := make([]*ManagedServer, 0, len(r.servers))
	for _, srv := range r.servers {
		servers = append(servers, srv)
	}

	// Sort by name for consistent ordering
	sort.Slice(servers, func(i, j int) bool {
		return servers[i].Name < servers[j].Name
	})

	return servers
}

// Names returns all registered server names sorted alphabetically.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.servers))
	for name := range r.servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Count returns the number of registered servers.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.servers)
}

// Start begins running a server.
func (r *Registry) Start(name string) error {
	r.mu.Lock()
	srv, exists := r.servers[name]
	if !exists {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrServerNotFound, name)
	}

	if srv.State == StateRunning || srv.State == StateStarting {
		r.mu.Unlock()
		return nil // Already running, not an error
	}
	if block := r.startupBlocks[name]; block != nil {
		guard := r.startupGuard
		r.mu.Unlock()
		if guard == nil {
			return fmt.Errorf("server %s startup blocked: %w", name, block)
		}
		if err := guard(name); err != nil {
			return err
		}
		r.mu.Lock()
		srv, exists = r.servers[name]
		if !exists {
			r.mu.Unlock()
			return fmt.Errorf("%w: %s", ErrServerNotFound, name)
		}
		if srv.State == StateRunning || srv.State == StateStarting {
			r.mu.Unlock()
			return nil
		}
		if r.startupBlocks[name] != nil {
			delete(r.startupBlocks, name)
		}
	}

	srv.State = StateStarting
	srv.StartedAt = time.Now()
	r.mu.Unlock()

	// For stdio transport, skip supervisor registration — session.Manager is
	// the sole lifecycle owner for stdio subprocesses. This prevents the leak
	// where unused daemon-scoped children accumulate across restart generations.
	if srv.Config.InferTransport() == config.TransportStdio {
		r.mu.Lock()
		srv.Process = nil
		srv.State = StateRunning
		r.mu.Unlock()

		r.logger.Info("server started (stdio, no supervisor process)",
			slog.String("name", name),
			slog.Int("port", srv.Config.Port),
		)

		r.fireEvent(ServerEvent{
			Type:   EventServerStarted,
			Name:   name,
			Server: srv,
		})

		return nil
	}

	// Non-stdio transports use the supervisor normally
	proc, err := r.supervisor.AddServer(name, srv.Config)
	if err != nil {
		r.mu.Lock()
		srv.State = StateFailed
		srv.LastError = err
		r.mu.Unlock()
		return fmt.Errorf("failed to start server %s: %w", name, err)
	}

	r.mu.Lock()
	srv.Process = proc
	srv.State = StateRunning
	r.mu.Unlock()

	r.logger.Info("server started",
		slog.String("name", name),
		slog.Int("port", srv.Config.Port),
	)

	// Fire event after successful start (outside lock)
	r.fireEvent(ServerEvent{
		Type:   EventServerStarted,
		Name:   name,
		Server: srv,
	})

	return nil
}

// Stop halts a running server.
func (r *Registry) Stop(name string) error {
	return r.stop(context.Background(), name)
}

func (r *Registry) stop(ctx context.Context, name string) error {
	r.mu.Lock()
	srv, exists := r.servers[name]
	if !exists {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrServerNotFound, name)
	}

	if srv.State == StateStopped {
		r.mu.Unlock()
		return nil // Already stopped, not an error
	}

	srv.State = StateStopping
	r.mu.Unlock()

	// For stdio servers (where Process is nil), skip supervisor removal.
	// For non-stdio servers, remove from supervisor (which stops the process).
	if srv.Process != nil {
		if err := r.supervisor.RemoveServerAndWait(ctx, name); err != nil {
			r.mu.Lock()
			srv.State = StateFailed
			srv.LastError = err
			r.mu.Unlock()
			return fmt.Errorf("stop server %s: %w", name, err)
		}
	}

	r.mu.Lock()
	srv.Process = nil
	srv.State = StateStopped
	srv.StoppedAt = time.Now()
	r.mu.Unlock()

	r.logger.Info("server stopped",
		slog.String("name", name),
	)

	// Fire event after successful stop (outside lock)
	r.fireEvent(ServerEvent{
		Type:   EventServerStopped,
		Name:   name,
		Server: srv,
	})

	return nil
}

// Restart stops and starts a server.
func (r *Registry) Restart(name string) error {
	if err := r.Stop(name); err != nil {
		return err
	}
	return r.Start(name)
}

// RequiredStartupError is returned by StartAll when one or more servers
// with Autostart=true AND Required=true failed to start. Callers can
// differentiate this from advisory best-effort errors via errors.As so
// that daemon startup can exit non-zero only for the required failures.
type RequiredStartupError struct {
	Failures []string // server names that failed required startup
	Joined   error    // underlying joined start errors (all startup errors)
}

func (e *RequiredStartupError) Error() string {
	return fmt.Sprintf("required servers failed to start: %v: %v", e.Failures, e.Joined)
}

func (e *RequiredStartupError) Unwrap() error { return e.Joined }

// StartAll starts all servers that have autostart enabled.
//
// Best-effort by default: advisory (Required=false) failures are
// logged + joined but do not fail the whole operation from the caller's
// perspective (caller decides). When a server has Autostart=true AND
// Required=true and its start fails, StartAll returns a
// *RequiredStartupError that wraps the joined error plus the list of
// required failures — daemon startup should treat this as fatal.
func (r *Registry) StartAll(ctx context.Context) error {
	r.mu.RLock()
	var toStart []string
	required := make(map[string]bool, len(r.servers))
	for name, srv := range r.servers {
		if srv.Config.Autostart && r.startupBlocks[name] == nil {
			toStart = append(toStart, name)
			required[name] = srv.Config.Required
		}
	}
	r.mu.RUnlock()

	var errs []error
	var requiredFailures []string
	r.mu.RLock()
	for name, block := range r.startupBlocks {
		if srv := r.servers[name]; srv != nil && srv.Config.Autostart {
			errs = append(errs, fmt.Errorf("%s: startup blocked: %w", name, block))
			if srv.Config.Required {
				requiredFailures = append(requiredFailures, name)
			}
		}
	}
	sort.Strings(toStart)
	sort.Strings(requiredFailures)
	r.mu.RUnlock()
	for _, name := range toStart {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := r.Start(name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			if required[name] {
				requiredFailures = append(requiredFailures, name)
			}
		}
	}

	if len(errs) == 0 {
		return nil
	}
	joined := errors.Join(errs...)
	if len(requiredFailures) > 0 {
		return &RequiredStartupError{Failures: requiredFailures, Joined: joined}
	}
	return joined
}

// StopAll stops all running servers.
func (r *Registry) StopAll(ctx context.Context) error {
	r.mu.RLock()
	var toStop []string
	for name, srv := range r.servers {
		if srv.Process != nil || srv.State == StateRunning || srv.State == StateStarting {
			toStop = append(toStop, name)
		}
	}
	r.mu.RUnlock()

	var errs []error
	for _, name := range toStop {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := r.stop(ctx, name); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// LoadFromConfig loads all servers from a configuration.
func (r *Registry) LoadFromConfig(cfg *config.Config) error {
	for name, serverCfg := range cfg.Servers {
		if err := r.Add(name, serverCfg); err != nil {
			return fmt.Errorf("failed to add server %s: %w", name, err)
		}
	}
	return nil
}

// Status returns a snapshot of the current registry state.
func (r *Registry) Status() RegistryStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	status := RegistryStatus{
		TotalServers:   len(r.servers),
		RunningServers: 0,
		StoppedServers: 0,
		FailedServers:  0,
		Servers:        make([]ServerStatus, 0, len(r.servers)),
	}

	for _, srv := range r.servers {
		snapshot := srv.Status()
		switch snapshot.State {
		case StateRunning, StateStarting:
			status.RunningServers++
		case StateStopped, StateStopping:
			status.StoppedServers++
		case StateFailed, StateCrashed:
			status.FailedServers++
		}

		status.Servers = append(status.Servers, snapshot)
	}

	// Sort for consistent output
	sort.Slice(status.Servers, func(i, j int) bool {
		return status.Servers[i].Name < status.Servers[j].Name
	})

	return status
}

// RegistryStatus contains aggregate information about the registry.
type RegistryStatus struct {
	TotalServers   int            `json:"total_servers"`
	RunningServers int            `json:"running_servers"`
	StoppedServers int            `json:"stopped_servers"`
	FailedServers  int            `json:"failed_servers"`
	Servers        []ServerStatus `json:"servers"`
}
