package supervisor

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// HealthChecker performs periodic health checks on MCP servers.
// It probes servers using the MCP tools/list method and tracks consecutive failures.
type HealthChecker struct {
	bridge           BridgeProvider
	checkInterval    time.Duration
	checkTimeout     time.Duration
	failureThreshold int

	// State tracking
	mu               sync.RWMutex
	consecutiveFails int
	lastCheck        time.Time
	lastError        error
	healthy          atomic.Bool

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	logger *slog.Logger

	// Callback for triggering restart
	onUnhealthy func()
}

// BridgeProvider provides access to the stdio bridge for health checks.
type BridgeProvider interface {
	ForwardRequest(ctx context.Context, data []byte) ([]byte, error)
}

// HealthCheckerConfig configures the health checker.
type HealthCheckerConfig struct {
	CheckInterval    time.Duration // How often to check (default: 30s)
	CheckTimeout     time.Duration // Timeout for each check (default: 10s)
	FailureThreshold int           // Consecutive failures before unhealthy (default: 3)
	Logger           *slog.Logger
	OnUnhealthy      func() // Called when threshold exceeded
}

// NewHealthChecker creates a new health checker for an MCP server.
func NewHealthChecker(bridge BridgeProvider, cfg HealthCheckerConfig) *HealthChecker {
	if cfg.CheckInterval == 0 {
		cfg.CheckInterval = 30 * time.Second
	}
	if cfg.CheckTimeout == 0 {
		cfg.CheckTimeout = 10 * time.Second
	}
	if cfg.FailureThreshold == 0 {
		cfg.FailureThreshold = 3
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())

	hc := &HealthChecker{
		bridge:           bridge,
		checkInterval:    cfg.CheckInterval,
		checkTimeout:     cfg.CheckTimeout,
		failureThreshold: cfg.FailureThreshold,
		ctx:              ctx,
		cancel:           cancel,
		logger:           cfg.Logger,
		onUnhealthy:      cfg.OnUnhealthy,
	}
	hc.healthy.Store(true) // Assume healthy until proven otherwise

	return hc
}

// Start begins periodic health checking.
func (hc *HealthChecker) Start() {
	hc.wg.Add(1)
	go hc.checkLoop()
}

// Stop stops the health checker.
func (hc *HealthChecker) Stop() {
	hc.cancel()
	hc.wg.Wait()
}

// IsHealthy returns the current health status.
func (hc *HealthChecker) IsHealthy() bool {
	return hc.healthy.Load()
}

// Status returns detailed health status.
func (hc *HealthChecker) Status() HealthStatus {
	hc.mu.RLock()
	defer hc.mu.RUnlock()

	var lastErr string
	if hc.lastError != nil {
		lastErr = hc.lastError.Error()
	}

	return HealthStatus{
		Healthy:          hc.healthy.Load(),
		ConsecutiveFails: hc.consecutiveFails,
		LastCheck:        hc.lastCheck,
		LastError:        lastErr,
	}
}

// HealthStatus contains the current health check status.
type HealthStatus struct {
	Healthy          bool      `json:"healthy"`
	ConsecutiveFails int       `json:"consecutive_fails"`
	LastCheck        time.Time `json:"last_check"`
	LastError        string    `json:"last_error,omitempty"`
}

// checkLoop runs periodic health checks.
func (hc *HealthChecker) checkLoop() {
	defer hc.wg.Done()

	ticker := time.NewTicker(hc.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-hc.ctx.Done():
			return
		case <-ticker.C:
			hc.performCheck()
		}
	}
}

// performCheck executes a single health check.
func (hc *HealthChecker) performCheck() {
	ctx, cancel := context.WithTimeout(hc.ctx, hc.checkTimeout)
	defer cancel()

	err := hc.probe(ctx)

	hc.mu.Lock()
	hc.lastCheck = time.Now()
	hc.lastError = err

	if err != nil {
		hc.consecutiveFails++
		hc.logger.Debug("health check failed",
			slog.Int("consecutive_fails", hc.consecutiveFails),
			slog.String("error", err.Error()),
		)

		if hc.consecutiveFails >= hc.failureThreshold {
			hc.healthy.Store(false)
			hc.mu.Unlock()

			hc.logger.Warn("server marked unhealthy",
				slog.Int("consecutive_fails", hc.consecutiveFails),
			)

			if hc.onUnhealthy != nil {
				hc.onUnhealthy()
			}
			return
		}
	} else {
		hc.consecutiveFails = 0
		hc.healthy.Store(true)
	}
	hc.mu.Unlock()
}

// probe sends a tools/list request to verify the server is responsive.
func (hc *HealthChecker) probe(ctx context.Context) error {
	// Build JSON-RPC request for tools/list
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "health-check",
		"method":  "tools/list",
		"params":  map[string]interface{}{},
	}

	data, err := json.Marshal(req)
	if err != nil {
		return err
	}

	resp, err := hc.bridge.ForwardRequest(ctx, data)
	if err != nil {
		return err
	}

	// Parse response to check for errors
	var result map[string]interface{}
	if err := json.Unmarshal(resp, &result); err != nil {
		return err
	}

	// Check if response contains an error
	if errObj, ok := result["error"]; ok && errObj != nil {
		return &ProbeError{Message: "server returned error"}
	}

	return nil
}

// ProbeError indicates a health probe failure.
type ProbeError struct {
	Message string
}

func (e *ProbeError) Error() string {
	return e.Message
}

// --- Startup Probe ---

// StartupProbe verifies that an MCP server has started correctly.
// It sends an initialize request and waits for a successful response.
type StartupProbe struct {
	bridge       BridgeProvider
	timeout      time.Duration
	pollInterval time.Duration
	logger       *slog.Logger
}

// StartupProbeConfig configures the startup probe.
type StartupProbeConfig struct {
	Timeout      time.Duration // Total time to wait for startup (default: 30s)
	PollInterval time.Duration // How often to retry (default: 1s)
	Logger       *slog.Logger
}

// NewStartupProbe creates a new startup probe.
func NewStartupProbe(bridge BridgeProvider, cfg StartupProbeConfig) *StartupProbe {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 1 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	return &StartupProbe{
		bridge:       bridge,
		timeout:      cfg.Timeout,
		pollInterval: cfg.PollInterval,
		logger:       cfg.Logger,
	}
}

// Wait blocks until the server is ready or timeout expires.
// Returns nil if server is ready, error if timeout or probe failure.
func (sp *StartupProbe) Wait(ctx context.Context) error {
	deadline := time.Now().Add(sp.timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := sp.probe(ctx)
		if err == nil {
			sp.logger.Info("server startup probe succeeded")
			return nil
		}

		sp.logger.Debug("startup probe attempt failed",
			slog.String("error", err.Error()),
			slog.Duration("remaining", time.Until(deadline)),
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sp.pollInterval):
		}
	}

	return &StartupTimeoutError{Timeout: sp.timeout}
}

// probe sends an initialize request to check if the server is ready.
func (sp *StartupProbe) probe(ctx context.Context) error {
	// Build JSON-RPC request for initialize
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "startup-probe",
		"method":  "initialize",
		"params": map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{},
			"clientInfo": map[string]interface{}{
				"name":    "vision-probe",
				"version": "1.0.0",
			},
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		return err
	}

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := sp.bridge.ForwardRequest(probeCtx, data)
	if err != nil {
		return err
	}

	// Parse response
	var result map[string]interface{}
	if err := json.Unmarshal(resp, &result); err != nil {
		return err
	}

	// Check for error response
	if errObj, ok := result["error"]; ok && errObj != nil {
		return &ProbeError{Message: "initialize returned error"}
	}

	return nil
}

// StartupTimeoutError indicates the server didn't start in time.
type StartupTimeoutError struct {
	Timeout time.Duration
}

func (e *StartupTimeoutError) Error() string {
	return "server did not start within " + e.Timeout.String()
}

// --- Metrics Tracking ---

// Metrics tracks request statistics for an MCP server.
// Uses a ring buffer for response times to avoid memory allocations
// and provide O(1) insertions with bounded memory usage.
type Metrics struct {
	mu sync.RWMutex

	// Counters
	requestCount int64
	errorCount   int64

	// Response times ring buffer (in nanoseconds)
	responseTimes [1000]int64 // Fixed-size ring buffer
	head          int         // Next write position
	count         int         // Number of samples (max 1000)

	// Uptime tracking
	startTime time.Time
}

// NewMetrics creates a new metrics tracker.
func NewMetrics() *Metrics {
	return &Metrics{
		startTime: time.Now(),
	}
}

// RecordRequest records a request completion.
func (m *Metrics) RecordRequest(duration time.Duration, isError bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.requestCount++
	if isError {
		m.errorCount++
	}

	// Add response time to ring buffer (O(1), no allocations)
	m.responseTimes[m.head] = duration.Nanoseconds()
	m.head = (m.head + 1) % len(m.responseTimes)
	if m.count < len(m.responseTimes) {
		m.count++
	}
}

// Snapshot returns current metrics.
func (m *Metrics) Snapshot() MetricsSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	snap := MetricsSnapshot{
		RequestCount: m.requestCount,
		ErrorCount:   m.errorCount,
		Uptime:       time.Since(m.startTime),
	}

	if m.count > 0 {
		snap.AvgResponseTime = m.calculateAvg()
		snap.P95ResponseTime = m.calculateP95()
	}

	return snap
}

// calculateAvg calculates average response time.
func (m *Metrics) calculateAvg() time.Duration {
	if m.count == 0 {
		return 0
	}

	var sum int64
	for i := 0; i < m.count; i++ {
		sum += m.responseTimes[i]
	}
	return time.Duration(sum / int64(m.count))
}

// calculateP95 calculates 95th percentile response time.
// Note: This is an approximation - proper implementation would use sorted data
func (m *Metrics) calculateP95() time.Duration {
	if m.count == 0 {
		return 0
	}

	// Simple approach: sort a copy and get 95th percentile
	sorted := make([]int64, m.count)
	copy(sorted, m.responseTimes[:m.count])

	// Insertion sort (good enough for 1000 samples)
	for i := 1; i < len(sorted); i++ {
		key := sorted[i]
		j := i - 1
		for j >= 0 && sorted[j] > key {
			sorted[j+1] = sorted[j]
			j--
		}
		sorted[j+1] = key
	}

	idx := int(float64(len(sorted)) * 0.95)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}

	return time.Duration(sorted[idx])
}

// Reset resets all metrics.
func (m *Metrics) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.requestCount = 0
	m.errorCount = 0
	m.head = 0
	m.count = 0
	m.startTime = time.Now()
}

// MetricsSnapshot contains a point-in-time metrics snapshot.
type MetricsSnapshot struct {
	RequestCount    int64         `json:"request_count"`
	ErrorCount      int64         `json:"error_count"`
	Uptime          time.Duration `json:"uptime"`
	AvgResponseTime time.Duration `json:"avg_response_time"`
	P95ResponseTime time.Duration `json:"p95_response_time"`
}
