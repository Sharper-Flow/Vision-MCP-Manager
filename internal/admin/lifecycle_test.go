package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
)

type testLifecycleAccessor struct {
	snapshot *SessionLifecycleSnapshot
}

func (a *testLifecycleAccessor) SessionLifecycleSnapshot(string) *SessionLifecycleSnapshot {
	return a.snapshot
}

func TestServerDiagnosticsExposeBoundedSafeLifecycleSnapshot(t *testing.T) {
	reg := newTestRegistry()
	if err := reg.Add("playwright", &config.ServerConfig{Port: 6287}); err != nil {
		t.Fatal(err)
	}
	accessor := &testLifecycleAccessor{snapshot: &SessionLifecycleSnapshot{
		BackendState: "ready",
		CapacityUsed: 1,
		CapacityMax:  6,
		Sessions: []SessionLifecycleRow{{
			SafeID: "12-char-hash", State: "active", AgeSeconds: 120,
			ApplicationIdleSeconds: 60, InFlight: 1, SSEConnections: 2,
			LifecycleReason: "initialized",
		}},
		Closed: []SessionLifecycleRow{{
			SafeID: "closed-hash", State: "closed", LifecycleReason: "idle_timeout",
		}},
		Omitted: 3,
	}}
	s := &Server{registry: reg, running: true, sessionLifecycleAccessor: accessor}
	req := httptest.NewRequest(http.MethodGet, "/v1/servers/playwright", nil)
	req.SetPathValue("name", "playwright")
	resp := httptest.NewRecorder()
	s.handleV1ServerDetail(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "raw-private-session-id") {
		t.Fatal("diagnostics exposed raw session ID")
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	lifecycle, ok := body["session_lifecycle"].(map[string]any)
	if !ok {
		t.Fatalf("session_lifecycle missing: %#v", body)
	}
	if lifecycle["capacity_used"] != float64(1) || lifecycle["capacity_max"] != float64(6) {
		t.Fatalf("capacity fields=%#v", lifecycle)
	}
	rows := lifecycle["sessions"].([]any)
	row := rows[0].(map[string]any)
	for _, key := range []string{"safe_id", "state", "age_seconds", "application_idle_seconds", "in_flight", "sse_connections", "lifecycle_reason"} {
		if _, exists := row[key]; !exists {
			t.Fatalf("missing lifecycle field %q in %#v", key, row)
		}
	}

	toolResult, err := s.toolList(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var list ListResponse
	if err := json.Unmarshal([]byte(toolResult.Content[0].Text), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Servers) != 1 || list.Servers[0].SessionLifecycle == nil {
		t.Fatalf("vision_list missing session lifecycle: %#v", list.Servers)
	}
}

func TestProjectReachabilitySelectsCompletedEvidenceOverInconclusiveAttempt(t *testing.T) {
	attempt := time.Unix(1700000000, 0).UTC()
	value := reachability.Reachability{
		State: reachability.StateReachable,
		Evidence: map[reachability.Depth]reachability.ProbeEvidence{
			reachability.DepthListener: {
				Depth:            reachability.DepthListener,
				LastProbeAttempt: attempt,
				LastOutcomeAt:    attempt,
				LastProbeOutcome: reachability.OutcomeSuccess,
			},
			reachability.DepthEndToEnd: {
				// A denied deep probe: newer attempt, no completed outcome.
				Depth:            reachability.DepthEndToEnd,
				LastProbeAttempt: attempt.Add(time.Minute),
			},
		},
	}

	details := projectReachability(value)
	if details.Reachability != string(reachability.StateReachable) {
		t.Fatalf("reachability = %q, want %q", details.Reachability, reachability.StateReachable)
	}
	if details.ProbeDepth != string(reachability.DepthListener) {
		t.Fatalf("probe depth = %q, want %q from completed evidence", details.ProbeDepth, reachability.DepthListener)
	}
	if details.LastProbeOutcome != string(reachability.OutcomeSuccess) {
		t.Fatalf("last probe outcome = %q, want %q", details.LastProbeOutcome, reachability.OutcomeSuccess)
	}

	// With no completed evidence at all, no probe status may be projected.
	onlyAttempt := reachability.Reachability{
		State: reachability.StateUnprobed,
		Evidence: map[reachability.Depth]reachability.ProbeEvidence{
			reachability.DepthEndToEnd: {
				Depth:            reachability.DepthEndToEnd,
				LastProbeAttempt: attempt,
			},
		},
	}
	if details := projectReachability(onlyAttempt); details.ProbeDepth != "" || details.LastProbeOutcome != "" {
		t.Fatalf("attempt-only evidence projected a probe status: %+v", details)
	}
}

// An inconclusive attempt that ran after a completed deep success must not
// redate that success: the projection carries the completed outcome and the
// time that outcome completed, never the denied attempt's time.
func TestProjectReachabilityInconclusiveAttemptDoesNotRedateCompletedOutcome(t *testing.T) {
	store := reachability.NewStore()
	successAt := time.Unix(1700000000, 0).UTC()
	deniedAt := successAt.Add(time.Hour)
	store.RecordProbe("backend", reachability.ProbeResult{Depth: reachability.DepthEndToEnd, AttemptedAt: successAt, Disposition: reachability.DispositionSuccess})
	store.RecordProbe("backend", reachability.ProbeResult{Depth: reachability.DepthListener, AttemptedAt: deniedAt.Add(-time.Second), Disposition: reachability.DispositionSuccess})
	store.StartProbe("backend", reachability.DepthEndToEnd, deniedAt)
	store.RecordProbe("backend", reachability.ProbeResult{Depth: reachability.DepthEndToEnd, AttemptedAt: deniedAt, Disposition: reachability.DispositionInconclusive})

	value, _ := store.Get("backend")
	projected := projectReachability(value)
	if projected.ProbeDepth == string(reachability.DepthEndToEnd) &&
		projected.LastProbeOutcome == string(reachability.OutcomeSuccess) &&
		projected.LastProbeAt != nil && projected.LastProbeAt.Equal(deniedAt) {
		t.Fatalf("inconclusive attempt projected as a new successful deep check: %+v", projected)
	}
	if projected.LastProbeAt == nil || projected.LastProbeAt.Equal(deniedAt) {
		t.Fatalf("projected probe time = %v, want the completed outcome time, not the denied attempt time", projected.LastProbeAt)
	}
	if projected.LastProbeOutcome != string(reachability.OutcomeSuccess) {
		t.Fatalf("projected outcome = %q, want %q (completed outcome stands)", projected.LastProbeOutcome, reachability.OutcomeSuccess)
	}
}

// An in-flight attempt must not redate the completed outcome either: while a
// retry runs, the projection keeps the standing failure and its time.
func TestProjectReachabilityInFlightAttemptDoesNotRedateCompletedOutcome(t *testing.T) {
	store := reachability.NewStore()
	failedAt := time.Unix(1700000000, 0).UTC()
	retryAt := failedAt.Add(time.Hour)
	for i := range reachability.FailureThreshold {
		store.RecordProbe("backend", reachability.ProbeResult{
			Depth:       reachability.DepthListener,
			AttemptedAt: failedAt.Add(time.Duration(i) * time.Second),
			Disposition: reachability.DispositionFailure,
			Error:       "connection refused",
		})
	}
	store.StartProbe("backend", reachability.DepthListener, retryAt)

	value, _ := store.Get("backend")
	projected := projectReachability(value)
	if projected.ProbeDepth != string(reachability.DepthListener) {
		t.Fatalf("projected depth = %q, want %q", projected.ProbeDepth, reachability.DepthListener)
	}
	if projected.LastProbeOutcome != string(reachability.OutcomeFailure) {
		t.Fatalf("projected outcome = %q, want %q", projected.LastProbeOutcome, reachability.OutcomeFailure)
	}
	completedAt := failedAt.Add(time.Duration(reachability.FailureThreshold-1) * time.Second)
	if projected.LastProbeAt == nil || !projected.LastProbeAt.Equal(completedAt) {
		t.Fatalf("projected time = %v, want completed outcome time %v", projected.LastProbeAt, completedAt)
	}
}

// A definitive failure projects as the standing failure at the time it was
// proven, and a later inconclusive attempt does not redate it.
func TestProjectReachabilityDefinitiveFailureKeepsProvenTime(t *testing.T) {
	store := reachability.NewStore()
	provenAt := time.Unix(1700000000, 0).UTC()
	later := provenAt.Add(time.Hour)
	store.RecordDefinitiveFailure("backend", reachability.DepthListener, provenAt, "bind failed")
	store.StartProbe("backend", reachability.DepthEndToEnd, later)
	store.RecordProbe("backend", reachability.ProbeResult{Depth: reachability.DepthEndToEnd, AttemptedAt: later, Disposition: reachability.DispositionInconclusive})

	value, _ := store.Get("backend")
	projected := projectReachability(value)
	if projected.Reachability != string(reachability.StateUnreachable) {
		t.Fatalf("projected reachability = %q, want %q", projected.Reachability, reachability.StateUnreachable)
	}
	if projected.ProbeDepth != string(reachability.DepthListener) {
		t.Fatalf("projected depth = %q, want %q (definitive failure outranks the inconclusive deeper attempt)", projected.ProbeDepth, reachability.DepthListener)
	}
	if projected.LastProbeOutcome != string(reachability.OutcomeFailure) {
		t.Fatalf("projected outcome = %q, want %q", projected.LastProbeOutcome, reachability.OutcomeFailure)
	}
	if projected.LastProbeAt == nil || !projected.LastProbeAt.Equal(provenAt) {
		t.Fatalf("projected time = %v, want proven failure time %v", projected.LastProbeAt, provenAt)
	}
}
