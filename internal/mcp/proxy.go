// proxy.go provides the per-session MCP proxy handler.
//
// For each upstream MCP session, a new mcp.Server is created, a downstream
// subprocess is spawned via session.Manager, and tool handlers are registered
// that forward calls to the downstream ClientSession.
//
// Downstream-to-upstream notification relay:
//   - tools/list_changed: re-discovers tools from downstream and updates upstream server
//   - logging/message: relayed via ServerSession.Log()
//   - progress: relayed via ServerSession.NotifyProgress()
//
// Lifecycle safety:
//
//	proxySession uses two separate locks:
//	  - mu: guards upstreamSession, upstreamSessionID, and currentTools
//	  - downstreamMu: guards downstream pointer and downstreamClosed flag
//
//	Callers (tool handlers, notification relays) acquire downstreamMu.RLock to
//	snapshot the downstream pointer and check the closed flag.  closeDownstream
//	acquires the write lock to set the flag and nil the pointer atomically before
//	handing off to IO teardown. This prevents calls being dispatched after close
//	has begun, eliminating the "client is closing" error surfacing to callers.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jrede/vision/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrDownstreamUnavailable is returned when a tool call or notification relay
// is attempted after the downstream session has been closed.
var ErrDownstreamUnavailable = errors.New("downstream session unavailable")

// closeReasonHealthCheck is the canonical close reason used when the proactive
// health probe detects a dead downstream. Used to emit session.health_respawn
// events after a successful respawn.
const closeReasonHealthCheck = "health check failed"

type sharedToolCacheEntry struct {
	result    *mcp.CallToolResult
	expiresAt time.Time
	createdAt time.Time
}

type sharedToolInFlight struct {
	done   chan struct{}
	result *mcp.CallToolResult
	err    error
}

type inFlightLimiter struct {
	sem chan struct{}
}

func newInFlightLimiter(max int) *inFlightLimiter {
	if max <= 0 {
		return nil
	}
	return &inFlightLimiter{sem: make(chan struct{}, max)}
}

func (l *inFlightLimiter) acquire(ctx context.Context) (func(), error) {
	if l == nil {
		return func() {}, nil
	}
	select {
	case l.sem <- struct{}{}:
		return func() { <-l.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type sharedToolCoordinator struct {
	logger     *slog.Logger
	enabled    map[string]struct{}
	cacheTTL   time.Duration
	maxEntries int
	mu         sync.Mutex
	inflight   map[string]*sharedToolInFlight
	cache      map[string]sharedToolCacheEntry
}

func newSharedToolCoordinator(logger *slog.Logger, tools []string, cacheTTL time.Duration, maxEntries int) *sharedToolCoordinator {
	if len(tools) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	if maxEntries <= 0 {
		maxEntries = 128
	}
	enabled := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		tool = strings.TrimSpace(tool)
		if tool != "" {
			enabled[tool] = struct{}{}
		}
	}
	if len(enabled) == 0 {
		return nil
	}
	return &sharedToolCoordinator{
		logger:     logger,
		enabled:    enabled,
		cacheTTL:   cacheTTL,
		maxEntries: maxEntries,
		inflight:   make(map[string]*sharedToolInFlight),
		cache:      make(map[string]sharedToolCacheEntry),
	}
}

// ProxyConfig configures a per-session proxy handler.
type ProxyConfig struct {
	// ServerName is the name of the MCP server being proxied.
	ServerName string

	// SessionManager manages downstream subprocess lifecycle.
	SessionManager *session.Manager

	// Logger for proxy operations.
	Logger *slog.Logger

	// HealthCheckInterval is how often to probe idle downstream sessions.
	// 0 means no proactive health checking (reactive respawn only).
	HealthCheckInterval time.Duration

	// RequestTimeout is the default downstream tool-call deadline when the
	// upstream request has no earlier deadline.
	RequestTimeout time.Duration

	// RetryConfig controls retry behavior for retryable downstream failures.
	RetryConfig RetryConfig

	// CircuitBreakerConfig controls fast-fail behavior after repeated failures.
	CircuitBreakerConfig CircuitBreakerConfig

	// SharedReadOnlyTools lists tool names that are safe to coalesce/cache across
	// concurrent sessions for this server.
	SharedReadOnlyTools []string

	// SharedResultCacheTTL controls how long successful shared read-only results
	// stay cached. 0 disables caching while still allowing in-flight coalescing.
	SharedResultCacheTTL time.Duration

	// SharedResultCacheSize caps cached shared read-only results per server.
	// 0 uses the default size.
	SharedResultCacheSize int

	// MaxInFlightRequests caps concurrent downstream tool calls per server.
	// 0 means unlimited.
	MaxInFlightRequests int
}

// NewProxyHandler creates a StreamableHTTPHandler that proxies MCP requests
// to per-session downstream subprocesses.
//
// For each new upstream session:
//  1. A new mcp.Server is created (with HasTools: true)
//  2. A downstream subprocess is spawned via SessionManager.SpawnSession()
//  3. Tools are discovered from the downstream and registered as proxy handlers
//  4. The server is returned to handle the session
//  5. Notifications from downstream are relayed to the upstream client
func NewProxyHandler(cfg ProxyConfig) http.Handler {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	logger := cfg.Logger.With(slog.String("component", "proxy"), slog.String("server", cfg.ServerName))
	sharedTools := newSharedToolCoordinator(logger, cfg.SharedReadOnlyTools, cfg.SharedResultCacheTTL, cfg.SharedResultCacheSize)
	inFlightLimiter := newInFlightLimiter(cfg.MaxInFlightRequests)

	type sessionIndex struct {
		mu           sync.RWMutex
		byUpstream   map[string]*proxySession // upstream MCP session ID → proxySession
		byDownstream map[string]*proxySession // downstream manager session ID → proxySession
	}
	idx := &sessionIndex{
		byUpstream:   make(map[string]*proxySession),
		byDownstream: make(map[string]*proxySession),
	}

	// Register a callback so that any removal path (reaper, RemoveSession,
	// CloseAll) triggers closeDownstream on the proxy session, setting the
	// closed flag before the SDK connection is torn down.
	if cfg.SessionManager != nil {
		cfg.SessionManager.SetOnSessionRemoved(func(sessionID string) {
			idx.mu.RLock()
			ps := idx.byDownstream[sessionID]
			idx.mu.RUnlock()
			if ps != nil {
				ps.closeDownstream("session removed by manager")
			}
		})
	}

	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		srv, err := newPerSessionServer(
			r.Context(),
			cfg.ServerName,
			cfg.SessionManager,
			logger,
			sharedTools,
			inFlightLimiter,
			cfg.HealthCheckInterval,
			cfg.RequestTimeout,
			cfg.RetryConfig,
			cfg.CircuitBreakerConfig,
			func(upstreamSessionID string, ps *proxySession) {
				idx.mu.Lock()
				idx.byUpstream[upstreamSessionID] = ps
				idx.byDownstream[ps.sessionID] = ps
				idx.mu.Unlock()
			},
			func(upstreamSessionID string) {
				idx.mu.Lock()
				ps := idx.byUpstream[upstreamSessionID]
				delete(idx.byUpstream, upstreamSessionID)
				if ps != nil {
					delete(idx.byDownstream, ps.sessionID)
				}
				idx.mu.Unlock()
			},
			func(oldSessionID, newSessionID string, ps *proxySession) {
				idx.mu.Lock()
				delete(idx.byDownstream, oldSessionID)
				idx.byDownstream[newSessionID] = ps
				idx.mu.Unlock()
			},
		)
		if err != nil {
			logger.Warn("failed to create per-session proxy server",
				slog.String("error", err.Error()),
			)
			return nil
		}
		return srv
	}, nil)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") == "" && isInitializeRequest(r) && cfg.SessionManager != nil {
			if atCapacity, current, max := cfg.SessionManager.AdmissionStatus(); atCapacity {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"error": map[string]any{
						"code":    -32000,
						"message": fmt.Sprintf("max sessions reached: limit %d (current %d)", max, current),
					},
				})
				return
			}
		}

		sessionHeader := r.Header.Get("Mcp-Session-Id")
		if sessionHeader != "" {
			idx.mu.RLock()
			ps := idx.byUpstream[sessionHeader]
			idx.mu.RUnlock()
			if ps != nil {
				ps.touch()
			}
		}

		handler.ServeHTTP(w, r)

		if r.Method == http.MethodDelete && sessionHeader != "" {
			idx.mu.RLock()
			ps := idx.byUpstream[sessionHeader]
			idx.mu.RUnlock()
			if ps != nil {
				ps.closeDownstream("upstream delete")
				// Trigger the manager's removal path (kills subprocess, fires
				// reaper cleanup). closeDownstream already set the closed flag,
				// so the onSessionRemoved callback will be a no-op.
				if err := ps.mgr.RemoveSession(ps.sessionID); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
					logger.Warn("failed to remove session on delete",
						slog.String("error", err.Error()),
					)
				}
			}
		}
	})
}

func isInitializeRequest(r *http.Request) bool {
	if r.Body == nil {
		return false
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	var payload struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	return payload.Method == "initialize"
}

// proxySession holds the state for a single proxied session, used to relay
// notifications from downstream to the upstream ServerSession.
type proxySession struct {
	serverName string
	server     *mcp.Server
	sessionID  string
	mgr        *session.Manager
	logger     *slog.Logger
	onClosed   func(string)

	// onRespawn is called after a successful downstream respawn to update
	// external indexes (e.g., idx.byDownstream). It receives the old and new
	// downstream session IDs plus the proxySession pointer so the caller can
	// atomically swap the index entry without needing to look up by old key
	// (which may have been deleted by closeDownstream's onClosed callback).
	onRespawn func(oldSessionID, newSessionID string, ps *proxySession)

	// clientOpts are the MCP client options used for downstream connections,
	// retained so that respawnDownstream can create a new session with the
	// same notification handlers.
	clientOpts *mcp.ClientOptions

	// healthCheckInterval is the interval for proactive downstream health probes.
	// 0 means no proactive health checking.
	healthCheckInterval time.Duration
	requestTimeout      time.Duration
	retryConfig         RetryConfig
	circuitBreaker      *circuitBreaker
	sharedTools         *sharedToolCoordinator
	inFlightLimiter     *inFlightLimiter

	// healthProbeCancel stops the active health probe goroutine.
	// nil when no probe is running.
	healthProbeCancel context.CancelFunc

	// mu guards upstreamSession, upstreamSessionID, and currentTools.
	// InitializedHandler runs concurrently with getServer return.
	mu                sync.Mutex
	upstreamSession   *mcp.ServerSession
	upstreamSessionID string
	currentTools      map[string]struct{}
	closeReason       string
	closeMu           sync.Mutex
	closeOnce         sync.Once

	// downstreamMu guards downstream and downstreamClosed.
	// Use RLock to snapshot/check before IO; Lock to transition to closed.
	downstreamMu     sync.RWMutex
	downstream       *mcp.ClientSession
	downstreamClosed bool

	// respawnMu serializes respawn attempts so only one goroutine respawns
	// at a time. Other callers wait for the result.
	respawnMu sync.Mutex
}

// newPerSessionServer creates a new mcp.Server for a single upstream session.
// It spawns a downstream subprocess, discovers tools, registers proxy handlers,
// and sets up notification relay from downstream to upstream.
func newPerSessionServer(
	ctx context.Context,
	serverName string,
	mgr *session.Manager,
	logger *slog.Logger,
	sharedTools *sharedToolCoordinator,
	inFlightLimiter *inFlightLimiter,
	healthCheckInterval time.Duration,
	requestTimeout time.Duration,
	retryConfig RetryConfig,
	circuitBreakerConfig CircuitBreakerConfig,
	onInitialized func(string, *proxySession),
	onClosed func(string),
	onRespawn func(oldSessionID, newSessionID string, ps *proxySession),
) (*mcp.Server, error) {
	sessionID := fmt.Sprintf("proxy-%s-%d", serverName, nextSessionID())
	logger = logger.With(slog.String("session_id", sessionID))

	// Create the proxy session state that will be shared between the upstream
	// server and the downstream notification handlers.
	ps := &proxySession{
		serverName:          serverName,
		sessionID:           sessionID,
		mgr:                 mgr,
		logger:              logger,
		onClosed:            onClosed,
		onRespawn:           onRespawn,
		sharedTools:         sharedTools,
		inFlightLimiter:     inFlightLimiter,
		healthCheckInterval: healthCheckInterval,
		requestTimeout:      requestTimeout,
		retryConfig:         retryConfig,
		circuitBreaker:      newCircuitBreaker(circuitBreakerConfig, nil),
	}
	if ps.requestTimeout <= 0 {
		ps.requestTimeout = 30 * time.Second
	}
	if ps.retryConfig.MaxAttempts <= 0 {
		ps.retryConfig.MaxAttempts = 1
	}
	if ps.retryConfig.InitialDelay <= 0 {
		ps.retryConfig.InitialDelay = 100 * time.Millisecond
	}
	if ps.retryConfig.MaxDelay <= 0 {
		ps.retryConfig.MaxDelay = 5 * time.Second
	}
	if len(ps.retryConfig.RetryableErrors) == 0 {
		ps.retryConfig.RetryableErrors = []string{"timeout", "429", "502", "503", "econnreset", "econnrefused", "enetunreach"}
	}

	// Create the upstream server with tools capability advertised.
	server := mcp.NewServer(&mcp.Implementation{
		Name:    fmt.Sprintf("vision-proxy-%s", serverName),
		Version: "1.0.0",
	}, &mcp.ServerOptions{
		HasTools: true,
		// Capture the ServerSession when the upstream client completes initialization.
		InitializedHandler: func(_ context.Context, req *mcp.InitializedRequest) {
			ps.mu.Lock()
			ps.upstreamSession = req.Session
			ps.upstreamSessionID = req.Session.ID()
			ps.mu.Unlock()
			if onInitialized != nil {
				onInitialized(req.Session.ID(), ps)
			}
			logger.Debug("upstream session initialized",
				slog.String("upstream_session_id", req.Session.ID()),
			)
		},
	})
	ps.server = server

	// Build client options with notification relay handlers.
	clientOpts := &mcp.ClientOptions{
		ToolListChangedHandler: func(ctx context.Context, _ *mcp.ToolListChangedRequest) {
			ps.handleToolListChanged(ctx)
		},
		LoggingMessageHandler: func(ctx context.Context, req *mcp.LoggingMessageRequest) {
			ps.handleLoggingMessage(ctx, req.Params)
		},
		ProgressNotificationHandler: func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
			ps.handleProgress(ctx, req.Params)
		},
	}
	ps.clientOpts = clientOpts

	// Spawn downstream subprocess with notification handlers.
	downstream, err := mgr.SpawnSession(ctx, sessionID, clientOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to spawn downstream session: %w", err)
	}
	// ps is local here (not yet returned or indexed), so no lock needed.
	ps.downstream = downstream

	// Discover tools from downstream.
	toolsResult, err := downstream.ListTools(ctx, nil)
	if err != nil {
		ps.closeDownstream("initial tools/list failed")
		return nil, fmt.Errorf("failed to list downstream tools: %w", err)
	}

	current := make(map[string]struct{}, len(toolsResult.Tools))
	for _, t := range toolsResult.Tools {
		current[t.Name] = struct{}{}
	}
	ps.mu.Lock()
	ps.currentTools = current
	ps.mu.Unlock()

	// Register proxy tool handlers that forward to the downstream.
	for _, tool := range toolsResult.Tools {
		server.AddTool(tool, makeProxyToolHandler(ps, tool.Name))
	}

	logger.Info("proxy session established",
		slog.Int("tools", len(toolsResult.Tools)),
	)

	// Start proactive health probe if configured.
	ps.startHealthProbe()

	return server, nil
}

// handleToolListChanged is called when the downstream server notifies that
// its tool list has changed. It re-discovers tools and updates the upstream
// server, which automatically sends tools/list_changed to the upstream client.
func (ps *proxySession) handleToolListChanged(ctx context.Context) {
	ps.logger.Info("downstream tools/list_changed, re-discovering tools")
	ps.touch()

	// Snapshot downstream under read lock; attempt respawn if closed.
	ps.downstreamMu.RLock()
	ds := ps.downstream
	closed := ps.downstreamClosed
	ps.downstreamMu.RUnlock()

	if closed || ds == nil {
		ps.logger.Info("downstream closed during tool list refresh, attempting respawn")
		_, err := ps.respawnDownstream(ctx, "notification_relay")
		if err != nil {
			ps.logger.Warn("respawn failed during tool list refresh",
				slog.String("error", err.Error()),
			)
			return
		}
		// respawnDownstream already re-discovered tools and updated registrations,
		// so we can return early — the tool list is already current.
		ps.logger.Info("downstream respawned during tool list refresh")
		return
	}

	// Re-discover tools from downstream.
	toolsResult, err := ds.ListTools(ctx, nil)
	if err != nil {
		if isDownstreamClosureError(err) {
			ps.logger.Debug("downstream closed during tool list refresh")
			return
		}
		ps.logger.Error("failed to re-list downstream tools", slog.String("error", err.Error()))
		return
	}

	newToolNames := make(map[string]struct{}, len(toolsResult.Tools))
	for _, t := range toolsResult.Tools {
		newToolNames[t.Name] = struct{}{}
	}

	for _, tool := range toolsResult.Tools {
		ps.server.AddTool(tool, makeProxyToolHandler(ps, tool.Name))
	}

	ps.mu.Lock()
	oldToolNames := ps.currentTools
	ps.currentTools = newToolNames
	ps.mu.Unlock()

	if len(oldToolNames) > 0 {
		toRemove := make([]string, 0)
		for name := range oldToolNames {
			if _, ok := newToolNames[name]; !ok {
				toRemove = append(toRemove, name)
			}
		}
		if len(toRemove) > 0 {
			ps.server.RemoveTools(toRemove...)
			ps.logger.Info("removed stale upstream tools",
				slog.Int("removed", len(toRemove)),
			)
		}
	}

	ps.logger.Info("tools re-discovered",
		slog.Int("tools", len(toolsResult.Tools)),
	)
}

// handleLoggingMessage relays a logging notification from downstream to upstream.
func (ps *proxySession) handleLoggingMessage(ctx context.Context, params *mcp.LoggingMessageParams) {
	ps.touch()
	ps.mu.Lock()
	ss := ps.upstreamSession
	ps.mu.Unlock()

	if ss == nil {
		ps.logger.Debug("dropping logging message: no upstream session yet")
		return
	}

	if err := ss.Log(ctx, params); err != nil {
		ps.logger.Warn("failed to relay logging message",
			slog.String("error", err.Error()),
		)
	}
}

// handleProgress relays a progress notification from downstream to upstream.
func (ps *proxySession) handleProgress(ctx context.Context, params *mcp.ProgressNotificationParams) {
	ps.touch()
	ps.mu.Lock()
	ss := ps.upstreamSession
	ps.mu.Unlock()

	if ss == nil {
		ps.logger.Debug("dropping progress notification: no upstream session yet")
		return
	}

	if err := ss.NotifyProgress(ctx, params); err != nil {
		ps.logger.Warn("failed to relay progress notification",
			slog.String("error", err.Error()),
		)
	}
}

// makeProxyToolHandler creates a ToolHandler that forwards calls to the downstream ClientSession.
// If the downstream is unavailable (reaped, crashed), it attempts a single respawn before failing.
func makeProxyToolHandler(ps *proxySession, toolName string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ps.touch()
		ps.logger.Debug("proxying tool call",
			slog.String("tool", toolName),
		)

		if ps.sharedTools != nil && ps.sharedTools.enabledFor(toolName) {
			return ps.sharedTools.execute(ctx, toolName, req.Params.Arguments, func() (*mcp.CallToolResult, error) {
				return ps.callDownstreamTool(ctx, req, toolName)
			})
		}

		return ps.callDownstreamTool(ctx, req, toolName)
	}
}

func (ps *proxySession) callDownstreamTool(ctx context.Context, req *mcp.CallToolRequest, toolName string) (*mcp.CallToolResult, error) {
	if ps.circuitBreaker != nil && !ps.circuitBreaker.allow() {
		err := &CircuitOpenError{Server: ps.serverName, RetryIn: ps.circuitBreaker.retryIn()}
		ps.logger.Warn("circuit breaker open, failing fast",
			slog.String("tool", toolName),
			slog.Duration("retry_in", err.RetryIn),
		)
		return nil, classifyToolCallError(err, false, ps.retryConfig.RetryableErrors)
	}

	release, err := ps.inFlightLimiter.acquire(ctx)
	if err != nil {
		return nil, classifyToolCallError(err, false, ps.retryConfig.RetryableErrors)
	}
	defer release()

	var lastErr error
	requestCtx, requestCancel := withRequestTimeoutBudget(ctx, ps.requestTimeout)
	defer requestCancel()

	for attempt := 1; attempt <= ps.retryConfig.MaxAttempts; attempt++ {
		ps.downstreamMu.RLock()
		ds := ps.downstream
		closed := ps.downstreamClosed
		ps.downstreamMu.RUnlock()

		if closed || ds == nil {
			ps.logger.Info("downstream unavailable, attempting respawn",
				slog.String("tool", toolName),
				slog.Int("attempt", attempt),
			)
			var err error
			ds, err = ps.respawnDownstream(requestCtx, "tool_call")
			if err != nil {
				lastErr = ErrDownstreamUnavailable
			} else {
				ps.logger.Info("downstream respawned successfully",
					slog.String("tool", toolName),
					slog.Int("attempt", attempt),
				)
				result, err := ds.CallTool(requestCtx, &mcp.CallToolParams{
					Name:      req.Params.Name,
					Arguments: req.Params.Arguments,
				})
				if err == nil {
					ps.circuitBreaker.recordSuccess()
					return result, nil
				}
				if isDownstreamClosureError(err) {
					lastErr = ErrDownstreamUnavailable
				} else {
					lastErr = err
				}
			}
		} else {
			result, err := ds.CallTool(requestCtx, &mcp.CallToolParams{
				Name:      req.Params.Name,
				Arguments: req.Params.Arguments,
			})
			if err == nil {
				ps.circuitBreaker.recordSuccess()
				return result, nil
			}
			if isDownstreamClosureError(err) {
				ps.logger.Debug("downstream closed during tool call",
					slog.String("tool", toolName),
					slog.Int("attempt", attempt),
				)
				lastErr = ErrDownstreamUnavailable
			} else {
				lastErr = err
			}
		}

		if !isRetryableToolCallError(lastErr, ps.retryConfig.RetryableErrors) || attempt == ps.retryConfig.MaxAttempts {
			break
		}

		delay := computeBackoffDelay(attempt, ps.retryConfig.InitialDelay, ps.retryConfig.MaxDelay)
		ps.logger.Warn("retrying downstream tool call",
			slog.String("tool", toolName),
			slog.Int("attempt", attempt),
			slog.Duration("backoff", delay),
			slog.String("error", lastErr.Error()),
		)
		select {
		case <-requestCtx.Done():
			return nil, classifyToolCallError(requestCtx.Err(), false, ps.retryConfig.RetryableErrors)
		case <-time.After(delay):
		}
	}

	classifiedErr := classifyToolCallError(lastErr, true, ps.retryConfig.RetryableErrors)
	if shouldRecordCircuitFailure(lastErr, ps.retryConfig.RetryableErrors) {
		ps.circuitBreaker.recordFailure()
	}
	ps.logger.Error("downstream tool call failed",
		slog.String("tool", toolName),
		slog.String("error", classifiedErr.Error()),
	)
	return nil, classifiedErr
}

func (c *sharedToolCoordinator) enabledFor(tool string) bool {
	if c == nil {
		return false
	}
	_, ok := c.enabled[tool]
	return ok
}

func (c *sharedToolCoordinator) execute(ctx context.Context, tool string, args any, fn func() (*mcp.CallToolResult, error)) (*mcp.CallToolResult, error) {
	if c == nil || !c.enabledFor(tool) {
		return fn()
	}

	key, err := sharedToolCacheKey(tool, args)
	if err != nil {
		return fn()
	}

	now := time.Now()
	c.mu.Lock()
	if entry, ok := c.cache[key]; ok {
		if c.cacheTTL > 0 && now.Before(entry.expiresAt) {
			c.mu.Unlock()
			c.logger.Debug("shared tool cache hit",
				slog.String("tool", tool),
			)
			return entry.result, nil
		}
		delete(c.cache, key)
	}
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-call.done:
			return call.result, call.err
		}
	}

	call := &sharedToolInFlight{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	result, callErr := fn()

	c.mu.Lock()
	delete(c.inflight, key)
	if callErr == nil && result != nil && c.cacheTTL > 0 {
		c.cache[key] = sharedToolCacheEntry{
			result:    result,
			expiresAt: time.Now().Add(c.cacheTTL),
			createdAt: time.Now(),
		}
		c.trimCacheLocked()
	}
	call.result = result
	call.err = callErr
	close(call.done)
	c.mu.Unlock()

	return result, callErr
}

func (c *sharedToolCoordinator) trimCacheLocked() {
	if c.maxEntries <= 0 {
		return
	}
	for len(c.cache) > c.maxEntries {
		var oldestKey string
		var oldestTime time.Time
		first := true
		for key, entry := range c.cache {
			if first || entry.createdAt.Before(oldestTime) {
				oldestKey = key
				oldestTime = entry.createdAt
				first = false
			}
		}
		if oldestKey == "" {
			return
		}
		delete(c.cache, oldestKey)
	}
}

func sharedToolCacheKey(tool string, args any) (string, error) {
	payload, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	return tool + ":" + string(payload), nil
}

// respawnDownstream attempts to create a new downstream session after the
// previous one was closed (reaped, crashed, etc.). It is serialized via
// respawnMu so that concurrent tool calls don't spawn multiple subprocesses.
//
// On success the proxySession's downstream pointer is updated and tools are
// re-discovered. Returns the new ClientSession or an error.
func (ps *proxySession) respawnDownstream(ctx context.Context, trigger string) (*mcp.ClientSession, error) {
	ps.respawnMu.Lock()
	defer ps.respawnMu.Unlock()

	// Double-check: another goroutine may have already respawned while we waited.
	ps.downstreamMu.RLock()
	if !ps.downstreamClosed && ps.downstream != nil {
		ds := ps.downstream
		ps.downstreamMu.RUnlock()
		return ds, nil
	}
	ps.downstreamMu.RUnlock()

	// Use a bounded context for the respawn attempt.
	spawnCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	newSessionID := fmt.Sprintf("%s-respawn-%d", ps.sessionID, nextSessionID())
	ps.mu.Lock()
	previousCloseReason := ps.closeReason
	ps.mu.Unlock()
	ps.logger.Info("respawning downstream session",
		slog.String("event", "session.respawn_start"),
		slog.String("trigger", trigger),
		slog.String("new_session_id", newSessionID),
	)

	downstream, err := ps.mgr.SpawnSession(spawnCtx, newSessionID, ps.clientOpts)
	if err != nil {
		ps.logger.Warn("downstream respawn failed",
			slog.String("event", "session.respawn_failed"),
			slog.String("trigger", trigger),
			slog.String("new_session_id", newSessionID),
			slog.String("error", err.Error()),
		)
		return nil, fmt.Errorf("respawn spawn failed: %w", err)
	}

	// Re-discover tools from the new downstream.
	toolsResult, err := downstream.ListTools(spawnCtx, nil)
	if err != nil {
		// Best-effort close of the just-spawned session.
		_ = ps.mgr.RemoveSession(newSessionID)
		return nil, fmt.Errorf("respawn tools/list failed: %w", err)
	}

	// Update tool registrations on the upstream server.
	newToolNames := make(map[string]struct{}, len(toolsResult.Tools))
	for _, t := range toolsResult.Tools {
		newToolNames[t.Name] = struct{}{}
	}
	for _, tool := range toolsResult.Tools {
		ps.server.AddTool(tool, makeProxyToolHandler(ps, tool.Name))
	}

	// Atomically swap the downstream pointer and reset the closed flag.
	// Reset closeOnce so that the new downstream can be closed cleanly later.
	ps.downstreamMu.Lock()
	ps.downstream = downstream
	ps.downstreamClosed = false
	ps.downstreamMu.Unlock()
	ps.closeMu.Lock()
	ps.closeOnce = sync.Once{}
	ps.closeMu.Unlock()

	// Update the session ID so that touch/close operate on the new session.
	oldSessionID := ps.sessionID
	ps.sessionID = newSessionID

	ps.mu.Lock()
	ps.currentTools = newToolNames
	ps.mu.Unlock()

	// Update external indexes BEFORE cleaning up the old session. This order
	// is critical: RemoveSession fires onSessionRemoved which looks up
	// idx.byDownstream. If the old key still exists, the callback would call
	// closeDownstream on the NEW downstream, breaking the just-respawned session.
	if ps.onRespawn != nil {
		ps.onRespawn(oldSessionID, newSessionID, ps)
	}

	// Clean up the old session from the manager to prevent admission counter
	// leaks. The old subprocess is already dead (reaped/crashed), but its
	// TrackedSession entry may still occupy a slot. Because onRespawn already
	// removed the old key from idx.byDownstream, the onSessionRemoved callback
	// will be a no-op (it won't find the proxy session by the old ID).
	if err := ps.mgr.RemoveSession(oldSessionID); err != nil {
		// Not fatal — the old session may have already been removed by the reaper.
		if !errors.Is(err, session.ErrSessionNotFound) {
			ps.logger.Warn("failed to remove old session after respawn",
				slog.String("old_session", oldSessionID),
				slog.String("error", err.Error()),
			)
		}
	}

	ps.logger.Info("downstream respawn complete",
		slog.String("event", "session.respawn"),
		slog.String("trigger", trigger),
		slog.String("old_session", oldSessionID),
		slog.String("new_session", newSessionID),
		slog.Int("tools", len(toolsResult.Tools)),
	)
	if previousCloseReason == closeReasonHealthCheck {
		ps.logger.Info("downstream respawn followed health probe closure",
			slog.String("event", "session.health_respawn"),
			slog.String("trigger", trigger),
			slog.String("old_session", oldSessionID),
			slog.String("new_session", newSessionID),
		)
	}

	// Start a new health probe for the respawned downstream.
	ps.startHealthProbe()

	return downstream, nil
}

func (ps *proxySession) touch() {
	if ps == nil || ps.mgr == nil {
		return
	}
	ps.mgr.TouchSession(ps.sessionID)
}

func (ps *proxySession) closeDownstream(reason string) {
	if ps == nil || ps.mgr == nil {
		return
	}

	ps.closeMu.Lock()
	defer ps.closeMu.Unlock()

	ps.closeOnce.Do(func() {
		ps.mu.Lock()
		ps.closeReason = reason
		ps.mu.Unlock()
		ps.logger.Info("closing proxy session downstream",
			slog.String("reason", reason),
		)

		// Stop the health probe before tearing down the downstream.
		ps.stopHealthProbe()

		// Atomically mark closed and detach the downstream pointer so that any
		// concurrent tool calls see the closed state without dispatching to a
		// closing SDK connection.
		ps.downstreamMu.Lock()
		ps.downstreamClosed = true
		ps.downstream = nil
		ps.downstreamMu.Unlock()

		// Clean up index maps via the onClosed callback.
		ps.mu.Lock()
		upstreamID := ps.upstreamSessionID
		onClosed := ps.onClosed
		ps.mu.Unlock()
		if onClosed != nil && upstreamID != "" {
			onClosed(upstreamID)
		}
	})
}

// isDownstreamClosureError reports whether err is an SDK-level connection
// closure error that Vision normalizes to ErrDownstreamUnavailable.
// Context errors (Canceled, DeadlineExceeded) are NOT matched — those are
// caller-side cancellations and should propagate as-is.
func isDownstreamClosureError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "client is closing")
}

// sessionCounter provides unique session IDs via atomic increment.
var sessionCounter uint64
var sessionCounterMu sync.Mutex

func nextSessionID() uint64 {
	sessionCounterMu.Lock()
	defer sessionCounterMu.Unlock()
	sessionCounter++
	return sessionCounter
}

// startHealthProbe starts a background goroutine that periodically probes the
// downstream session with a tools/list call. If the probe fails consecutively
// (3 times), it triggers closeDownstream so the next tool call will respawn.
// This detects dead subprocesses before a tool call hits the failure path.
func (ps *proxySession) startHealthProbe() {
	if ps.healthCheckInterval <= 0 {
		return
	}

	// Stop any existing probe before starting a new one.
	ps.stopHealthProbe()

	ctx, cancel := context.WithCancel(context.Background())
	ps.mu.Lock()
	ps.healthProbeCancel = cancel
	ps.mu.Unlock()

	go func() {
		ticker := time.NewTicker(ps.healthCheckInterval)
		defer ticker.Stop()

		consecutiveFails := 0
		const failureThreshold = 3

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ps.downstreamMu.RLock()
				ds := ps.downstream
				closed := ps.downstreamClosed
				ps.downstreamMu.RUnlock()

				if closed || ds == nil {
					// Already closed; probe is no longer needed.
					return
				}

				probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
				_, err := ds.ListTools(probeCtx, nil)
				probeCancel()

				if err != nil {
					consecutiveFails++
					ps.logger.Debug("health probe failed",
						slog.Int("consecutive_fails", consecutiveFails),
						slog.String("error", err.Error()),
					)

					if consecutiveFails >= failureThreshold {
						ps.logger.Warn("health probe threshold exceeded, closing downstream",
							slog.String("event", "session.health_check_failed"),
							slog.Int("consecutive_failures", consecutiveFails),
						)
						ps.closeDownstream(closeReasonHealthCheck)
						return
					}
				} else {
					if consecutiveFails > 0 {
						ps.logger.Debug("health probe recovered",
							slog.Int("previous_fails", consecutiveFails),
						)
					}
					consecutiveFails = 0
				}
			}
		}
	}()

	ps.logger.Debug("health probe started",
		slog.Duration("interval", ps.healthCheckInterval),
	)
}

// stopHealthProbe cancels the active health probe goroutine, if any.
func (ps *proxySession) stopHealthProbe() {
	ps.mu.Lock()
	cancel := ps.healthProbeCancel
	ps.healthProbeCancel = nil
	ps.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
