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

	// ReachabilityStore receives session-depth probe evidence from the existing
	// downstream health probes. Optional; nil disables reporting and leaves
	// probe behavior unchanged.
	ReachabilityStore *reachability.Store

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

	// DaemonMetrics records daemon-wide tool call and forwarding failure
	// counters. Optional; nil = no daemon-wide counting.
	DaemonMetrics *metrics.DaemonMetrics
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

	// Register callbacks so that manager removals set the proxy closed flag
	// before the SDK connection is torn down. The removal-owner index
	// (idx.byDownstream) is populated through the onSpawned callback before
	// the downstream spawn or shared lease is created, so a removal or lease
	// expiry that lands during startup reaches the proxy session that owns
	// the downstream: closeDownstream marks the generation closed and
	// publishInitialized refuses credit for it. A spawn that fails cleans the
	// early registration, and a respawn that loses its generation removes the
	// rekeyed registration unconditionally, so no owner outlives a rejected
	// generation. The lookup deletes the entry: the manager removed the ID,
	// so the mapping is dead even when the session was never published
	// upstream.
	if cfg.SessionManager != nil {
		cfg.SessionManager.SetOnSessionRemoved(func(sessionID string) {
			idx.mu.Lock()
			ps := idx.byDownstream[sessionID]
			delete(idx.byDownstream, sessionID)
			idx.mu.Unlock()
			if ps != nil {
				ps.closeDownstream("session removed by manager")
			}
		})
	} else if cfg.Selector != nil {
		cfg.Selector.SetOnSessionRemoved(func(sessionID string) {
			idx.mu.Lock()
			ps := idx.byDownstream[sessionID]
			delete(idx.byDownstream, sessionID)
			idx.mu.Unlock()
			if ps != nil {
				ps.closeDownstream("session removed by manager")
			}
		})
	}
	if cfg.SharedManager != nil {
		cfg.SharedManager.SetOnSessionExpired(func(sessionID string) {
			idx.mu.Lock()
			ps := idx.byDownstream[sessionID]
			delete(idx.byDownstream, sessionID)
			idx.mu.Unlock()
			if ps != nil {
				ps.closeDownstream("idle_timeout")
			}
			// Every expiry must end without the lease. closeDownstream removes
			// it on the close it performs; a proxy whose close path was
			// already consumed (a startup expiry that raced initialization)
			// still loses the lease here, so the refcount cannot leak.
			_ = cfg.SharedManager.RemoveSession(sessionID)
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
				cfg.DaemonMetrics,
				getReachabilityStore,
				func(ps *proxySession) {
					// Register the removal owner before the shared lease
					// exists, so an expiry during startup reaches this proxy.
					idx.mu.Lock()
					idx.byDownstream[ps.downstreamID()] = ps
					idx.mu.Unlock()
				},
				func(upstreamSessionID string, ps *proxySession) {
					ps.SetReachabilityStore(getReachabilityStore())
					idx.mu.Lock()
					idx.byUpstream[upstreamSessionID] = ps
					idx.byDownstream[ps.downstreamID()] = ps
					idx.mu.Unlock()
				},
				func(upstreamSessionID string) {
					idx.mu.Lock()
					ps := idx.byUpstream[upstreamSessionID]
					delete(idx.byUpstream, upstreamSessionID)
					if ps != nil {
						delete(idx.byDownstream, ps.downstreamID())
					}
					idx.mu.Unlock()
				},
				func(sessionID string) {
					idx.mu.Lock()
					delete(idx.byDownstream, sessionID)
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
		srvMetrics := cfg.Metrics
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
			// Feed the group-created session into its member server's metrics
			// owner so read-time session derivation sees group sessions.
			if resolver, ok := cfg.Selector.(SlotMetricsResolver); ok {
				if reporter := resolver.ReporterFor(mgr); reporter != nil {
					srvMetrics = reporter
				}
			}
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
			srvMetrics,
			cfg.DaemonMetrics,
			getReachabilityStore,
			func(ps *proxySession) {
				// Register the removal owner before the spawn, so a removal
				// during the spawn window reaches this proxy.
				idx.mu.Lock()
				idx.byDownstream[ps.downstreamID()] = ps
				idx.mu.Unlock()
			},
			func(upstreamSessionID string, ps *proxySession) {
				ps.SetReachabilityStore(getReachabilityStore())
				if cfg.Selector != nil && pendingKey != "" {
					cfg.Selector.Rebind(pendingKey, upstreamSessionID)
				}
				idx.mu.Lock()
				idx.byUpstream[upstreamSessionID] = ps
				idx.byDownstream[ps.downstreamID()] = ps
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
					delete(idx.byDownstream, ps.downstreamID())
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
			func(sessionID string) {
				idx.mu.Lock()
				delete(idx.byDownstream, sessionID)
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
			applicationActivity := isApplicationActivityRequest(r)
			idx.mu.RLock()
			ps := idx.byUpstream[sessionHeader]
			idx.mu.RUnlock()
			if ps != nil && applicationActivity {
				ps.touch()
				if ps.shared && ps.sharedMgr != nil {
					release := ps.sharedMgr.BeginRequest(ps.downstreamID())
					defer release()
				}
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
					if err := ps.mgr.RemoveSession(ps.downstreamID()); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
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

	// Inject the reachability store at construction time. Callers that build a
	// proxy with a store configured get downstream probe reporting without
	// needing a type assertion on the returned http.Handler.
	if cfg.ReachabilityStore != nil {
		setReachabilityStore(cfg.ReachabilityStore)
	}

	return resultHandler
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

func isApplicationActivityRequest(r *http.Request) bool {
	if r.Method != http.MethodPost || r.Body == nil {
		return false
	}
	body, err := readAndRestoreRequestBody(r)
	if err != nil {
		return false
	}
	active, err := ClassifyApplicationActivity(body)
	return err == nil && active
}

// proxySession holds the state for a single proxied session, used to relay
// notifications from downstream to the upstream ServerSession.
type proxySession struct {
	serverName string
	server     *mcp.Server

	// sessionID is the current downstream session identifier. It has one
	// writer: the closeMu critical section that publishes a respawned
	// generation. Every reader keys manager and index operations on it
	// through downstreamID, so a cleanup path always reads the identifier
	// of the generation it tears down.
	sessionIDMu sync.RWMutex
	sessionID   string
	mgr         *session.Manager
	logger      *slog.Logger
	onClosed    func(string)

	// onRespawn is called after a successful downstream respawn to update
	// external indexes (e.g., idx.byDownstream). It receives the old and new
	// downstream session IDs plus the proxySession pointer so the caller can
	// atomically swap the index entry without needing to look up by old key
	// (which may have been deleted by closeDownstream's onClosed callback).
	onRespawn func(oldSessionID, newSessionID string, ps *proxySession)

	// onDownstreamClosed is called with the current downstream session ID
	// on every closeDownstream completion, including closes that happen
	// before upstream initialization published the session. It removes the
	// pre-spawn byDownstream registration, which the upstream-keyed
	// onClosed callback cannot reach without an upstream ID. Both modes set
	// it; the stateful respawn failure and abandonment paths also call it
	// directly to remove a rekeyed registration whose generation the manager
	// already dropped.
	onDownstreamClosed func(sessionID string)

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

	// sessionCounted reports whether this proxy session currently holds one
	// active-session credit on ps.metrics. Guarded by mu. A session acquires
	// its credit when the upstream client completes initialization, releases
	// it once in closeDownstream, and reacquires it after a respawn that
	// continues the same initialized upstream session. Closing a session that
	// holds no credit (an initial tools/list that failed before
	// initialization) leaves the counter untouched instead of driving it
	// negative.
	sessionCounted bool

	closeMu   sync.Mutex
	closeOnce sync.Once

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

	// daemonMetrics records daemon-wide tool call and forwarding failure
	// counters. Optional; nil = no daemon-wide counting.
	daemonMetrics *metrics.DaemonMetrics
}

// downstreamID returns the current downstream session identifier.
//
// The identifier is written only inside the closeMu critical section that
// publishes a respawned generation, before that section releases closeMu.
// Readers that key manager or index operations on the identifier take this
// accessor, so a cleanup path either reads the identifier of the generation
// the manager holds or waits for the publication to complete; no reader can
// pair the rekeyed index entry with the previous identifier.
func (ps *proxySession) downstreamID() string {
	if ps == nil {
		return ""
	}
	ps.sessionIDMu.RLock()
	defer ps.sessionIDMu.RUnlock()
	return ps.sessionID
}

// setDownstreamID publishes a new downstream session identifier. It runs
// only inside the closeMu critical section that publishes the respawned
// generation.
func (ps *proxySession) setDownstreamID(id string) {
	ps.sessionIDMu.Lock()
	ps.sessionID = id
	ps.sessionIDMu.Unlock()
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

// acquireActiveSession marks this proxy session as holding one active-session
// credit and increments the per-server counter. A session acquires its credit
// once, when the upstream client completes initialization; a respawn that
// continues the same initialized upstream session reacquires it after the
// reap released it. Acquire is idempotent for the session's lifetime.
func (ps *proxySession) acquireActiveSession() {
	if ps == nil {
		return
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.sessionCounted {
		return
	}
	ps.sessionCounted = true
	if ps.metrics != nil {
		ps.metrics.IncActiveSessions()
	}
}

// releaseActiveSession drops this proxy session's active-session credit, if
// one is held, decrementing the per-server counter exactly once. A session
// that never acquired a credit — an initial tools/list that failed before
// any upstream client initialized — releases nothing, so the counter cannot
// be driven below zero.
func (ps *proxySession) releaseActiveSession() {
	if ps == nil {
		return
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if !ps.sessionCounted {
		return
	}
	ps.sessionCounted = false
	if ps.metrics != nil {
		ps.metrics.DecActiveSessions()
	}
}

// hasInitializedUpstreamSession reports whether an upstream client has
// completed initialization on this proxy session.
func (ps *proxySession) hasInitializedUpstreamSession() bool {
	if ps == nil {
		return false
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.upstreamSessionID != ""
}

// publishInitialized runs the initialization transition in the only safe
// order: acquire the active-session credit first, then publish the session
// through the onInitialized callback. From the publish onward a manager
// removal can reach closeDownstream, and every removal that finds this
// session must find the credit it releases. Acquiring after the publish
// would let a removal consume closeOnce against a creditless session and
// strand the later acquisition forever.
//
// The acquisition serializes on closeMu against the close transition and
// requires a live downstream generation: a removal that landed between the
// spawn and the initialization closed the generation, and late
// initialization must not credit it. The next tool call respawns a live
// generation and reacquires the credit there, inside the same closeMu
// window.
//
// A shared generation closed before initialization is terminal: shared
// proxies never respawn a lease, so the dead owner must not reenter the
// indexes through the publication callback. The startup close consumed
// closeOnce before any upstream session existed, so the terminal close
// re-arms it and tears the late-initialized upstream down through the normal
// close path — index cleanup and upstream close included — which leaves the
// expired session id answered with 404.
//
// The publication itself runs outside closeMu, so a close whose completion
// lands between the liveness check and the publication callback — a shared
// lease expiry, a manager removal — consumes the generation close and clears
// the indexes, and the callback then reinserts the closed proxy with no lease
// left to expire again. After the publication the liveness state is
// re-checked under closeMu: a generation that was live at entry and is closed
// after the publication is closed again with reason publish_lost_race, and
// the idempotent terminal owner cleanup removes exactly the entries the
// publication inserted. A close that lands after the publication cleans
// through its own terminal tail instead.
func (ps *proxySession) publishInitialized(publish func(string, *proxySession)) {
	if ps == nil {
		return
	}
	ps.closeMu.Lock()
	ps.downstreamMu.RLock()
	wasLive := !ps.downstreamClosed && ps.downstream != nil
	ps.downstreamMu.RUnlock()
	if wasLive {
		ps.acquireActiveSession()
		ps.closeMu.Unlock()
	} else if ps.shared {
		ps.closeOnce = sync.Once{}
		ps.closeMu.Unlock()
		ps.closeDownstream("idle_timeout")
		return
	} else {
		// A dead stateful generation still publishes: the next tool call
		// respawns it and reacquires the credit there.
		ps.closeMu.Unlock()
	}
	if publish == nil {
		return
	}
	ps.mu.Lock()
	upstreamID := ps.upstreamSessionID
	ps.mu.Unlock()
	publish(upstreamID, ps)

	ps.closeMu.Lock()
	ps.downstreamMu.RLock()
	stillLive := !ps.downstreamClosed && ps.downstream != nil
	ps.downstreamMu.RUnlock()
	ps.closeMu.Unlock()
	if wasLive && !stillLive {
		ps.closeDownstream("publish_lost_race")
	}
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
	daemonMetrics *metrics.DaemonMetrics,
	reachabilityStore func() *reachability.Store,
	onSpawned func(*proxySession),
	onInitialized func(string, *proxySession),
	onClosed func(string),
	onRespawn func(oldSessionID, newSessionID string, ps *proxySession),
	onDownstreamClosed func(sessionID string),
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
		onDownstreamClosed:  onDownstreamClosed,
		sharedTools:         sharedTools,
		inFlightLimiter:     inFlightLimiter,
		suggestionProvider:  suggestionProvider,
		healthCheckInterval: healthCheckInterval,
		requestTimeout:      requestTimeout,
		retryConfig:         retryConfig,
		circuitBreaker:      newCircuitBreaker(circuitBreakerConfig, nil),
		metrics:             srvMetrics,
		daemonMetrics:       daemonMetrics,
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
		// Logging preserves the SDK default that a non-nil Capabilities value
		// otherwise suppresses (go-sdk capabilities() clones user values
		// without defaults).
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}, Logging: &mcp.LoggingCapabilities{}},
		// Capture the ServerSession when the upstream client completes initialization.
		InitializedHandler: func(_ context.Context, req *mcp.InitializedRequest) {
			ps.mu.Lock()
			ps.upstreamSession = req.Session
			ps.upstreamSessionID = req.Session.ID()
			ps.mu.Unlock()
			ps.publishInitialized(onInitialized)
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

	// Register the removal owner before the spawn: the manager owns the
	// downstream from its internal registration on, so a removal that lands
	// inside the spawn window reaches this proxy, closeDownstream marks the
	// generation closed, and publishInitialized refuses credit for it.
	if onSpawned != nil {
		onSpawned(ps)
	}

	// Spawn downstream subprocess with notification handlers.
	downstream, err := mgr.SpawnSession(ctx, sessionID, clientOpts)
	if err != nil {
		// The manager never tracked the session; drop the early registration
		// so no owner outlives a failed spawn.
		if ps.onDownstreamClosed != nil {
			ps.onDownstreamClosed(sessionID)
		}
		return nil, fmt.Errorf("failed to spawn downstream session: %w", err)
	}
	// A removal that landed inside the spawn window closed this proxy through
	// the early registration; a closed generation must not gain a live
	// downstream pointer.
	ps.downstreamMu.Lock()
	if !ps.downstreamClosed {
		ps.downstream = downstream
	}
	ps.downstreamMu.Unlock()

	// Discover tools from downstream.
	toolsResult, err := downstream.ListTools(ctx, nil)
	if err != nil {
		ps.closeDownstream("initial tools/list failed")
		// SpawnSession left a half-built session owned by the manager. Return
		// it through the manager's removal path, which closes the SDK
		// connection and subprocess and frees the admission slot. Without
		// this, one rejected initialization occupies a manager slot until the
		// reaper runs and the next initialization is denied for capacity.
		// closeDownstream already set the closed flag, so the removal
		// callback finds nothing new to close.
		if err := mgr.RemoveSession(sessionID); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
			logger.Warn("failed to remove session after initial tools/list failure",
				slog.String("session_id", sessionID),
				slog.String("error", err.Error()),
			)
		}
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
	daemonMetrics *metrics.DaemonMetrics,
	reachabilityStore func() *reachability.Store,
	onSpawned func(*proxySession),
	onInitialized func(string, *proxySession),
	onClosed func(string),
	onDownstreamClosed func(string),
) (*mcp.Server, error) {
	sessionID := fmt.Sprintf("proxy-%s-%d", serverName, nextSessionID())
	logger = logger.With(slog.String("session_id", sessionID))

	ps := &proxySession{
		serverName:         serverName,
		sessionID:          sessionID,
		logger:             logger,
		onClosed:           onClosed,
		onDownstreamClosed: onDownstreamClosed,
		shared:             true,
		sharedMgr:          sm,
		sharedTools:        sharedTools,
		inFlightLimiter:    inFlightLimiter,
		suggestionProvider: suggestionProvider,
		requestTimeout:     requestTimeout,
		retryConfig:        retryConfig,
		circuitBreaker:     newCircuitBreaker(circuitBreakerConfig, nil),
		metrics:            srvMetrics,
		daemonMetrics:      daemonMetrics,
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
		// Logging preserves the SDK default that a non-nil Capabilities value
		// otherwise suppresses (go-sdk capabilities() clones user values
		// without defaults).
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}, Logging: &mcp.LoggingCapabilities{}},
		InitializedHandler: func(_ context.Context, req *mcp.InitializedRequest) {
			ps.mu.Lock()
			ps.upstreamSession = req.Session
			ps.upstreamSessionID = req.Session.ID()
			ps.mu.Unlock()
			ps.publishInitialized(onInitialized)
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

	// Register the removal owner before the lease exists: the shared manager
	// exposes the lease to its reaper the moment GetOrCreateSession tracks
	// it, and an expiry that lands before an owner is registered cannot
	// reach closeDownstream, so late initialization would credit a phantom
	// session.
	if onSpawned != nil {
		onSpawned(ps)
	}

	// Get or create shared downstream session.
	downstream, err := sm.GetOrCreateSession(ctx, sessionID)
	if err != nil {
		// The lease was never created; drop the early registration so no
		// owner outlives a failed spawn.
		if ps.onDownstreamClosed != nil {
			ps.onDownstreamClosed(sessionID)
		}
		return nil, fmt.Errorf("failed to get shared downstream session: %w", err)
	}
	// An expiry that landed inside GetOrCreateSession closed this proxy
	// through the early registration; a closed generation must not gain a
	// live downstream pointer.
	ps.downstreamMu.Lock()
	if !ps.downstreamClosed {
		ps.downstream = downstream
	}
	ps.downstreamMu.Unlock()

	// Subscribe to respawn notifications so we update our local pointer. A
	// respawn notification must not resurrect a closed proxy generation.
	sm.Subscribe(sessionID, func(newDS *mcp.ClientSession) {
		ps.downstreamMu.Lock()
		if !ps.downstreamClosed {
			ps.downstream = newDS
			ps.downstreamClosed = false
		}
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
			ds, err = ps.sharedMgr.GetOrCreateSession(ctx, ps.downstreamID())
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
		// Count at handler entry so every upstream tools/call reports once,
		// including cache hits and coalesced joins that never dispatch.
		if ps.daemonMetrics != nil {
			ps.daemonMetrics.IncToolCalls()
		}
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
			ds, err = ps.sharedMgr.GetOrCreateSession(ctx, ps.downstreamID())
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
		return nil, ps.recordForwardingFailure(classifyToolCallError(err, false, ps.retryConfig.RetryableErrors))
	}

	release, err := ps.inFlightLimiter.acquire(ctx)
	if err != nil {
		return nil, ps.recordForwardingFailure(classifyToolCallError(err, false, ps.retryConfig.RetryableErrors))
	}
	defer release()

	requestCtx, requestCancel := withRequestTimeoutBudget(ctx, ps.requestTimeout)
	defer requestCancel()

	ds, closed, err := ps.getDownstream(requestCtx)
	if err != nil {
		return nil, ps.recordForwardingFailure(classifyToolCallError(err, false, ps.retryConfig.RetryableErrors))
	}
	if closed || ds == nil {
		if ps.shared {
			return nil, ps.recordForwardingFailure(classifyToolCallError(ErrDownstreamUnavailable, false, ps.retryConfig.RetryableErrors))
		}
		ps.logger.Info("downstream unavailable before dispatch, attempting respawn",
			slog.String("tool", toolName),
		)
		ds, err = ps.respawnDownstream(requestCtx, "tool_call_pre_dispatch")
		if err != nil {
			return nil, ps.recordForwardingFailure(classifyToolCallError(ErrDownstreamUnavailable, false, ps.retryConfig.RetryableErrors))
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
	classifiedErr := ps.recordForwardingFailure(classifyToolCallError(lastErr, false, ps.retryConfig.RetryableErrors))
	ps.logger.Error("downstream tool call failed without replay",
		slog.String("tool", toolName),
		slog.String("error", classifiedErr.Error()),
	)
	return nil, classifiedErr
}

// recordForwardingFailure counts one daemon-wide forwarding failure and passes
// the classified error through. Every classified error return from the stdio
// tools/call path goes through here exactly once.
func (ps *proxySession) recordForwardingFailure(err error) error {
	if ps.daemonMetrics != nil {
		ps.daemonMetrics.IncErrors()
	}
	return err
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

	newSessionID := fmt.Sprintf("%s-respawn-%d", ps.downstreamID(), nextSessionID())
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

	// Rekey the external removal-owner index to the new generation before
	// anything else can happen on it. Until the rekey, a manager removal of
	// newSessionID finds no owner and cannot close this proxy, which would
	// leave the respawn publishing a generation the manager no longer holds.
	// The rekey also drops the old key, so the later RemoveSession of the old
	// session cannot reach this proxy and close the new downstream through it.
	oldSessionID := ps.downstreamID()
	if ps.onRespawn != nil {
		ps.onRespawn(oldSessionID, newSessionID, ps)
	}

	// Re-discover tools from the new downstream.
	toolsResult, err := downstream.ListTools(spawnCtx, nil)
	if err != nil {
		// Best-effort close of the just-spawned session. The removal-owner
		// rekey above lets the manager callback reach closeDownstream, which
		// cleans the index entry; a removal that landed before the rekey
		// found no owner, so the rekeyed registration is removed here
		// unconditionally — including when the manager already dropped the
		// generation and RemoveSession reports ErrSessionNotFound.
		_ = ps.mgr.RemoveSession(newSessionID)
		if ps.onDownstreamClosed != nil {
			ps.onDownstreamClosed(newSessionID)
		}
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

	// Run the survival check, the closeOnce reset, the credit reacquisition,
	// and the pointer publication inside one closeMu critical section. A
	// manager removal of the new generation between the spawn and this
	// critical section either completed (the callback closed this proxy
	// through the rekeyed owner) or is blocked on closeMu right now; both
	// leave the manager holding no session for this generation, because
	// RemoveSession deletes the tracked session before it fires callbacks.
	ps.closeMu.Lock()
	if ps.mgr.GetSession(newSessionID) == nil {
		ps.closeMu.Unlock()
		// The generation was removed under us and the manager has closed its
		// SDK connection. The proxy must stay detached (downstreamClosed set,
		// no live pointer): publishing here would leave a closed SDK session
		// marked live with a consumed closeOnce, and every later call would
		// fail against it instead of respawning. The next tool call respawns.
		ps.logger.Warn("respawned session removed during setup, abandoning respawn",
			slog.String("event", "session.respawn_abandoned"),
			slog.String("new_session_id", newSessionID),
		)
		// The rekeyed registration of the abandoned generation must not
		// outlive it: a removal that landed before the rekey found no owner,
		// and no later callback will clean this entry.
		if ps.onDownstreamClosed != nil {
			ps.onDownstreamClosed(newSessionID)
		}
		// Best-effort cleanup of the previous generation's manager entry, the
		// same as the success path: after a health-check trigger the manager
		// still holds the dead session, and abandonment must not extend its
		// slot occupancy. The rekey above dropped the old byDownstream key, so
		// the removal callback is a no-op for this proxy.
		if err := ps.mgr.RemoveSession(oldSessionID); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
			ps.logger.Warn("failed to remove old session after abandoned respawn",
				slog.String("old_session", oldSessionID),
				slog.String("error", err.Error()),
			)
		}
		return nil, fmt.Errorf("respawn abandoned: manager removed session %s during setup", newSessionID)
	}
	ps.closeOnce = sync.Once{}
	// Publish the new identifier inside the same critical section that
	// publishes the generation, before the credit reacquisition: every
	// cleanup path that acquires closeMu after this point keys on the
	// identifier of the published generation, and the gauge flip the
	// reacquisition causes proves the identifier is already published.
	ps.setDownstreamID(newSessionID)
	// The reap released this session's active-session credit, but the
	// initialized upstream session continues on the respawned downstream.
	// Reacquire the credit in the same closeMu window as the publication: a
	// concurrent closeDownstream either runs entirely before this critical
	// section (a no-op against the old closeOnce) or entirely after it (a
	// balanced release against the published, live generation), never
	// between the credit and the pointer it belongs to.
	if ps.hasInitializedUpstreamSession() {
		ps.acquireActiveSession()
	}
	// Only now publish the pointer: the generation is proven live in the
	// manager, its close path is armed through the fresh closeOnce, the new
	// identifier is published, and the credit is held. A removal that lands
	// after closeMu is released sees exactly this published generation and
	// tears it down in a balanced way.
	ps.downstreamMu.Lock()
	ps.downstream = downstream
	ps.downstreamClosed = false
	ps.downstreamMu.Unlock()
	ps.closeMu.Unlock()

	ps.mu.Lock()
	ps.currentTools = newToolNames
	ps.mu.Unlock()

	// Clean up the old session from the manager to prevent admission counter
	// leaks. The old subprocess is already dead (reaped/crashed), but its
	// TrackedSession entry may still occupy a slot. The removal-owner rekey
	// at spawn time already removed the old key from idx.byDownstream, so
	// the onSessionRemoved callback is a no-op for this proxy.
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
	// Shared mode tracks activity on the upstream session record.
	if ps.shared {
		if ps.sharedMgr != nil {
			ps.sharedMgr.TouchSession(ps.downstreamID())
		}
		return
	}
	if ps.mgr == nil {
		return
	}
	ps.mgr.TouchSession(ps.downstreamID())
}

// closeDownstream tears the proxy's downstream generation down and then runs
// the terminal owner cleanup. The generation close is guarded by closeOnce,
// which respawn re-arms for the next generation; the terminal cleanup sits
// outside it because it is keyed to the upstream session, not the generation:
// a close whose generation close was already consumed by an earlier removal
// (a manager removal that landed before notifications/initialized published
// the session) must still release the slot reservation and the index entries
// of the published upstream session. Both cleanup callbacks are idempotent
// map deletions and slot releases, so repeated closes are no-ops.
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
		}
		// Release the active-session credit only when one is held: a failed
		// initial tools/list closes before any upstream client initialized,
		// and decrementing then would drive the counter below zero.
		ps.releaseActiveSession()

		ps.mu.Lock()
		ps.closeReason = reason
		ps.mu.Unlock()
		ps.logger.Info("closing proxy session downstream",
			slog.String("reason", reason),
		)

		// In shared mode: decrement refcount and unsubscribe, but do NOT close
		// the actual ClientSession (it's shared across upstream sessions).
		if ps.shared && ps.sharedMgr != nil {
			// The terminal close of a startup-expired generation finds the
			// lease already removed by the expiry callback; ErrSessionNotFound
			// is the expected outcome there, not a leak.
			if err := ps.sharedMgr.RemoveSession(ps.downstreamID()); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
				ps.logger.Warn("failed to remove shared session",
					slog.String("error", err.Error()),
				)
			}
			ps.sharedMgr.Unsubscribe(ps.downstreamID())
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

		// A shared-mode proxy session never reacquires its lease, so the
		// upstream session must end with it. Closing the upstream session
		// removes it from the streamable HTTP handler, which then answers the
		// session ID with HTTP 404; the MCP transport requires the client to
		// initialize a new session on 404. Close waits for in-flight upstream
		// requests, so it runs outside closeMu.
		ps.mu.Lock()
		upstream := ps.upstreamSession
		upstreamID := ps.upstreamSessionID
		ps.mu.Unlock()
		if ps.shared && upstream != nil {
			go func() {
				if err := upstream.Close(); err != nil {
					ps.logger.Debug("upstream session close returned error",
						slog.String("upstream_session_id", upstreamID),
						slog.String("error", err.Error()),
					)
				}
			}()
		}
	})

	// Terminal owner cleanup: release the slot reservation and remove the
	// index entries. This runs on every close, outside closeOnce: a
	// generation close consumed by an earlier removal must not stop the
	// later DELETE or upstream-end close from cleaning the published upstream
	// session's owner entries. The downstream-keyed registration is removed
	// unconditionally: a close before upstream initialization (failed initial
	// tools/list, pre-publish removal) has no upstream ID, so the gated
	// onClosed call cannot reach it. Both calls are idempotent.
	ps.mu.Lock()
	upstreamID := ps.upstreamSessionID
	onClosed := ps.onClosed
	onDownstreamClosed := ps.onDownstreamClosed
	sessionID := ps.downstreamID()
	ps.mu.Unlock()
	if onDownstreamClosed != nil {
		onDownstreamClosed(sessionID)
	}
	if onClosed != nil && upstreamID != "" {
		onClosed(upstreamID)
	}
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
		failureThreshold := reachability.FailureThreshold

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
