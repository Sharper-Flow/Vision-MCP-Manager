// Package metrics provides daemon-wide atomic counters for observability.
// Distinct from supervisor.Metrics which tracks per-process ring-buffer stats;
// this package tracks daemon-level aggregate counters.
package metrics

import "sync/atomic"

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
