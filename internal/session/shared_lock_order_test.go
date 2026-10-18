package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
)

func TestSharedManagerLockOrder(t *testing.T) {
	if schedule := os.Getenv("VISION_SHARED_LOCK_ORDER_CASE"); schedule != "" {
		runSharedManagerLockOrder(t, schedule)
		return
	}
	for _, schedule := range []string{"remove_close", "remove_reap", "reap_close"} {
		t.Run(schedule, func(t *testing.T) {
			// A deadlock must not leave blocked goroutines in the parent suite.
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSharedManagerLockOrder$", "-test.timeout=10s")
			cmd.Env = append(os.Environ(), "VISION_SHARED_LOCK_ORDER_CASE="+schedule)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("lock-order schedule %s: %v\n%s", schedule, err, output)
			}
		})
	}
}

func runSharedManagerLockOrder(t *testing.T, schedule string) {
	sm := NewSharedSessionManager("lock-order", &config.ServerConfig{}, testLogger(t), time.Hour, nil)
	retainedState := errors.New("downstream generation state")
	sm.mu.Lock()
	sm.spawnErr = retainedState
	sm.mu.Unlock()
	seedSession := func() {
		sm.refMu.Lock()
		sm.upstreamSessions["session"] = struct{}{}
		sm.sessions["session"] = &sharedSession{lastActivity: time.Now()}
		sm.refCount = 1
		sm.refMu.Unlock()
	}
	if schedule == "remove_close" {
		seedSession()
	}

	// Hold teardown until the opposing operation reaches its lock boundary
	// or finishes retiring the lease.
	sm.mu.RLock()
	unlock := sync.OnceFunc(sm.mu.RUnlock)
	defer unlock()
	done := make(chan error, 2)
	go func() {
		if schedule == "remove_reap" {
			sm.reapDownstream()
		} else {
			sm.CloseAll()
		}
		done <- nil
	}()
	waitForSharedLockState(t, "teardown writer queued", func() bool {
		if sm.mu.TryRLock() {
			sm.mu.RUnlock()
			return false
		}
		return true
	})
	if schedule == "remove_reap" {
		// Model a lease arriving while an idle reap waits to tear down.
		seedSession()
	}
	method := "(*SharedSessionManager).RemoveSession("
	if schedule == "reap_close" {
		method = "(*SharedSessionManager).reapDownstream("
	}
	go func() {
		if schedule == "reap_close" {
			sm.reapDownstream()
			done <- nil
		} else {
			done <- sm.RemoveSession("session")
		}
	}()
	waitForSharedLockState(t, "opposing operation completed or blocked on its mutex", func() bool {
		if len(done) != 0 {
			return true
		}
		// Observe the blocked state instead of assuming the goroutine ran.
		var stack [64 * 1024]byte
		n := runtime.Stack(stack[:], true)
		for _, goroutine := range strings.Split(string(stack[:n]), "\n\n") {
			header, _, _ := strings.Cut(goroutine, "\n")
			if strings.Contains(header, "[sync.") && strings.Contains(goroutine, method) {
				return true
			}
		}
		return false
	})
	var retiredTimer *time.Timer
	if schedule == "remove_reap" && len(done) != 0 {
		sm.refMu.Lock()
		retiredTimer = sm.idleTimer
		sm.refMu.Unlock()
	}
	unlock()

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-deadline.C:
			t.Fatalf("shared manager lock cycle in %s", schedule)
		}
	}
	// Cleanup is safe only after both operations have returned.
	defer sm.CloseAll()
	if got := sm.RefCount(); got != 0 {
		t.Errorf("reference count = %d, want 0", got)
	}
	if got := sm.SessionCount(); got != 0 {
		t.Errorf("session count = %d, want 0", got)
	}
	wantClosed := schedule != "remove_reap"
	if got := sm.isClosed(); got != wantClosed {
		t.Errorf("closed = %v, want %v", got, wantClosed)
	}
	sm.mu.RLock()
	stateRetained := sm.spawnErr == retainedState
	sm.mu.RUnlock()
	if stateRetained {
		t.Error("teardown retained downstream state")
	}
	sm.refMu.Lock()
	hasIdleTimer := sm.idleTimer != nil
	sm.refMu.Unlock()
	if hasIdleTimer {
		t.Error("teardown retained an idle timer")
	}
	if retiredTimer != nil && retiredTimer.Stop() {
		t.Error("reap discarded an idle timer without stopping it")
	}
}

func TestSharedManagerReapKeepsReferencedGeneration(t *testing.T) {
	sm := NewSharedSessionManager("referenced-generation", &config.ServerConfig{}, testLogger(t), time.Hour, nil)
	defer sm.CloseAll()
	retainedState := errors.New("referenced downstream generation")
	sm.mu.Lock()
	sm.spawnErr = retainedState
	sm.mu.Unlock()
	sm.refMu.Lock()
	sm.upstreamSessions["session"] = struct{}{}
	sm.sessions["session"] = &sharedSession{lastActivity: time.Now()}
	sm.refCount = 1
	sm.refMu.Unlock()

	sm.reapDownstream()
	if sm.RefCount() != 1 || sm.SessionCount() != 1 {
		t.Error("reap changed the referenced session population")
	}
	sm.mu.RLock()
	stateRetained := sm.spawnErr == retainedState
	sm.mu.RUnlock()
	if !stateRetained {
		t.Error("reap cleared a referenced downstream generation")
	}
}

func waitForSharedLockState(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for !ready() {
		select {
		case <-deadline.C:
			t.Fatalf("did not observe %s", description)
		default:
			runtime.Gosched()
		}
	}
}
