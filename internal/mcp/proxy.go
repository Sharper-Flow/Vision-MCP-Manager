// proxy.go provides the per-session MCP proxy handler.
//
// For each upstream MCP session, a new mcp.Server is created, a downstream
// subprocess is spawned via session.Manager, and tool handlers are registered
// that forward calls to the downstream ClientSession.
//
// Routing modes:
//   - Fixed manager: ProxyConfig.SessionManager is set. Every upstream session
//     spawns a downstream through that single manager. Used by single-server
//     stdio proxies.
//   - Slot-group selector: ProxyConfig.Selector is set. On each new upstream
//     session, the selector picks which underlying manager should own the
//     session (least-loaded healthy slot). The chosen manager is stored on the
//     resulting proxySession so pre-dispatch respawn stays on the same slot.
//     Used by the virtual group listener built by the daemon for
//     transparent multi-slot routing.
//
// Exactly one of SessionManager or Selector must be set; NewProxyHandler panics
// otherwise.
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

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
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

	// Selector chooses which session manager should own a new upstream session.
	// Used by slot groups for transparent routing. When nil, SessionManager is
	// used directly.
	Selector ManagerSelector

	// SharedManager manages a single downstream subprocess shared across all
	// upstream sessions for stateless MCP servers. When set, SessionManager and
	// Selector must both be nil.
	SharedManager *session.SharedSessionManager

	// Logger for proxy operations.
	Logger *slog.Logger

	// SuggestionProvider supplies alternative tool suggestions when downstream
	// availability failures are returned to the upstream client as tool results.
	SuggestionProvider FallbackSuggestionProvider

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

	// DisconnectGracePeriod is the time to wait after the last HTTP connection
	// closes before removing a shared-mode upstream session. 0 disables disconnect
	// detection. Set from ServerConfig.ResolvedDisconnectGracePeriod().
	DisconnectGracePeriod time.Duration

	// Metrics tracks per-server session lifecycle counters. Optional; nil = no metrics.
	Metrics metrics.ServerMetricsReporter
}

type reachabilityAwareHandler struct {
	handler  http.Handler
	setStore func(*reachability.Store)
}

func (h *reachabilityAwareHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.handler.ServeHTTP(w, r)
}

// SetReachabilityStore configures the optional store for current and future
// downstream probes. A nil store disables reporting.
func (h *reachabilityAwareHandler) SetReachabilityStore(store *reachability.Store) {
	if h == nil || h.setStore == nil {
		return
	}
	h.setStore(store)
}

// hasExactlyOneManagerSource reports whether exactly one of SessionManager,
// SharedManager, or Selector is set. NewProxyHandler requires this invariant.
func (cfg ProxyConfig) hasExactlyOneManagerSource() bool {
	hasManager := cfg.SessionManager != nil
	hasShared := cfg.SharedManager != nil
	hasSelector := cfg.Selector != nil
	count := 0
	if hasManager {
		count++
	}
	if hasShared {
		count++
	}
	if hasSelector {
		count++
	}
	return count == 1
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
	if !cfg.hasExactlyOneManagerSource() {
		panic("mcp: exactly one of SessionManager, SharedManager, or Selector must be set")
	}
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
	var reachabilityMu sync.RWMutex
	var reachabilityStore *reachability.Store
	getReachabilityStore := func() *reachability.Store {
		reachabilityMu.RLock()
		defer reachabilityMu.RUnlock()
		return reachabilityStore
	}
	setReachabilityStore := func(store *reachability.Store) {
		reachabilityMu.Lock()
		reachabilityStore = store
		reachabilityMu.Unlock()
		if cfg.SharedManager != nil {
			cfg.SharedManager.SetReachabilityStore(store)
		}
		idx.mu.RLock()
		sessions := make([]*proxySession, 0, len(idx.byUpstream))
		for _, ps := range idx.byUpstream {
			sessions = append(sessions, ps)
		}
		idx.mu.RUnlock()
		for _, ps := range sessions {
			ps.SetReachabilityStore(store)
		}
	}

	// Disconnect tracker for shared-mode servers: detects client disconnect
	// via r.Context().Done() and reaps stale sessions after a grace period.
	var tracker *DisconnectTracker
	if cfg.SharedManager != nil && cfg.DisconnectGracePeriod > 0 {
		tracker = newDisconnectTracker(
			cfg.ServerName,
			cfg.DisconnectGracePeriod,
			logger,
			func(sid string) *proxySession {
				idx.mu.RLock()
				ps := idx.byUpstream[sid]
				idx.mu.RUnlock()
				return ps
			},
		)
	}

	// Register a callback so that any removal path (reaper, RemoveSession,
	// CloseAll) triggers closeDownstream on the proxy session, setting the
	// closed flag before the SDK connection is torn down.
	// SharedManager handles its own lifecycle; no callback needed.
	if cfg.SessionManager != nil {
		cfg.SessionManager.SetOnSessionRemoved(func(sessionID string) {
			idx.mu.RLock()
			ps := idx.byDownstream[sessionID]
			idx.mu.RUnlock()
			if ps != nil {
				ps.closeDownstream("session removed by manager")
			}
		})
	} else if cfg.Selector != nil {
		cfg.Selector.SetOnSessionRemoved(func(sessionID string) {
			idx.mu.RLock()
			ps := idx.byDownstream[sessionID]
			idx.mu.RUnlock()
			if ps != nil {
				ps.closeDownstream("session removed by manager")
			}
		})
	}

	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		// --- Shared mode ---
		if cfg.SharedManager != nil {
			srv, err := newSharedModeServer(
				r.Context(),
				cfg.ServerName,
				cfg.SharedManager,
				logger,
				sharedTools,
				inFlightLimiter,
				cfg.SuggestionProvider,
				cfg.RequestTimeout,
				cfg.RetryConfig,
				cfg.CircuitBreakerConfig,
				cfg.Metrics,
				getReachabilityStore,
				func(upstreamSessionID string, ps *proxySession) {
					ps.SetReachabilityStore(getReachabilityStore())
					idx.mu.Lock()
					idx.byUpstream[upstreamSessionID] = ps
					idx.mu.Unlock()
				},
				func(upstreamSessionID string) {
					idx.mu.Lock()
					delete(idx.byUpstream, upstreamSessionID)
					idx.mu.Unlock()
				},
			)
			if err != nil {
				logger.Warn("failed to create shared-mode proxy server",
					slog.String("error", err.Error()),
				)
				return nil
			}
			return srv
		}

		// --- Stateful mode (fixed manager or selector) ---
		mgr := cfg.SessionManager
		pendingKey := ""
		if cfg.Selector != nil {
			pendingKey = fmt.Sprintf("selector-%s-%d", cfg.ServerName, nextSessionID())
			selectedMgr, err := cfg.Selector.SelectForNewSession(r.Context(), pendingKey)
			if err != nil {
				logger.Warn("failed to select session manager",
					slog.String("error", err.Error()),
				)
				return nil
			}
			mgr = selectedMgr
		}

		srv, err := newPerSessionServer(
			r.Context(),
			cfg.ServerName,
			mgr,
			logger,
			sharedTools,
			inFlightLimiter,
			cfg.SuggestionProvider,
			cfg.HealthCheckInterval,
			cfg.RequestTimeout,
			cfg.RetryConfig,
			cfg.CircuitBreakerConfig,
			cfg.Metrics,
			getReachabilityStore,
			func(upstreamSessionID string, ps *proxySession) {
				ps.SetReachabilityStore(getReachabilityStore())
				if cfg.Selector != nil && pendingKey != "" {
					cfg.Selector.Rebind(pendingKey, upstreamSessionID)
				}
				idx.mu.Lock()
				idx.byUpstream[upstreamSessionID] = ps
				idx.byDownstream[ps.sessionID] = ps
				idx.mu.Unlock()
			},
			func(upstreamSessionID string) {
				if cfg.Selector != nil {
					if upstreamSessionID != "" {
						cfg.Selector.Release(upstreamSessionID)
					} else if pendingKey != "" {
						cfg.Selector.Release(pendingKey)
					}
				}
				idx.mu.Lock()
				ps := idx.byUpstream[upstreamSessionID]
				delete(idx.byUpstream, upstreamSessionID)
				if ps != nil {
					delete(idx.byDownstream, ps.sessionID)
				}
				idx.mu.Unlock()
			},
			func(oldSessionID, newSessionID string, ps *proxySession) {
				ps.mu.Lock()
				upstreamID := ps.upstreamSessionID
				ps.mu.Unlock()
				idx.mu.Lock()
				delete(idx.byDownstream, oldSessionID)
				idx.byDownstream[newSessionID] = ps
				// Re-register in byUpstream. After a health-probe-triggered
				// closeDownstream, the onClosed callback removes from byUpstream,
				// but the go-sdk still has the session internally. Once respawn
				// completes, the proxy session is fully functional again.
				if upstreamID != "" {
					idx.byUpstream[upstreamID] = ps
				}
				idx.mu.Unlock()
			},
		)
		if err != nil {
			if cfg.Selector != nil && pendingKey != "" {
				cfg.Selector.ReportSpawnResult(pendingKey, err)
				cfg.Selector.Release(pendingKey)
			}
			logger.Warn("failed to create per-session proxy server",
				slog.String("error", err.Error()),
			)
			return nil
		}
		if cfg.Selector != nil && pendingKey != "" {
			cfg.Selector.ReportSpawnResult(pendingKey, nil)
		}
		return srv
	}, nil)

	outerHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") == "" && isInitializeRequest(r) {
			var admission AdmissionStatuser
			if cfg.Selector != nil {
				admission = cfg.Selector
			} else if cfg.SharedManager != nil {
				admission = cfg.SharedManager
			} else {
				admission = cfg.SessionManager
			}
			if atCapacity, current, max := admission.AdmissionStatus(); atCapacity {
				if cfg.Metrics != nil {
					cfg.Metrics.IncAdmissionDenied()
				}
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
				// In shared mode, closeDownstream already handles RemoveSession +
				// Unsubscribe. In stateful mode, trigger the manager's removal
				// path (kills subprocess, fires reaper cleanup). closeDownstream
				// already set the closed flag, so the onSessionRemoved callback
				// will be a no-op.
				if !ps.shared && ps.mgr != nil {
					if err := ps.mgr.RemoveSession(ps.sessionID); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
						logger.Warn("failed to remove session on delete",
							slog.String("error", err.Error()),
						)
					}
				}
			}
			// Cancel any pending disconnect grace timer for this session.
			if tracker != nil {
				tracker.HandleDelete(sessionHeader)
			}
		}
	})

	// For shared mode with disconnect detection, wrap the outer handler.
	resultHandler := http.Handler(outerHandler)
	if tracker != nil {
		resultHandler = tracker.Wrap(resultHandler)
	}
	return &reachabilityAwareHandler{handler: resultHandler, setStore: setReachabilityStore}
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
	suggestionProvider  FallbackSuggestionProvider

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
	reachabilityStore *reachability.Store
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

	// shared indicates that this proxySession uses a SharedSessionManager
	// instead of a per-session Manager. In shared mode, the downstream is
	// shared across all upstream sessions and closeDownstream only decrements
	// refcount.
	shared bool

	// sharedMgr is the SharedSessionManager used in shared mode. Nil when
	// shared is false.
	sharedMgr *session.SharedSessionManager

	// metrics tracks per-server session lifecycle counters. Optional; nil = no metrics.
	metrics metrics.ServerMetricsReporter
}

// SetReachabilityStore configures the optional store used by this session's
// existing health probe. A nil store disables reporting.
func (ps *proxySession) SetReachabilityStore(store *reachability.Store) {
	if ps == nil {
		return
	}
	ps.mu.Lock()
	ps.reachabilityStore = store
	ps.mu.Unlock()
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
	suggestionProvider FallbackSuggestionProvider,
	healthCheckInterval time.Duration,
	requestTimeout time.Duration,
	retryConfig RetryConfig,
	circuitBreakerConfig CircuitBreakerConfig,
	srvMetrics metrics.ServerMetricsReporter,
	reachabilityStore func() *reachability.Store,
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
		suggestionProvider:  suggestionProvider,
		healthCheckInterval: healthCheckInterval,
		requestTimeout:      requestTimeout,
		retryConfig:         retryConfig,
		circuitBreaker:      newCircuitBreaker(circuitBreakerConfig, nil),
		metrics:             srvMetrics,
	}
	if reachabilityStore != nil {
		ps.SetReachabilityStore(reachabilityStore())
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
			if ps.metrics != nil {
				ps.metrics.IncActiveSessions()
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

// newSharedModeServer creates a new mcp.Server for a single upstream session
// in shared-subprocess mode. It uses a SharedSessionManager to share a single
// downstream subprocess across all upstream sessions.
func newSharedModeServer(
	ctx context.Context,
	serverName string,
	sm *session.SharedSessionManager,
	logger *slog.Logger,
	sharedTools *sharedToolCoordinator,
	inFlightLimiter *inFlightLimiter,
	suggestionProvider FallbackSuggestionProvider,
	requestTimeout time.Duration,
	retryConfig RetryConfig,
	circuitBreakerConfig CircuitBreakerConfig,
	srvMetrics metrics.ServerMetricsReporter,
	reachabilityStore func() *reachability.Store,
	onInitialized func(string, *proxySession),
	onClosed func(string),
) (*mcp.Server, error) {
	sessionID := fmt.Sprintf("proxy-%s-%d", serverName, nextSessionID())
	logger = logger.With(slog.String("session_id", sessionID))

	ps := &proxySession{
		serverName:         serverName,
		sessionID:          sessionID,
		logger:             logger,
		onClosed:           onClosed,
		shared:             true,
		sharedMgr:          sm,
		sharedTools:        sharedTools,
		inFlightLimiter:    inFlightLimiter,
		suggestionProvider: suggestionProvider,
		requestTimeout:     requestTimeout,
		retryConfig:        retryConfig,
		circuitBreaker:     newCircuitBreaker(circuitBreakerConfig, nil),
		metrics:            srvMetrics,
	}
	if reachabilityStore != nil {
		ps.SetReachabilityStore(reachabilityStore())
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
		InitializedHandler: func(_ context.Context, req *mcp.InitializedRequest) {
			ps.mu.Lock()
			ps.upstreamSession = req.Session
			ps.upstreamSessionID = req.Session.ID()
			ps.mu.Unlock()
			if onInitialized != nil {
				onInitialized(req.Session.ID(), ps)
			}
			if ps.metrics != nil {
				ps.metrics.IncActiveSessions()
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

	// Get or create shared downstream session.
	downstream, err := sm.GetOrCreateSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get shared downstream session: %w", err)
	}
	ps.downstream = downstream

	// Subscribe to respawn notifications so we update our local pointer.
	sm.Subscribe(sessionID, func(newDS *mcp.ClientSession) {
		ps.downstreamMu.Lock()
		ps.downstream = newDS
		ps.downstreamClosed = false
		ps.downstreamMu.Unlock()
		logger.Info("shared downstream respawned, updated local pointer",
			slog.String("event", "shared_session.respawn"),
		)
	})

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

	logger.Info("shared proxy session established",
		slog.Int("tools", len(toolsResult.Tools)),
	)

	// No per-session health probe in shared mode — SharedManager handles it.

	return server, nil
}

// handleToolListChanged is called when the downstream server notifies that
// its tool list has changed. It re-discovers tools and updates the upstream
// server, which automatically sends tools/list_changed to the upstream client.
//
// NOTE: We deliberately do NOT call ps.touch() here. This handler runs in
// response to a *downstream-initiated* notification, not client activity.
// Touching on server-initiated traffic resets the idle reaper indefinitely
// and was the root cause of orphaned per-session subprocesses surviving
// long after the upstream client process exited (see LEAK_REPORT.md).
func (ps *proxySession) handleToolListChanged(ctx context.Context) {
	ps.logger.Info("downstream tools/list_changed, re-discovering tools")

	// Snapshot downstream under read lock; attempt respawn if closed.
	ps.downstreamMu.RLock()
	ds := ps.downstream
	closed := ps.downstreamClosed
	ps.downstreamMu.RUnlock()

	if closed || ds == nil {
		if ps.shared && ps.sharedMgr != nil {
			// In shared mode, get or respawn via SharedManager.
			var err error
			ds, err = ps.sharedMgr.GetOrCreateSession(ctx, ps.sessionID)
			if err != nil {
				ps.logger.Warn("failed to get shared downstream during tool list refresh",
					slog.String("error", err.Error()),
				)
				return
			}
			ps.downstreamMu.Lock()
			ps.downstream = ds
			ps.downstreamClosed = false
			ps.downstreamMu.Unlock()
		} else {
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
//
// Does NOT touch the session — this is downstream-initiated traffic, not
// client activity. See handleToolListChanged for rationale.
func (ps *proxySession) handleLoggingMessage(ctx context.Context, params *mcp.LoggingMessageParams) {
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
//
// Does NOT touch the session — this is downstream-initiated traffic, not
// client activity. See handleToolListChanged for rationale.
func (ps *proxySession) handleProgress(ctx context.Context, params *mcp.ProgressNotificationParams) {
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
			result, err := ps.sharedTools.execute(ctx, toolName, req.Params.Arguments, func() (*mcp.CallToolResult, error) {
				return ps.callDownstreamTool(ctx, req, toolName)
			})
			return ps.finishToolCall(ctx, toolName, result, err)
		}

		result, err := ps.callDownstreamTool(ctx, req, toolName)
		return ps.finishToolCall(ctx, toolName, result, err)
	}
}

func (ps *proxySession) finishToolCall(ctx context.Context, toolName string, result *mcp.CallToolResult, err error) (*mcp.CallToolResult, error) {
	if err == nil {
		return result, nil
	}

	// Defense-in-depth: result should be nil when err is non-nil per
	// callDownstreamTool contract (result, nil) | (nil, err). Log if
	// violated so upstream debugging isn't silently confused.
	if result != nil {
		ps.logger.Warn("finishToolCall received non-nil result with non-nil error",
			slog.String("tool", toolName),
			slog.String("server", ps.serverName),
			slog.String("error", err.Error()),
		)
	}

	var availabilityErr *AvailabilityError
	if !errors.As(err, &availabilityErr) {
		return nil, err
	}

	var suggestions []FallbackSuggestion
	if ps.suggestionProvider != nil {
		suggestions = ps.suggestionProvider.SuggestAlternatives(ctx, ps.serverName, toolName)
	}
	return availabilityErrorToResult(availabilityErr, ps.serverName, toolName, suggestions), nil
}

// getDownstream returns the current downstream session and whether the session
// is closed. In shared mode, it queries SharedManager.Downstream() and falls
// back to GetOrCreateSession if the downstream is nil.
func (ps *proxySession) getDownstream(ctx context.Context) (*mcp.ClientSession, bool, error) {
	if ps.shared && ps.sharedMgr != nil {
		ds := ps.sharedMgr.Downstream()
		ps.downstreamMu.RLock()
		closed := ps.downstreamClosed
		ps.downstreamMu.RUnlock()

		if closed {
			return nil, true, nil
		}

		if ds == nil {
			var err error
			ds, err = ps.sharedMgr.GetOrCreateSession(ctx, ps.sessionID)
			if err != nil {
				return nil, false, err
			}
		}
		return ds, false, nil
	}

	ps.downstreamMu.RLock()
	ds := ps.downstream
	closed := ps.downstreamClosed
	ps.downstreamMu.RUnlock()
	return ds, closed, nil
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

	requestCtx, requestCancel := withRequestTimeoutBudget(ctx, ps.requestTimeout)
	defer requestCancel()

	ds, closed, err := ps.getDownstream(requestCtx)
	if err != nil {
		return nil, classifyToolCallError(err, false, ps.retryConfig.RetryableErrors)
	}
	if closed || ds == nil {
		if ps.shared {
			return nil, classifyToolCallError(ErrDownstreamUnavailable, false, ps.retryConfig.RetryableErrors)
		}
		ps.logger.Info("downstream unavailable before dispatch, attempting respawn",
			slog.String("tool", toolName),
		)
		ds, err = ps.respawnDownstream(requestCtx, "tool_call_pre_dispatch")
		if err != nil {
			return nil, classifyToolCallError(ErrDownstreamUnavailable, false, ps.retryConfig.RetryableErrors)
		}
	}

	// The application request is forwarded exactly once. Any error after this
	// call begins has uncertain execution state and must not trigger replay.
	result, callErr := ds.CallTool(requestCtx, &mcp.CallToolParams{
		Name:      req.Params.Name,
		Arguments: req.Params.Arguments,
	})
	if callErr == nil {
		ps.circuitBreaker.recordSuccess()
		return result, nil
	}

	lastErr := callErr
	if isDownstreamClosureError(callErr) {
		lastErr = ErrDownstreamUnavailable
		ps.logger.Warn("downstream closed after tool dispatch; request will not be retried",
			slog.String("tool", toolName),
			slog.String("error", callErr.Error()),
		)
	}
	if shouldRecordCircuitFailure(lastErr, ps.retryConfig.RetryableErrors) {
		ps.circuitBreaker.recordFailure()
	}
	classifiedErr := classifyToolCallError(lastErr, false, ps.retryConfig.RetryableErrors)
	ps.logger.Error("downstream tool call failed without replay",
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
		return nil, err
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
	if ps == nil {
		return
	}
	// In shared mode, there is no per-session timeout to touch.
	if ps.shared {
		return
	}
	if ps.mgr == nil {
		return
	}
	ps.mgr.TouchSession(ps.sessionID)
}

func (ps *proxySession) closeDownstream(reason string) {
	if ps == nil {
		return
	}
	// Allow stateful mode (mgr != nil) or shared mode (sharedMgr != nil).
	if ps.mgr == nil && ps.sharedMgr == nil {
		return
	}

	ps.closeMu.Lock()
	defer ps.closeMu.Unlock()

	ps.closeOnce.Do(func() {
		// Increment reap counter
		if ps.metrics != nil {
			ps.metrics.IncReaped(reason)
			ps.metrics.DecActiveSessions()
		}

		ps.mu.Lock()
		ps.closeReason = reason
		ps.mu.Unlock()
		ps.logger.Info("closing proxy session downstream",
			slog.String("reason", reason),
		)

		// In shared mode: decrement refcount and unsubscribe, but do NOT close
		// the actual ClientSession (it's shared across upstream sessions).
		if ps.shared && ps.sharedMgr != nil {
			if err := ps.sharedMgr.RemoveSession(ps.sessionID); err != nil {
				ps.logger.Warn("failed to remove shared session",
					slog.String("error", err.Error()),
				)
			}
			ps.sharedMgr.Unsubscribe(ps.sessionID)
		} else {
			// Stop the health probe before tearing down the downstream.
			ps.stopHealthProbe()
		}

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

				ps.mu.Lock()
				store := ps.reachabilityStore
				ps.mu.Unlock()
				attemptedAt := time.Now()
				if store != nil {
					store.StartProbe(ps.serverName, reachability.DepthSession, attemptedAt)
				}

				probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
				_, err := ds.ListTools(probeCtx, nil)
				probeCancel()
				if store != nil {
					store.RecordProbe(ps.serverName, reachability.ProbeResult{
						Depth:       reachability.DepthSession,
						AttemptedAt: attemptedAt,
						Success:     err == nil,
						Error:       errorString(err),
					})
				}

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

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
