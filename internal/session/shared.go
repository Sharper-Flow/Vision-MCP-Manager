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
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SharedSessionManager maintains a single downstream subprocess shared across
// all upstream sessions for stateless MCP servers.
type SharedSessionManager struct {
	serverName string
	config     *config.ServerConfig
	logger     *slog.Logger

	// When both locks are needed, acquire mu before refMu. A refMu holder
	// must never acquire mu.
	mu              sync.RWMutex
	downstream      *mcp.ClientSession
	client          *mcp.Client
	spawnErr        error
	closed          bool      // Writes require mu and refMu; reads require either lock.
	cmd             *exec.Cmd // stored at spawn for PID capture in reap events
	downstreamDone  <-chan struct{}
	startedAt       time.Time
	lastSpawnReason string
	starting        *sharedStartup
	lifetimeCtx     context.Context
	lifetimeCancel  context.CancelFunc
	probeMu         sync.Mutex

	// healthCheckFailures counts consecutive failed health probes against
	// the current downstream for the respawn gate in healthCheck. Guarded by mu.
	healthCheckFailures int

	refMu            sync.Mutex
	refCount         int
	activeRequests   int
	upstreamSessions map[string]struct{}
	sessions         map[string]*sharedSession
	onSessionExpired func(sessionID string)
	reaperCancel     context.CancelFunc

	// sessionTimeout expires an upstream session after application inactivity.
	// It is separate from idleTimeout, which reaps the shared subprocess after
	// the last upstream session has been removed.
	sessionTimeout time.Duration

	// Idle reap: when refCount drops to 0, start idleTimer. On expiry,
	// tear down the downstream subprocess. Cancelled by GetOrCreateSession.
	// All idle fields guarded by refMu.
	idleTimer   *time.Timer
	idleTimeout time.Duration

	// metrics tracks per-server session lifecycle counters. Optional; nil = no metrics.
	metrics metrics.ServerMetricsReporter

	// reachabilityStore receives observations from the existing health probe.
	// Optional; nil disables reachability reporting without changing probing.
	reachabilityStore *reachability.Store

	healthCtx    context.Context
	healthCancel context.CancelFunc

	subMu       sync.RWMutex
	subscribers map[string]sharedSubscriber
}

type sharedSubscriber struct {
	onRespawn         func(*mcp.ClientSession)
	onToolListChanged func(context.Context)
}

type sharedSession struct {
	lastActivity time.Time
	inFlight     int
	expiring     bool
	pending      int
	admitted     bool
}

type sharedStartup struct {
	done chan struct{}
	ds   *mcp.ClientSession
	err  error
}

// SharedStartingError is a pre-dispatch refusal. Retrying admission is safe;
// it says nothing about the outcome of a previously dispatched tool call.
type SharedStartingError struct{ Cause error }

func (e *SharedStartingError) Error() string   { return "shared downstream is starting; retry admission" }
func (e *SharedStartingError) Unwrap() error   { return e.Cause }
func (e *SharedStartingError) Temporary() bool { return true }

const sharedStartupTimeout = 15 * time.Second

// NewSharedSessionManager creates a new shared session manager.
// idleTimeout controls the idle reap behavior: when refCount drops to 0, the
// downstream subprocess is torn down after this duration. 0 disables idle reaping.
func NewSharedSessionManager(serverName string, cfg *config.ServerConfig, logger *slog.Logger, idleTimeout time.Duration, m metrics.ServerMetricsReporter) *SharedSessionManager {
	if logger == nil {
		logger = slog.Default()
	}
	lifetimeCtx, lifetimeCancel := context.WithCancel(context.Background())
	return &SharedSessionManager{
		lifetimeCtx:      lifetimeCtx,
		lifetimeCancel:   lifetimeCancel,
		serverName:       serverName,
		config:           cfg,
		logger:           logger.With(slog.String("server", serverName)),
		upstreamSessions: make(map[string]struct{}),
		sessions:         make(map[string]*sharedSession),
		subscribers:      make(map[string]sharedSubscriber),
		idleTimeout:      idleTimeout,
		sessionTimeout:   time.Duration(cfg.SessionTimeout),
		metrics:          m,
	}
}

// SetReachabilityStore configures the optional store used by health probes.
// A nil store disables reporting while leaving the probe behavior unchanged.
func (sm *SharedSessionManager) SetReachabilityStore(store *reachability.Store) {
	if sm == nil {
		return
	}
	sm.mu.Lock()
	sm.reachabilityStore = store
	sm.mu.Unlock()
}

// GetOrCreateSession returns the shared downstream session, lazily spawning it
// on first use. The sessionID is tracked for admission control and refcounting.
func (sm *SharedSessionManager) GetOrCreateSession(ctx context.Context, sessionID string) (*mcp.ClientSession, error) {
	if sm.isClosed() {
		return nil, errors.New("shared session manager is closed")
	}

	// Check admission and track session
	sm.refMu.Lock()
	if sm.closed {
		sm.refMu.Unlock()
		return nil, errors.New("shared session manager is closed")
	}
	if _, exists := sm.upstreamSessions[sessionID]; !exists {
		// Check max sessions
		if sm.config.MaxSessions > 0 && sm.refCount >= sm.config.MaxSessions {
			sm.refMu.Unlock()
			return nil, fmt.Errorf("%w: limit %d", ErrMaxSessions, sm.config.MaxSessions)
		}
		sm.upstreamSessions[sessionID] = struct{}{}
		sm.sessions[sessionID] = &sharedSession{lastActivity: time.Now()}
		sm.refCount++
	}
	tracked := sm.sessions[sessionID]
	tracked.pending++

	// Cancel idle timer on new session arrival
	if sm.idleTimer != nil {
		sm.idleTimer.Stop()
		sm.idleTimer = nil
		sm.logger.Info("idle reap timer cancelled by new session",
			slog.String("event", "shared_session.idle_cancelled"),
		)
	}

	sm.refMu.Unlock()

	// Get or create downstream
	ds, err := sm.getOrCreateDownstream(ctx, false)
	sm.refMu.Lock()
	if sm.sessions[sessionID] == tracked {
		tracked.pending--
		if err == nil {
			tracked.admitted = true
			tracked.lastActivity = time.Now()
		} else if !tracked.admitted && tracked.pending == 0 {
			delete(sm.upstreamSessions, sessionID)
			delete(sm.sessions, sessionID)
			sm.refCount--
			sm.armIdleTimerLocked()
		}
	}
	sm.refMu.Unlock()
	return ds, err
}

func (sm *SharedSessionManager) isClosed() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.closed
}

func (sm *SharedSessionManager) getOrCreateDownstream(ctx context.Context, refuseStarting bool) (*mcp.ClientSession, error) {
	sm.mu.Lock()
	if sm.closed {
		sm.mu.Unlock()
		return nil, errors.New("shared session manager is closed")
	}
	if sm.starting != nil && refuseStarting {
		sm.mu.Unlock()
		return nil, &SharedStartingError{}
	}
	if sm.starting == nil && sm.downstream != nil && !channelClosed(sm.downstreamDone) {
		ds := sm.downstream
		sm.mu.Unlock()
		return ds, nil
	}
	if sm.starting == nil {
		reason := metrics.SpawnReasonFirstUse
		if sm.downstream != nil {
			reason = metrics.SpawnReasonLookupRespawn
		} else if sm.lastSpawnReason != "" {
			reason = sm.lastSpawnReason
		}
		sm.startDownstreamLocked(reason)
	}
	startup := sm.starting
	sm.mu.Unlock()
	select {
	case <-startup.done:
		return startup.ds, startup.err
	case <-ctx.Done():
		return nil, &SharedStartingError{Cause: ctx.Err()}
	}
}

// EnsureReady starts the same single-flight startup used by admission without
// reserving an upstream session. Additional HTTP callers receive a retryable
// refusal before SDK dispatch while that startup is pending.
func (sm *SharedSessionManager) EnsureReady(ctx context.Context) error {
	_, err := sm.getOrCreateDownstream(ctx, true)
	return err
}

func channelClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// mu serializes publication and reserves the replacement before any close.
// Neither process shutdown nor startup runs while holding manager locks.
func (sm *SharedSessionManager) startDownstreamLocked(reason string) {
	old := sm.downstream
	startup := &sharedStartup{done: make(chan struct{})}
	sm.starting = startup
	sm.downstream, sm.client, sm.cmd = nil, nil, nil
	sm.downstreamDone = nil
	sm.startedAt = time.Time{}
	sm.lastSpawnReason = reason
	sm.healthCheckFailures = 0
	go func() {
		if old != nil {
			if closeErr := old.Close(); closeErr != nil {
				sm.logger.Warn("closing shared downstream failed, respawning anyway",
					slog.String("event", "shared_session.close_failed"),
					slog.String("reason", reason), slog.String("error", closeErr.Error()))
			}
		}
		ctx, cancel := context.WithTimeout(sm.lifetimeCtx, sharedStartupTimeout)
		defer cancel()
		startedAt := time.Now()
		ds, client, cmd, err := sm.spawn(ctx, reason)
		sm.mu.Lock()
		if sm.closed {
			sm.mu.Unlock()
			if ds != nil {
				_ = ds.Close()
			}
			sm.mu.Lock()
			err = errors.New("shared session manager is closed")
		} else if err == nil {
			sm.downstream, sm.client, sm.cmd = ds, client, cmd
			sm.startedAt = startedAt
			done := make(chan struct{})
			sm.downstreamDone = done
			go func() { _ = ds.Wait(); close(done) }()
		}
		if !sm.closed {
			sm.spawnErr = err
		}
		sm.mu.Unlock()
		if err == nil {
			sm.notifySubscribers(func(sub sharedSubscriber) {
				if sub.onRespawn != nil {
					sub.onRespawn(ds)
				}
			})
		}
		sm.mu.Lock()
		if sm.closed {
			ds, err = nil, errors.New("shared session manager is closed")
		}
		startup.ds, startup.err = ds, err
		sm.starting = nil
		close(startup.done)
		sm.refMu.Lock()
		sm.armIdleTimerLocked()
		sm.refMu.Unlock()
		sm.mu.Unlock()
	}()
}

func (sm *SharedSessionManager) spawn(ctx context.Context, reason string) (*mcp.ClientSession, *mcp.Client, *exec.Cmd, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	cmd, err := sm.buildCommand()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to build command: %w", err)
	}

	// Args resolve ${VAR} at config load and may hold credentials; the count
	// identifies the spawn without writing their values to the journal.
	sm.logger.Info("spawning shared downstream subprocess",
		slog.String("event", "shared_session.spawn"),
		slog.String("command", sm.config.Command),
		slog.Int("arg_count", len(sm.config.Args)),
		slog.String("reason", reason),
	)

	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    fmt.Sprintf("vision-proxy-%s", sm.serverName),
		Version: "1.0.0",
	}, &mcp.ClientOptions{
		ToolListChangedHandler: func(ctx context.Context, _ *mcp.ToolListChangedRequest) {
			sm.notifySubscribers(func(sub sharedSubscriber) {
				if sub.onToolListChanged != nil {
					sub.onToolListChanged(ctx)
				}
			})
		},
	})

	clientSession, err := client.Connect(ctx, transport, nil)
	if cmd.Process != nil && sm.metrics != nil {
		sm.metrics.IncSpawned(reason)
	}
	if err != nil {
		sm.logger.Error("failed to connect to downstream",
			slog.String("event", "shared_session.spawn_failed"),
			slog.String("error", err.Error()),
		)
		return nil, nil, nil, fmt.Errorf("failed to connect downstream: %w", err)
	}

	sm.logger.Info("shared downstream subprocess connected",
		slog.String("event", "shared_session.connected"),
		slog.Int("pid", cmd.Process.Pid),
		slog.String("reason", reason),
	)

	return clientSession, client, cmd, nil
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
// If refCount reaches 0 and idle reap is enabled, starts the idle reap timer.
func (sm *SharedSessionManager) RemoveSession(sessionID string) error {
	sm.refMu.Lock()
	defer sm.refMu.Unlock()

	if _, exists := sm.upstreamSessions[sessionID]; !exists {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}

	delete(sm.upstreamSessions, sessionID)
	delete(sm.sessions, sessionID)
	sm.refCount--

	sm.armIdleTimerLocked()

	return nil
}

// refMu must be held. Startup completion also arms the timer, so a cancelled
// last waiter cannot leave an unreferenced subprocess running indefinitely.
func (sm *SharedSessionManager) armIdleTimerLocked() {
	if sm.refCount == 0 && sm.idleTimeout > 0 && !sm.closed {
		if sm.idleTimer != nil {
			sm.idleTimer.Stop()
		}
		sm.idleTimer = time.AfterFunc(sm.idleTimeout, sm.reapDownstream)

		sm.logger.Info("all sessions removed, idle reap timer started",
			slog.String("event", "shared_session.idle_timeout_started"),
			slog.Duration("idle_timeout", sm.idleTimeout),
		)
	}
}

// SetOnSessionExpired registers the callback used when a shared upstream
// session exceeds session_timeout. The callback owns proxy-session cleanup.
func (sm *SharedSessionManager) SetOnSessionExpired(fn func(sessionID string)) {
	sm.refMu.Lock()
	sm.onSessionExpired = fn
	sm.refMu.Unlock()
}

// TouchSession records valid application activity for one upstream session.
// Protocol pings, notifications, responses, and GET/SSE requests do not call
// this method.
func (sm *SharedSessionManager) TouchSession(sessionID string) {
	sm.refMu.Lock()
	defer sm.refMu.Unlock()
	if tracked := sm.sessions[sessionID]; tracked != nil && !tracked.expiring {
		tracked.lastActivity = time.Now()
	}
}

// BeginRequest marks an application request as in flight until the returned
// release function runs. Idle reaping never removes such a session.
func (sm *SharedSessionManager) BeginRequest(sessionID string) (func(), error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if sm.closed {
		return nil, errors.New("shared session manager is closed")
	}
	if sm.starting != nil {
		return nil, &SharedStartingError{}
	}
	sm.refMu.Lock()
	tracked := sm.sessions[sessionID]
	if tracked == nil || tracked.expiring {
		sm.refMu.Unlock()
		return nil, ErrSessionNotFound
	}
	tracked.inFlight++
	sm.activeRequests++
	sm.refMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			sm.refMu.Lock()
			if tracked.inFlight > 0 {
				tracked.inFlight--
				sm.activeRequests--
			}
			sm.refMu.Unlock()
		})
	}, nil
}

// StartReaper starts the shared upstream-session idle reaper.
func (sm *SharedSessionManager) StartReaper(ctx context.Context, checkInterval time.Duration) {
	if sm.sessionTimeout <= 0 {
		return
	}
	derivedInterval := checkInterval <= 0
	if checkInterval <= 0 {
		checkInterval = sm.sessionTimeout / 2
	}
	if derivedInterval && checkInterval < time.Second {
		checkInterval = time.Second
	}
	if checkInterval > 30*time.Second {
		checkInterval = 30 * time.Second
	}

	reaperCtx, cancel := context.WithCancel(ctx)
	sm.refMu.Lock()
	if sm.reaperCancel != nil {
		sm.reaperCancel()
	}
	sm.reaperCancel = cancel
	sm.refMu.Unlock()

	go func() {
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for {
			select {
			case <-reaperCtx.Done():
				return
			case <-ticker.C:
				sm.reapExpiredSessions()
			}
		}
	}()
}

func (sm *SharedSessionManager) reapExpiredSessions() {
	now := time.Now()
	var expired []string

	sm.refMu.Lock()
	for sessionID, tracked := range sm.sessions {
		if tracked == nil || tracked.expiring {
			continue
		}
		if tracked.inFlight != 0 {
			continue
		}
		if now.Sub(tracked.lastActivity) > sm.sessionTimeout {
			tracked.expiring = true
			expired = append(expired, sessionID)
		}
	}
	callback := sm.onSessionExpired
	sm.refMu.Unlock()

	for _, sessionID := range expired {
		if callback != nil {
			callback(sessionID)
			continue
		}
		_ = sm.RemoveSession(sessionID)
	}
}

// reapDownstream is the idle timer callback. It tears down the downstream
// subprocess when the idle timeout expires.
func (sm *SharedSessionManager) reapDownstream() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.refMu.Lock()
	defer sm.refMu.Unlock()
	if sm.refCount != 0 || sm.closed {
		return
	}
	if sm.starting != nil || sm.activeRequests != 0 {
		sm.armIdleTimerLocked()
		return
	}

	pid := 0
	if sm.cmd != nil && sm.cmd.Process != nil {
		pid = sm.cmd.Process.Pid
	}

	ds := sm.downstream
	sm.downstream = nil
	sm.spawnErr = nil
	sm.cmd = nil
	sm.client = nil
	sm.downstreamDone = nil
	sm.startedAt = time.Time{}
	sm.lastSpawnReason = metrics.SpawnReasonIdleReapRespawn
	if sm.idleTimer != nil {
		sm.idleTimer.Stop()
		sm.idleTimer = nil
	}

	// Idle teardown has no active requests. Retain the locks through Close so
	// an arriving admission cannot start a replacement before teardown finishes.
	if ds != nil {
		_ = ds.Close()
	}
	sm.logger.Info("idle timeout expired, downstream subprocess reaped",
		slog.String("event", "shared_session.idle_reaped"),
		slog.Int("pid", pid),
		slog.Duration("idle_timeout", sm.idleTimeout),
	)

	if sm.metrics != nil {
		sm.metrics.IncReaped(metrics.ReapReasonIdleTimeout)
	}
}

// CloseAll terminates the shared downstream subprocess and clears all state.
func (sm *SharedSessionManager) CloseAll() {
	sm.mu.Lock()

	if sm.closed {
		sm.mu.Unlock()
		return
	}
	sm.lifetimeCancel()

	// Cancel health probe
	if sm.healthCancel != nil {
		sm.healthCancel()
		sm.healthCancel = nil
	}

	// Cancel idle timer and clear upstream sessions
	sm.refMu.Lock()
	sm.closed = true
	if sm.reaperCancel != nil {
		sm.reaperCancel()
		sm.reaperCancel = nil
	}
	if sm.idleTimer != nil {
		sm.idleTimer.Stop()
		sm.idleTimer = nil
	}
	sm.upstreamSessions = make(map[string]struct{})
	sm.sessions = make(map[string]*sharedSession)
	sm.refCount = 0
	sm.refMu.Unlock()

	ds, startup := sm.downstream, sm.starting
	sm.downstream = nil
	sm.client = nil
	sm.spawnErr = nil
	sm.cmd = nil
	sm.startedAt = time.Time{}
	sm.downstreamDone = nil
	sm.mu.Unlock()
	if ds != nil {
		_ = ds.Close()
	}
	if startup != nil {
		<-startup.done
	}
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

// Subscribe registers callbacks for downstream replacement and tool-list changes.
func (sm *SharedSessionManager) Subscribe(sessionID string, onRespawn func(*mcp.ClientSession), onToolListChanged func(context.Context)) {
	sm.subMu.Lock()
	defer sm.subMu.Unlock()
	sm.subscribers[sessionID] = sharedSubscriber{onRespawn: onRespawn, onToolListChanged: onToolListChanged}
}

// Unsubscribe removes the downstream callbacks for the given session ID.
func (sm *SharedSessionManager) Unsubscribe(sessionID string) {
	sm.subMu.Lock()
	defer sm.subMu.Unlock()
	delete(sm.subscribers, sessionID)
}

func (sm *SharedSessionManager) notifySubscribers(notify func(sharedSubscriber)) {
	sm.subMu.RLock()
	subscribers := make([]sharedSubscriber, 0, len(sm.subscribers))
	for _, sub := range sm.subscribers {
		subscribers = append(subscribers, sub)
	}
	sm.subMu.RUnlock()
	for _, sub := range subscribers {
		func() {
			defer func() {
				if r := recover(); r != nil {
					sm.logger.Error("subscriber callback panicked",
						slog.String("event", "shared_session.subscriber_panic"),
						slog.Any("panic", r),
					)
				}
			}()
			notify(sub)
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
	if sm.closed {
		sm.mu.Unlock()
		return
	}
	healthCtx, cancel := context.WithCancel(ctx)
	sm.healthCtx, sm.healthCancel = healthCtx, cancel
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
			case <-healthCtx.Done():
				return
			case <-ticker.C:
				sm.healthCheck()
			}
		}
	}()
}

// maxConsecutiveHealthCheckTimeouts is the consecutive-failure gate for live
// transports. Threshold recovery still waits for active requests to drain.
const maxConsecutiveHealthCheckTimeouts = 3

func (sm *SharedSessionManager) healthCheck() {
	sm.probeMu.Lock()
	defer sm.probeMu.Unlock()
	sm.mu.RLock()
	ds := sm.downstream
	closed := sm.closed
	store := sm.reachabilityStore
	starting := sm.starting
	healthCtx := sm.healthCtx
	if healthCtx == nil {
		healthCtx = sm.lifetimeCtx
	}
	sm.mu.RUnlock()

	if closed || ds == nil || starting != nil || healthCtx.Err() != nil {
		return
	}

	attemptedAt := time.Now()
	if store != nil {
		store.StartProbe(sm.serverName, reachability.DepthSession, attemptedAt)
	}

	checkCtx, cancel := context.WithTimeout(healthCtx, 5*time.Second)
	defer cancel()

	_, err := ds.ListTools(checkCtx, nil)
	if healthCtx.Err() != nil {
		return
	}
	if store != nil {
		result := reachability.ProbeResult{Depth: reachability.DepthSession, AttemptedAt: attemptedAt, Disposition: reachability.DispositionSuccess}
		if err != nil {
			result.Disposition, result.Error = reachability.DispositionFailure, err.Error()
		}
		store.RecordProbe(sm.serverName, result)
	}
	sm.mu.Lock()
	if sm.closed || sm.downstream != ds || sm.starting != nil {
		sm.mu.Unlock()
		return
	}
	if err == nil {
		sm.healthCheckFailures = 0
		sm.mu.Unlock()
		return
	}
	sm.healthCheckFailures++
	failures := sm.healthCheckFailures
	transportClosed := channelClosed(sm.downstreamDone) || errors.Is(err, mcp.ErrConnectionClosed)
	sm.refMu.Lock()
	active := sm.activeRequests
	if !transportClosed && (failures < maxConsecutiveHealthCheckTimeouts || active != 0) {
		sm.refMu.Unlock()
		sm.mu.Unlock()
		sm.logger.Warn("health check failed, respawn deferred", slog.String("event", "shared_session.health_check_failed"), slog.Int("consecutive_failures", failures), slog.Int("active_requests", active), slog.String("error", err.Error()))
		return
	}
	// Reserve recovery under the same locks as BeginRequest. New application
	// calls receive a pre-dispatch refusal until replacement is published.
	sm.startDownstreamLocked(metrics.SpawnReasonHealthRespawn)
	startup := sm.starting
	sm.refMu.Unlock()
	sm.mu.Unlock()
	sm.logger.Warn("health check failed, triggering respawn", slog.String("event", "shared_session.health_check_failed"), slog.Int("consecutive_failures", failures), slog.String("error", err.Error()))
	<-startup.done
}

// ProcessStatus returns only live process identity, never arguments or env.
func (sm *SharedSessionManager) ProcessStatus() (int, time.Time) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if sm.downstream == nil || channelClosed(sm.downstreamDone) || sm.cmd == nil || sm.cmd.Process == nil {
		return 0, time.Time{}
	}
	return sm.cmd.Process.Pid, sm.startedAt
}

// HasDownstream reports whether a downstream session is currently active.
func (sm *SharedSessionManager) HasDownstream() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.downstream != nil && sm.spawnErr == nil && !channelClosed(sm.downstreamDone)
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
