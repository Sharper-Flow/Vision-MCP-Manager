package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SharedSessionManager maintains a single downstream subprocess shared across
// all upstream sessions for stateless MCP servers.
type SharedSessionManager struct {
	serverName string
	config     *config.ServerConfig
	logger     *slog.Logger

	mu         sync.RWMutex
	downstream *mcp.ClientSession
	client     *mcp.Client
	spawnErr   error
	closed     bool

	refMu            sync.Mutex
	refCount         int
	upstreamSessions map[string]struct{}

	healthCtx    context.Context
	healthCancel context.CancelFunc

	subMu       sync.RWMutex
	subscribers map[string]func(*mcp.ClientSession)
}

// NewSharedSessionManager creates a new shared session manager.
func NewSharedSessionManager(serverName string, cfg *config.ServerConfig, logger *slog.Logger) *SharedSessionManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &SharedSessionManager{
		serverName:       serverName,
		config:           cfg,
		logger:           logger.With(slog.String("server", serverName)),
		upstreamSessions: make(map[string]struct{}),
		subscribers:      make(map[string]func(*mcp.ClientSession)),
	}
}

// GetOrCreateSession returns the shared downstream session, lazily spawning it
// on first use. The sessionID is tracked for admission control and refcounting.
func (sm *SharedSessionManager) GetOrCreateSession(ctx context.Context, sessionID string) (*mcp.ClientSession, error) {
	if sm.isClosed() {
		return nil, errors.New("shared session manager is closed")
	}

	// Check admission and track session
	newSession := false
	sm.refMu.Lock()
	if _, exists := sm.upstreamSessions[sessionID]; !exists {
		// Check max sessions
		if sm.config.MaxSessions > 0 && sm.refCount >= sm.config.MaxSessions {
			sm.refMu.Unlock()
			return nil, fmt.Errorf("%w: limit %d", ErrMaxSessions, sm.config.MaxSessions)
		}
		sm.upstreamSessions[sessionID] = struct{}{}
		sm.refCount++
		newSession = true
	}
	sm.refMu.Unlock()

	// Get or create downstream
	ds, err := sm.getOrCreateDownstream(ctx)
	if err != nil && newSession {
		// Undo refcount increment on spawn failure to prevent leak
		sm.refMu.Lock()
		delete(sm.upstreamSessions, sessionID)
		sm.refCount--
		sm.refMu.Unlock()
	}
	return ds, err
}

func (sm *SharedSessionManager) isClosed() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.closed
}

func (sm *SharedSessionManager) getOrCreateDownstream(ctx context.Context) (*mcp.ClientSession, error) {
	// Fast path: check if downstream is healthy
	sm.mu.RLock()
	ds := sm.downstream
	err := sm.spawnErr
	closed := sm.closed
	sm.mu.RUnlock()

	if closed {
		return nil, errors.New("shared session manager is closed")
	}

	if ds != nil && err == nil {
		// Quick health check
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if _, pingErr := ds.ListTools(checkCtx, nil); pingErr == nil {
			return ds, nil
		}
		// Dead, need respawn
	}

	// Slow path: spawn or respawn
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.closed {
		return nil, errors.New("shared session manager is closed")
	}

	// Double-check
	if sm.downstream != nil && sm.spawnErr == nil {
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if _, pingErr := sm.downstream.ListTools(checkCtx, nil); pingErr == nil {
			return sm.downstream, nil
		}
	}

	// Close old downstream if exists
	if sm.downstream != nil {
		sm.downstream.Close()
	}

	// Spawn new downstream
	sm.downstream, sm.client, sm.spawnErr = sm.spawn(ctx)
	if sm.spawnErr != nil {
		return nil, sm.spawnErr
	}

	sm.notifySubscribers(sm.downstream)

	return sm.downstream, nil
}

func (sm *SharedSessionManager) spawn(ctx context.Context) (*mcp.ClientSession, *mcp.Client, error) {
	cmd, err := sm.buildCommand()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build command: %w", err)
	}

	sm.logger.Info("spawning shared downstream subprocess",
		slog.String("event", "shared_session.spawn"),
		slog.String("command", sm.config.Command),
		slog.Any("args", sm.config.Args),
	)

	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    fmt.Sprintf("vision-proxy-%s", sm.serverName),
		Version: "1.0.0",
	}, nil)

	clientSession, err := client.Connect(ctx, transport, nil)
	if err != nil {
		sm.logger.Error("failed to connect to downstream",
			slog.String("event", "shared_session.spawn_failed"),
			slog.String("error", err.Error()),
		)
		return nil, nil, fmt.Errorf("failed to connect downstream: %w", err)
	}

	sm.logger.Info("shared downstream subprocess connected",
		slog.String("event", "shared_session.connected"),
	)

	return clientSession, client, nil
}

func (sm *SharedSessionManager) buildCommand() (*exec.Cmd, error) {
	cmdPath, err := exec.LookPath(sm.config.Command)
	if err != nil {
		return nil, fmt.Errorf("command not found: %s: %w", sm.config.Command, err)
	}

	cmd := exec.Command(cmdPath, sm.config.Args...)
	cmd.Env = sm.buildEnv()

	return cmd, nil
}

func (sm *SharedSessionManager) buildEnv() []string {
	if len(sm.config.Env) == 0 {
		return nil
	}
	env := os.Environ()
	for key, value := range sm.config.Env {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}
	return env
}

// RemoveSession removes the upstream session from tracking and decrements refcount.
func (sm *SharedSessionManager) RemoveSession(sessionID string) error {
	sm.refMu.Lock()
	defer sm.refMu.Unlock()

	if _, exists := sm.upstreamSessions[sessionID]; !exists {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}

	delete(sm.upstreamSessions, sessionID)
	sm.refCount--

	return nil
}

// CloseAll terminates the shared downstream subprocess and clears all state.
func (sm *SharedSessionManager) CloseAll() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.closed {
		return
	}

	sm.closed = true

	// Cancel health probe
	if sm.healthCancel != nil {
		sm.healthCancel()
		sm.healthCancel = nil
	}

	// Clear upstream sessions
	sm.refMu.Lock()
	sm.upstreamSessions = make(map[string]struct{})
	sm.refCount = 0
	sm.refMu.Unlock()

	// Close downstream
	if sm.downstream != nil {
		sm.downstream.Close()
		sm.downstream = nil
	}

	sm.client = nil
	sm.spawnErr = nil
}

// AdmissionStatus returns current admission-control state.
func (sm *SharedSessionManager) AdmissionStatus() (atCapacity bool, current int, max int) {
	sm.refMu.Lock()
	defer sm.refMu.Unlock()
	current = sm.refCount
	max = sm.config.MaxSessions
	atCapacity = max > 0 && current >= max
	return atCapacity, current, max
}

// Subscribe registers a callback to be invoked when the downstream is respawned.
func (sm *SharedSessionManager) Subscribe(sessionID string, onRespawn func(*mcp.ClientSession)) {
	sm.subMu.Lock()
	defer sm.subMu.Unlock()
	sm.subscribers[sessionID] = onRespawn
}

// Unsubscribe removes the respawn callback for the given session ID.
func (sm *SharedSessionManager) Unsubscribe(sessionID string) {
	sm.subMu.Lock()
	defer sm.subMu.Unlock()
	delete(sm.subscribers, sessionID)
}

func (sm *SharedSessionManager) notifySubscribers(ds *mcp.ClientSession) {
	sm.subMu.RLock()
	defer sm.subMu.RUnlock()
	for _, cb := range sm.subscribers {
		func() {
			defer func() {
				if r := recover(); r != nil {
					sm.logger.Error("subscriber callback panicked",
						slog.String("event", "shared_session.subscriber_panic"),
						slog.Any("panic", r),
					)
				}
			}()
			cb(ds)
		}()
	}
}

// StartHealthProbe starts a background goroutine that periodically probes the
// shared downstream for liveness. If the probe fails, the downstream is respawned.
func (sm *SharedSessionManager) StartHealthProbe(ctx context.Context) {
	sm.mu.Lock()
	if sm.healthCancel != nil {
		sm.healthCancel()
	}
	sm.healthCtx, sm.healthCancel = context.WithCancel(ctx)
	sm.mu.Unlock()

	interval := time.Duration(sm.config.HealthCheckInterval)
	if interval <= 0 {
		interval = 30 * time.Second
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-sm.healthCtx.Done():
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				sm.healthCheck()
			}
		}
	}()
}

func (sm *SharedSessionManager) healthCheck() {
	sm.mu.RLock()
	ds := sm.downstream
	closed := sm.closed
	sm.mu.RUnlock()

	if closed || ds == nil {
		return
	}

	checkCtx, cancel := context.WithTimeout(sm.healthCtx, 5*time.Second)
	defer cancel()

	if _, err := ds.ListTools(checkCtx, nil); err != nil {
		sm.logger.Warn("health check failed, triggering respawn",
			slog.String("event", "shared_session.health_check_failed"),
			slog.String("error", err.Error()),
		)

		// Invalidate downstream to force respawn
		sm.mu.Lock()
		if sm.downstream == ds {
			sm.downstream = nil
			sm.spawnErr = errors.New("health check failed")
		}
		sm.mu.Unlock()

		// Trigger respawn
		spawnCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = sm.getOrCreateDownstream(spawnCtx)
	}
}

// HasDownstream reports whether a downstream session is currently active.
func (sm *SharedSessionManager) HasDownstream() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.downstream != nil && sm.spawnErr == nil
}

// RefCount returns the current upstream refcount.
func (sm *SharedSessionManager) RefCount() int {
	sm.refMu.Lock()
	defer sm.refMu.Unlock()
	return sm.refCount
}

// Downstream returns the current downstream session (may be nil).
func (sm *SharedSessionManager) Downstream() *mcp.ClientSession {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.downstream
}

// SessionCount returns the number of tracked upstream sessions.
func (sm *SharedSessionManager) SessionCount() int {
	sm.refMu.Lock()
	defer sm.refMu.Unlock()
	return len(sm.upstreamSessions)
}
