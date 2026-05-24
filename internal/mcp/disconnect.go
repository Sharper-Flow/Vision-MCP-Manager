package mcp

import (
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DisconnectTracker tracks active HTTP connections per upstream MCP session ID
// and triggers deferred session cleanup after a configurable grace period when
// all connections for a session close.
//
// Only used for shared-mode servers. All refcount and timer operations are
// serialized under a single mutex to eliminate TOCTOU races.
type DisconnectTracker struct {
	serverName  string
	gracePeriod time.Duration
	logger      *slog.Logger
	stopped     bool

	mu       sync.Mutex
	sessions map[string]*connTracker // upstream session ID → state

	// lookup finds the proxySession for an upstream session ID (read-only from idx).
	lookup func(string) *proxySession

	// onReap is called when the grace period expires. Defaults to looking up
	// the proxySession and calling closeDownstream. Overridable for testing.
	onReap func(string)
}

// connTracker holds per-session connection state.
type connTracker struct {
	refCount int
	timer    *time.Timer
}

// newDisconnectTracker creates a new DisconnectTracker.
func newDisconnectTracker(
	serverName string,
	gracePeriod time.Duration,
	logger *slog.Logger,
	lookup func(string) *proxySession,
) *DisconnectTracker {
	if logger == nil {
		logger = slog.Default()
	}
	dt := &DisconnectTracker{
		serverName:  serverName,
		gracePeriod: gracePeriod,
		logger:      logger.With(slog.String("component", "disconnect_tracker"), slog.String("server", serverName)),
		sessions:    make(map[string]*connTracker),
		lookup:      lookup,
	}
	dt.onReap = dt.defaultReap
	return dt
}

// Wrap returns middleware that tracks long-lived HTTP stream lifecycle per session.
func (dt *DisconnectTracker) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !shouldTrackDisconnect(r) {
			next.ServeHTTP(w, r)
			return
		}

		sessionID := r.Header.Get("Mcp-Session-Id")
		dt.trackRequest(sessionID)
		defer dt.releaseConnection(sessionID)

		next.ServeHTTP(w, r)
	})
}

// shouldTrackDisconnect reports whether a request represents a long-lived
// client receive stream whose closure is a useful disconnect signal.
//
// Normal POST requests are deliberately excluded. For incoming Go HTTP
// requests, r.Context() is cancelled when ServeHTTP returns, so tracking POSTs
// would treat ordinary request completion as client/session disconnect.
func shouldTrackDisconnect(r *http.Request) bool {
	if r.Header.Get("Mcp-Session-Id") == "" {
		return false
	}
	if r.Method != http.MethodGet {
		return false
	}
	return acceptsEventStream(r)
}

func acceptsEventStream(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		if strings.EqualFold(mediaType, "text/event-stream") {
			return true
		}
	}
	return false
}

// trackRequest increments the connection count for a session and cancels any
// pending grace timer (reconnect during grace period).
func (dt *DisconnectTracker) trackRequest(sessionID string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	if dt.stopped {
		return
	}

	ct, exists := dt.sessions[sessionID]
	if !exists {
		ct = &connTracker{}
		dt.sessions[sessionID] = ct
	}

	ct.refCount++

	// Cancel any pending grace timer (reconnect during grace period)
	if ct.timer != nil {
		ct.timer.Stop()
		ct.timer = nil

		dt.logger.Info("disconnect grace period cancelled by reconnect",
			slog.String("event", "session.disconnect_cancelled"),
			slog.String("upstream_session_id", sessionID),
		)
	}
}

// releaseConnection decrements the connection count for a session. If the count
// reaches 0, starts the grace period timer.
func (dt *DisconnectTracker) releaseConnection(sessionID string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	ct, exists := dt.sessions[sessionID]
	if !exists {
		return
	}

	ct.refCount--
	if ct.refCount <= 0 {
		ct.refCount = 0

		if dt.stopped {
			return
		}

		// Start grace period timer
		if ct.timer != nil {
			ct.timer.Stop()
		}
		sid := sessionID // capture for closure
		ct.timer = time.AfterFunc(dt.gracePeriod, func() {
			dt.onReap(sid)
		})

		dt.logger.Info("all connections closed, grace period started",
			slog.String("event", "session.disconnect_detected"),
			slog.String("upstream_session_id", sessionID),
			slog.Duration("grace_period", dt.gracePeriod),
		)
	}
}

// HandleDelete cancels any pending grace timer for a session and removes it
// from tracking. Called when DELETE arrives for a session.
func (dt *DisconnectTracker) HandleDelete(sessionID string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	ct, exists := dt.sessions[sessionID]
	if !exists {
		return
	}

	if ct.timer != nil {
		ct.timer.Stop()
		dt.logger.Info("disconnect grace timer cancelled by DELETE",
			slog.String("event", "session.disconnect_cleanup"),
			slog.String("upstream_session_id", sessionID),
		)
	}

	delete(dt.sessions, sessionID)
}

// Stop cancels all pending grace timers and prevents new tracking.
func (dt *DisconnectTracker) Stop() {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.stopped = true

	for sid, ct := range dt.sessions {
		if ct.timer != nil {
			ct.timer.Stop()
			ct.timer = nil
		}
		delete(dt.sessions, sid)
	}
}

// refCount returns the current connection count for a session (testing only).
func (dt *DisconnectTracker) refCount(sessionID string) int {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	ct, exists := dt.sessions[sessionID]
	if !exists {
		return 0
	}
	return ct.refCount
}

// defaultReap is called when the grace period expires. It looks up the
// proxySession and calls closeDownstream.
func (dt *DisconnectTracker) defaultReap(sessionID string) {
	dt.mu.Lock()
	delete(dt.sessions, sessionID)
	dt.mu.Unlock()

	if dt.lookup == nil {
		return
	}

	ps := dt.lookup(sessionID)
	if ps == nil {
		return
	}

	dt.logger.Info("disconnect grace period expired, reaping session",
		slog.String("event", "session.disconnect_reaped"),
		slog.String("upstream_session_id", sessionID),
		slog.String("server", dt.serverName),
	)

	ps.closeDownstream("client_disconnected")
}
