package metrics

import (
	"sync"
	"testing"
)

func TestDaemonMetrics_IncrementAndFetch(t *testing.T) {
	m := NewDaemonMetrics()

	// Initial values should be zero
	s := m.Snapshot()
	if s.ToolCallsTotal != 0 {
		t.Errorf("initial ToolCallsTotal = %d, want 0", s.ToolCallsTotal)
	}
	if s.ErrorsTotal != 0 {
		t.Errorf("initial ErrorsTotal = %d, want 0", s.ErrorsTotal)
	}
	if s.SessionsActive != 0 {
		t.Errorf("initial SessionsActive = %d, want 0", s.SessionsActive)
	}
	if s.SubprocessesActive != 0 {
		t.Errorf("initial SubprocessesActive = %d, want 0", s.SubprocessesActive)
	}

	// Increment and verify
	m.IncToolCalls()
	m.IncToolCalls()
	m.IncErrors()
	m.IncSessionsActive()
	m.IncSessionsActive()
	m.IncSessionsActive()
	m.IncSubprocessesActive()

	s = m.Snapshot()
	if s.ToolCallsTotal != 2 {
		t.Errorf("ToolCallsTotal = %d, want 2", s.ToolCallsTotal)
	}
	if s.ErrorsTotal != 1 {
		t.Errorf("ErrorsTotal = %d, want 1", s.ErrorsTotal)
	}
	if s.SessionsActive != 3 {
		t.Errorf("SessionsActive = %d, want 3", s.SessionsActive)
	}
	if s.SubprocessesActive != 1 {
		t.Errorf("SubprocessesActive = %d, want 1", s.SubprocessesActive)
	}
}

func TestDaemonMetrics_DecrementSessions(t *testing.T) {
	m := NewDaemonMetrics()

	m.IncSessionsActive()
	m.IncSessionsActive()
	m.DecSessionsActive()

	s := m.Snapshot()
	if s.SessionsActive != 1 {
		t.Errorf("SessionsActive = %d, want 1", s.SessionsActive)
	}
}

func TestDaemonMetrics_DecrementSubprocesses(t *testing.T) {
	m := NewDaemonMetrics()

	m.IncSubprocessesActive()
	m.DecSubprocessesActive()

	s := m.Snapshot()
	if s.SubprocessesActive != 0 {
		t.Errorf("SubprocessesActive = %d, want 0", s.SubprocessesActive)
	}
}

func TestDaemonMetrics_ConcurrentAccess(t *testing.T) {
	m := NewDaemonMetrics()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.IncToolCalls()
			m.IncSessionsActive()
			m.DecSessionsActive()
		}()
	}
	wg.Wait()

	s := m.Snapshot()
	if s.ToolCallsTotal != 100 {
		t.Errorf("ToolCallsTotal = %d, want 100", s.ToolCallsTotal)
	}
	if s.SessionsActive != 0 {
		t.Errorf("SessionsActive = %d, want 0 (all inc/dec paired)", s.SessionsActive)
	}
}

func TestDaemonMetrics_SnapshotIsCopy(t *testing.T) {
	m := NewDaemonMetrics()
	m.IncToolCalls()

	s1 := m.Snapshot()
	m.IncToolCalls()
	s2 := m.Snapshot()

	if s1.ToolCallsTotal != 1 {
		t.Errorf("s1.ToolCallsTotal = %d, want 1 (snapshot before second inc)", s1.ToolCallsTotal)
	}
	if s2.ToolCallsTotal != 2 {
		t.Errorf("s2.ToolCallsTotal = %d, want 2 (snapshot after second inc)", s2.ToolCallsTotal)
	}
}

func TestServerMetrics_BasicOperations(t *testing.T) {
	m := NewServerMetrics()

	// Initial state
	s := m.Snapshot()
	if s.ActiveSessions != 0 {
		t.Errorf("initial ActiveSessions = %d, want 0", s.ActiveSessions)
	}
	if s.AdmissionDenied != 0 {
		t.Errorf("initial AdmissionDenied = %d, want 0", s.AdmissionDenied)
	}
	if len(s.ReapedByReason) != 0 {
		t.Errorf("initial ReapedByReason = %v, want empty", s.ReapedByReason)
	}

	// Inc/dec active sessions
	m.IncActiveSessions()
	m.IncActiveSessions()
	m.DecActiveSessions()
	s = m.Snapshot()
	if s.ActiveSessions != 1 {
		t.Errorf("ActiveSessions = %d, want 1", s.ActiveSessions)
	}

	// Admission denial
	m.IncAdmissionDenied()
	m.IncAdmissionDenied()
	s = m.Snapshot()
	if s.AdmissionDenied != 2 {
		t.Errorf("AdmissionDenied = %d, want 2", s.AdmissionDenied)
	}

	// Reap by reason
	m.IncReaped("client_disconnected")
	m.IncReaped("idle_timeout")
	m.IncReaped("client_disconnected")
	s = m.Snapshot()
	if s.ReapedByReason["client_disconnected"] != 2 {
		t.Errorf("ReapedByReason[client_disconnected] = %d, want 2", s.ReapedByReason["client_disconnected"])
	}
	if s.ReapedByReason["idle_timeout"] != 1 {
		t.Errorf("ReapedByReason[idle_timeout] = %d, want 1", s.ReapedByReason["idle_timeout"])
	}
}

func TestNormalizeReapReason(t *testing.T) {
	tests := map[string]string{
		"never_streamed":      "never_streamed",
		"client_disconnected": "client_disconnected",
		"genuinely unknown":   "unknown",
	}
	for input, want := range tests {
		if got := NormalizeReapReason(input); got != want {
			t.Errorf("NormalizeReapReason(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestServerMetrics_BackendHeaderTimeoutIsIndependentFromReaps(t *testing.T) {
	m := NewServerMetrics()

	m.IncBackendHeaderTimeout()
	m.IncBackendHeaderTimeout()
	m.IncReaped("client_disconnected")

	s := m.Snapshot()
	if s.BackendHeaderTimeout != 2 {
		t.Errorf("BackendHeaderTimeout = %d, want 2", s.BackendHeaderTimeout)
	}
	if _, ok := s.ReapedByReason["backend_header_timeout"]; ok {
		t.Error("backend header timeout should not appear in ReapedByReason")
	}
}

func TestServerMetrics_NormalizesReapReasons(t *testing.T) {
	m := NewServerMetrics()

	m.IncReaped("upstream delete")
	m.IncReaped("session removed by manager")
	m.IncReaped("health check failed")
	m.IncReaped("initial tools/list failed")
	m.IncReaped("surprising future reason")

	s := m.Snapshot()
	cases := map[string]int64{
		ReapReasonUpstreamDelete:  1,
		ReapReasonSessionRemoved:  1,
		ReapReasonHealthCheck:     1,
		ReapReasonToolsListFailed: 1,
		ReapReasonUnknown:         1,
	}
	for reason, want := range cases {
		if got := s.ReapedByReason[reason]; got != want {
			t.Errorf("ReapedByReason[%s] = %d, want %d", reason, got, want)
		}
	}
	if _, ok := s.ReapedByReason["surprising future reason"]; ok {
		t.Error("raw unknown reason should not be exposed as metric key")
	}
}

func TestServerMetrics_ConcurrentAccess(t *testing.T) {
	m := NewServerMetrics()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.IncActiveSessions()
			m.DecActiveSessions()
			m.IncReaped("client_disconnected")
			m.IncAdmissionDenied()
		}()
	}
	wg.Wait()

	s := m.Snapshot()
	if s.ActiveSessions != 0 {
		t.Errorf("ActiveSessions = %d, want 0", s.ActiveSessions)
	}
	if s.ReapedByReason["client_disconnected"] != 100 {
		t.Errorf("ReapedByReason[client_disconnected] = %d, want 100", s.ReapedByReason["client_disconnected"])
	}
	if s.AdmissionDenied != 100 {
		t.Errorf("AdmissionDenied = %d, want 100", s.AdmissionDenied)
	}
}
