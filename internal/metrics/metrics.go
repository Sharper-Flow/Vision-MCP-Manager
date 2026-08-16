// Package metrics provides daemon-wide atomic counters for observability.
// Distinct from supervisor.Metrics which tracks per-process ring-buffer stats;
// this package tracks daemon-level aggregate counters.
package metrics

import (
	"sync"
	"sync/atomic"
)

// Snapshot holds a point-in-time copy of daemon metrics.
type Snapshot struct {
	ToolCallsTotal     int64 `json:"tool_calls_total"`
	ErrorsTotal        int64 `json:"errors_total"`
	SessionsActive     int64 `json:"sessions_active"`
	SubprocessesActive int64 `json:"subprocesses_active"`
}

// DaemonMetrics provides thread-safe daemon-wide counters using sync/atomic.
type DaemonMetrics struct {
	toolCallsTotal     atomic.Int64
	errorsTotal        atomic.Int64
	sessionsActive     atomic.Int64
	subprocessesActive atomic.Int64
}

// NewDaemonMetrics creates a new zeroed DaemonMetrics.
func NewDaemonMetrics() *DaemonMetrics {
	return &DaemonMetrics{}
}

// IncToolCalls atomically increments the tool call counter.
func (m *DaemonMetrics) IncToolCalls() {
	m.toolCallsTotal.Add(1)
}

// IncErrors atomically increments the error counter.
func (m *DaemonMetrics) IncErrors() {
	m.errorsTotal.Add(1)
}

// IncSessionsActive atomically increments the active session counter.
func (m *DaemonMetrics) IncSessionsActive() {
	m.sessionsActive.Add(1)
}

// DecSessionsActive atomically decrements the active session counter.
func (m *DaemonMetrics) DecSessionsActive() {
	m.sessionsActive.Add(-1)
}

// IncSubprocessesActive atomically increments the active subprocess counter.
func (m *DaemonMetrics) IncSubprocessesActive() {
	m.subprocessesActive.Add(1)
}

// DecSubprocessesActive atomically decrements the active subprocess counter.
func (m *DaemonMetrics) DecSubprocessesActive() {
	m.subprocessesActive.Add(-1)
}

// Snapshot returns a point-in-time copy of all counters.
func (m *DaemonMetrics) Snapshot() Snapshot {
	return Snapshot{
		ToolCallsTotal:     m.toolCallsTotal.Load(),
		ErrorsTotal:        m.errorsTotal.Load(),
		SessionsActive:     m.sessionsActive.Load(),
		SubprocessesActive: m.subprocessesActive.Load(),
	}
}

// ServerMetricsSnapshot holds a point-in-time copy of per-server metrics.
type ServerMetricsSnapshot struct {
	ActiveSessions       int64            `json:"active_sessions"`
	ReapedByReason       map[string]int64 `json:"reaped_by_reason"`
	AdmissionDenied      int64            `json:"admission_denied"`
	BackendHeaderTimeout int64            `json:"backend_header_timeout"`
}

const (
	ReapReasonClientDisconnected = "client_disconnected"
	ReapReasonNeverStreamed      = "never_streamed"
	ReapReasonIdleTimeout        = "idle_timeout"
	ReapReasonUpstreamDelete     = "upstream_delete"
	ReapReasonSessionRemoved     = "session_removed"
	ReapReasonHealthCheck        = "health_check"
	ReapReasonToolsListFailed    = "tools_list_failed"
	ReapReasonProcessLost        = "process_lost"
	ReapReasonListenerClosed     = "listener_closed"
	ReapReasonUnknown            = "unknown"
)

// ServerMetrics provides thread-safe per-server counters using sync/atomic.
// Tracks active sessions, reap counts by reason, admission denials, and backend header timeouts.
type ServerMetrics struct {
	activeSessions       atomic.Int64
	admissionDenied      atomic.Int64
	backendHeaderTimeout atomic.Int64

	mu             sync.Mutex
	reapedByReason map[string]int64
}

// ServerMetricsReporter decouples consumers from concrete ServerMetrics.
// SharedSessionManager uses this interface so tests can inject stubs.
type ServerMetricsReporter interface {
	IncActiveSessions()
	DecActiveSessions()
	IncAdmissionDenied()
	IncReaped(reason string)
}

// NewServerMetrics creates a new zeroed ServerMetrics.
func NewServerMetrics() *ServerMetrics {
	return &ServerMetrics{
		reapedByReason: make(map[string]int64),
	}
}

// IncActiveSessions atomically increments the active session counter.
func (m *ServerMetrics) IncActiveSessions() {
	m.activeSessions.Add(1)
}

// DecActiveSessions atomically decrements the active session counter.
func (m *ServerMetrics) DecActiveSessions() {
	m.activeSessions.Add(-1)
}

// IncAdmissionDenied atomically increments the admission denial counter.
func (m *ServerMetrics) IncAdmissionDenied() {
	m.admissionDenied.Add(1)
}

// IncBackendHeaderTimeout atomically increments the backend header timeout counter.
func (m *ServerMetrics) IncBackendHeaderTimeout() {
	m.backendHeaderTimeout.Add(1)
}

// IncReaped increments the reap counter for the given reason.
// Reasons are normalized to bounded canonical keys before counting.
func (m *ServerMetrics) IncReaped(reason string) {
	reason = NormalizeReapReason(reason)
	m.mu.Lock()
	m.reapedByReason[reason]++
	m.mu.Unlock()
}

// NormalizeReapReason maps legacy close reason text to canonical metrics keys.
// Unknown input collapses to "unknown" to avoid unbounded metric cardinality.
func NormalizeReapReason(reason string) string {
	switch reason {
	case ReapReasonClientDisconnected:
		return ReapReasonClientDisconnected
	case ReapReasonNeverStreamed:
		return ReapReasonNeverStreamed
	case ReapReasonIdleTimeout, "idle timeout":
		return ReapReasonIdleTimeout
	case ReapReasonUpstreamDelete, "upstream delete":
		return ReapReasonUpstreamDelete
	case ReapReasonSessionRemoved, "session removed by manager":
		return ReapReasonSessionRemoved
	case ReapReasonHealthCheck, "health check failed":
		return ReapReasonHealthCheck
	case ReapReasonToolsListFailed, "initial tools/list failed":
		return ReapReasonToolsListFailed
	case ReapReasonProcessLost:
		return ReapReasonProcessLost
	case ReapReasonListenerClosed:
		return ReapReasonListenerClosed
	default:
		return ReapReasonUnknown
	}
}

// Snapshot returns a point-in-time copy of all per-server counters.
func (m *ServerMetrics) Snapshot() ServerMetricsSnapshot {
	m.mu.Lock()
	reaped := make(map[string]int64, len(m.reapedByReason))
	for k, v := range m.reapedByReason {
		reaped[k] = v
	}
	m.mu.Unlock()

	return ServerMetricsSnapshot{
		ActiveSessions:       m.activeSessions.Load(),
		ReapedByReason:       reaped,
		AdmissionDenied:      m.admissionDenied.Load(),
		BackendHeaderTimeout: m.backendHeaderTimeout.Load(),
	}
}
