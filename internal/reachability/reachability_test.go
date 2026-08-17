package reachability

import (
	"sync"
	"testing"
	"time"
)

func TestStoreInitialStateIsUnprobed(t *testing.T) {
	store := NewStore()

	reachability, ok := store.Get("alpha")
	if ok {
		t.Fatalf("Get on an unseen server returned ok with %#v", reachability)
	}
	if got := reachability.State; got != StateUnprobed {
		t.Fatalf("initial state = %q, want %q", got, StateUnprobed)
	}
}

func TestStoreFailureThresholdAndSuccessReset(t *testing.T) {
	store := NewStore()
	at := time.Unix(100, 0)

	for i := 1; i <= 2; i++ {
		got := store.RecordProbe("alpha", ProbeResult{
			AttemptedAt: at.Add(time.Duration(i) * time.Second),
			Depth:       DepthListener,
			Error:       "connection refused",
		})
		if got.State == StateUnreachable {
			t.Fatalf("failure %d reported unreachable", i)
		}
		if got.Evidence[DepthListener].ConsecutiveFailures != i {
			t.Fatalf("failure %d count = %d, want %d", i, got.Evidence[DepthListener].ConsecutiveFailures, i)
		}
	}

	got := store.RecordProbe("alpha", ProbeResult{
		AttemptedAt: at.Add(3 * time.Second),
		Depth:       DepthListener,
		Error:       "connection refused",
	})
	if got.State != StateUnreachable {
		t.Fatalf("third failure state = %q, want %q", got.State, StateUnreachable)
	}

	got = store.RecordProbe("alpha", ProbeResult{
		AttemptedAt: at.Add(4 * time.Second),
		Depth:       DepthListener,
		Success:     true,
	})
	if got.State != StateReachable {
		t.Fatalf("success state = %q, want %q", got.State, StateReachable)
	}
	if got.Evidence[DepthListener].ConsecutiveFailures != 0 {
		t.Fatalf("success failure count = %d, want 0", got.Evidence[DepthListener].ConsecutiveFailures)
	}
}

func TestStoreStartProbeReportsProbing(t *testing.T) {
	store := NewStore()

	got := store.StartProbe("alpha", DepthEndToEnd, time.Unix(300, 0))
	if got.State != StateProbing {
		t.Fatalf("started probe state = %q, want %q", got.State, StateProbing)
	}
	if got.Evidence[DepthEndToEnd].LastProbeAttempt != time.Unix(300, 0) {
		t.Fatalf("attempt timestamp = %v, want %v", got.Evidence[DepthEndToEnd].LastProbeAttempt, time.Unix(300, 0))
	}
}

func TestStoreDepthCompositionPreservesDeeperEvidence(t *testing.T) {
	store := NewStore()
	at := time.Unix(200, 0)

	for i := 0; i < 3; i++ {
		store.RecordProbe("alpha", ProbeResult{
			AttemptedAt: at.Add(time.Duration(i) * time.Second),
			Depth:       DepthSession,
			Error:       "session rejected",
		})
	}
	got := store.RecordProbe("alpha", ProbeResult{
		AttemptedAt: at.Add(4 * time.Second),
		Depth:       DepthListener,
		Success:     true,
	})

	if got.State != StateUnreachable {
		t.Fatalf("shallower success state = %q, want %q", got.State, StateUnreachable)
	}
	if got.Evidence[DepthSession].LastProbeError != "session rejected" {
		t.Fatalf("deeper error was erased: %#v", got.Evidence)
	}
	if got.Evidence[DepthSession].ConsecutiveFailures != 3 {
		t.Fatalf("deeper failure count = %d, want 3", got.Evidence[DepthSession].ConsecutiveFailures)
	}
	if got.Evidence[DepthListener].LastProbeOutcome != OutcomeSuccess {
		t.Fatalf("shallow success outcome = %q, want %q", got.Evidence[DepthListener].LastProbeOutcome, OutcomeSuccess)
	}
}

func TestStoreDepthCompositionPreservesDeeperSuccess(t *testing.T) {
	store := NewStore()

	store.RecordProbe("alpha", ProbeResult{Depth: DepthSession, Success: true})
	got := store.RecordProbe("alpha", ProbeResult{Depth: DepthListener, Success: true})

	if got.State != StateReachable {
		t.Fatalf("shallower success state = %q, want %q", got.State, StateReachable)
	}
	if got.Evidence[DepthSession].LastProbeOutcome != OutcomeSuccess {
		t.Fatalf("deeper success was erased: %#v", got.Evidence)
	}
}

// TestStoreShallowFailureIsNotMaskedByStaleDeeperSuccess guards the inverse of
// TestStoreDepthCompositionPreservesDeeperEvidence, and it is the case that
// matters most for this change.
//
// "Deepest evidence wins" is wrong when the deep evidence is stale. If an
// end-to-end probe succeeded earlier but the listener has since stopped
// answering, clients cannot connect — reporting reachable there would recreate
// the precise defect this change exists to eliminate: a healthy-looking status
// for a server nobody can reach.
//
// Reachability must be pessimistic: unreachable at any depth dominates.
func TestStoreShallowFailureIsNotMaskedByStaleDeeperSuccess(t *testing.T) {
	store := NewStore()
	at := time.Unix(300, 0)

	// A deep probe succeeded earlier.
	store.RecordProbe("alpha", ProbeResult{
		AttemptedAt: at,
		Depth:       DepthEndToEnd,
		Success:     true,
	})

	// The listener then fails past the threshold: nothing is answering now.
	var got Reachability
	for i := range FailureThreshold {
		got = store.RecordProbe("alpha", ProbeResult{
			AttemptedAt: at.Add(time.Duration(i+1) * time.Second),
			Depth:       DepthListener,
			Error:       "connection refused",
		})
	}

	if got.State != StateUnreachable {
		t.Fatalf("state = %q, want %q — a stale deeper success must not mask a failing listener", got.State, StateUnreachable)
	}
	// The earlier deep success must still be visible as evidence.
	if got.Evidence[DepthEndToEnd].LastProbeOutcome != OutcomeSuccess {
		t.Fatalf("deeper success evidence was erased: %#v", got.Evidence)
	}
}

func TestStoreProbeInProgressDoesNotMaskExistingUnreachableEvidence(t *testing.T) {
	store := NewStore()
	store.RecordDefinitiveFailure("alpha", DepthListener, time.Unix(300, 0), "listener bind failed")

	got := store.StartProbe("alpha", DepthEndToEnd, time.Unix(301, 0))
	if got.State != StateUnreachable {
		t.Fatalf("state = %q, want %q — an in-flight deeper probe must not mask listener failure", got.State, StateUnreachable)
	}
}

func TestStoreRemoveClearsState(t *testing.T) {
	store := NewStore()
	store.RecordProbe("alpha", ProbeResult{Depth: DepthListener, Success: true})
	store.Remove("alpha")

	got, ok := store.Get("alpha")
	if ok {
		t.Fatalf("removed server returned ok with %#v", got)
	}
	if got.State != StateUnprobed {
		t.Fatalf("removed state = %q, want %q", got.State, StateUnprobed)
	}
}

func TestStoreConcurrentReadWrite(t *testing.T) {
	store := NewStore()
	depths := []Depth{DepthListener, DepthSession, DepthEndToEnd}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				store.RecordProbe("alpha", ProbeResult{
					AttemptedAt: time.Unix(int64(i*100+j), 0),
					Depth:       depths[i%len(depths)],
					Success:     (i+j)%2 == 0,
				})
				store.Get("alpha")
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				store.Get("alpha")
			}
		}()
	}
	wg.Wait()
}
