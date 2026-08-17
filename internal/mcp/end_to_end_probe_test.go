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
		reachable, err := probe.Probe(context.Background(), target)
		if err != nil || !reachable {
			t.Fatalf("cycle %d probe = %t, %v; want reachable", cycle, reachable, err)
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
	if reachable, err := probe.Probe(context.Background(), target); err == nil || reachable {
		t.Fatalf("failed probe = %t, %v; want failure", reachable, err)
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

func TestManagerRecordsCapacityDenialAsReachable(t *testing.T) {
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
	evidence := waitForProbeEvidence(t, store, target.Name, reachability.DepthEndToEnd)
	manager.Close()
	if evidence.LastProbeOutcome != reachability.OutcomeSuccess || evidence.ConsecutiveFailures != 0 {
		t.Fatalf("capacity-denied evidence = %#v, want successful reachable evidence", evidence)
	}
	if value, _ := store.Get(target.Name); value.State == reachability.StateUnreachable {
		t.Fatal("capacity denial made server unreachable")
	}
	deleteRequest := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	deleteRequest.Header.Set("Mcp-Session-Id", active)
	gateway.ServeHTTP(httptest.NewRecorder(), deleteRequest)
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

func TestEndToEndProbeTreatsCapacityDenialAsReachable(t *testing.T) {
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
	reachable, err := probe.Probe(context.Background(), reachability.Target{Port: listener.Listener.Addr().(*net.TCPAddr).Port})
	if err != nil || !reachable {
		t.Fatalf("capacity-denied probe = %t, %v; want reachable", reachable, err)
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
