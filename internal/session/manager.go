// Package session provides per-session subprocess management for MCP servers.
//
// Each upstream MCP session gets its own isolated subprocess connected via
// the go-sdk CommandTransport. The Manager tracks the mapping from session IDs
// to downstream ClientSessions and handles lifecycle (spawn, teardown, cleanup).
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

	"github.com/jrede/vision/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Errors returned by session Manager operations.
var (
	ErrSessionExists   = errors.New("session: session already exists")
	ErrSessionNotFound = errors.New("session: session not found")
	ErrMaxSessions     = errors.New("session: maximum concurrent sessions reached")
)

// TrackedSession holds the downstream client session and metadata
// for a single upstream MCP session.
type TrackedSession struct {
	SessionID    string
	Downstream   *mcp.ClientSession
	Client       *mcp.Client
	CreatedAt    time.Time // When the session was spawned
	LastActivity time.Time // Last time the session was actively used
}

// Manager manages per-session subprocess lifecycle for a single MCP server.
// Each upstream session gets its own subprocess, connected via CommandTransport.
type Manager struct {
	serverName string
	config     *config.ServerConfig
	logger     *slog.Logger

	mu       sync.RWMutex
	sessions map[string]*TrackedSession
}

// NewManager creates a new session Manager for the given server.
func NewManager(serverName string, cfg *config.ServerConfig, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		serverName: serverName,
		config:     cfg,
		logger:     logger.With(slog.String("server", serverName)),
		sessions:   make(map[string]*TrackedSession),
	}
}

// SpawnSession creates a new downstream subprocess for the given session ID.
// It connects via CommandTransport, performs the initialize handshake,
// and returns the ready-to-use ClientSession.
// If clientOpts is non-nil, it is used to configure the downstream Client
// (e.g., for notification handlers).
func (m *Manager) SpawnSession(ctx context.Context, sessionID string, clientOpts ...*mcp.ClientOptions) (*mcp.ClientSession, error) {
	m.mu.Lock()

	// Check if session already exists
	if _, exists := m.sessions[sessionID]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrSessionExists, sessionID)
	}

	// Check admission limits
	if m.config.MaxSessions > 0 && len(m.sessions) >= m.config.MaxSessions {
		currentCount := len(m.sessions)
		m.mu.Unlock()
		m.logger.Warn("admission denied: max sessions reached",
			slog.String("event", "session.admission_denied"),
			slog.String("session_id", sessionID),
			slog.Int("max", m.config.MaxSessions),
			slog.Int("current", currentCount),
		)
		return nil, fmt.Errorf("%w: limit %d", ErrMaxSessions, m.config.MaxSessions)
	}

	// Reserve the slot before releasing the lock (prevents races)
	m.sessions[sessionID] = nil
	m.mu.Unlock()

	// Build the command
	cmd, err := m.buildCommand()
	if err != nil {
		m.mu.Lock()
		delete(m.sessions, sessionID)
		m.mu.Unlock()
		return nil, fmt.Errorf("failed to build command: %w", err)
	}

	m.logger.Info("spawning downstream subprocess",
		slog.String("event", "session.spawn"),
		slog.String("session_id", sessionID),
		slog.String("command", m.config.Command),
		slog.Any("args", m.config.Args),
	)

	// Create CommandTransport and Client
	transport := &mcp.CommandTransport{Command: cmd}
	var opts *mcp.ClientOptions
	if len(clientOpts) > 0 && clientOpts[0] != nil {
		opts = clientOpts[0]
	}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    fmt.Sprintf("vision-proxy-%s", m.serverName),
		Version: "1.0.0",
	}, opts)

	// Connect performs: start process, initialize handshake, notifications/initialized
	clientSession, err := client.Connect(ctx, transport, nil)
	if err != nil {
		m.mu.Lock()
		delete(m.sessions, sessionID)
		m.mu.Unlock()
		m.logger.Error("failed to connect to downstream",
			slog.String("event", "session.spawn_failed"),
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()),
		)
		return nil, fmt.Errorf("failed to connect downstream for session %s: %w", sessionID, err)
	}

	// Store the tracked session
	now := time.Now()
	tracked := &TrackedSession{
		SessionID:    sessionID,
		Downstream:   clientSession,
		Client:       client,
		CreatedAt:    now,
		LastActivity: now,
	}

	m.mu.Lock()
	m.sessions[sessionID] = tracked
	activeSessions := len(m.sessions)
	m.mu.Unlock()

	m.logger.Info("downstream subprocess connected",
		slog.String("event", "session.connected"),
		slog.String("session_id", sessionID),
		slog.Int("active_sessions", activeSessions),
	)

	return clientSession, nil
}

// GetSession returns the tracked session for the given session ID, or nil.
func (m *Manager) GetSession(sessionID string) *TrackedSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[sessionID]
}

// RemoveSession terminates the downstream subprocess and removes the session.
// It gracefully closes the ClientSession (which closes stdin, waits, then SIGTERM/SIGKILL).
func (m *Manager) RemoveSession(sessionID string) error {
	m.mu.Lock()
	tracked, exists := m.sessions[sessionID]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}
	delete(m.sessions, sessionID)
	m.mu.Unlock()

	m.logger.Info("removing session",
		slog.String("event", "session.remove"),
		slog.String("session_id", sessionID),
		slog.Duration("lifetime", time.Since(tracked.CreatedAt)),
	)

	return m.closeTrackedSession(tracked)
}

// SessionCount returns the number of active sessions.
func (m *Manager) SessionCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// AdmissionStatus returns current admission-control state.
// atCapacity is true only when MaxSessions is configured (>0) and reached.
func (m *Manager) AdmissionStatus() (atCapacity bool, current int, max int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	current = len(m.sessions)
	max = m.config.MaxSessions
	atCapacity = max > 0 && current >= max
	return atCapacity, current, max
}

// Sessions returns a snapshot of all active session IDs.
func (m *Manager) Sessions() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	return ids
}

// TouchSession updates the LastActivity timestamp for the given session.
// This should be called on each tool call or notification to keep the session alive.
func (m *Manager) TouchSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if tracked, exists := m.sessions[sessionID]; exists && tracked != nil {
		tracked.LastActivity = time.Now()
	}
}

// StartReaper starts a background goroutine that periodically checks for
// idle and TTL-expired sessions and removes them.
// The reaper stops when the context is cancelled.
func (m *Manager) StartReaper(ctx context.Context, checkInterval time.Duration) {
	idleTimeout := time.Duration(m.config.SessionTimeout)
	sessionTTL := time.Duration(m.config.SessionTTL)

	go func() {
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.reapExpiredSessions(idleTimeout, sessionTTL)
			}
		}
	}()
}

// reapExpiredSessions removes sessions that have exceeded the idle timeout or TTL.
func (m *Manager) reapExpiredSessions(idleTimeout, sessionTTL time.Duration) {
	now := time.Now()

	m.mu.Lock()
	var toRemove []*TrackedSession
	for id, tracked := range m.sessions {
		if tracked == nil {
			continue
		}

		expired := false
		reason := ""

		// Check idle timeout (0 means disabled).
		if idleTimeout > 0 && now.Sub(tracked.LastActivity) > idleTimeout {
			expired = true
			reason = "idle timeout"
		}

		// Check absolute TTL (0 means disabled).
		if sessionTTL > 0 && now.Sub(tracked.CreatedAt) > sessionTTL {
			expired = true
			reason = "TTL expired"
		}

		if expired {
			m.logger.Info("reaping expired session",
				slog.String("event", "session.reaped"),
				slog.String("session_id", id),
				slog.String("reason", reason),
				slog.Duration("lifetime", now.Sub(tracked.CreatedAt)),
				slog.Duration("idle", now.Sub(tracked.LastActivity)),
			)
			toRemove = append(toRemove, tracked)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()

	// Close sessions outside the lock to avoid holding it during IO.
	for _, tracked := range toRemove {
		if err := m.closeTrackedSession(tracked); err != nil {
			m.logger.Warn("error closing reaped session",
				slog.String("session_id", tracked.SessionID),
				slog.String("error", err.Error()),
			)
		}
	}
}

// CloseAll terminates all sessions and their subprocesses.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	toClose := make([]*TrackedSession, 0, len(m.sessions))
	for _, tracked := range m.sessions {
		if tracked != nil {
			toClose = append(toClose, tracked)
		}
	}
	m.sessions = make(map[string]*TrackedSession)
	m.mu.Unlock()

	for _, tracked := range toClose {
		if err := m.closeTrackedSession(tracked); err != nil {
			m.logger.Warn("error closing session during CloseAll",
				slog.String("session_id", tracked.SessionID),
				slog.String("error", err.Error()),
			)
		}
	}
}

// closeTrackedSession closes the downstream client session, which terminates the subprocess.
// The go-sdk CommandTransport.Close() follows the spec:
// 1. Close stdin
// 2. Wait for exit (up to TerminateDuration)
// 3. SIGTERM if not exited
// 4. Wait again
// 5. SIGKILL if still alive
func (m *Manager) closeTrackedSession(tracked *TrackedSession) error {
	if tracked == nil || tracked.Downstream == nil {
		return nil
	}

	m.logger.Debug("closing downstream session",
		slog.String("event", "session.closing"),
		slog.String("session_id", tracked.SessionID),
	)

	if err := tracked.Downstream.Close(); err != nil {
		m.logger.Warn("error closing downstream session",
			slog.String("session_id", tracked.SessionID),
			slog.String("error", err.Error()),
		)
		return err
	}

	m.logger.Info("downstream session closed",
		slog.String("event", "session.closed"),
		slog.String("session_id", tracked.SessionID),
	)
	return nil
}

// buildCommand creates an exec.Cmd for the server's configured command.
func (m *Manager) buildCommand() (*exec.Cmd, error) {
	cmdPath, err := exec.LookPath(m.config.Command)
	if err != nil {
		return nil, fmt.Errorf("command not found: %s: %w", m.config.Command, err)
	}

	cmd := exec.Command(cmdPath, m.config.Args...)

	// Build environment: inherit current env + server-specific vars
	cmd.Env = m.buildEnv()

	return cmd, nil
}

// buildEnv creates the environment for the subprocess.
// If the server has no custom env vars, returns nil (inherits parent env).
// If it has custom vars, merges with parent env.
func (m *Manager) buildEnv() []string {
	if len(m.config.Env) == 0 {
		return nil // nil means inherit parent environment
	}
	// Start with parent environment
	env := os.Environ()
	// Add server-specific variables
	for key, value := range m.config.Env {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}
	return env
}
