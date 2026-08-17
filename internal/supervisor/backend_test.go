package supervisor

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestBackendCoordinatorDrainRejectsDeadlineContextWithoutChangingState(t *testing.T) {
	c := NewBackendCoordinator()
	c.MarkReady()

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if err := c.DrainAndRecycle(ctx, func() error { return nil }); err != ErrDrainContextHasDeadline {
		t.Fatalf("DrainAndRecycle() error = %v, want ErrDrainContextHasDeadline", err)
	}
	if got := c.State(); got != BackendReady {
		t.Fatalf("State() after rejected drain = %q, want ready", got)
	}
}

func TestBackendCoordinatorCancelledDrainTransitionsToRestarting(t *testing.T) {
	c := NewBackendCoordinator()
	c.MarkReady()
	done, err := c.BeginRequest()
	if err != nil {
		t.Fatal(err)
	}
	defer done()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.DrainAndRecycle(ctx, func() error { return nil }); err != context.Canceled {
		t.Fatalf("DrainAndRecycle() error = %v, want context.Canceled", err)
	}
	if got := c.State(); got != BackendRestarting {
		t.Fatalf("State() after cancelled drain = %q, want restarting", got)
	}
}

// This is the regression guard for the original defect. It is deliberately
// phrased as an invariant ("never stranded in draining") rather than asserting
// a specific successor state, so it keeps guarding the defect class even if the
// cancellation target legitimately changes. Verified to fail against the
// pre-fix coordinator at trunk 2278d65 with:
//
//	stranded in "draining" after failed drain: backend is permanently unavailable
//
// Nothing exits BackendDraining, and DrainAndRecycle itself gates entry on
// BackendReady, so a stranded coordinator cannot even retry its own recycle.
func TestBackendCoordinatorFailedDrainNeverStrandsDraining(t *testing.T) {
	c := NewBackendCoordinator()
	c.MarkReady()
	done, err := c.BeginRequest()
	if err != nil {
		t.Fatal(err)
	}
	defer done()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // caller gives up while the request is still in flight

	if err := c.DrainAndRecycle(ctx, func() error {
		// ADR 0001 lines 27-28: never tear down with admitted work in flight.
		t.Error("recycle must never run with a request in flight")
		return nil
	}); err == nil {
		t.Fatal("DrainAndRecycle() error = nil, want failure")
	}

	if got := c.State(); got == BackendDraining {
		t.Fatalf("stranded in %q after failed drain: backend is permanently unavailable", got)
	}
}

func TestBackendCoordinatorDeadlineDrainDoesNotStart(t *testing.T) {
	c := NewBackendCoordinator()
	c.MarkReady()
	requestDone, err := c.BeginRequest()
	if err != nil {
		t.Fatal(err)
	}
	defer requestDone()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now())
	defer cancel()
	var recycleCalls atomic.Int32
	err = c.DrainAndRecycle(ctx, func() error {
		recycleCalls.Add(1)
		return nil
	})
	if err == nil {
		t.Fatal("DrainAndRecycle() unexpectedly succeeded")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DrainAndRecycle() returned context deadline error: %v", err)
	}
	if got := c.State(); got != BackendReady {
		t.Fatalf("state after deadline drain = %q, want ready", got)
	}
	if got := recycleCalls.Load(); got != 0 {
		t.Fatalf("recycle calls = %d, want 0", got)
	}
}

func TestBackendCoordinatorDrainWaitsForInFlight(t *testing.T) {
	c := NewBackendCoordinator()
	c.MarkReady()
	done, err := c.BeginRequest()
	if err != nil {
		t.Fatal(err)
	}

	recycleStarted := make(chan struct{})
	recycleDone := make(chan error, 1)
	go func() {
		recycleDone <- c.DrainAndRecycle(context.Background(), func() error {
			close(recycleStarted)
			return nil
		})
	}()

	select {
	case <-recycleStarted:
		t.Fatal("recycle started before in-flight request completed")
	case <-time.After(25 * time.Millisecond):
	}
	if c.State() != BackendDraining {
		t.Fatalf("State() = %q, want draining", c.State())
	}
	if _, err := c.BeginRequest(); err != ErrBackendUnavailable {
		t.Fatalf("BeginRequest() while draining error = %v, want ErrBackendUnavailable", err)
	}

	done()
	select {
	case err := <-recycleDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("drain did not complete")
	}
	if c.State() != BackendProbing {
		t.Fatalf("State() after recycle = %q, want probing", c.State())
	}
	c.MarkReady()
	if c.State() != BackendReady {
		t.Fatalf("State() = %q, want ready", c.State())
	}
}

func TestBackendCoordinatorCompletionGuardIsIdempotent(t *testing.T) {
	c := NewBackendCoordinator()
	c.MarkReady()
	done, err := c.BeginRequest()
	if err != nil {
		t.Fatal(err)
	}
	done()
	done()
	if got := c.InFlight(); got != 0 {
		t.Fatalf("InFlight() = %d, want 0", got)
	}
}

func TestBackendCoordinatorSerializesRecycle(t *testing.T) {
	c := NewBackendCoordinator()
	c.MarkReady()
	var calls atomic.Int32
	fn := func() error { calls.Add(1); return nil }

	if err := c.DrainAndRecycle(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
	if err := c.DrainAndRecycle(context.Background(), fn); err != ErrBackendUnavailable {
		t.Fatalf("second DrainAndRecycle() error = %v, want ErrBackendUnavailable", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("recycle calls = %d, want 1", calls.Load())
	}
}

func TestBackendCoordinatorConcurrentSlowDrainRecyclesOnceAfterInFlight(t *testing.T) {
	c := NewBackendCoordinator()
	c.MarkReady()
	requestDone, err := c.BeginRequest()
	if err != nil {
		t.Fatal(err)
	}

	const callers = 8
	start := make(chan struct{})
	results := make(chan error, callers)
	var recycleCalls atomic.Int32
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			results <- c.DrainAndRecycle(context.Background(), func() error {
				if got := c.InFlight(); got != 0 {
					t.Errorf("recycle started with %d requests in flight", got)
				}
				recycleCalls.Add(1)
				return nil
			})
		}()
	}
	close(start)

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for c.State() != BackendDraining {
		select {
		case <-deadline.C:
			t.Fatalf("backend state = %q, want draining", c.State())
		default:
			runtime.Gosched()
		}
	}
	requestDone()

	for i := 0; i < callers; i++ {
		select {
		case err := <-results:
			if err != nil && err != ErrBackendUnavailable {
				t.Errorf("DrainAndRecycle() error = %v", err)
			}
		case <-deadline.C:
			t.Fatal("concurrent drains did not complete")
		}
	}
	if got := recycleCalls.Load(); got != 1 {
		t.Fatalf("recycle calls = %d, want 1", got)
	}
	if got := c.State(); got != BackendProbing {
		t.Fatalf("state after concurrent drain = %q, want probing", got)
	}
}
