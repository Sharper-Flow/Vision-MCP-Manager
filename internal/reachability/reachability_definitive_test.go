package reachability

import (
	"testing"
	"time"
)

// A terminal setup failure must report unreachable immediately, and must report
// the single failure that actually happened. The earlier implementation looped
// RecordProbe FailureThreshold times to trip the threshold, which reported
// consecutive_probe_failures=3 for one failure in the same admin payload
// operators use to diagnose an outage.
func TestRecordDefinitiveFailureIsImmediateAndReportsOneAttempt(t *testing.T) {
	s := NewStore()
	got := s.RecordDefinitiveFailure("srv", DepthListener, time.Now(), "add streamable proxy: bind failed")

	if got.State != StateUnreachable {
		t.Fatalf("state = %v, want %v", got.State, StateUnreachable)
	}
	ev := got.Evidence[DepthListener]
	if ev.ConsecutiveFailures != 1 {
		t.Fatalf("ConsecutiveFailures = %d, want 1 (must not fabricate attempts)", ev.ConsecutiveFailures)
	}
	if ev.LastProbeOutcome != OutcomeFailure {
		t.Fatalf("LastProbeOutcome = %v, want %v", ev.LastProbeOutcome, OutcomeFailure)
	}
	if ev.LastProbeError == "" {
		t.Fatal("LastProbeError is empty; operators need the cause")
	}
}

// A single transient probe failure must NOT be immediate — this is the contrast
// that proves the threshold still protects against flaky probes.
func TestRecordProbeSingleFailureStillBelowThreshold(t *testing.T) {
	s := NewStore()
	got := s.RecordProbe("srv", ProbeResult{Depth: DepthListener, AttemptedAt: time.Now(), Error: "timeout"})
	if got.State == StateUnreachable {
		t.Fatal("single transient failure reported unreachable; FailureThreshold no longer protects flaky probes")
	}
}

// A later success must clear a definitive failure, so a repaired server recovers.
func TestRecordDefinitiveFailureRecoversOnSuccess(t *testing.T) {
	s := NewStore()
	s.RecordDefinitiveFailure("srv", DepthListener, time.Now(), "bind failed")
	got := s.RecordProbe("srv", ProbeResult{Depth: DepthListener, AttemptedAt: time.Now(), Success: true})
	if got.State != StateReachable {
		t.Fatalf("state = %v, want %v (server must recover after repair)", got.State, StateReachable)
	}
	if got.Evidence[DepthListener].ConsecutiveFailures != 0 {
		t.Fatal("failure streak not reset on success")
	}
}
