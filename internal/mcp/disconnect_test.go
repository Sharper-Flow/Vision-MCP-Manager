package mcp

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDisconnectTracker_RefCount(t *testing.T) {
	tracker := newDisconnectTracker("test", 50*time.Millisecond, nil, func(sid string) *proxySession {
		return nil
	})

	sessionID := "sess-123"

	// Simulate 2 concurrent connections
	tracker.trackRequest(sessionID)
	tracker.trackRequest(sessionID)

	if got := tracker.refCount(sessionID); got != 2 {
		t.Fatalf("refcount after 2 tracks = %d, want 2", got)
	}

	// Release one
	tracker.releaseConnection(sessionID)
	if got := tracker.refCount(sessionID); got != 1 {
		t.Fatalf("refcount after 1 release = %d, want 1", got)
	}

	// Release second — should trigger grace timer
	tracker.releaseConnection(sessionID)
	if got := tracker.refCount(sessionID); got != 0 {
		t.Fatalf("refcount after 2 releases = %d, want 0", got)
	}

	// Verify grace timer is pending
	tracker.mu.Lock()
	ct, exists := tracker.sessions[sessionID]
	tracker.mu.Unlock()
	if !exists || ct.timer == nil {
		t.Fatal("expected grace timer to be started")
	}

	// Clean up
	tracker.Stop()
}

func TestDisconnectTracker_GracePeriodStarts(t *testing.T) {
	var reapCalled atomic.Int32
	tracker := newDisconnectTracker("test", 50*time.Millisecond, nil, func(sid string) *proxySession {
		return nil
	})
	tracker.onReap = func(sid string) {
		reapCalled.Add(1)
	}

	sessionID := "sess-grace"
	tracker.trackRequest(sessionID)
	tracker.releaseConnection(sessionID)

	// Wait for grace period to fire
	time.Sleep(100 * time.Millisecond)

	if reapCalled.Load() != 1 {
		t.Fatalf("reap called %d times, want 1", reapCalled.Load())
	}
}

func TestDisconnectTracker_GracePeriodCancelled(t *testing.T) {
	var reapCalled atomic.Int32
	tracker := newDisconnectTracker("test", 100*time.Millisecond, nil, func(sid string) *proxySession {
		return nil
	})
	tracker.onReap = func(sid string) {
		reapCalled.Add(1)
	}

	sessionID := "sess-cancel"
	tracker.trackRequest(sessionID)
	tracker.releaseConnection(sessionID) // refcount → 0, grace timer starts

	// Reconnect before grace period
	time.Sleep(30 * time.Millisecond)
	tracker.trackRequest(sessionID) // should cancel timer

	// Wait past original grace period
	time.Sleep(150 * time.Millisecond)

	if reapCalled.Load() != 0 {
		t.Fatalf("reap called %d times, want 0 (reconnect cancelled timer)", reapCalled.Load())
	}
}

func TestDisconnectTracker_SkipNoSessionID(t *testing.T) {
	tracker := newDisconnectTracker("test", 50*time.Millisecond, nil, nil)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := tracker.Wrap(inner)

	// Request without Mcp-Session-Id
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Should not have tracked any sessions
	tracker.mu.Lock()
	count := len(tracker.sessions)
	tracker.mu.Unlock()
	if count != 0 {
		t.Fatalf("tracked sessions = %d, want 0", count)
	}
}

func TestDisconnectTracker_SkipDELETE(t *testing.T) {
	tracker := newDisconnectTracker("test", 50*time.Millisecond, nil, nil)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := tracker.Wrap(inner)

	// DELETE request with session ID
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set("Mcp-Session-Id", "sess-delete")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	tracker.mu.Lock()
	count := len(tracker.sessions)
	tracker.mu.Unlock()
	if count != 0 {
		t.Fatalf("tracked sessions after DELETE = %d, want 0", count)
	}
}

func TestDisconnectTracker_Stop(t *testing.T) {
	var reapCalled atomic.Int32
	tracker := newDisconnectTracker("test", 50*time.Millisecond, nil, func(sid string) *proxySession {
		return nil
	})
	tracker.onReap = func(sid string) {
		reapCalled.Add(1)
	}

	sessionID := "sess-stop"
	tracker.trackRequest(sessionID)
	tracker.releaseConnection(sessionID) // refcount → 0, timer starts

	// Stop before grace period fires
	tracker.Stop()

	// Wait past grace period
	time.Sleep(100 * time.Millisecond)

	if reapCalled.Load() != 0 {
		t.Fatalf("reap called after Stop = %d, want 0", reapCalled.Load())
	}
}

func TestDisconnectTracker_HandleDelete(t *testing.T) {
	var reapCalled atomic.Int32
	tracker := newDisconnectTracker("test", 50*time.Millisecond, nil, func(sid string) *proxySession {
		return nil
	})
	tracker.onReap = func(sid string) {
		reapCalled.Add(1)
	}

	sessionID := "sess-del-handle"
	tracker.trackRequest(sessionID)
	tracker.releaseConnection(sessionID) // refcount → 0, timer starts

	// DELETE cancels timer
	tracker.HandleDelete(sessionID)

	// Wait past grace period
	time.Sleep(100 * time.Millisecond)

	if reapCalled.Load() != 0 {
		t.Fatalf("reap called after HandleDelete = %d, want 0", reapCalled.Load())
	}
}

func TestDisconnectTracker_ConcurrentConnections(t *testing.T) {
	tracker := newDisconnectTracker("test", 200*time.Millisecond, nil, func(sid string) *proxySession {
		return nil
	})

	sessionID := "sess-concurrent"
	var wg sync.WaitGroup

	// 10 goroutines track, 10 release
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.trackRequest(sessionID)
		}()
	}
	wg.Wait()

	if got := tracker.refCount(sessionID); got != 10 {
		t.Fatalf("refcount after 10 tracks = %d, want 10", got)
	}

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.releaseConnection(sessionID)
		}()
	}
	wg.Wait()

	if got := tracker.refCount(sessionID); got != 0 {
		t.Fatalf("refcount after 10 releases = %d, want 0", got)
	}

	// Should have a timer pending (grace period)
	tracker.mu.Lock()
	ct, exists := tracker.sessions[sessionID]
	tracker.mu.Unlock()
	if !exists || ct.timer == nil {
		t.Fatal("expected grace timer to be started after refcount hit 0")
	}
}
