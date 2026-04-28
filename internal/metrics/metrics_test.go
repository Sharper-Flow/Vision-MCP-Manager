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
