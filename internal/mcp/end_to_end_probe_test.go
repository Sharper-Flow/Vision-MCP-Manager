package mcp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
)

func TestEndToEndProbeLeavesAdmissionAtBaselineAcrossCycles(t *testing.T) {
	var next atomic.Int32
	var deletes atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("probe-%d", next.Add(1)))
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			return
		}
		deletes.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	listener := httptest.NewServer(gateway)
	defer listener.Close()

	probe := NewEndToEndProbe()
	target := reachability.Target{Port: listener.Listener.Addr().(*net.TCPAddr).Port}
	baseline := gateway.Snapshot(0)
	const cycles = 10
	for cycle := 0; cycle < cycles; cycle++ {
		attempt := probe.Probe(context.Background(), target)
		if attempt.Disposition != reachability.DispositionSuccess || attempt.Err != nil {
			t.Fatalf("cycle %d probe = %+v; want reachable", cycle, attempt)
		}
		snapshot := gateway.Snapshot(0)
		if snapshot.CapacityUsed != baseline.CapacityUsed || len(snapshot.Rows) != len(baseline.Rows) {
			t.Fatalf("cycle %d admission residue: snapshot=%#v baseline=%#v", cycle, snapshot, baseline)
		}
	}
	if got := deletes.Load(); got != cycles {
		t.Fatalf("cleanup DELETE count = %d, want %d", got, cycles)
	}
}

func TestEndToEndProbeCleansUpAfterInitializeFailure(t *testing.T) {
	var deletes atomic.Int32
	var deleteSessionID atomic.Value
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", "failed-probe-session")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer backend.Close()
	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
			deleteSessionID.Store(r.Header.Get("Mcp-Session-Id"))
		}
		gateway.ServeHTTP(w, r)
	}))
	defer listener.Close()

	probe := NewEndToEndProbe()
	target := reachability.Target{Port: listener.Listener.Addr().(*net.TCPAddr).Port}
	baseline := gateway.Snapshot(0)
	if attempt := probe.Probe(context.Background(), target); attempt.Disposition != reachability.DispositionFailure || attempt.Err == nil {
		t.Fatalf("failed probe = %+v; want failure", attempt)
	}
	if deletes.Load() != 1 {
		t.Fatalf("cleanup DELETE count = %d, want 1", deletes.Load())
	}
	if got := deleteSessionID.Load().(string); got != "failed-probe-session" {
		t.Fatalf("cleanup session ID = %q, want failed-probe-session", got)
	}
	if snapshot := gateway.Snapshot(0); snapshot.CapacityUsed != baseline.CapacityUsed || len(snapshot.Rows) != len(baseline.Rows) {
		t.Fatalf("error-path admission residue: snapshot=%#v baseline=%#v", snapshot, baseline)
	}
}

func TestManagerRecordsEndToEndSuccess(t *testing.T) {
	var next atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("manager-probe-%d", next.Add(1)))
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	listener := httptest.NewServer(ListenerProbeMiddleware()(gateway))
	defer listener.Close()

	store := reachability.NewStore()
	manager := reachability.NewManager(store, reachability.NewVersionSelector(NewListenerProbe(), NewEndToEndProbe()))
	target := reachability.Target{Name: "manager-healthy", Port: listener.Listener.Addr().(*net.TCPAddr).Port, ProtocolVersion: reachability.ProtocolVersion2025_11_25}
	if err := manager.Start(context.Background(), target, 2*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	evidence := waitForProbeEvidence(t, store, target.Name, reachability.DepthEndToEnd)
	if evidence.LastProbeOutcome != reachability.OutcomeSuccess {
		t.Fatalf("end-to-end outcome = %q, want success", evidence.LastProbeOutcome)
	}
	if got := gateway.Snapshot(0).CapacityUsed; got != 0 {
		t.Fatalf("capacity after manager probe = %d, want zero", got)
	}
}

// With no prior deep evidence, capacity-denied deep probes complete
// inconclusively: they record the attempt, invent no deep outcome, and leave
// the aggregate state to the completed shallower evidence. The first-ever
// inconclusive deep result must select that completed evidence — never an
// empty state and never a permanently probing one.
func TestManagerCapacityDenialRecordsNoDeepOutcomeAndStaysReachable(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", "occupied")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer backend.Close()
	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	active := initializeManagedSession(t, gateway)
	listener := httptest.NewServer(ListenerProbeMiddleware()(gateway))
	defer listener.Close()

	store := reachability.NewStore()
	manager := reachability.NewManager(store, reachability.NewVersionSelector(NewListenerProbe(), NewEndToEndProbe()))
	target := reachability.Target{Name: "manager-at-cap", Port: listener.Listener.Addr().(*net.TCPAddr).Port, ProtocolVersion: reachability.ProtocolVersion2025_11_25}
	if err := manager.Start(context.Background(), target, 2*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	// Wait until a deep attempt actually ran (a denied probe leaves an
	// attempt timestamp but no completed outcome) and the aggregate state
	// settled back onto the completed listener evidence.
	sawDeepAttempt := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		value, ok := store.Get(target.Name)
		if ok {
			if value.State == "" {
				t.Fatal("denied deep probe left an empty aggregate state")
			}
			if evidence, ok := value.Evidence[reachability.DepthEndToEnd]; ok && !evidence.LastProbeAttempt.IsZero() {
				sawDeepAttempt = true
				if evidence.LastProbeOutcome != "" {
					t.Fatalf("denied deep probe invented outcome = %q, want none", evidence.LastProbeOutcome)
				}
				if evidence.ConsecutiveFailures != 0 {
					t.Fatalf("denied deep probe recorded failures = %d, want 0", evidence.ConsecutiveFailures)
				}
			}
			if sawDeepAttempt && value.State == reachability.StateReachable {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	if !sawDeepAttempt {
		t.Fatal("no denied deep attempt ran within the deadline")
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	deleteRequest.Header.Set("Mcp-Session-Id", active)
	gateway.ServeHTTP(httptest.NewRecorder(), deleteRequest)

	// Freed capacity lets the next deep probe reach the backend: only then
	// may a deep outcome exist, and it must be a success.
	evidence := waitForProbeEvidence(t, store, target.Name, reachability.DepthEndToEnd)
	if evidence.LastProbeOutcome != reachability.OutcomeSuccess || evidence.ConsecutiveFailures != 0 {
		t.Fatalf("post-capacity deep evidence = %#v, want successful evidence", evidence)
	}
}

func waitForProbeEvidence(t *testing.T, store *reachability.Store, name string, depth reachability.Depth) reachability.ProbeEvidence {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if value, ok := store.Get(name); ok {
			if evidence, ok := value.Evidence[depth]; ok && evidence.LastProbeOutcome != "" {
				return evidence
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no %s evidence for %q", depth, name)
	return reachability.ProbeEvidence{}
}

func TestEndToEndProbeTreatsCapacityDenialAsInconclusive(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", "existing")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer backend.Close()

	gateway := newTestManagedGateway(t, backend.URL+"/mcp", 1, backend.Client().Transport)
	active := initializeManagedSession(t, gateway)
	listener := httptest.NewServer(gateway)
	defer listener.Close()

	transport := &probeRecordingTransport{base: http.DefaultTransport}
	probe := &EndToEndProbe{Client: &http.Client{Transport: transport}}
	baseline := gateway.Snapshot(0)
	attempt := probe.Probe(context.Background(), reachability.Target{Port: listener.Listener.Addr().(*net.TCPAddr).Port})
	if attempt.Disposition != reachability.DispositionInconclusive || attempt.Err == nil {
		t.Fatalf("capacity-denied probe = %+v, want inconclusive with a denial reason", attempt)
	}
	if len(transport.statuses) != 1 || transport.statuses[0] != http.StatusTooManyRequests {
		t.Fatalf("capacity-denied HTTP statuses = %v, want [%d]", transport.statuses, http.StatusTooManyRequests)
	}
	if got := transport.deletes.Load(); got != 0 {
		t.Fatalf("capacity-denied cleanup DELETE count = %d, want 0", got)
	}
	if snapshot := gateway.Snapshot(0); snapshot.CapacityUsed != baseline.CapacityUsed || len(snapshot.Rows) != len(baseline.Rows) {
		t.Fatalf("capacity-denied residue: snapshot=%#v baseline=%#v", snapshot, baseline)
	}
	deleteRequest := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	deleteRequest.Header.Set("Mcp-Session-Id", active)
	gateway.ServeHTTP(httptest.NewRecorder(), deleteRequest)
}
