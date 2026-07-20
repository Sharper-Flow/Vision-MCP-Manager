package supervisor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

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
