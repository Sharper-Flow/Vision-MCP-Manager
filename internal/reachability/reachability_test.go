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
			Disposition: DispositionFailure,
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
		Disposition: DispositionFailure,
		Error:       "connection refused",
	})
	if got.State != StateUnreachable {
		t.Fatalf("third failure state = %q, want %q", got.State, StateUnreachable)
	}

	got = store.RecordProbe("alpha", ProbeResult{
		AttemptedAt: at.Add(4 * time.Second),
		Depth:       DepthListener,
		Disposition: DispositionSuccess,
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
			Disposition: DispositionFailure,
			Error:       "session rejected",
		})
	}
	got := store.RecordProbe("alpha", ProbeResult{
		AttemptedAt: at.Add(4 * time.Second),
		Depth:       DepthListener,
		Disposition: DispositionSuccess,
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

	store.RecordProbe("alpha", ProbeResult{Depth: DepthSession, Disposition: DispositionSuccess})
	got := store.RecordProbe("alpha", ProbeResult{Depth: DepthListener, Disposition: DispositionSuccess})

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
		Disposition: DispositionSuccess,
	})

	// The listener then fails past the threshold: nothing is answering now.
	var got Reachability
	for i := range FailureThreshold {
		got = store.RecordProbe("alpha", ProbeResult{
			AttemptedAt: at.Add(time.Duration(i+1) * time.Second),
			Depth:       DepthListener,
			Disposition: DispositionFailure,
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
	store.RecordProbe("alpha", ProbeResult{Depth: DepthListener, Disposition: DispositionSuccess})
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
				disposition := DispositionFailure
				if (i+j)%2 == 0 {
					disposition = DispositionSuccess
				}
				store.RecordProbe("alpha", ProbeResult{
					AttemptedAt: time.Unix(int64(i*100+j), 0),
					Depth:       depths[i%len(depths)],
					Disposition: disposition,
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

func TestStoreInconclusiveProbeJoinsStartedProbeAndPreservesEvidence(t *testing.T) {
	store := NewStore()
	at := time.Unix(400, 0)

	for i := 0; i < FailureThreshold; i++ {
		store.RecordProbe("alpha", ProbeResult{
			AttemptedAt: at.Add(time.Duration(i) * time.Second),
			Depth:       DepthEndToEnd,
			Disposition: DispositionFailure,
			Error:       "end-to-end initialize: context deadline exceeded",
		})
	}
	store.StartProbe("alpha", DepthEndToEnd, at.Add(10*time.Second))
	got := store.RecordProbe("alpha", ProbeResult{
		AttemptedAt: at.Add(10 * time.Second),
		Depth:       DepthEndToEnd,
		Disposition: DispositionInconclusive,
	})

	if got.State != StateUnreachable {
		t.Fatalf("state after inconclusive deep attempt = %q, want %q", got.State, StateUnreachable)
	}
	deep := got.Evidence[DepthEndToEnd]
	if deep.LastProbeOutcome != OutcomeFailure {
		t.Fatalf("inconclusive attempt changed outcome = %q, want %q", deep.LastProbeOutcome, OutcomeFailure)
	}
	if deep.LastProbeError != "end-to-end initialize: context deadline exceeded" {
		t.Fatalf("inconclusive attempt changed error = %q", deep.LastProbeError)
	}
	if deep.ConsecutiveFailures != FailureThreshold {
		t.Fatalf("inconclusive attempt changed failure streak = %d, want %d", deep.ConsecutiveFailures, FailureThreshold)
	}
	if deep.LastProbeAttempt != at.Add(10*time.Second) {
		t.Fatalf("inconclusive attempt did not record its attempt time = %v", deep.LastProbeAttempt)
	}
	if got.State == StateProbing {
		t.Fatal("inconclusive completion left the depth permanently probing")
	}

	// A later completed recovery probe still clears the failure normally.
	recovered := store.RecordProbe("alpha", ProbeResult{
		AttemptedAt: at.Add(20 * time.Second),
		Depth:       DepthEndToEnd,
		Disposition: DispositionSuccess,
	})
	if recovered.State != StateReachable {
		t.Fatalf("state after later deep success = %q, want %q", recovered.State, StateReachable)
	}
	if recovered.Evidence[DepthEndToEnd].ConsecutiveFailures != 0 {
		t.Fatalf("recovery did not reset failure streak = %d", recovered.Evidence[DepthEndToEnd].ConsecutiveFailures)
	}
}

func TestStoreFirstEverInconclusiveDeepSelectsCompletedShallowerEvidence(t *testing.T) {
	store := NewStore()
	at := time.Unix(500, 0)

	store.RecordProbe("alpha", ProbeResult{
		AttemptedAt: at,
		Depth:       DepthListener,
		Disposition: DispositionSuccess,
	})
	store.StartProbe("alpha", DepthEndToEnd, at.Add(time.Second))
	got := store.RecordProbe("alpha", ProbeResult{
		AttemptedAt: at.Add(time.Second),
		Depth:       DepthEndToEnd,
		Disposition: DispositionInconclusive,
	})

	if got.State != StateReachable {
		t.Fatalf("first-ever inconclusive deep state = %q, want %q (completed listener evidence)", got.State, StateReachable)
	}
	if got.Evidence[DepthEndToEnd].LastProbeOutcome != "" {
		t.Fatalf("inconclusive attempt invented a deep outcome = %q", got.Evidence[DepthEndToEnd].LastProbeOutcome)
	}
}

func TestStoreFirstEverInconclusiveDeepWithNoCompletedEvidenceIsUnprobed(t *testing.T) {
	store := NewStore()
	at := time.Unix(600, 0)

	store.StartProbe("alpha", DepthEndToEnd, at)
	got := store.RecordProbe("alpha", ProbeResult{
		AttemptedAt: at,
		Depth:       DepthEndToEnd,
		Disposition: DispositionInconclusive,
	})

	if got.State != StateUnprobed {
		t.Fatalf("state = %q, want %q: an attempt that proved nothing must not report an empty or stuck state", got.State, StateUnprobed)
	}
}

// Completed-outcome evidence time is separate from latest attempt time: an
// inconclusive attempt records that it ran but must not redate the completed
// deep outcome that stands.
func TestStoreInconclusiveAttemptDoesNotRedateCompletedOutcomeTime(t *testing.T) {
	store := NewStore()
	successAt := time.Unix(1700000000, 0)
	deniedAt := successAt.Add(time.Hour)

	store.RecordProbe("alpha", ProbeResult{AttemptedAt: successAt, Depth: DepthEndToEnd, Disposition: DispositionSuccess})
	store.RecordProbe("alpha", ProbeResult{AttemptedAt: deniedAt.Add(-time.Second), Depth: DepthListener, Disposition: DispositionSuccess})
	store.StartProbe("alpha", DepthEndToEnd, deniedAt)
	got := store.RecordProbe("alpha", ProbeResult{AttemptedAt: deniedAt, Depth: DepthEndToEnd, Disposition: DispositionInconclusive, Error: "admission denied: HTTP 429"})

	deep := got.Evidence[DepthEndToEnd]
	if deep.LastProbeOutcome != OutcomeSuccess {
		t.Fatalf("inconclusive attempt replaced outcome = %q, want %q", deep.LastProbeOutcome, OutcomeSuccess)
	}
	if !deep.LastOutcomeAt.Equal(successAt) {
		t.Fatalf("inconclusive attempt redated completed outcome time = %v, want %v", deep.LastOutcomeAt, successAt)
	}
	if !deep.LastProbeAttempt.Equal(deniedAt) {
		t.Fatalf("inconclusive attempt time = %v, want %v", deep.LastProbeAttempt, deniedAt)
	}
	if got.State != StateReachable {
		t.Fatalf("state after inconclusive attempt = %q, want %q", got.State, StateReachable)
	}
}

// An in-flight attempt must not redate the completed failure evidence that
// stands while the attempt runs.
func TestStoreStartProbeDoesNotRedateCompletedFailureTime(t *testing.T) {
	store := NewStore()
	failedAt := time.Unix(1700000000, 0)
	retryAt := failedAt.Add(time.Hour)

	for i := range FailureThreshold {
		store.RecordProbe("alpha", ProbeResult{
			AttemptedAt: failedAt.Add(time.Duration(i) * time.Second),
			Depth:       DepthListener,
			Disposition: DispositionFailure,
			Error:       "connection refused",
		})
	}
	got := store.StartProbe("alpha", DepthListener, retryAt)

	deep := got.Evidence[DepthListener]
	if deep.LastProbeOutcome != OutcomeFailure {
		t.Fatalf("started probe replaced outcome = %q, want %q", deep.LastProbeOutcome, OutcomeFailure)
	}
	if !deep.LastOutcomeAt.Equal(failedAt.Add(time.Duration(FailureThreshold-1) * time.Second)) {
		t.Fatalf("started probe redated completed outcome time = %v", deep.LastOutcomeAt)
	}
	if !deep.LastProbeAttempt.Equal(retryAt) {
		t.Fatalf("started probe attempt time = %v, want %v", deep.LastProbeAttempt, retryAt)
	}
	if got.State != StateUnreachable {
		t.Fatalf("state while retry in flight = %q, want %q (unreachable dominates probing)", got.State, StateUnreachable)
	}
}

// A definitive failure is a completed outcome: it carries the outcome time of
// the attempt that proved it.
func TestStoreDefinitiveFailureRecordsOutcomeTime(t *testing.T) {
	store := NewStore()
	at := time.Unix(1700000000, 0)

	got := store.RecordDefinitiveFailure("alpha", DepthListener, at, "bind failed")
	evidence := got.Evidence[DepthListener]
	if evidence.LastProbeOutcome != OutcomeFailure {
		t.Fatalf("definitive failure outcome = %q, want %q", evidence.LastProbeOutcome, OutcomeFailure)
	}
	if !evidence.LastOutcomeAt.Equal(at) {
		t.Fatalf("definitive failure outcome time = %v, want %v", evidence.LastOutcomeAt, at)
	}
	if !evidence.LastProbeAttempt.Equal(at) {
		t.Fatalf("definitive failure attempt time = %v, want %v", evidence.LastProbeAttempt, at)
	}
}

// A completed outcome must never carry a zero evidence time: every recording
// path that sets an outcome sets when that outcome completed.
func TestStoreCompletedOutcomeAlwaysCarriesOutcomeTime(t *testing.T) {
	store := NewStore()
	at := time.Unix(700, 0)

	for _, result := range []ProbeResult{
		{AttemptedAt: at, Depth: DepthListener, Disposition: DispositionSuccess},
		{AttemptedAt: at.Add(time.Second), Depth: DepthSession, Disposition: DispositionFailure, Error: "session rejected"},
	} {
		got := store.RecordProbe("alpha", result)
		if evidence := got.Evidence[result.Depth]; evidence.LastOutcomeAt.IsZero() {
			t.Fatalf("depth %s completed with zero outcome time: %+v", result.Depth, evidence)
		}
	}
}
